package simulation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// ---------------------------------------------------------------------------
// test fakes for the runner
// ---------------------------------------------------------------------------

// funcSandbox is a scripted AppSandbox: it records every event and state it
// receives and answers with the handler's result, so tests can model an
// app's behavior across input and world_change rounds.
type funcSandbox struct {
	mu      sync.Mutex
	handler func(call int, state domain.AppState, event domain.AppEvent) domain.AppResult
	events  []domain.AppEvent
	states  []domain.AppState
}

func (s *funcSandbox) Run(_ context.Context, _ domain.AppArtifact, state domain.AppState, event domain.AppEvent, _ ports.SandboxLimits) (domain.AppResult, error) {
	s.mu.Lock()
	call := len(s.events)
	s.events = append(s.events, event)
	s.states = append(s.states, state)
	s.mu.Unlock()
	return s.handler(call, state, event), nil
}

// recorded returns the events and states the sandbox has seen, oldest first.
func (s *funcSandbox) recorded() ([]domain.AppEvent, []domain.AppState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.AppEvent(nil), s.events...), append([]domain.AppState(nil), s.states...)
}

func (s *funcSandbox) runCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// errWorld fails path lookups with a plain error (not a not-found error) so
// a test can prove resolution failures propagate out of the runner. Other
// operations pass through to the wrapped store.
type errWorld struct {
	ports.WorldStore
}

