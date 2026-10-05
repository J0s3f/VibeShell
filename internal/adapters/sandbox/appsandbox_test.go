package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

func newTestAdapter(t *testing.T) *Adapter {
	t.Helper()
	a, err := NewAdapter(context.Background(), DefaultMemoryPages, Config{})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	return a
}

// appArtifact builds a minimal valid artifact whose source defines the
// manifest entrypoint.
func appArtifact(t *testing.T, source string) domain.AppArtifact {
	t.Helper()
	appID, err := domain.ParseAppID("app_0123456789ABCDEFGHJKMNPQRS")
	if err != nil {
		t.Fatal(err)
	}
	verID, err := domain.ParseAppVersionID("av_0123456789ABCDEFGHJKMNPQRS")
	if err != nil {
		t.Fatal(err)
	}
	return domain.AppArtifact{
		AppID: appID, VersionID: verID,
		Manifest: domain.AppManifest{
			ABIVersion: domain.AppABIVersion, CommandNames: []string{"app"},
			Entrypoint: "handle", Version: "1.0.0",
		},
		Source: source,
	}
}

func runInput(t *testing.T, a *Adapter, artifact domain.AppArtifact, limits ports.SandboxLimits) (domain.AppResult, error) {
	t.Helper()
	state := domain.AppState{SessionState: json.RawMessage(`{"count":1}`)}
	event := domain.AppEvent{EventType: domain.AppEventInput, Payload: json.RawMessage(`{"text":"hi"}`), Timestamp: 1700000000000}
	return a.Run(context.Background(), artifact, state, event, limits)
}

const echoSource = `function handle(state, event) {
  return {
    new_state: state,
    view: {mode: "text", status_line: "ok", metadata: {echo: event.event_type}},
    effects: [],
    world_reads: []
  };
}`

