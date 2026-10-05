package sandbox

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestEngine builds an engine with default limits for functional tests.
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	engine, err := NewEngine(context.Background(), DefaultMemoryPages)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	return engine
}

func newInstance(t *testing.T, engine *Engine, cfg Config) *Instance {
	t.Helper()
	inst, err := engine.NewInstance(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close() })
	return inst
}

// TestBasicEval is the sanity gate: pure computation, string results, and
// stdout capture all work through the capability-free embedding.
func TestBasicEval(t *testing.T) {
	engine := newTestEngine(t)
	inst := newInstance(t, engine, Config{})

	cases := []struct {
		code string
		want string
	}{
		{"1 + 1", "2"},
		{"'hello' + ' ' + 'world'", "hello world"},
		{"[3,1,2].sort().join(',')", "1,2,3"},
		{"JSON.stringify({a:1,b:[2,3]})", `{"a":1,"b":[2,3]}`},
		{"(function(){let s=0;for(let i=0;i<1000;i++)s+=i;return s;})()", "499500"},
	}
	for _, tc := range cases {
		got, err := inst.Eval(context.Background(), tc.code)
		if err != nil {
			t.Errorf("Eval(%q) error: %v", tc.code, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Eval(%q) = %q, want %q", tc.code, got, tc.want)
		}
	}

	// stdout capture through the bounded fd_write stub.
	if _, err := inst.Eval(context.Background(), "console.log('to stdout'); print('via print')"); err != nil {
		t.Fatalf("console.log eval: %v", err)
	}
	out := inst.Output()
	if !strings.Contains(out, "to stdout") || !strings.Contains(out, "via print") {
		t.Errorf("output capture = %q, want both log lines", out)
	}
}

// TestInfiniteLoopDeadline verifies an unbounded guest loop is terminated by
// the host deadline and that the interrupted instance closes without hanging.
func TestInfiniteLoopDeadline(t *testing.T) {
	engine := newTestEngine(t)
	inst := newInstance(t, engine, Config{})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := inst.Eval(ctx, "while(true){}")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("infinite loop returned no error")
	}
	if elapsed > 5*time.Second {
		t.Errorf("deadline took %v to terminate the guest; want prompt cancellation", elapsed)
	}
	t.Logf("infinite loop terminated after %v with: %v", elapsed.Round(time.Millisecond), err)

	// The interrupted instance must close promptly.
	closeStart := time.Now()
	if err := inst.Close(); err != nil {
		t.Errorf("Close after interruption: %v", err)
	}
	if d := time.Since(closeStart); d > 2*time.Second {
		t.Errorf("Close after interruption took %v; want prompt", d)
	}

	// A fresh instance in the same engine is unaffected by the interruption.
	inst2 := newInstance(t, engine, Config{})
	if got, err := inst2.Eval(context.Background(), "40+2"); err != nil || got != "42" {
		t.Errorf("fresh instance after interruption: got %q err %v", got, err)
	}
}

// TestAllocationExhaustion verifies guest allocation is bounded by the
// QuickJS heap limit: the guest fails with an out-of-memory error while the
// host process stays stable.
func TestAllocationExhaustion(t *testing.T) {
	engine := newTestEngine(t)
	// A small heap cap makes the bound easy to hit.
	inst := newInstance(t, engine, Config{HeapLimit: 4 << 20})

	_, err := inst.Eval(context.Background(), `
		let parts = [];
		while (true) {
			parts.push(new Array(65536).fill("x"));
		}
	`)
	if err == nil {
		t.Fatal("unbounded allocation returned no error")
	}
	t.Logf("allocation exhaustion bounded: %v", err)

	// The instance survived and can still run small computations.
	if got, err := inst.Eval(context.Background(), "1+1"); err != nil || got != "2" {
		t.Errorf("instance after OOM: got %q err %v", got, err)
	}
}

// TestWasmMemoryCap verifies the engine-wide linear-memory cap bounds guest
// allocation even when the QuickJS heap limit is disabled.
func TestWasmMemoryCap(t *testing.T) {
	// A dedicated engine with a tiny 3 MiB (48-page) memory cap.
	engine, err := NewEngine(context.Background(), 48)
	if err != nil {
		t.Fatalf("NewEngine(48 pages): %v", err)
	}
	defer func() { _ = engine.Close(context.Background()) }()

	inst, err := engine.NewInstance(context.Background(), Config{DisableHeapLimit: true})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	defer func() { _ = inst.Close() }()

	_, err = inst.Eval(context.Background(), `
		let parts = [];
		while (true) {
			parts.push(new Array(1048576).fill("y"));
		}
	`)
	if err == nil {
		t.Fatal("allocation beyond wasm memory cap returned no error")
	}
	t.Logf("wasm memory cap bounded allocation: %v", err)
}

