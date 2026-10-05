package application

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"j0s.at/vibeshell/internal/domain"
)

// harness wires a coordinator to fresh doubles. Every test that exercises the
// turn flow uses one, so no test depends on another test's state.
type harness struct {
	coord     *Coordinator
	order     *orderLog
	events    *stubEventStore
	world     *stubWorldStore
	content   *stubContentStore
	renderer  *stubRenderer
	snapshots *stubSnapshots
	engine    *scriptedEngine
	tools     *stubTools
}

// newHarness builds a coordinator whose every dependency is a double.
func newHarness(t *testing.T, limits Limits) *harness {
	t.Helper()
	h := &harness{
		order:     newOrderLog(),
		events:    newStubEventStore(),
		content:   newStubContentStore(),
		renderer:  nil,
		snapshots: newStubSnapshots(),
		tools:     newStubTools(),
	}
	h.world = newStubWorldStore(h.order)
	h.renderer = newStubRenderer(h.order)
	h.engine = newScriptedEngine(h.order)
	coord, err := NewCoordinator(CoordinatorOptions{
		Engine:    h.engine,
		Tools:     h.tools,
		Events:    h.events,
		World:     h.world,
		Content:   h.content,
		Renderer:  h.renderer,
		Clock:     newStubClock(1_700_000_000_000),
		Random:    &countingRandom{},
		Snapshots: h.snapshots,
		Limits:    limits,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	h.coord = coord
	return h
}

// openSession accepts one session and starts draining its delivery channels,
// which is what an inbound transport adapter must do.
func (h *harness) openSession(t *testing.T, principal domain.UserID, cwd domain.ValidPath) (*shellDrain, Shell) {
	t.Helper()
	ctx := context.Background()
	shell, err := h.coord.Open(ctx, OpenSessionRequest{
		Principal: principal,
		AuthMode:  AuthModeSecure,
		Terminal:  TerminalMetadata{Term: "xterm-256color", Size: DefaultTerminalSize, ClientAddr: "127.0.0.1:2222"},
		CWD:       cwd,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	drain := newShellDrain(shell)
	t.Cleanup(func() {
		_ = shell.End(context.Background(), EndShutdown)
		drain.wait(t, 10*time.Second)
	})
	return drain, shell
}

// testSession opens one session with the shared test principal.
func (h *harness) testSession(t *testing.T) (*shellDrain, Shell) {
	t.Helper()
	return h.openSession(t, testPrincipal, "/home/alice")
}

// shellDrain is the adapter side of the session contract: it consumes outputs
// and outcomes the way a transport adapter must, and lets a test wait for a
// turn's terminal result without racing the worker goroutines.
type shellDrain struct {
	shell Shell

	mu       sync.Mutex
	outputs  []SessionOutput
	outcomes []SessionOutcome
	closed   chan struct{}
}

func newShellDrain(shell Shell) *shellDrain {
	d := &shellDrain{shell: shell, closed: make(chan struct{})}
	go d.consume()
	return d
}

func (d *shellDrain) consume() {
	defer close(d.closed)
	outputs := d.shell.Outputs()
	outcomes := d.shell.Outcomes()
	for outputs != nil || outcomes != nil {
		select {
		case item, ok := <-outputs:
			if !ok {
				outputs = nil
				continue
			}
			d.mu.Lock()
			d.outputs = append(d.outputs, item)
			d.mu.Unlock()
		case outcome, ok := <-outcomes:
			if !ok {
				outcomes = nil
				continue
			}
			d.mu.Lock()
			d.outcomes = append(d.outcomes, outcome)
			d.mu.Unlock()
		}
	}
}

// wait blocks until the shell's delivery channels are closed.
func (d *shellDrain) wait(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-d.closed:
	case <-time.After(within):
		t.Fatalf("session delivery channels did not close within %s", within)
	}
}

// awaitOutcome waits for one turn's terminal outcome.
func (d *shellDrain) awaitOutcome(t *testing.T, turn domain.TurnID, within time.Duration) SessionOutcome {
	t.Helper()
	deadline := time.After(within)
	for {
		d.mu.Lock()
		for _, outcome := range d.outcomes {
			if outcome.Turn == turn {
				d.mu.Unlock()
				return outcome
			}
		}
		d.mu.Unlock()
		select {
		case <-deadline:
			t.Fatalf("turn %s produced no terminal outcome within %s", turn, within)
		case <-time.After(time.Millisecond):
		}
	}
}

// awaitOutputs waits until at least count output items have been delivered.
func (d *shellDrain) awaitOutputs(t *testing.T, count int, within time.Duration) []SessionOutput {
	t.Helper()
	deadline := time.After(within)
	for {
		d.mu.Lock()
		delivered := append([]SessionOutput(nil), d.outputs...)
		d.mu.Unlock()
		if len(delivered) >= count {
			return delivered
		}
		select {
		case <-deadline:
			t.Fatalf("expected %d output items, got %d within %s", count, len(delivered), within)
		case <-time.After(time.Millisecond):
		}
	}
}

// awaitStableOutputs waits for count output items and then for a short quiet
// period, so an assertion about "no further output" is not satisfied merely by
// winning a race against the worker.
func (d *shellDrain) awaitStableOutputs(t *testing.T, count int, within time.Duration) []SessionOutput {
	t.Helper()
	d.awaitOutputs(t, count, within)
	time.Sleep(20 * time.Millisecond)
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]SessionOutput(nil), d.outputs...)
}

func (d *shellDrain) outcomeCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.outcomes)
}

