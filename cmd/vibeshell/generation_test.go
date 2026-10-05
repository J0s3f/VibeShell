package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"j0s.at/vibeshell/internal/adapters/sandbox"
	"j0s.at/vibeshell/internal/application"
	"j0s.at/vibeshell/internal/apps"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
	"j0s.at/vibeshell/internal/routing"
	"j0s.at/vibeshell/internal/simulation"
	"j0s.at/vibeshell/internal/system"
)

// testRouteID and testAccountID are stable identities for the fake gateway.
func testRouteID() domain.RouteID {
	return domain.MustParseRouteID(domain.PrefixRoute + "_0123456789ABCDEFGHJKMNPQRS")
}

func testAccountID() domain.AccountID {
	return domain.MustParseAccountID(domain.PrefixAccount + "_0123456789ABCDEFGHJKMNPQRS")
}

// fakeGateway is a deterministic ports.ModelGateway double. It returns the
// configured content for every request and records how many requests it saw,
// so a test can prove a stored app serves later invocations without another
// model call. No live provider is contacted.
type fakeGateway struct {
	mu       sync.Mutex
	content  string
	usage    domain.Usage
	err      error
	requests int
}

var _ ports.ModelGateway = (*fakeGateway)(nil)

func (g *fakeGateway) Request(_ context.Context, req domain.ModelRequest) (domain.ModelResponse, error) {
	g.mu.Lock()
	g.requests++
	g.mu.Unlock()
	if g.err != nil {
		return domain.ModelResponse{}, g.err
	}
	return domain.ModelResponse{
		RequestID: req.RequestID,
		Message:   domain.Message{Role: domain.RoleAssistant, Content: g.content},
		Usage:     g.usage,
	}, nil
}

func (g *fakeGateway) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requests
}

// fakeExecutor adapts the deterministic gateway double to the router's
// failover executor for engine tests. It performs one attempt and applies the
// same Accept check the router would, so an unusable artifact is reported as
// an error rather than silently accepted.
type fakeExecutor struct {
	gateway *fakeGateway
}

var _ failoverExecutor = (*fakeExecutor)(nil)

func (e *fakeExecutor) Execute(ctx context.Context, req routing.ExecuteRequest) (routing.ExecuteResult, error) {
	resp, err := e.gateway.Request(ctx, domain.ModelRequest{
		Messages:   req.Messages,
		MaxTokens:  req.MaxTokens,
		DeadlineMs: req.DeadlineMs,
	})
	if err != nil {
		return routing.ExecuteResult{}, err
	}
	if req.Accept != nil {
		if aerr := req.Accept(resp); aerr != nil {
			return routing.ExecuteResult{}, aerr
		}
	}
	return routing.ExecuteResult{Response: resp}, nil
}

// generatedArtifactJSON builds the JSON proposal a provider would return for
// the given command, entrypoint, and source.
func generatedArtifactJSON(command, entrypoint, source string) string {
	proposal := generationProposal{
		CommandNames: []string{command},
		Description:  "generated " + command,
		Entrypoint:   entrypoint,
		Source:       source,
	}
	raw, _ := json.Marshal(proposal)
	return string(raw)
}

// constSource defines `handle` and echoes a fixed marker plus the command.
const constSource = `function handle(state, event) {
  return {
    new_state: state,
    view: {mode: "text", status_line: "ok", metadata: {output: "moon-orchard online"}},
    effects: [], world_reads: []
  };
}`