func (errWorld) LookupPath(context.Context, domain.NamespaceID, domain.ValidPath) (domain.Node, error) {
	return domain.Node{}, errors.New("lookup failed")
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestAppRunnerWorldReadRoundTrip(t *testing.T) {
	reader, ns, world, content := newWorldReader(t)
	seedFile(t, world, content, ns, "/home/alice/notes.txt", []byte("hello notes"))
	notesPath := domain.MustParsePath("/home/alice/notes.txt")

	// The scripted app asks for one read on input and renders whatever the
	// world_change event hands back.
	sandbox := &funcSandbox{handler: func(_ int, _ domain.AppState, event domain.AppEvent) domain.AppResult {
		switch event.EventType {
		case domain.AppEventInput:
			return domain.AppResult{
				NewState:   domain.AppState{SessionState: json.RawMessage(`{"asked":true}`)},
				View:       domain.AppView{Mode: domain.AppViewModeText},
				WorldReads: []domain.WorldReadRequest{{RequestID: "r1", Path: notesPath}},
			}
		case domain.AppEventWorldChange:
			var payload struct {
				Results []struct {
					RequestID string           `json:"request_id"`
					Path      domain.ValidPath `json:"path"`
					Found     bool             `json:"found"`
					Content   string           `json:"content"`
				} `json:"results"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("decode world_change payload: %v", err)
			}
			if len(payload.Results) != 1 {
				t.Fatalf("world_change results = %d, want 1", len(payload.Results))
			}
			if !payload.Results[0].Found {
				t.Errorf("results[0].Found = false, want true")
			}
			return domain.AppResult{
				View: domain.AppView{Mode: domain.AppViewModeText, Metadata: []byte(payload.Results[0].Content)},
			}
		}
		t.Fatalf("unexpected event type %q", event.EventType)
		return domain.AppResult{}
	}}

	now := int64(1_700_000_000_000)
	runner := &AppRunner{Sandbox: sandbox, Reader: reader}
	result, err := runner.Run(context.Background(), AppRunRequest{
		State:        domain.AppState{SessionState: json.RawMessage(`{"initial":true}`)},
		Command:      "notes",
		Args:         []string{"--show"},
		CWD:          domain.MustParsePath("/home/alice"),
		Namespace:    ns,
		NowUnixMilli: now,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(result.View.Metadata) != "hello notes" {
		t.Errorf("rendered content = %q, want %q", result.View.Metadata, "hello notes")
	}

	events, states := sandbox.recorded()
	if len(events) != 2 {
		t.Fatalf("sandbox runs = %d, want exactly 2", len(events))
	}
	if events[0].EventType != domain.AppEventInput {
		t.Errorf("event[0].EventType = %q, want %q", events[0].EventType, domain.AppEventInput)
	}
	var input struct {
		Command string           `json:"command"`
		Args    []string         `json:"args"`
		CWD     domain.ValidPath `json:"cwd"`
	}
	if err := json.Unmarshal(events[0].Payload, &input); err != nil {
		t.Fatalf("decode input payload: %v", err)
	}
	if input.Command != "notes" || input.CWD != "/home/alice" || len(input.Args) != 1 || input.Args[0] != "--show" {
		t.Errorf("input payload = %+v, want command=notes args=[--show] cwd=/home/alice", input)
	}
	if events[1].EventType != domain.AppEventWorldChange {
		t.Errorf("event[1].EventType = %q, want %q", events[1].EventType, domain.AppEventWorldChange)
	}
	for i, event := range events {
		if event.Timestamp != now {
			t.Errorf("event[%d].Timestamp = %d, want %d", i, event.Timestamp, now)
		}
	}
	// The first run receives the caller's state; the second the first run's
	// new state, so the app keeps its continuity across rounds.
	if string(states[0].SessionState) != `{"initial":true}` {
		t.Errorf("states[0] = %s, want the caller's initial state", states[0].SessionState)
	}
	if string(states[1].SessionState) != `{"asked":true}` {
		t.Errorf("states[1] = %s, want the state returned by the first run", states[1].SessionState)
	}
}

func TestAppRunnerStopsAfterMaxRounds(t *testing.T) {
	reader, ns, _, _ := newWorldReader(t)
	cases := []struct {
		name      string
		maxRounds int
		wantRuns  int
	}{
		{name: "default budget", maxRounds: 0, wantRuns: DefaultWorldReadRounds + 1},
		{name: "explicit budget", maxRounds: 2, wantRuns: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The app asks for a read after every single run, forever.
			sandbox := &funcSandbox{handler: func(_ int, _ domain.AppState, _ domain.AppEvent) domain.AppResult {
				return domain.AppResult{WorldReads: []domain.WorldReadRequest{
					{RequestID: "always", Path: domain.MustParsePath("/home/alice/notes.txt")},
				}}
			}}
			runner := &AppRunner{Sandbox: sandbox, Reader: reader, MaxRounds: tc.maxRounds}
			result, err := runner.Run(context.Background(), AppRunRequest{
				Command:   "loop",
				CWD:       domain.MustParsePath("/home/alice"),
				Namespace: ns,
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := sandbox.runCount(); got != tc.wantRuns {
				t.Errorf("sandbox runs = %d, want %d (bounded)", got, tc.wantRuns)
			}
			// A final result that still requests reads is returned as-is.
			if len(result.WorldReads) == 0 {
				t.Errorf("final result dropped the still-requesting world reads")
			}
		})
	}
}

func TestAppRunnerSingleRunWithoutWorldReads(t *testing.T) {
	reader, ns, _, _ := newWorldReader(t)
	sandbox := &funcSandbox{handler: func(_ int, _ domain.AppState, _ domain.AppEvent) domain.AppResult {
		return domain.AppResult{View: domain.AppView{Mode: domain.AppViewModeText}}
	}}
	runner := &AppRunner{Sandbox: sandbox, Reader: reader}

	result, err := runner.Run(context.Background(), AppRunRequest{
		Command:   "plain",
		CWD:       domain.MustParsePath("/home/alice"),
		Namespace: ns,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sandbox.runCount(); got != 1 {
		t.Errorf("sandbox runs = %d, want 1", got)
	}
	events, _ := sandbox.recorded()
	if events[0].EventType != domain.AppEventInput {
		t.Errorf("event[0].EventType = %q, want %q", events[0].EventType, domain.AppEventInput)
	}
	if result.View.Mode != domain.AppViewModeText {
		t.Errorf("result view mode = %q, want %q", result.View.Mode, domain.AppViewModeText)
	}
}

func TestAppRunnerReturnsResolutionError(t *testing.T) {
	world := newFakeWorld()
	ns := testNamespaces().User
	reader := &WorldReader{World: errWorld{WorldStore: world}, Content: newFakeContent()}
	sandbox := &funcSandbox{handler: func(_ int, _ domain.AppState, _ domain.AppEvent) domain.AppResult {
		return domain.AppResult{WorldReads: []domain.WorldReadRequest{
			{RequestID: "r1", Path: domain.MustParsePath("/home/alice/notes.txt")},
		}}
	}}
	runner := &AppRunner{Sandbox: sandbox, Reader: reader}

	_, err := runner.Run(context.Background(), AppRunRequest{
		Command:   "reader",
		CWD:       domain.MustParsePath("/home/alice"),
		Namespace: ns,
	})
	if err == nil {
		t.Fatalf("Run succeeded, want the resolution error")
	}
	if got := sandbox.runCount(); got != 1 {
		t.Errorf("sandbox runs = %d, want 1 (resolution failed before a second run)", got)
	}
}

func TestAppRunnerBoundsWorldReadsPerRound(t *testing.T) {
	reader, ns, world, content := newWorldReader(t)
	seedFile(t, world, content, ns, "/home/alice/notes.txt", []byte("hello notes"))
	notesPath := domain.MustParsePath("/home/alice/notes.txt")

	// One request more than a round may resolve; every request names an
	// existing file, so only the cap can produce a found:false outcome.
	reads := make([]domain.WorldReadRequest, MaxWorldReadsPerRound+1)
	for i := range reads {
		reads[i] = domain.WorldReadRequest{RequestID: fmt.Sprintf("r%d", i), Path: notesPath}
	}
	sandbox := &funcSandbox{handler: func(call int, _ domain.AppState, _ domain.AppEvent) domain.AppResult {
		if call == 0 {
			return domain.AppResult{WorldReads: reads}
		}
		return domain.AppResult{View: domain.AppView{Mode: domain.AppViewModeText}}
	}}
	runner := &AppRunner{Sandbox: sandbox, Reader: reader}
	if _, err := runner.Run(context.Background(), AppRunRequest{
		Command:   "many",
		CWD:       domain.MustParsePath("/home/alice"),
		Namespace: ns,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	events, _ := sandbox.recorded()
	if len(events) != 2 {
		t.Fatalf("sandbox runs = %d, want 2", len(events))
	}
	var payload struct {
		Results []struct {
			RequestID string `json:"request_id"`
			Found     bool   `json:"found"`
		} `json:"results"`
	}
	if err := json.Unmarshal(events[1].Payload, &payload); err != nil {
		t.Fatalf("decode world_change payload: %v", err)
	}
	if len(payload.Results) != MaxWorldReadsPerRound+1 {
		t.Fatalf("results = %d, want %d (one per request)", len(payload.Results), MaxWorldReadsPerRound+1)
	}
	last := len(payload.Results) - 1
	if !payload.Results[last-1].Found {
		t.Errorf("results[%d].Found = false, want true (within the cap)", last-1)
	}
	if payload.Results[last].Found {
		t.Errorf("results[%d].Found = true, want false (beyond the cap)", last)
	}
	if payload.Results[last].RequestID != fmt.Sprintf("r%d", last) {
		t.Errorf("results[%d].RequestID = %q, want %q", last, payload.Results[last].RequestID, fmt.Sprintf("r%d", last))
	}
}
