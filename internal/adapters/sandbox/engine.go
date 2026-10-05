// Package sandbox adapts the qualified A04 QuickJS/Wasm bridge to the
// ports.AppSandbox contract: one engine compiled once, capability-free
// host modules, disposable isolated instances per run, explicit seeded
// randomness and simulated time, and bounded JSON in/out.
//
// Threat model: the guest is untrusted generated JavaScript. It receives
// no host filesystem, network, environment, process, or proxy capability;
// its only effects are a bounded stdout/stderr capture and a declarative
// JSON result. A run that traps, exits, or hangs is disposable and never
// shared across runs.
//
// The qjs guest ABI matters here: QJS_ToCString and QJS_JSONStringify
// return a guest pointer to an 8-byte packed (address<<32 | length)
// struct, not the packed value itself. resultString dereferences that
// pointer first; treating the pointer as the value reads arbitrary guest
// memory.
package sandbox

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/sys"
)

//go:embed qjs.wasm
var quickJSWasm []byte

const (
	// DefaultMemoryPages is the runtime-wide guest linear-memory cap in
	// 64 KiB pages (256 pages = 16 MiB). Every instance's linear memory is
	// individually bounded by this cap; it is the hard wasm boundary and
	// the outer bound around the per-run JS heap limit.
	DefaultMemoryPages = 256
	// DefaultHeapLimit is the per-run QuickJS heap cap (8 MiB) applied when
	// the run's SandboxLimits.MaxMemoryB is unset or larger.
	DefaultHeapLimit = 8 << 20
	// DefaultStackSize is the per-run QuickJS interpreter stack cap.
	DefaultStackSize = 512 << 10
	// DefaultGCThreshold is the per-run QuickJS GC threshold.
	DefaultGCThreshold = 1 << 20
	// DefaultMaxOutputBytes bounds captured stdout/stderr per run.
	DefaultMaxOutputBytes = 1 << 20
	// DefaultDeadline bounds a run that carries no SandboxLimits deadline.
	DefaultDeadline = 5 * time.Second
	// DefaultMaxEvents bounds declared effects+world reads+AI requests.
	DefaultMaxEvents = 128

	// maxResultStringBytes bounds any single guest-to-host string so a
	// corrupt or hostile length prefix cannot force a huge host allocation.
	maxResultStringBytes = 4 << 20
	// maxInputBytes bounds the marshaled state+event payload passed in.
	maxInputBytes = 4 << 20

	// evalTypeGlobal evaluates in global scope (QuickJS JS_EVAL_TYPE_GLOBAL).
	evalTypeGlobal = 0
	// evalFlagStrict forces strict mode (QuickJS JS_EVAL_FLAG_STRICT).
	evalFlagStrict = 1 << 3
)

// Config configures the engine and per-run defaults. The zero value is
// valid and applies the package defaults.
type Config struct {
	// HeapLimit is the QuickJS JS_SetMemoryLimit bound applied when a run
	// does not carry its own limit; default DefaultHeapLimit.
	HeapLimit uint64
	// StackSize is the QuickJS JS_SetMaxStackSize bound; 0 = default.
	StackSize uint32
	// GCThreshold is the QuickJS JS_SetGCThreshold value; 0 = default.
	GCThreshold uint64
	// MaxOutputBytes bounds captured stdout/stderr; 0 = default.
	MaxOutputBytes int
}

func (c Config) withDefaults() Config {
	if c.HeapLimit == 0 {
		c.HeapLimit = DefaultHeapLimit
	}
	if c.StackSize == 0 {
		c.StackSize = DefaultStackSize
	}
	if c.GCThreshold == 0 {
		c.GCThreshold = DefaultGCThreshold
	}
	if c.MaxOutputBytes == 0 {
		c.MaxOutputBytes = DefaultMaxOutputBytes
	}
	return c
}

// Engine holds the shared wazero runtime, the compiled QuickJS module, and
// the capability-free host modules. The compiled module is immutable and
// safe to instantiate many times; host modules are instantiated exactly
// once per Engine: per-instance state (output capture, simulated clock) is
// routed through the instance registry keyed by the calling guest module.
type Engine struct {
	rt       wazero.Runtime
	compiled wazero.CompiledModule
	cfg      Config

	mu        sync.Mutex
	instances map[api.Module]*Instance
	nextID    uint64
	// resultMask holds a per-export bitmask applied to the first result:
	// 0xFFFFFFFF for i32 results (whose high 32 bits are undefined), and
	// MaxUint64 for i64 results such as JSValue handles.
	resultMask map[string]uint64
}