func (d *shellDrain) outputCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.outputs)
}

// textOutputs returns the plain-text items of one turn, in delivery order.
func (d *shellDrain) textOutputs(turn domain.TurnID) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var texts []string
	for _, item := range d.outputs {
		if item.Turn == turn && item.Kind == OutputText {
			texts = append(texts, item.Text)
		}
	}
	return texts
}

// waitForState blocks until a session's active turn reaches one of the wanted
// states. It is how a test synchronizes with the worker goroutine without
// sleeping for a fixed period.
func waitForState(t *testing.T, shell Shell, within time.Duration, wanted ...TurnState) ActiveTurn {
	t.Helper()
	deadline := time.After(within)
	var last ActiveTurn
	for {
		snapshot := shell.Snapshot()
		if snapshot.ActiveTurn != nil {
			last = *snapshot.ActiveTurn
			for _, state := range wanted {
				if last.State == state {
					return last
				}
			}
		}
		select {
		case <-deadline:
			t.Fatalf("active turn never reached %v; last state %q", wanted, last.State)
		case <-time.After(time.Millisecond):
		}
	}
}

// awaitIdle blocks until no turn is running or queued.
func awaitIdle(t *testing.T, shell Shell, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		snapshot := shell.Snapshot()
		if snapshot.ActiveTurn == nil && snapshot.QueuedTurns == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("session never became idle; active=%v queued=%d", snapshot.ActiveTurn, snapshot.QueuedTurns)
		case <-time.After(time.Millisecond):
		}
	}
}

// commandLine builds one accepted command input with its channel sequence.
func commandLine(shell Shell, sequence uint64, text string) SessionInput {
	return SessionInput{
		Session:   shell.ID(),
		Principal: shell.Principal(),
		Sequence:  sequence,
		Kind:      InputCommand,
		Command:   text,
	}
}

// textCandidate builds a candidate that only produces output.
func textCandidate(turn domain.TurnID, text string) *TurnCandidate {
	return &TurnCandidate{
		CommitKey: CommitKey{Turn: turn, Logical: "stage-1"},
		Output:    CandidateOutput{Text: text},
	}
}

// mutatingCandidate builds a candidate that writes one file and changes the
// session working directory at the commit point.
func mutatingCandidate(turn domain.TurnID, path domain.ValidPath, text string, cwd domain.ValidPath) *TurnCandidate {
	candidate := textCandidate(turn, text)
	candidate.CommitKey.Logical = string(path)
	candidate.Changes = domain.ChangeSet{Mutations: []domain.Mutation{{
		Type:        domain.MutationCreate,
		NamespaceID: testNamespace,
		Path:        path,
		Kind:        domain.NodeKindFile,
		Metadata:    domain.NewNodeMetadata(0o644, 1, 1, 1_700_000_000_000),
		Content:     domain.ContentRef{Hash: testContentID(7), Size: int64(len(text)), MediaType: "text/plain; charset=utf-8"},
	}}}
	candidate.SessionPatch = SessionPatch{CWD: &cwd}
	return candidate
}

// providerError is a retryable provider failure.
func providerError(message string) error {
	return domain.ErrorEnvelope{
		Class:   domain.FailureProviderOutage,
		Message: message,
	}
}

// storageFailure is a durable-recording failure with a shared typed code.
func storageFailure() error {
	return domain.NewUnavailableError(domain.CodeDatabaseUnavailable, "event store is unavailable", nil, nil)
}

// awaitPromptOutput waits for the prompt output item that ends a turn.
func awaitPromptOutput(t *testing.T, outputs []SessionOutput) PromptState {
	t.Helper()
	for i := len(outputs) - 1; i >= 0; i-- {
		if outputs[i].Kind == OutputPrompt && outputs[i].Prompt != nil {
			return *outputs[i].Prompt
		}
	}
	t.Fatalf("no prompt output item among %d items", len(outputs))
	return PromptState{}
}

// errorsReported extracts the code of a typed domain error for assertions.
func errorsReported(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var domainErr *domain.DomainError
	if !errors.As(err, &domainErr) {
		t.Fatalf("expected a *domain.DomainError, got %T: %v", err, err)
	}
	return domainErr.Code
}

// assertStates compares a recorded transition path with the expected one.
func assertStates(t *testing.T, got []TurnState, want []TurnState) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("transition path %v does not match expected %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("transition path %v does not match expected %v", got, want)
		}
	}
}

// assertStrings compares recorded text with expected text.
func assertStrings(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}