// TestDeepRecursion verifies unbounded recursion is bounded and cannot
// crash the host. In this build the binding constraint is wazero's native
// stack guard (a wasm out-of-bounds trap), which fires before QuickJS's own
// interpreter stack limit: the offending instance is terminated, the host
// and peer instances are unaffected, and a fresh instance keeps working.
// Instance reuse after such a trap is therefore not expected; fast failure
// of the trapped instance is.
func TestDeepRecursion(t *testing.T) {
	engine := newTestEngine(t)
	inst := newInstance(t, engine, Config{})

	start := time.Now()
	_, err := inst.Eval(context.Background(), "(function f(){ return 1 + f(); })()")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("unbounded recursion returned no error")
	}
	if elapsed > 10*time.Second {
		t.Errorf("recursion took %v to terminate; want a prompt bound", elapsed)
	}
	t.Logf("deep recursion bounded after %v: %v", elapsed.Round(time.Millisecond), err)

	// The trapped instance is dead and must fail fast, not hang.
	reuseStart := time.Now()
	if _, err := inst.Eval(context.Background(), "1+1"); err == nil {
		t.Error("trapped instance accepted work after a stack trap; want fast failure")
	}
	if d := time.Since(reuseStart); d > 5*time.Second {
		t.Errorf("reuse of trapped instance took %v; want fast failure", d)
	}

	// A fresh instance in the same engine is unaffected.
	fresh := newInstance(t, engine, Config{})
	if got, err := fresh.Eval(context.Background(), "1+1"); err != nil || got != "2" {
		t.Errorf("fresh instance after stack trap: got %q err %v", got, err)
	}
}

// TestMalformedJSON verifies malformed JSON is rejected safely at the
// host/guest boundary in both directions.
func TestMalformedJSON(t *testing.T) {
	engine := newTestEngine(t)
	inst := newInstance(t, engine, Config{})

	// Host -> guest: malformed JSON passed to the guest parser.
	badInputs := []string{
		`{"a": }`,
		`[1, 2,`,
		`{"a": 1} extra`,
		`{unquoted: 1}`,
		``,
	}
	for _, bad := range badInputs {
		_, err := inst.ParseJSON(context.Background(), bad)
		if err == nil {
			t.Errorf("ParseJSON(%q) accepted malformed JSON", bad)
		} else {
			t.Logf("ParseJSON(%q) rejected: %v", bad, err)
		}
	}

	// Guest -> host: a circular structure must not crash stringification.
	_, err := inst.Eval(context.Background(), `
		let o = {}; o.self = o; JSON.stringify(o);
	`)
	if err == nil {
		t.Error("circular JSON.stringify returned no error")
	} else {
		t.Logf("circular structure rejected: %v", err)
	}

	// Valid JSON still parses.
	got, err := inst.ParseJSON(context.Background(), `{"a":1,"b":[2,3]}`)
	if err != nil {
		t.Errorf("ParseJSON valid: %v", err)
	} else {
		t.Logf("valid JSON parsed to: %s", got)
	}
}