// NewEngine compiles the embedded QuickJS module once and installs the
// capability-free host modules. memoryPages is the runtime-wide guest
// linear-memory cap in 64 KiB pages.
func NewEngine(ctx context.Context, memoryPages uint32, cfg Config) (*Engine, error) {
	if memoryPages == 0 {
		memoryPages = DefaultMemoryPages
	}
	cfg = cfg.withDefaults()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithMemoryLimitPages(memoryPages).
		WithCloseOnContextDone(true))
	compiled, err := rt.CompileModule(ctx, quickJSWasm)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("compile quickjs module: %w", err)
	}
	e := &Engine{rt: rt, compiled: compiled, cfg: cfg, instances: make(map[api.Module]*Instance)}
	e.resultMask = make(map[string]uint64, len(compiled.ExportedFunctions()))
	for name, def := range compiled.ExportedFunctions() {
		mask := ^uint64(0)
		if len(def.ResultTypes()) == 1 && def.ResultTypes()[0] == api.ValueTypeI32 {
			mask = 0xFFFFFFFF
		}
		e.resultMask[name] = mask
	}
	if err := e.instantiateEnv(ctx); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("instantiate env stub: %w", err)
	}
	if err := e.instantiateWASI(ctx); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("instantiate wasi stubs: %w", err)
	}
	return e, nil
}

// Close releases the engine runtime. Instances must be closed first.
func (e *Engine) Close(ctx context.Context) error {
	return e.rt.Close(ctx)
}

// Imports returns the module-qualified import names the guest requests.
// The qualification gate asserts these contain no filesystem, environment,
// or network capabilities beyond the denied stubs provided here.
func (e *Engine) Imports() []string {
	var out []string
	for _, imp := range e.compiled.ImportedFunctions() {
		module, name, isImport := imp.Import()
		if isImport {
			out = append(out, module+"."+name)
		}
	}
	return out
}

func (e *Engine) unregister(inst *Instance) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.instances, inst.module)
}

func (e *Engine) instanceFor(mod api.Module) *Instance {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.instances[mod]
}

// Instance is one isolated, disposable QuickJS runtime.
type Instance struct {
	engine *Engine
	output *boundedOutput
	// simNowMilli is the simulated clock this instance reports through the
	// WASI clock stub and the Date.now override.
	simNowMilli int64

	module api.Module
	// qjsPtr is the QJSRuntime handle, ctxPtr the JSContext handle.
	qjsPtr uint32
	ctxPtr uint32

	closeOnce sync.Once
	closed    bool
}

// NewInstance instantiates an isolated QuickJS runtime with the given JS
// heap/stack limits and simulated clock. The instance owns its guest linear
// memory, heap, and output buffer; closing it releases all of them.
func (e *Engine) NewInstance(ctx context.Context, heapLimit uint64, simNowMilli int64) (*Instance, error) {
	if heapLimit == 0 {
		heapLimit = e.cfg.HeapLimit
	}
	inst := &Instance{engine: e, simNowMilli: simNowMilli, output: &boundedOutput{limit: e.cfg.MaxOutputBytes}}
	if err := inst.instantiate(ctx, heapLimit); err != nil {
		return nil, err
	}
	return inst, nil
}

// instantiate creates the guest module and the QuickJS runtime with the
// configured heap/stack limits. Host imports resolve to the engine-wide
// stubs; per-instance output and clock are routed via the engine registry.
func (i *Instance) instantiate(ctx context.Context, heapLimit uint64) error {
	e := i.engine
	e.mu.Lock()
	e.nextID++
	id := e.nextID
	e.mu.Unlock()

	module, err := e.rt.InstantiateModule(ctx, e.compiled,
		wazero.NewModuleConfig().
			WithName(fmt.Sprintf("qjs-%d", id)).
			WithStartFunctions())
	if err != nil {
		return fmt.Errorf("instantiate quickjs module: %w", err)
	}
	i.module = module
	e.mu.Lock()
	e.instances[module] = i
	e.mu.Unlock()

	// New_QJS takes (memory_limit, max_stack_size, max_execution_time,
	// gc_threshold) as four i32 arguments and returns the QJSRuntime handle.
	qjsPtr, err := i.call(ctx, "New_QJS",
		heapLimit, uint64(e.cfg.StackSize), 0, e.cfg.GCThreshold)
	if err != nil {
		i.Close()
		return fmt.Errorf("create quickjs runtime: %w", err)
	}
	if qjsPtr == 0 {
		i.Close()
		return errors.New("create quickjs runtime: guest returned null handle")
	}
	i.qjsPtr = uint32(qjsPtr)

	ctxPtr, err := i.call(ctx, "QJS_GetContext", qjsPtr)
	if err != nil {
		i.Close()
		return fmt.Errorf("get quickjs context: %w", err)
	}
	if ctxPtr == 0 {
		i.Close()
		return errors.New("get quickjs context: guest returned null handle")
	}
	i.ctxPtr = uint32(ctxPtr)

	// Record the wasm stack top so the interpreter stack limit is enforced
	// against the real stack (mirrors the upstream pool discipline).
	if _, err := i.call(ctx, "QJS_UpdateStackTop", qjsPtr); err != nil {
		i.Close()
		return fmt.Errorf("update quickjs stack top: %w", err)
	}
	return nil
}

