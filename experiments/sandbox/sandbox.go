// Package sandbox provides a capability-free QuickJS WebAssembly embedding
// used to qualify the VibeShell generated-application sandbox decision.
//
// The guest is the pinned QuickJS wasm build from github.com/fastschema/qjs
// (see PROVENANCE.md for revision, build flags, checksum, and license). It is
// embedded with go:embed and instantiated with
// github.com/tetratelabs/wazero. Unlike the stock qjs adapter, this embedding
// provides:
//
//   - no host filesystem mounts (every path/fd WASI import is denied),
//   - no environment (environ/args imports return empty),
//   - no network (the module imports no socket functions; nothing is provided),
//   - a bounded guest memory cap (wazero WithMemoryLimitPages),
//   - a bounded QuickJS heap and interpreter stack (New_QJS limits),
//   - a bounded stdout/stderr capture (fd_write into a capped buffer),
//   - execution deadlines via context cancellation (WithCloseOnContextDone).
//
// The stock adapter's WithDirMount(cwd, "/") default is NOT used.
//
// The qjs guest ABI matters here: QJS_ToCString and QJS_JSONStringify return
// a guest pointer to an 8-byte packed (address<<32 | length) struct, not the
// packed value itself. resultString dereferences that pointer first; treating
// the pointer as the value reads arbitrary guest memory.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	_ "embed"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/sys"
)

//go:embed qjs.wasm
var quickJSWasm []byte

const (
	// DefaultMemoryPages is the engine-wide guest linear-memory cap in
	// 64 KiB pages (256 pages = 16 MiB). This is the hard wasm boundary.
	DefaultMemoryPages = 256
	// DefaultHeapLimit is the per-instance QuickJS heap cap (8 MiB).
	DefaultHeapLimit = 8 << 20
	// DefaultStackSize is the per-instance QuickJS interpreter stack cap.
	DefaultStackSize = 512 << 10
	// DefaultGCThreshold is the per-instance QuickJS GC threshold.
	DefaultGCThreshold = 1 << 20
	// DefaultMaxOutputBytes bounds captured stdout/stderr per instance.
	DefaultMaxOutputBytes = 1 << 20

	// maxResultStringBytes bounds any single guest-to-host string so a
	// corrupt or hostile length prefix cannot force a huge host allocation.
	maxResultStringBytes = 4 << 20

	// evalTypeGlobal evaluates in global scope (QuickJS JS_EVAL_TYPE_GLOBAL).
	evalTypeGlobal = 0
	// evalFlagStrict forces strict mode (QuickJS JS_EVAL_FLAG_STRICT).
	evalFlagStrict = 1 << 3
)

// Config configures one sandbox instance. The zero value is valid and
// applies the package defaults.
type Config struct {
	// HeapLimit is the QuickJS JS_SetMemoryLimit bound. The default
	// (DefaultHeapLimit) applies unless DisableHeapLimit is set; a nonzero
	// HeapLimit together with DisableHeapLimit is a configuration error.
	HeapLimit uint64
	// DisableHeapLimit removes the QuickJS heap bound, leaving only the
	// engine-wide wasm linear-memory cap. Used to prove the wasm boundary
	// binds on its own.
	DisableHeapLimit bool
	// StackSize is the QuickJS JS_SetMaxStackSize bound (0 = default).
	StackSize uint32
	// GCThreshold is the QuickJS JS_SetGCThreshold value (0 = default).
	GCThreshold uint64
	// MaxOutputBytes bounds captured stdout/stderr (0 = default).
	MaxOutputBytes int
}

func (c Config) withDefaults() (Config, error) {
	if c.DisableHeapLimit && c.HeapLimit != 0 {
		return c, errors.New("sandbox: HeapLimit and DisableHeapLimit are mutually exclusive")
	}
	if !c.DisableHeapLimit && c.HeapLimit == 0 {
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
	return c, nil
}

// Engine holds the shared wazero runtime, the compiled QuickJS module, and
// the capability-free host modules. The compiled module is immutable and
// safe to instantiate many times; the runtime carries the engine-wide memory
// cap and close-on-context-done. Host modules are instantiated exactly once
// per Engine: per-instance state (stdout capture) is routed through the
// instance registry keyed by the calling guest module.
type Engine struct {
	rt               wazero.Runtime
	compiled         wazero.CompiledModule
	idleTimeout      time.Duration
	idleEvictionCount int

	mu        sync.Mutex
	instances map[api.Module]*Instance
	nextID    uint64
	// resultMask holds a per-export bitmask applied to the first result:
	// 0xFFFFFFFF for i32 results (whose high 32 bits are undefined), and
	// MaxUint64 for i64 results such as JSValue handles.
	resultMask map[string]uint64
}

// NewEngine compiles the embedded QuickJS module once and installs the
// capability-free host modules. memoryPages is the engine-wide guest
// linear-memory cap in 64 KiB pages.
func NewEngine(ctx context.Context, memoryPages uint32) (*Engine, error) {
	if memoryPages == 0 {
		memoryPages = DefaultMemoryPages
	}
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithMemoryLimitPages(memoryPages).
		WithCloseOnContextDone(true))
	compiled, err := rt.CompileModule(ctx, quickJSWasm)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("compile quickjs module: %w", err)
	}
	e := &Engine{rt: rt, compiled: compiled, instances: make(map[api.Module]*Instance)}
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