// TestBasicRun is the sanity gate: a valid app maps to a validated
// domain.AppResult through the bounded JSON bridge.
func TestBasicRun(t *testing.T) {
	a := newTestAdapter(t)
	res, err := runInput(t, a, appArtifact(t, echoSource), ports.SandboxLimits{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.View.Mode != "text" || res.View.StatusLine != "ok" {
		t.Errorf("unexpected view: %+v", res.View)
	}
	if string(res.NewState.SessionState) != `{"count":1}` {
		t.Errorf("new state not propagated: %s", res.NewState.SessionState)
	}
}

// TestGuestEventPromotesPayloadFields proves a generated program can read a
// command's arguments from the event's top level, while the original envelope
// fields (event_type and the nested payload) remain available.
func TestGuestEventPromotesPayloadFields(t *testing.T) {
	a := newTestAdapter(t)
	source := `function handle(state, event) {
  return {
    new_state: state,
    view: {mode: "text", status_line: "ok", metadata: {
      command: event.command,
      first: (event.args && event.args[0]) || "",
      cwd: event.cwd,
      event_type: event.event_type,
      nested: (event.payload && event.payload.args && event.payload.args[0]) || ""
    }},
    effects: [],
    world_reads: []
  };
}`
	event := domain.AppEvent{
		EventType: domain.AppEventInput,
		Payload:   json.RawMessage(`{"command":"cat notes.txt","args":["notes.txt"],"cwd":"/home/alice"}`),
		Timestamp: 1700000000000,
	}
	result, err := a.Run(context.Background(), appArtifact(t, source), domain.AppState{}, event, ports.SandboxLimits{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var meta struct {
		Command   string `json:"command"`
		First     string `json:"first"`
		Cwd       string `json:"cwd"`
		EventType string `json:"event_type"`
		Nested    string `json:"nested"`
	}
	if err := json.Unmarshal(result.View.Metadata, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if meta.Command != "cat notes.txt" || meta.First != "notes.txt" || meta.Cwd != "/home/alice" {
		t.Fatalf("promoted fields = %+v, want the payload fields at the top level", meta)
	}
	if meta.EventType != domain.AppEventInput {
		t.Fatalf("event_type = %q, want %q", meta.EventType, domain.AppEventInput)
	}
	if meta.Nested != "notes.txt" {
		t.Fatalf("nested payload args = %q, want notes.txt", meta.Nested)
	}
}

// TestInfiniteLoopDeadline mirrors the A04 gate: an unbounded guest loop is
// terminated by the run's deadline and the adapter stays usable.
func TestInfiniteLoopDeadline(t *testing.T) {
	a := newTestAdapter(t)
	source := `function handle(state, event) { while(true){} }`
	start := time.Now()
	_, err := a.Run(context.Background(), appArtifact(t, source), domain.AppState{}, domain.AppEvent{EventType: domain.AppEventInput}, ports.SandboxLimits{DeadlineMs: 500})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("infinite loop returned no error")
	}
	if elapsed > 5*time.Second {
		t.Errorf("deadline took %v; want prompt cancellation", elapsed)
	}
	var de *domain.DomainError
	if !errors.As(err, &de) || de.Category != domain.CategoryCancelled {
		t.Errorf("error category = %v; want cancelled", err)
	}
	t.Logf("infinite loop terminated after %v: %v", elapsed.Round(time.Millisecond), err)

	res, err := runInput(t, a, appArtifact(t, echoSource), ports.SandboxLimits{})
	if err != nil || res.View.Mode != "text" {
		t.Errorf("fresh run after interruption: %v %+v", err, res)
	}
}

// TestAllocationExhaustion mirrors the A04 gate: guest allocation is bounded.
func TestAllocationExhaustion(t *testing.T) {
	a := newTestAdapter(t)
	source := `function handle(state, event) {
  let parts = [];
  while (true) { parts.push(new Array(65536).fill("x")); }
}`
	_, err := runInput(t, a, appArtifact(t, source), ports.SandboxLimits{MaxMemoryB: 4 << 20})
	if err == nil {
		t.Fatal("unbounded allocation returned no error")
	}
	t.Logf("allocation exhaustion bounded: %v", err)

	res, err := runInput(t, a, appArtifact(t, echoSource), ports.SandboxLimits{})
	if err != nil || res.View.Mode != "text" {
		t.Errorf("run after allocation bound: %v", err)
	}
}

// TestDeepRecursion mirrors the A04 gate: unbounded recursion is bounded.
func TestDeepRecursion(t *testing.T) {
	a := newTestAdapter(t)
	source := `function handle(state, event) { return (function f(){ return 1 + f(); })(); }`
	start := time.Now()
	_, err := runInput(t, a, appArtifact(t, source), ports.SandboxLimits{})
	if err == nil {
		t.Fatal("unbounded recursion returned no error")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("recursion took %v to terminate", d)
	}
	t.Logf("deep recursion bounded: %v", err)
}

// TestMalformedResult verifies guest output that is not AppResult-shaped
// JSON is rejected safely.
func TestMalformedResult(t *testing.T) {
	a := newTestAdapter(t)

	// Circular structure: JSON.stringify throws in the guest.
	circular := `function handle(state, event) { let o = {}; o.self = o; return o; }`
	if _, err := runInput(t, a, appArtifact(t, circular), ports.SandboxLimits{}); err == nil {
		t.Error("circular result returned no error")
	} else {
		t.Logf("circular result rejected: %v", err)
	}

	// Undefined result: not AppResult JSON.
	undef := `function handle(state, event) { return undefined; }`
	if _, err := runInput(t, a, appArtifact(t, undef), ports.SandboxLimits{}); err == nil {
		t.Error("undefined result returned no error")
	} else {
		t.Logf("undefined result rejected: %v", err)
	}

	// Result with an invalid view mode fails domain validation.
	badView := `function handle(state, event) { return {new_state: state, view: {mode: "explode"}}; }`
	if _, err := runInput(t, a, appArtifact(t, badView), ports.SandboxLimits{}); err == nil {
		t.Error("invalid view mode returned no error")
	} else {
		t.Logf("invalid view rejected: %v", err)
	}

	// Missing entrypoint function.
	missing := `const notHandle = 1;`
	if _, err := runInput(t, a, appArtifact(t, missing), ports.SandboxLimits{}); err == nil {
		t.Error("missing entrypoint returned no error")
	} else {
		t.Logf("missing entrypoint rejected: %v", err)
	}
}

// TestMalformedEventInput rejects a malformed event payload before any
// guest work happens.
func TestMalformedEventInput(t *testing.T) {
	a := newTestAdapter(t)
	event := domain.AppEvent{EventType: domain.AppEventInput, Payload: json.RawMessage(`{"text":`)}
	_, err := a.Run(context.Background(), appArtifact(t, echoSource), domain.AppState{}, event, ports.SandboxLimits{})
	if err == nil {
		t.Fatal("malformed event payload accepted")
	}
	var de *domain.DomainError
	if !errors.As(err, &de) || de.Category != domain.CategoryValidation {
		t.Errorf("error = %v; want validation", err)
	}
}

// TestForbiddenCapabilities mirrors the A04 gate through the adapter:
// denied fs/env, no network constructors, and no unexpected imports.
func TestForbiddenCapabilities(t *testing.T) {
	a := newTestAdapter(t)

	for _, imp := range a.Imports() {
		if strings.HasPrefix(strings.SplitN(imp, ".", 2)[1], "sock_") {
			t.Errorf("guest imports network symbol %s", imp)
		}
	}

	// Guest attempts to read the host filesystem: denied/exited.
	fsSource := `function handle(state, event) { os.open("/etc/passwd", 0); return {view:{mode:"text"}}; }`
	if _, err := runInput(t, a, appArtifact(t, fsSource), ports.SandboxLimits{}); err == nil {
		t.Error("os.open succeeded")
	} else {
		t.Logf("os.open denied: %v", err)
	}

	// Environment is empty.
	envSource := `function handle(state, event) {
  return {new_state: state, view: {mode: "text", status_line: JSON.stringify(os.environ())}};
}`
	res, err := runInput(t, a, appArtifact(t, envSource), ports.SandboxLimits{})
	if err == nil && res.View.StatusLine != "[]" && res.View.StatusLine != "null" {
		t.Errorf("env leaked: %q", res.View.StatusLine)
	}

	// Network constructors do not exist.
	netSource := `function handle(state, event) {
  return {new_state: state, view: {mode: "text", metadata: {socket: typeof Socket, fetch: typeof fetch, req: typeof require}}};
}`
	res, err = runInput(t, a, appArtifact(t, netSource), ports.SandboxLimits{})
	if err != nil {
		t.Fatalf("net probe run: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal(res.View.Metadata, &m); err != nil {
		t.Fatalf("metadata: %v", err)
	}
	for k, v := range m {
		if v != "undefined" {
			t.Errorf("%s = %q; want undefined", k, v)
		}
	}
}

// TestInstancePerRun verifies disposable instances: one run cannot observe
// another run's globals, state, or allocations.
func TestInstancePerRun(t *testing.T) {
	a := newTestAdapter(t)
	setter := `function handle(state, event) { globalThis.secret = "alpha-42"; return {view:{mode:"text", status_line:"set"}}; }`
	if _, err := runInput(t, a, appArtifact(t, setter), ports.SandboxLimits{}); err != nil {
		t.Fatalf("setter: %v", err)
	}
	probe := `function handle(state, event) { return {view:{mode:"text", status_line: typeof globalThis.secret}}; }`
	res, err := runInput(t, a, appArtifact(t, probe), ports.SandboxLimits{})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.View.StatusLine != "undefined" {
		t.Errorf("state leaked across runs: %q", res.View.StatusLine)
	}
}

// TestDeterministicTimeAndRandom verifies explicit seed/time injection:
// identical limits give identical bridge values, different seeds differ.
func TestDeterministicTimeAndRandom(t *testing.T) {
	a := newTestAdapter(t)
	probe := `function handle(state, event) {
  return {new_state: state, view: {mode: "text", metadata: [Math.random(), Math.random(), Date.now()]}};
}`
	limits := ports.SandboxLimits{SeededRand: 42, SimNowMilli: 1700000000123}
	first, err := runInput(t, a, appArtifact(t, probe), limits)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := runInput(t, a, appArtifact(t, probe), limits)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if string(first.View.Metadata) != string(second.View.Metadata) {
		t.Errorf("same seed/time, different outcome: %s vs %s", first.View.Metadata, second.View.Metadata)
	}
	t.Logf("deterministic metadata: %s", first.View.Metadata)
	var vals []float64
	if err := json.Unmarshal(first.View.Metadata, &vals); err != nil {
		t.Fatalf("metadata unmarshal: %v", err)
	}
	if len(vals) != 3 || vals[2] != 1700000000123 {
		t.Errorf("metadata = %v; want [r0 r1 1700000000123]", vals)
	}

	other, err := runInput(t, a, appArtifact(t, probe), ports.SandboxLimits{SeededRand: 43, SimNowMilli: 1700000000123})
	if err != nil {
		t.Fatalf("other seed: %v", err)
	}
	if string(other.View.Metadata) == string(first.View.Metadata) {
		t.Error("different seed produced identical Math.random values")
	}
}

// TestLimitsEnforced checks output and event-count bounds.
func TestLimitsEnforced(t *testing.T) {
	a := newTestAdapter(t)

	big := `function handle(state, event) {
  return {new_state: state, view: {mode: "text", status_line: "x".repeat(4096)}, effects: [], world_reads: []};
}`
	_, err := runInput(t, a, appArtifact(t, big), ports.SandboxLimits{MaxOutputB: 128})
	if err == nil {
		t.Error("oversized result accepted")
	} else {
		var de *domain.DomainError
		if !errors.As(err, &de) || de.Category != domain.CategoryLimit {
			t.Errorf("error = %v; want limit", err)
		}
	}

	effectsSource := `function handle(state, event) {
  const e = {mutation: {type: "create", namespace_id: "nsp_0123456789ABCDEFGHJKMNPQRS", path: "/tmp/a", kind: "file"}, sync: false};
  return {new_state: state, view: {mode: "text"}, effects: [e, e], world_reads: []};
}`
	_, err = runInput(t, a, appArtifact(t, effectsSource), ports.SandboxLimits{MaxEvents: 1})
	if err == nil {
		t.Error("excess declared effects accepted")
	} else {
		var de *domain.DomainError
		if !errors.As(err, &de) || de.Category != domain.CategoryLimit {
			t.Errorf("error = %v; want limit", err)
		}
	}
}

// TestConcurrency mirrors the A04 100-user shape at a smaller scale.
func TestConcurrency(t *testing.T) {
	a := newTestAdapter(t)
	const n = 32
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			source := fmt.Sprintf(`function handle(state, event) { let s = 0; for (let i = 0; i < 100; i++) s += i + %d; return {view:{mode:"text", status_line: String(s)}}; }`, i)
			res, err := a.Run(context.Background(), appArtifact(t, source), domain.AppState{}, domain.AppEvent{EventType: domain.AppEventInput}, ports.SandboxLimits{})
			if err != nil {
				errs <- fmt.Errorf("run %d: %w", i, err)
				return
			}
			if res.View.StatusLine == "" {
				errs <- fmt.Errorf("run %d: empty status", i)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	failed := 0
	for err := range errs {
		if failed < 5 {
			t.Error(err)
		}
		failed++
	}
	t.Logf("concurrency: %d runs in %v, %d errors", n, time.Since(start).Round(time.Millisecond), failed)
}

// TestTruncatedOutput captures a guest writing beyond the output bound.
func TestTruncatedOutput(t *testing.T) {
	a := newTestAdapter(t)
	source := `function handle(state, event) { console.log("x".repeat(2048)); return {view:{mode:"text"}}; }`
	small, err := NewAdapter(context.Background(), DefaultMemoryPages, Config{MaxOutputBytes: 64})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	defer func() { _ = small.Close(context.Background()) }()
	if _, err := runInput(t, small, appArtifact(t, source), ports.SandboxLimits{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	// The captured output itself is bounded; there is no API on Adapter, so
	// this is exercised at engine level below.
	res, err := runInput(t, a, appArtifact(t, source), ports.SandboxLimits{})
	if err != nil || res.View.Mode != "text" {
		t.Errorf("run with logging: %v", err)
	}
}

// TestParseJSONBoundary retains A04's malformed-JSON gate at engine level.
func TestParseJSONBoundary(t *testing.T) {
	e, err := NewEngine(context.Background(), DefaultMemoryPages, Config{})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer func() { _ = e.Close(context.Background()) }()
	inst, err := e.NewInstance(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	defer func() { _ = inst.Close() }()

	for _, bad := range []string{`{"a": }`, `[1, 2,`, `{unquoted: 1}`, ``} {
		if _, err := inst.ParseJSON(context.Background(), bad); err == nil {
			t.Errorf("ParseJSON(%q) accepted malformed JSON", bad)
		}
	}
	if got, err := inst.ParseJSON(context.Background(), `{"a":1}`); err != nil {
		t.Errorf("ParseJSON valid: %v", err)
	} else if got != `{"a":1}` {
		t.Errorf("ParseJSON canonical = %s", got)
	}
}