// Eval runs code in the instance and returns its string result. The caller's
// context bounds execution: cancelling it or reaching its deadline terminates
// the guest and closes the instance. On error the returned string is empty
// and err describes the failure (guest exception, deadline, or host fault).
func (i *Instance) Eval(ctx context.Context, code string) (string, error) {
	if i.closed {
		return "", errors.New("sandbox instance is closed")
	}

	codePtr, err := i.writeGuestString(ctx, code)
	if err != nil {
		return "", err
	}
	defer i.guestFree(ctx, codePtr)

	filenamePtr, err := i.writeGuestString(ctx, "<sandbox>")
	if err != nil {
		return "", err
	}
	defer i.guestFree(ctx, filenamePtr)

	opts, err := i.call(ctx, "QJS_CreateEvalOption",
		uint64(codePtr), 0, 0, uint64(filenamePtr), evalTypeGlobal|evalFlagStrict)
	if err != nil {
		return "", fmt.Errorf("create eval option: %w", err)
	}
	defer i.guestFree(ctx, uint32(opts))

	if _, err := i.call(ctx, "QJS_UpdateStackTop", uint64(i.qjsPtr)); err != nil {
		return "", i.evalError(ctx, err)
	}

	resultVal, err := i.call(ctx, "QJS_Eval", uint64(i.ctxPtr), opts)
	if err != nil {
		return "", i.evalError(ctx, err)
	}

	isExc, err := i.call(ctx, "QJS_IsException", resultVal)
	if err != nil {
		return "", err
	}

	if isExc != 0 {
		// QJS_Eval re-threw the pending exception; retrieve and stringify it.
		excVal, err := i.call(ctx, "JS_GetException", uint64(i.ctxPtr))
		if err != nil {
			return "", err
		}
		defer func() { _, _ = i.call(ctx, "QJS_FreeValue", uint64(i.ctxPtr), excVal) }()
		strPtr, err := i.call(ctx, "QJS_ToCString", uint64(i.ctxPtr), excVal)
		if err != nil {
			return "", err
		}
		s, serr := i.resultString(ctx, strPtr)
		if serr != nil {
			return "", fmt.Errorf("guest exception (unprintable: %v)", serr)
		}
		return "", fmt.Errorf("guest exception: %s", s)
	}

	defer func() { _, _ = i.call(ctx, "QJS_FreeValue", uint64(i.ctxPtr), resultVal) }()
	strPtr, err := i.call(ctx, "QJS_ToCString", uint64(i.ctxPtr), resultVal)
	if err != nil {
		return "", err
	}
	return i.resultString(ctx, strPtr)
}

// ParseJSON parses a host-supplied JSON string in the guest and returns its
// canonical JSON form. Malformed JSON is rejected with an error, exercising
// the host/guest JSON boundary.
func (i *Instance) ParseJSON(ctx context.Context, json string) (string, error) {
	if i.closed {
		return "", errors.New("sandbox instance is closed")
	}
	jsonPtr, err := i.writeGuestString(ctx, json)
	if err != nil {
		return "", err
	}
	defer i.guestFree(ctx, jsonPtr)

	val, err := i.call(ctx, "QJS_ParseJSON", uint64(i.ctxPtr), uint64(jsonPtr))
	if err != nil {
		return "", err
	}
	isExc, err := i.call(ctx, "QJS_IsException", val)
	if err != nil {
		return "", err
	}
	if isExc != 0 {
		excVal, err := i.call(ctx, "JS_GetException", uint64(i.ctxPtr))
		if err != nil {
			return "", err
		}
		defer func() { _, _ = i.call(ctx, "QJS_FreeValue", uint64(i.ctxPtr), excVal) }()
		strPtr, err := i.call(ctx, "QJS_ToCString", uint64(i.ctxPtr), excVal)
		if err != nil {
			return "", err
		}
		s, serr := i.resultString(ctx, strPtr)
		if serr != nil {
			return "", fmt.Errorf("guest exception (unprintable: %v)", serr)
		}
		return "", fmt.Errorf("guest exception: %s", s)
	}
	defer func() { _, _ = i.call(ctx, "QJS_FreeValue", uint64(i.ctxPtr), val) }()

	// Canonicalize through JSON.stringify so the host receives a bounded,
	// well-formed JSON string rather than an opaque handle.
	strPtr, err := i.call(ctx, "QJS_JSONStringify", uint64(i.ctxPtr), val)
	if err != nil {
		return "", err
	}
	return i.resultString(ctx, strPtr)
}