func (e *Engine) register(inst *Instance) uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nextID++
	id := e.nextID
	e.instances[inst.module] = inst
	return id
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

// NewInstance instantiates an isolated QuickJS runtime against the engine's
// shared capability-free host modules. The instance owns its guest linear
// memory, heap, and output buffer; closing it releases all of them.
func (e *Engine) NewInstance(ctx context.Context, cfg Config) (*Instance, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	inst := &Instance{
		engine: e,
		cfg:    cfg,
		output: &boundedOutput{limit: cfg.MaxOutputBytes},
	}
	if err := inst.instantiate(ctx); err != nil {
		return nil, err
	}
	inst.lastActivity = time.Now()
	return inst, nil
}

// EvictIdle closes instances that have been idle for longer than the engine's
// configured idleTimeout. It returns the number of instances evicted.
// Idle-eviction is disabled if idleTimeout is 0.
func (e *Engine) EvictIdle() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.idleTimeout == 0 {
		return 0
	}
	now := time.Now()
	count := 0
	// Collect modules to evict to avoid modification during iteration issues
	var modulesToEvict []api.Module
	for mod, inst := range e.instances {
		if now.Sub(inst.lastActivity) > e.idleTimeout {
			modulesToEvict = append(modulesToEvict, mod)
		}
	}
	for _, mod := range modulesToEvict {
		inst := e.instances[mod]
		if inst != nil {
			// Close the instance directly, avoiding the unregister call
			// since we already hold the engine mutex and will delete below.
			inst.closeOnce.Do(func() {
				inst.closed = true
				_ = inst.module.Close(context.Background())
				inst.module = nil
			})
			delete(e.instances, mod)
			count++
		}
	}
	e.idleEvictionCount += count
	return count
}

// Instance is one isolated QuickJS runtime.
type Instance struct {
	engine *Engine
	cfg    Config
	output *boundedOutput

	module api.Module
	// qjsPtr is the QJSRuntime handle, ctxPtr the JSContext handle.
	qjsPtr uint32
	ctxPtr uint32

	closeOnce sync.Once
	closed    bool

	// lastActivity is the time the instance was last used for Eval.
	// Used by the engine's idle-eviction logic.
	lastActivity time.Time
}

// saveState serializes the instance's guest memory to a byte slice.
// The caller must ensure the instance is still open and its memory is valid.
func (i *Instance) saveState(ctx context.Context) ([]byte, error) {
	mem := i.module.Memory()
	size := mem.Size()
	// mem.Read(offset, length) returns the bytes at that offset for the given length.
	bytes, ok := mem.Read(0, size)
	if !ok {
		return nil, errors.New("failed to read guest memory")
	}
	data := make([]byte, len(bytes))
	copy(data, bytes)
	return data, nil
}

// restoreState replaces the instance's guest memory with saved data.
// The instance must have been created with the same memory page cap.
func (i *Instance) restoreState(ctx context.Context, saved []byte) error {
	mem := i.module.Memory()
	savedSize := uint32(len(saved))
	memSize := mem.Size()
	if savedSize > memSize {
		return errors.New("saved state larger than guest memory cap")
	}
	// Write saved state to the beginning of guest memory.
	ok := mem.Write(0, saved)
	if !ok {
		return errors.New("failed to write saved state to guest memory")
	}
	// Zero out any remaining memory beyond the saved state.
	if savedSize < memSize {
		zeroBuf := make([]byte, memSize-savedSize)
		ok := mem.Write(savedSize, zeroBuf)
		if !ok {
			return errors.New("failed to zero remaining guest memory")
		}
	}
	return nil
}

// IdleInfo holds eviction metadata for an instance.
type IdleInfo struct {
	LastActivity time.Time
}

// instantiate creates the guest module and the QuickJS runtime with the
// configured heap/stack limits. Host imports resolve to the engine-wide
// stubs; per-instance output is routed via the engine registry.
func (i *Instance) instantiate(ctx context.Context) error {
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

	// Create the QuickJS runtime with the configured limits. New_QJS takes
	// (memory_limit, max_stack_size, max_execution_time, gc_threshold) as
	// four size_t (i32 in wasm32) arguments and returns the QJSRuntime handle.
	qjsPtr, err := i.call(ctx, "New_QJS",
		uint64(i.cfg.HeapLimit), uint64(i.cfg.StackSize), 0, uint64(i.cfg.GCThreshold))
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
	i.lastActivity = time.Now()

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
	s, err := i.resultString(ctx, strPtr)
	if err != nil {
		return "", err
	}
	return s, nil
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