// newGenerationEngine builds an engine over a real sandbox adapter and the
// given gateway. The sandbox is the only place generated source runs.
func newGenerationEngine(t *testing.T, gateway *fakeGateway) (*generationEngine, *apps.InMemoryRegistry) {
	t.Helper()
	adapter, err := sandbox.NewAdapter(context.Background(), sandbox.DefaultMemoryPages, sandbox.Config{})
	if err != nil {
		t.Fatalf("sandbox.NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close(context.Background()) })

	clock := system.NewClock()
	random := system.NewRandom()
	registry := apps.NewInMemoryRegistry(clock)
	service := apps.NewService(registry, adapter, clock, random, nil)
	engine := &generationEngine{
		fallback: &localEngine{identity: presentationIdentity{System: "VibeOS", Hostname: "vibeos"}, generationAvailable: true},
		apps:     service,
		registry: registry,
		runner:   &simulation.AppRunner{Sandbox: adapter},
		world:    fakeUserNamespaces{},
		index:    newAppCommandIndex(),
		executor: &fakeExecutor{gateway: gateway},
		clock:    clock,
		random:   random,
	}
	return engine, registry
}

// fakeUserNamespaces returns a zero namespace; the test apps read no world.
type fakeUserNamespaces struct{}

func (fakeUserNamespaces) userNamespace(context.Context, domain.UserID, domain.ValidPath) (domain.NamespaceID, error) {
	return domain.NamespaceID{}, nil
}

// mustTurnID returns a valid turn identity for the test.
func mustTurnID(t *testing.T) domain.TurnID {
	t.Helper()
	id, err := domain.ParseTurnID(domain.PrefixTurn + "_0123456789ABCDEFGHJKMNPQRS")
	if err != nil {
		t.Fatalf("ParseTurnID: %v", err)
	}
	return id
}

func testTurnRequest(t *testing.T, command string) application.TurnRequest {
	t.Helper()
	return testTurnRequestIn(t, domain.PrefixSession+"_0123456789ABCDEFGHJKMNPQRS", command)
}

// testTurnRequestIn builds a turn request for a specific session identity, so
// a test can model a second session reusing a stored application.
func testTurnRequestIn(t *testing.T, sessionValue, command string) application.TurnRequest {
	t.Helper()
	principal, err := domain.ParseUserID(domain.PrefixUser + "_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err != nil {
		t.Fatalf("ParseUserID: %v", err)
	}
	session, err := domain.ParseSessionID(sessionValue)
	if err != nil {
		t.Fatalf("ParseSessionID: %v", err)
	}
	return application.TurnRequest{
		Session:   session,
		Principal: principal,
		Turn:      mustTurnID(t),
		Input:     application.SessionInput{Kind: application.InputCommand, Command: command},
		Context:   application.SessionContext{CWD: domain.MustParsePath("/home/alice")},
		Snapshot:  application.ConfigSnapshot{ScopePolicy: domain.DefaultScopePolicy()},
	}
}

func generateOnce(t *testing.T, engine *generationEngine, command string) application.StepOutcome {
	t.Helper()
	out, err := engine.Generate(context.Background(), application.GenerationInput{Request: testTurnRequest(t, command)})
	if err != nil {
		t.Fatalf("Generate(%q): %v", command, err)
	}
	if out.Phase != application.StepCandidate || out.Candidate == nil {
		t.Fatalf("Generate(%q) phase=%v candidate=%v, want a candidate", command, out.Phase, out.Candidate)
	}
	return out
}

// TestGenerationCreatesActivatesAndReusesApp is the end-to-end proof: a novel
// command is generated through the fake gateway, validated in the sandbox,
// activated, and served from storage on a second invocation without another
// model call.
// TestGenerationGathersWorldContextThroughTheToolLayer proves the engine asks
// the tool layer for the working directory once before generating, so a wired
// ToolExecutor is actually exercised rather than constructed and unused.
func TestGenerationGathersWorldContextThroughTheToolLayer(t *testing.T) {
	gateway := &fakeGateway{content: generatedArtifactJSON("moon-orchard", "handle", constSource)}
	engine, _ := newGenerationEngine(t, gateway)
	engine.useTools = true
	req := testTurnRequest(t, "moon-orchard")

	step, err := engine.Generate(context.Background(), application.GenerationInput{Request: req})
	if err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	if step.Phase != application.StepAwaitingTool {
		t.Fatalf("first phase = %q, want %q", step.Phase, application.StepAwaitingTool)
	}
	if len(step.ToolCalls) != 1 || step.ToolCalls[0].Name != "world.list" {
		t.Fatalf("tool calls = %+v, want exactly one world.list", step.ToolCalls)
	}

	step, err = engine.Generate(context.Background(), application.GenerationInput{
		Request:     req,
		ToolResults: []application.ToolCallResult{{Name: "world.list", Result: json.RawMessage(`{"nodes":[{"name":"notes.txt"}]}`)}},
	})
	if err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	if step.Phase != application.StepCandidate || step.Candidate == nil {
		t.Fatalf("second phase = %q, want a candidate", step.Phase)
	}
	if gateway.count() == 0 {
		t.Fatal("generation never reached the model")
	}
}

func TestGenerationCreatesActivatesAndReusesApp(t *testing.T) {
	gateway := &fakeGateway{content: generatedArtifactJSON("moon-orchard", "handle", constSource)}
	engine, registry := newGenerationEngine(t, gateway)

	// First invocation: generate, validate, activate, run.
	first := generateOnce(t, engine, "moon-orchard --interactive")
	if first.Candidate.Output.Text != "moon-orchard online" {
		t.Fatalf("first output = %q, want the generated program's output", first.Candidate.Output.Text)
	}
	if gateway.count() != 1 {
		t.Fatalf("gateway requests after first invocation = %d, want 1", gateway.count())
	}
	appID, ok := engine.index.lookup("moon-orchard")
	if !ok {
		t.Fatal("the accepted app was not indexed under its command name")
	}
	if _, err := registry.Current(context.Background(), appID); err != nil {
		t.Fatalf("the accepted app has no active version: %v", err)
	}

	// Second invocation from a *different session*: served by the stored app,
	// no further generation, proving cross-session reuse.
	secondReq := testTurnRequestIn(t, domain.PrefixSession+"_01ARZ3NDEKTSV4RRFFQ69G5FBW", "moon-orchard")
	second, err := engine.Generate(context.Background(), application.GenerationInput{Request: secondReq})
	if err != nil {
		t.Fatalf("Generate second session: %v", err)
	}
	if second.Candidate == nil || second.Candidate.Output.Text != "moon-orchard online" {
		t.Fatalf("second-session output = %+v, want the stored app's output", second.Candidate)
	}
	if gateway.count() != 1 {
		t.Fatalf("gateway requests after second session = %d, want 1 (stored app must serve it)", gateway.count())
	}
}

// TestInvalidCandidateLeavesNoActiveVersion proves a rejected candidate is
// never activated and does not enter the command index.
func TestInvalidCandidateLeavesNoActiveVersion(t *testing.T) {
	// Source omits the entrypoint function, so staging validation fails.
	gateway := &fakeGateway{content: generatedArtifactJSON("ghost-cmd", "handle", "const notHandle = 1;")}
	engine, registry := newGenerationEngine(t, gateway)

	out := generateOnce(t, engine, "ghost-cmd")
	if out.Candidate.ExitStatus != 127 {
		t.Fatalf("invalid candidate exit = %d, want 127", out.Candidate.ExitStatus)
	}
	if !strings.Contains(out.Candidate.Output.Text, "ghost-cmd") {
		t.Fatalf("invalid candidate output = %q, want a truthful failure naming the command", out.Candidate.Output.Text)
	}
	if _, ok := engine.index.lookup("ghost-cmd"); ok {
		t.Fatal("an invalid candidate was indexed as active")
	}
	if len(registry.History()) != 0 {
		t.Fatalf("activation history = %+v, want no activation for an invalid candidate", registry.History())
	}
}

// TestProviderAbsentStaysTruthful proves the no-provider path is unchanged: an
// unknown command reports generation unavailable and never fabricates output.
func TestProviderAbsentStaysTruthful(t *testing.T) {
	fallback := &localEngine{identity: presentationIdentity{System: "VibeOS"}, generationAvailable: false}
	engine := &generationEngine{fallback: fallback, index: newAppCommandIndex()}

	out := generateOnce(t, engine, "moon-orchard")
	if out.Candidate.ExitStatus != 127 {
		t.Fatalf("provider-absent exit = %d, want 127", out.Candidate.ExitStatus)
	}
	if !strings.Contains(out.Candidate.Output.Text, "generation is unavailable") {
		t.Fatalf("provider-absent output = %q, want the truthful unavailable message", out.Candidate.Output.Text)
	}
	if strings.Contains(out.Candidate.Output.Text, "command not found") {
		t.Fatalf("provider-absent output fabricated shell behavior: %q", out.Candidate.Output.Text)
	}

	// An identity command still answers deterministically without a provider.
	identity := generateOnce(t, engine, "pwd")
	if identity.Candidate.Output.Text != "/home/alice" {
		t.Fatalf("identity output = %q, want /home/alice", identity.Candidate.Output.Text)
	}
}

// parseProposalRoundTrip guards the proposal parser against the exact JSON the
// fake gateway uses, so a parser change breaks here rather than silently.
func TestParseGenerationProposalRoundTrip(t *testing.T) {
	content := generatedArtifactJSON("moon-orchard", "handle", constSource)
	proposal, err := parseGenerationProposal(content)
	if err != nil {
		t.Fatalf("parseGenerationProposal: %v", err)
	}
	if fmt.Sprint(proposal.CommandNames) != "[moon-orchard]" || proposal.Entrypoint != "handle" {
		t.Fatalf("parsed proposal = %+v", proposal)
	}
}

// TestParseGenerationProposalToleratesReasoningPreamble guards the parser
// against reasoning models that emit a chain-of-thought block inside the
// content channel before the artifact. The block may contain braces, so it
// must be removed before the JSON object is extracted.
func TestParseGenerationProposalToleratesReasoningPreamble(t *testing.T) {
	artifact := generatedArtifactJSON("moon-orchard", "handle", constSource)
	content := " thinkingThe user wants { a whimsical } orchard tool. Plan: emit JSON.</think>\n" +
		"Here is the artifact:\n" + artifact + "\nDone."
	proposal, err := parseGenerationProposal(content)
	if err != nil {
		t.Fatalf("parseGenerationProposal: %v", err)
	}
	if fmt.Sprint(proposal.CommandNames) != "[moon-orchard]" || proposal.Entrypoint != "handle" {
		t.Fatalf("parsed proposal = %+v", proposal)
	}
}

// interactiveSource is a line-based application: it counts the lines it is
// given, keeps the count in its state, and exits when the line is "quit".
const interactiveSource = `function handle(state, event) {
  var s = (state && state.session_state) || {};
  var count = s.count || 0;
  if (event.line === "quit") {
    return {
      new_state: {session_state: {count: count}},
      view: {mode: "text", status_line: "bye", metadata: {output: "goodbye"}},
      effects: [], world_reads: [], exited: true
    };
  }
  count = count + 1;
  return {
    new_state: {session_state: {count: count}},
    view: {mode: "text", status_line: "ok", metadata: {output: "line " + event.line + " #" + count}},
    effects: [], world_reads: [],
    awaiting_input: true, prompt: "grove> "
  };
}`

// TestForegroundLineBasedAppKeepsStateAcrossLines is the end-to-end proof of the
// line-based interaction: the first invocation leaves the app foreground with
// its prompt, each later line is one turn of the same pinned app with its state
// carried forward, and the app's exit returns the session to the shell.
func TestForegroundLineBasedAppKeepsStateAcrossLines(t *testing.T) {
	gateway := &fakeGateway{content: generatedArtifactJSON("grove", "handle", interactiveSource)}
	engine, _ := newGenerationEngine(t, gateway)

	// The first invocation starts the app and leaves it foreground.
	start := testTurnRequest(t, "grove")
	first, err := engine.Generate(context.Background(), application.GenerationInput{Request: start})
	if err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	if first.Candidate == nil {
		t.Fatal("first invocation produced no candidate")
	}
	foreground := first.Candidate.SessionPatch.Foreground
	if foreground == nil || foreground.Kind != application.ForegroundApp {
		t.Fatalf("first patch foreground = %+v, want a foreground app", foreground)
	}
	if foreground.AppVersionID.IsZero() {
		t.Fatal("foreground app did not pin a version")
	}
	if prompt := first.Candidate.SessionPatch.AppPrompt; prompt == nil || prompt.Prompt != "grove> " {
		t.Fatalf("first patch app prompt = %+v, want grove>", prompt)
	}
	if first.Candidate.Output.Text != "line grove #1" {
		t.Fatalf("first output = %q", first.Candidate.Output.Text)
	}
	if gateway.count() != 1 {
		t.Fatalf("gateway requests = %d, want 1", gateway.count())
	}

	// A later line is one turn of the same pinned app: its state carries the
	// count forward and it stays foreground.
	line := testTurnRequestIn(t, start.Session.String(), "look")
	line.Context.Foreground = *foreground
	second, err := engine.Generate(context.Background(), application.GenerationInput{Request: line})
	if err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	if second.Candidate.Output.Text != "line look #2" {
		t.Fatalf("second output = %q, want state carried across lines", second.Candidate.Output.Text)
	}
	if got := second.Candidate.SessionPatch.Foreground; got == nil || got.Kind != application.ForegroundApp {
		t.Fatalf("second patch foreground = %+v, want the app still foreground", got)
	}
	if gateway.count() != 1 {
		t.Fatalf("gateway requests after a line = %d, want 1 (no regeneration)", gateway.count())
	}

	// The app exits: the session returns to the shell and the app prompt clears.
	quit := testTurnRequestIn(t, start.Session.String(), "quit")
	quit.Context.Foreground = *foreground
	third, err := engine.Generate(context.Background(), application.GenerationInput{Request: quit})
	if err != nil {
		t.Fatalf("third Generate: %v", err)
	}
	if third.Candidate.Output.Text != "goodbye" {
		t.Fatalf("exit output = %q", third.Candidate.Output.Text)
	}
	if got := third.Candidate.SessionPatch.Foreground; got == nil || got.Kind != application.ForegroundShell {
		t.Fatalf("exit patch foreground = %+v, want the shell", got)
	}
	if third.Candidate.SessionPatch.AppPrompt != nil {
		t.Fatalf("exit patch app prompt = %+v, want none", third.Candidate.SessionPatch.AppPrompt)
	}
}

// TestGenerationABIDocumentsStateScopes guards the state contract: a generated
// app must return new_state under the scope keys the shell decodes, or its state
// is silently dropped between lines. A live app that used session/user/shared
// instead of session_state/user_state/shared_state lost its notes this way.
func TestGenerationABIDocumentsStateScopes(t *testing.T) {
	for _, key := range []string{"session_state", "user_state", "shared_state", "state.session_state"} {
		if !strings.Contains(generationABI, key) {
			t.Errorf("generation ABI does not document the state scope key %q", key)
		}
	}
}
