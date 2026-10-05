package simulation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"j0s.at/vibeshell/internal/application"
	"j0s.at/vibeshell/internal/domain"
)

// ---------------------------------------------------------------------------
// Test doubles for the adapter's composition seam
// ---------------------------------------------------------------------------

// stubNamespaces is the composition root's NamespaceResolver double: it records
// the request it was asked about and returns a canned resolution or failure.
type stubNamespaces struct {
	resolved CallNamespaces
	err      error
	seen     []application.TurnRequest
	calls    int
}

func (s *stubNamespaces) Namespaces(_ context.Context, req application.TurnRequest) (CallNamespaces, error) {
	s.calls++
	s.seen = append(s.seen, req)
	return s.resolved, s.err
}

// stubClock is a fixed ports.Clock so a call's timestamp is assertable.
type stubClock struct{ now int64 }

func (c stubClock) NowUnixMilli() int64   { return c.now }
func (c stubClock) MonotonicNanos() int64 { return 0 }

// echoCallTool is a test-only handler that records the trusted CallContext the
// adapter injected and returns a trivial result. It exists so the test can
// inspect the whole injected context; a real echo tool would leak the server's
// trust internals to a model.
const echoCallTool = "test.echo_call"

type callEcho struct {
	contexts []CallContext
	args     []json.RawMessage
}

func (e *callEcho) handler(_ context.Context, call CallContext, args json.RawMessage) (json.RawMessage, error) {
	e.contexts = append(e.contexts, call)
	e.args = append(e.args, append(json.RawMessage(nil), args...))
	return json.RawMessage(`{"echo":true}`), nil
}

// installEchoTool registers the echo handler on a test registry and returns its
// recorder. The registry's allowlist map is package-private, so a test can
// extend it without widening the production surface.
func installEchoTool(registry *Registry) *callEcho {
	echo := &callEcho{}
	registry.handlers[echoCallTool] = echo.handler
	return echo
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const (
	adapterClockNow = 1_700_000_123_456
	adapterCWD      = "/home/alice/work"
	adapterPrompt   = "prompt-v42"
)

// adapterRequest builds the pinned turn request the coordinator hands the tool
// executor. Its values are trusted application context, not model input.
func adapterRequest() application.TurnRequest {
	return application.TurnRequest{
		Session:   testSession,
		Principal: testUser,
		Turn:      testTurn,
		Attempt:   testAttempt,
		Context: application.SessionContext{
			CWD:  domain.MustParsePath(adapterCWD),
			Home: domain.MustParsePath("/home/alice"),
		},
		Snapshot: application.ConfigSnapshot{
			PromptVersion: adapterPrompt,
			ScopePolicy:   domain.DefaultScopePolicy(),
		},
	}
}

// newTestAdapter wires an adapter over a fresh registry and a namespace
// resolver that returns the standard test namespaces.
func newTestAdapter(t *testing.T) (*ToolAdapter, *Registry, *stubNamespaces) {
	t.Helper()
	registry, _, _, _, _, _, _, _, _, _ := newTestRegistry()
	resolver := &stubNamespaces{resolved: testNamespaces()}
	adapter := &ToolAdapter{
		Registry:   registry,
		Namespaces: resolver,
		Clock:      stubClock{now: adapterClockNow},
		UID:        4242,
		GID:        4343,
	}
	return adapter, registry, resolver
}

// adapterCall builds one requested call; arguments are encoded verbatim.
func adapterCall(t *testing.T, name string, args any) application.ToolCallRequest {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args for %s: %v", name, err)
	}
	return application.ToolCallRequest{Name: name, Arguments: raw}
}