// TestForbiddenCapabilities verifies the module requests no network imports
// and that guest attempts to read files, open sockets, or read the
// environment all fail.
func TestForbiddenCapabilities(t *testing.T) {
	engine := newTestEngine(t)

	// Import audit: no socket/network imports, and no unexpected modules.
	// Each forbidden attempt runs in a fresh instance because a denied
	// capability may terminate the offending guest (proc_exit), which
	// closes that instance by design; the engine and peers must survive.
	imports := engine.Imports()
	for _, imp := range imports {
		module, name := splitImport(imp)
		if strings.HasPrefix(name, "sock_") {
			t.Errorf("module imports network symbol %s.%s", module, name)
		}
		if module != "wasi_snapshot_preview1" && module != "env" {
			t.Errorf("module imports from unexpected module %s.%s", module, name)
		}
	}
	t.Logf("import audit: %d imports, none from network symbols", len(imports))

	// Each attempt runs in a fresh instance: a denied capability may
	// terminate the offending guest via proc_exit (closing that instance
	// by design), which must not affect the engine or its peers.
	try := func(code string) (string, error) {
		fresh, err := engine.NewInstance(context.Background(), Config{})
		if err != nil {
			t.Fatalf("NewInstance: %v", err)
		}
		defer func() { _ = fresh.Close() }()
		return fresh.Eval(context.Background(), code)
	}

	// Filesystem reads must fail.
	fsAttempts := []string{
		`os.open("/etc/passwd", 0)`,
		`std.loadFile("/etc/passwd")`,
		`std.loadFile("/workspace/experiments/sandbox/qjs.wasm")`,
		`os.read(0, new Uint8Array(16))`,
		`os.stat("/")`,
	}
	for _, code := range fsAttempts {
		_, err := try(code)
		if err == nil {
			t.Errorf("filesystem attempt %q succeeded; want failure", code)
		} else {
			t.Logf("fs attempt %q denied: %v", code, err.Error())
		}
	}

	// Environment access must yield nothing.
	envAttempts := []string{
		`JSON.stringify(os.environ())`,
		`os.getenviron().length`,
	}
	for _, code := range envAttempts {
		got, err := try(code)
		if err != nil {
			t.Logf("env attempt %q errored (acceptable): %v", code, err.Error())
			continue
		}
		if got != "[]" && got != "0" && got != "null" && got != "undefined" {
			t.Errorf("env attempt %q leaked environment: %q", code, got)
		} else {
			t.Logf("env attempt %q returned %q (empty)", code, got)
		}
	}

	// Network constructors must not exist.
	netAttempts := []string{
		`typeof Socket`,
		`typeof fetch`,
		`typeof XMLHttpRequest`,
		`typeof require`,
		`typeof process`,
		`typeof globalThis.require`,
	}
	for _, code := range netAttempts {
		got, err := try(code)
		if err != nil {
			t.Logf("net probe %q errored: %v", code, err.Error())
			continue
		}
		if got != "undefined" {
			t.Errorf("net probe %q = %q; want undefined", code, got)
		}
	}

	// A fresh instance after all denied attempts still works.
	inst := newInstance(t, engine, Config{})
	if got, err := inst.Eval(context.Background(), "40+2"); err != nil || got != "42" {
		t.Errorf("fresh instance after denials: got %q err %v", got, err)
	}
}