// Output returns the bounded stdout/stderr captured during Eval.
func (i *Instance) Output() string {
	return i.output.string()
}

// Close releases the instance. It is safe to call multiple times.
func (i *Instance) Close() error {
	i.closeOnce.Do(func() {
		i.closed = true
		if i.module != nil {
			i.engine.unregister(i)
			_ = i.module.Close(context.Background())
			i.module = nil
		}
	})
	return nil
}

// evalError classifies a failure from a guest call. A context cancellation or
// deadline surfaces as sys.ExitError once close-on-context-done closes the
// module; a guest proc_exit (for example after a denied capability attempt)
// surfaces the same way while the caller's context is still alive, and is
// reported as a guest exit instead.
func (i *Instance) evalError(ctx context.Context, err error) error {
	var exitErr *sys.ExitError
	if errors.As(err, &exitErr) {
		if ctx.Err() != nil {
			return fmt.Errorf("execution interrupted (module closed, exit code %d): %w",
				exitErr.ExitCode(), ctx.Err())
		}
		return fmt.Errorf("guest exited with code %d (instance closed)", exitErr.ExitCode())
	}
	return err
}

// resultString converts a QJS_ToCString/QJS_JSONStringify packed-pointer
// result to a Go string. The guest returns a pointer to an 8-byte
// (address<<32 | length) struct; this dereferences it, bounds-checks the
// length, copies the bytes out, and frees both the QuickJS string and the
// packed wrapper.
func (i *Instance) resultString(ctx context.Context, packedPtr uint64) (string, error) {
	if packedPtr == 0 {
		return "", errors.New("guest returned null string")
	}
	mem := i.module.Memory()
	packed, ok := mem.ReadUint64Le(uint32(packedPtr))
	if !ok {
		return "", fmt.Errorf("guest packed-pointer read out of bounds at %d", uint32(packedPtr))
	}
	ptr := uint32(packed >> 32)
	length := uint32(packed)
	if ptr == 0 {
		_, _ = i.call(ctx, "free", packedPtr)
		return "", errors.New("guest returned null string address")
	}
	if length > maxResultStringBytes {
		_, _ = i.call(ctx, "JS_FreeCString", uint64(i.ctxPtr), uint64(ptr))
		_, _ = i.call(ctx, "free", packedPtr)
		return "", fmt.Errorf("guest string too large: %d bytes", length)
	}
	bytes, ok := mem.Read(ptr, length)
	if !ok {
		return "", fmt.Errorf("guest string read out of bounds: ptr=%d len=%d", ptr, length)
	}
	out := make([]byte, len(bytes))
	copy(out, bytes)
	_, _ = i.call(ctx, "JS_FreeCString", uint64(i.ctxPtr), uint64(ptr))
	_, _ = i.call(ctx, "free", packedPtr)
	return string(out), nil
}

// call invokes an exported guest function and returns its first result.
// i32 results are masked to 32 bits: the wasm spec leaves the high half of
// a 32-bit result undefined, and the QuickJS predicates (notably
// QJS_IsException) have been observed returning nonzero garbage there,
// which would turn every pointer-valued result into a phantom exception.
func (i *Instance) call(ctx context.Context, name string, args ...uint64) (uint64, error) {
	fn := i.module.ExportedFunction(name)
	if fn == nil {
		return 0, fmt.Errorf("exported function %q not found", name)
	}
	results, err := fn.Call(ctx, args...)
	if err != nil {
		return 0, err
	}
	if len(results) == 0 {
		return 0, nil
	}
	return results[0] & i.engine.resultMask[name], nil
}

// writeGuestString allocates len(s)+1 bytes in guest memory, writes s with a
// NUL terminator, and returns the guest pointer.
func (i *Instance) writeGuestString(ctx context.Context, s string) (uint32, error) {
	ptr, err := i.call(ctx, "malloc", uint64(len(s)+1))
	if err != nil {
		return 0, i.evalError(ctx, err)
	}
	p := uint32(ptr)
	if p == 0 {
		return 0, errors.New("guest malloc returned null")
	}
	if !i.module.Memory().Write(p, append([]byte(s), 0)) {
		return 0, fmt.Errorf("guest write out of bounds at %d", p)
	}
	return p, nil
}

func (i *Instance) guestFree(ctx context.Context, ptr uint32) {
	if ptr == 0 {
		return
	}
	_, _ = i.call(ctx, "free", uint64(ptr))
}