// worldLookupResultFields are the fields of a world.lookup result the tests
// assert on.
type worldLookupResultFields struct {
	Scope domain.Scope `json:"scope"`
	Node  struct {
		Path domain.ValidPath `json:"path"`
		Kind domain.NodeKind  `json:"kind"`
	} `json:"node"`
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestToolAdapterRunsBatchThroughRegistry proves the adapter actually reaches
// the server-owned registry: a world.lookup resolves against the world store
// and every requested call yields a result slot, in request order.
func TestToolAdapterRunsBatchThroughRegistry(t *testing.T) {
	adapter, _, resolver := newTestAdapter(t)

	batch := application.ToolBatch{
		Request: adapterRequest(),
		Calls: []application.ToolCallRequest{
			adapterCall(t, "world.lookup", worldLookupArgs{Scope: "user", Path: "/home/alice"}),
			adapterCall(t, "world.lookup", worldLookupArgs{Scope: "shared", Path: "/etc"}),
		},
	}
	got, err := adapter.Execute(context.Background(), batch)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(got.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(got.Results))
	}
	for i, want := range []struct {
		name  string
		scope domain.Scope
		path  domain.ValidPath
	}{
		{"world.lookup", domain.ScopeUser, domain.MustParsePath("/home/alice")},
		{"world.lookup", domain.ScopeShared, domain.MustParsePath("/etc")},
	} {
		res := got.Results[i]
		if res.Name != want.name {
			t.Errorf("result %d name = %q, want %q", i, res.Name, want.name)
		}
		if res.Error != nil {
			t.Errorf("result %d unexpected error: %v", i, res.Error)
			continue
		}
		var payload worldLookupResultFields
		if err := json.Unmarshal(res.Result, &payload); err != nil {
			t.Fatalf("result %d is not a world.lookup payload: %v (%s)", i, err, res.Result)
		}
		if payload.Scope != want.scope {
			t.Errorf("result %d scope = %v, want %v", i, payload.Scope, want.scope)
		}
		if payload.Node.Path != want.path {
			t.Errorf("result %d path = %s, want %s", i, payload.Node.Path, want.path)
		}
		if payload.Node.Kind != domain.NodeKindDir {
			t.Errorf("result %d kind = %q, want %q", i, payload.Node.Kind, domain.NodeKindDir)
		}
	}
	if resolver.calls != 1 {
		t.Errorf("namespace resolution calls = %d, want 1", resolver.calls)
	}
}