// TestInstanceIsolation verifies two runtimes cannot observe each other's
// state and that a closed or interrupted instance cannot affect another.
func TestInstanceIsolation(t *testing.T) {
	engine := newTestEngine(t)
	a := newInstance(t, engine, Config{})
	b := newInstance(t, engine, Config{})

	if _, err := a.Eval(context.Background(), "globalThis.secret = 'alpha-42'"); err != nil {
		t.Fatalf("set global in a: %v", err)
	}
	if got, err := b.Eval(context.Background(), "typeof globalThis.secret"); err != nil || got != "undefined" {
		t.Errorf("b observed a's global: got %q err %v", got, err)
	}

	// Guest memory is not shared.
	if _, err := a.Eval(context.Background(), "globalThis.big = new Array(100000).fill('a')"); err != nil {
		t.Fatalf("allocate in a: %v", err)
	}
	if got, err := b.Eval(context.Background(), "typeof globalThis.big"); err != nil || got != "undefined" {
		t.Errorf("b observed a's allocation: got %q err %v", got, err)
	}

	// Closing one instance leaves the other fully functional.
	if err := a.Close(); err != nil {
		t.Fatalf("close a: %v", err)
	}
	if got, err := b.Eval(context.Background(), "2*21"); err != nil || got != "42" {
		t.Errorf("b after closing a: got %q err %v", got, err)
	}

	// An interrupted instance does not affect a peer.
	c := newInstance(t, engine, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	if _, err := c.Eval(ctx, "while(true){}"); err == nil {
		t.Fatal("infinite loop in c returned no error")
	}
	cancel()
	if got, err := b.Eval(context.Background(), "3*14"); err != nil || got != "42" {
		t.Errorf("b after interrupting c: got %q err %v", got, err)
	}
}

// TestColdWarmLatency measures one-time engine compilation, the first
// (cold) instance, and warm instantiation over N=100.
func TestColdWarmLatency(t *testing.T) {
	const n = 100

	start := time.Now()
	engine, err := NewEngine(context.Background(), DefaultMemoryPages)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer func() { _ = engine.Close(context.Background()) }()
	compileElapsed := time.Since(start)
	t.Logf("engine compile (one-time): %v", compileElapsed.Round(time.Millisecond))

	// Cold instance: the first instantiation pays guest module setup.
	start = time.Now()
	cold, err := engine.NewInstance(context.Background(), Config{})
	if err != nil {
		t.Fatalf("cold NewInstance: %v", err)
	}
	coldElapsed := time.Since(start)
	_ = cold.Close()
	t.Logf("cold instance (first): %v", coldElapsed.Round(time.Millisecond))

	// Warm instances: N sequential instantiations.
	start = time.Now()
	for i := 0; i < n; i++ {
		inst, err := engine.NewInstance(context.Background(), Config{})
		if err != nil {
			t.Fatalf("warm NewInstance %d: %v", i, err)
		}
		_ = inst.Close()
	}
	warmTotal := time.Since(start)
	t.Logf("warm instances: %d in %v (avg %v)", n, warmTotal.Round(time.Millisecond), (warmTotal / n).Round(time.Microsecond))

	// Per-instance guest memory footprint.
	inst := newInstance(t, engine, Config{})
	if _, err := inst.Eval(context.Background(), "globalThis.x = new Array(1000).fill('z')"); err != nil {
		t.Fatalf("alloc: %v", err)
	}
	t.Logf("guest memory after small alloc: %d bytes", inst.module.Memory().Size())
}

// TestConcurrency runs 100 simultaneous instances, each evaluating a
// representative workload, and reports total time and peak host memory.
func TestConcurrency(t *testing.T) {
	const n = 100

	engine := newTestEngine(t)

	var m0 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)

	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			inst, err := engine.NewInstance(context.Background(), Config{})
			if err != nil {
				errs <- fmt.Errorf("instance %d: %w", i, err)
				return
			}
			defer func() { _ = inst.Close() }()
			code := fmt.Sprintf(
				"let state={count:%d}; for(let i=0;i<100;i++)state.count+=i; state.count", i)
			if _, err := inst.Eval(context.Background(), code); err != nil {
				errs <- fmt.Errorf("instance %d eval: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	elapsed := time.Since(start)

	var errCount int
	for err := range errs {
		if errCount < 5 {
			t.Errorf("concurrent instance: %v", err)
		}
		errCount++
	}

	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	t.Logf("concurrency: %d instances in %v, %d errors", n, elapsed.Round(time.Millisecond), errCount)
	t.Logf("host heap alloc: %d MiB before, %d MiB after (delta %d MiB)",
		m0.HeapAlloc>>20, m1.HeapAlloc>>20, (int64(m1.HeapAlloc)-int64(m0.HeapAlloc))>>20)
	t.Logf("host total alloc: %d MiB, sys: %d MiB", m1.TotalAlloc>>20, m1.Sys>>20)
}

// TestLongRunningGuests runs N simultaneous long-running guests (each
// executing a repetitive computation with a deadline) and measures total
// time and peak host memory. This exercises the engine under sustained
// load rather than just instantiation.
func TestLongRunningGuests(t *testing.T) {
	const n = 50

	engine := newTestEngine(t)

	var m0 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)

	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			inst, err := engine.NewInstance(context.Background(), Config{})
			if err != nil {
				errs <- fmt.Errorf("instance %d: %w", i, err)
				return
			}
			defer func() { _ = inst.Close() }()
			// Run representative workload 200 times within the deadline.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			for j := 0; j < 200; j++ {
				select {
				case <-ctx.Done():
					return
				default:
				}
				_, err := inst.Eval(ctx, "1+1")
				if err != nil {
					errs <- fmt.Errorf("instance %d eval: %w", i, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	elapsed := time.Since(start)

	var errCount int
	for err := range errs {
		if errCount < 5 {
			t.Errorf("long-running guest: %v", err)
		}
		errCount++
	}

	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	t.Logf("long-running guests: %d instances in %v, %d errors", n, elapsed.Round(time.Millisecond), errCount)
	t.Logf("host heap alloc: %d MiB before, %d MiB after (delta %d MiB)",
		m0.HeapAlloc>>20, m1.HeapAlloc>>20, (int64(m1.HeapAlloc)-int64(m0.HeapAlloc))>>20)
	t.Logf("host total alloc: %d MiB, sys: %d MiB", m1.TotalAlloc>>20, m1.Sys>>20)
}

// TestIdleEviction verifies that the engine's EvictIdle closes instances
// that have been idle for longer than the configured idleTimeout. When
// idleTimeout is 0, eviction is a no-op.
func TestIdleEviction(t *testing.T) {
	const idleTimeout = 0 // test disabled eviction first

	engine := newTestEngine(t)
	engine.idleTimeout = idleTimeout

	// Test that EvictIdle with timeout=0 returns 0 (no eviction).
	evicted := engine.EvictIdle()
	if evicted != 0 {
		t.Errorf("expected 0 evictions with timeout=0, got %d", evicted)
	}

	// Now test actual eviction with a very short timeout.
	engine3 := newTestEngine(t)
	engine3.idleTimeout = 1 * time.Millisecond

	// Create an instance and don't use it.
	inst3 := newInstance(t, engine3, Config{})
	// Wait for the timeout to elapse.
	time.Sleep(5 * time.Millisecond)

	// Evict should catch this instance.
	evicted = engine3.EvictIdle()
	if evicted != 1 {
		t.Errorf("expected 1 eviction with short timeout, got %d", evicted)
	}

	// The instance should be closed.
	if engine3.instanceFor(inst3.module) != nil {
		t.Error("instance should have been evicted")
	}
}

// TestRestoreFromState verifies that an instance's guest memory can be
// saved and restored, and that the restored instance produces the same
// evaluation results.
func TestRestoreFromState(t *testing.T) {
	engine := newTestEngine(t)
	inst := newInstance(t, engine, Config{})

	// Run some code to establish guest state.
	inst.Eval(context.Background(), "globalThis.counter = 42")
	inst.Eval(context.Background(), "globalThis.counter = globalThis.counter + 10")

	// Save the instance's guest memory state.
	saved, err := inst.saveState(context.Background())
	if err != nil {
		t.Fatalf("saveState: %v", err)
	}
	t.Logf("saved state size: %d bytes", len(saved))

	// Close the original instance.
	inst.Close()

	// Create a new instance in the same engine.
	inst2, err := engine.NewInstance(context.Background(), Config{})
	if err != nil {
		t.Fatalf("NewInstance after close: %v", err)
	}
	defer func() { _ = inst2.Close() }()

	// Restore the saved state onto the new instance.
	if err := inst2.restoreState(context.Background(), saved); err != nil {
		t.Fatalf("restoreState: %v", err)
	}

	// The restored instance should have the same guest state.
	got, err := inst2.Eval(context.Background(), "globalThis.counter")
	if err != nil || got != "52" {
		t.Errorf("restored counter: got %q err %v", got, err)
	}

	// Run additional computation and verify it persists.
	inst2.Eval(context.Background(), "globalThis.counter = globalThis.counter + 100")
	got, err = inst2.Eval(context.Background(), "globalThis.counter")
	if err != nil || got != "152" {
		t.Errorf("restored counter after increment: got %q err %v", got, err)
	}
}

// TestEvictionLeakCycle repeatedly opens, closes, and evicts instances
// over multiple cycles, checking that host memory does not grow
// unboundedly.
func TestEvictionLeakCycle(t *testing.T) {
	const cycles = 10
	const perCycle = 5

	engine := newTestEngine(t)
	engine.idleTimeout = 1 * time.Millisecond // short timeout for eviction

	var mBefore runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&mBefore)
	beforeAlloc := mBefore.HeapAlloc

	for cycle := 0; cycle < cycles; cycle++ {
		// Create instances.
		var wg sync.WaitGroup
		errs := make(chan error, perCycle)
		for i := 0; i < perCycle; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				inst, err := engine.NewInstance(context.Background(), Config{})
				if err != nil {
					errs <- fmt.Errorf("NewInstance: %w", err)
					return
				}
				defer func() { _ = inst.Close() }()
				// Exercise the instance briefly.
				inst.Eval(context.Background(), "1+1")
			}()
		}
		wg.Wait()
		close(errs)

		// Evict idle instances (they were just used, so none should be evicted
		// if idleTimeout is short but we just used them).
		engine.EvictIdle()
	}

	runtime.GC()
	var mAfter runtime.MemStats
	runtime.ReadMemStats(&mAfter)
	delta := mAfter.HeapAlloc - beforeAlloc
	// Handle uint64 wraparound: if mAfter < beforeAlloc, GC freed memory,
	// so delta should be treated as 0 (no growth).
	if mAfter.HeapAlloc < beforeAlloc {
		delta = 0
	}
	t.Logf("after %d cycles of open/close/evict: heap alloc delta %d MiB", cycles*perCycle, delta>>20)
	// Allow some growth but not unbounded: 50 MiB maximum delta.
	maxDelta := uint64(50) * 1024 * 1024 // 50 MiB in bytes (uint64 comparison)
	if delta > maxDelta {
		t.Errorf("unexpected memory growth: %d bytes delta (want < %d bytes)", delta, maxDelta)
	}
}

// --- helpers ---

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func splitImport(imp string) (module, name string) {
	if i := strings.IndexByte(imp, '.'); i >= 0 {
		return imp[:i], imp[i+1:]
	}
	return "", imp
}

// splitImport splits a "module.name" import label for the audit in
// TestForbiddenCapabilities.