// TestToolAdapterBuildsCallContextFromRequest proves every field of the trusted
// CallContext comes from the pinned turn request and the injected composition,
// never from the model.
func TestToolAdapterBuildsCallContextFromRequest(t *testing.T) {
	adapter, registry, resolver := newTestAdapter(t)
	echo := installEchoTool(registry)
	wantRequest := adapterRequest()

	_, err := adapter.Execute(context.Background(), application.ToolBatch{
		Request: wantRequest,
		Calls:   []application.ToolCallRequest{adapterCall(t, echoCallTool, worldLookupArgs{})},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(echo.contexts) != 1 {
		t.Fatalf("echo tool invocations = %d, want 1", len(echo.contexts))
	}
	got := echo.contexts[0]

	want := CallContext{
		SessionID:     wantRequest.Session,
		UserID:        wantRequest.Principal,
		CWD:           wantRequest.Context.CWD,
		Policy:        wantRequest.Snapshot.ScopePolicy,
		Namespaces:    resolver.resolved,
		UID:           adapter.UID,
		GID:           adapter.GID,
		TurnID:        wantRequest.Turn,
		AttemptID:     wantRequest.Attempt,
		PromptVersion: adapterPrompt,
		NowUnixMilli:  adapterClockNow,
	}
	if got != want {
		t.Errorf("call context =\n%+v\nwant\n%+v", got, want)
	}
	if got.CWD != domain.MustParsePath(adapterCWD) {
		t.Errorf("call cwd = %s, want %s", got.CWD, adapterCWD)
	}
	if got.Policy != domain.DefaultScopePolicy() {
		t.Errorf("call policy = %+v, want the snapshot's default policy", got.Policy)
	}
}

// TestToolAdapterReportsUnknownToolPerCall proves a tool outside the
// server-owned allowlist is a typed per-call failure that leaves the rest of
// the batch intact: the model must be able to see and recover from it.
func TestToolAdapterReportsUnknownToolPerCall(t *testing.T) {
	adapter, _, _ := newTestAdapter(t)

	got, err := adapter.Execute(context.Background(), application.ToolBatch{
		Request: adapterRequest(),
		Calls: []application.ToolCallRequest{
			adapterCall(t, "world.lookup", worldLookupArgs{Scope: "user", Path: "/home/alice"}),
			adapterCall(t, "world.shell", worldLookupArgs{}),
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(got.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(got.Results))
	}
	if len(got.Results[0].Result) == 0 || got.Results[0].Error != nil {
		t.Errorf("known call result = %+v, want a successful payload", got.Results[0])
	}
	unknown := got.Results[1]
	if unknown.Name != "world.shell" {
		t.Errorf("failed result name = %q, want %q", unknown.Name, "world.shell")
	}
	if unknown.Error == nil {
		t.Fatal("unknown tool result has no error")
	}
	if len(unknown.Result) != 0 {
		t.Errorf("unknown tool result carries a payload: %s", unknown.Result)
	}
	if unknown.Error.Category != domain.CategoryValidation {
		t.Errorf("unknown tool error category = %q, want %q", unknown.Error.Category, domain.CategoryValidation)
	}
	if unknown.Error.Code != CodeUnknownTool {
		t.Errorf("unknown tool error code = %q, want %q", unknown.Error.Code, CodeUnknownTool)
	}
}

// TestToolAdapterKeepsBatchAfterToolFailure proves a handler failure is a
// per-call error rather than a batch failure, and that later calls still run.
func TestToolAdapterKeepsBatchAfterToolFailure(t *testing.T) {
	adapter, _, _ := newTestAdapter(t)

	got, err := adapter.Execute(context.Background(), application.ToolBatch{
		Request: adapterRequest(),
		Calls: []application.ToolCallRequest{
			adapterCall(t, "world.lookup", worldLookupArgs{Scope: "user", Path: "/home/alice/missing"}),
			adapterCall(t, "world.lookup", worldLookupArgs{Scope: "user", Path: "/home/alice"}),
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(got.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(got.Results))
	}
	missing := got.Results[0]
	if missing.Error == nil {
		t.Fatalf("missing-path call returned a payload: %s", missing.Result)
	}
	if missing.Error.Category != domain.CategoryNotFound {
		t.Errorf("missing-path error category = %q, want %q", missing.Error.Category, domain.CategoryNotFound)
	}
	if len(got.Results[1].Result) == 0 || got.Results[1].Error != nil {
		t.Errorf("call after the failure = %+v, want a successful payload", got.Results[1])
	}
}

// TestToolAdapterForwardsModelArgumentsUnchanged proves the adapter does not
// rewrite untrusted arguments: the registry's own argument validation is what
// rejects them, per call.
func TestToolAdapterForwardsModelArgumentsUnchanged(t *testing.T) {
	adapter, registry, _ := newTestAdapter(t)
	echo := installEchoTool(registry)
	rawArgs := json.RawMessage(`{"scope":"user","path":"/home/alice"}`)

	got, err := adapter.Execute(context.Background(), application.ToolBatch{
		Request: adapterRequest(),
		Calls: []application.ToolCallRequest{
			{Name: echoCallTool, Arguments: rawArgs},
			{Name: "world.lookup", Arguments: json.RawMessage(`{"scope":`)},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(echo.args) != 1 || string(echo.args[0]) != string(rawArgs) {
		t.Errorf("echo tool received arguments %q, want %q", echo.args, rawArgs)
	}
	malformed := got.Results[1]
	if malformed.Error == nil {
		t.Fatalf("malformed arguments returned a payload: %s", malformed.Result)
	}
	if malformed.Error.Code != CodeToolArgsInvalid {
		t.Errorf("malformed argument error code = %q, want %q", malformed.Error.Code, CodeToolArgsInvalid)
	}
}

// TestToolAdapterResolverErrorAbortsBatch proves a namespace resolution failure
// fails the whole batch and runs no tool: without trusted namespaces no call
// could be authorized.
func TestToolAdapterResolverErrorAbortsBatch(t *testing.T) {
	adapter, registry, resolver := newTestAdapter(t)
	echo := installEchoTool(registry)
	wantErr := domain.NewUnavailableError(domain.CodeDatabaseUnavailable, "namespace store is down", nil, nil)
	resolver.err = wantErr

	got, err := adapter.Execute(context.Background(), application.ToolBatch{
		Request: adapterRequest(),
		Calls:   []application.ToolCallRequest{adapterCall(t, echoCallTool, worldLookupArgs{})},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Execute error = %v, want the resolver's error", err)
	}
	if got.Results != nil {
		t.Errorf("results = %+v, want none", got.Results)
	}
	if len(echo.contexts) != 0 {
		t.Errorf("tool ran %d times after resolution failed, want 0", len(echo.contexts))
	}
}

// TestToolAdapterRejectsIncompleteComposition proves an adapter missing a
// dependency reports a typed internal error instead of panicking.
func TestToolAdapterRejectsIncompleteComposition(t *testing.T) {
	registry, _, _, _, _, _, _, _, _, _ := newTestRegistry()
	resolver := &stubNamespaces{resolved: testNamespaces()}
	batch := application.ToolBatch{
		Request: adapterRequest(),
		Calls:   []application.ToolCallRequest{adapterCall(t, echoCallTool, worldLookupArgs{})},
	}

	for name, adapter := range map[string]*ToolAdapter{
		"missing registry":   {Namespaces: resolver, Clock: stubClock{now: adapterClockNow}},
		"missing resolver":   {Registry: registry, Clock: stubClock{now: adapterClockNow}},
		"missing clock":      {Registry: registry, Namespaces: resolver},
		"missing everything": {},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := adapter.Execute(context.Background(), batch)
			if err == nil {
				t.Fatalf("Execute succeeded with %+v, want a typed error", got)
			}
			if !domain.IsInternalError(err) {
				t.Errorf("error category = %q, want %q", domain.GetErrorCategory(err), domain.CategoryInternal)
			}
			if domain.GetErrorCode(err) != domain.CodeInvariantViolation {
				t.Errorf("error code = %q, want %q", domain.GetErrorCode(err), domain.CodeInvariantViolation)
			}
			if got.Results != nil {
				t.Errorf("results = %+v, want none", got.Results)
			}
		})
	}
}
