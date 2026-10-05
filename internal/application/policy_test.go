package application

import (
	"errors"
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// The tests in this file cover the parts of the coordinator that are pure
// policy: the transition table, the retry budgets, error classification, the
// output-safety gate, and the composition boundary. They perform no I/O and need
// no coordinator, so a failure here points at a rule rather than at plumbing.

// TestTurnTransitionTableCoversEveryState proves the table is total over the
// documented state set: an unlisted state would otherwise be silently accepted
// by CanTransitionTurn.
func TestTurnTransitionTableCoversEveryState(t *testing.T) {
	for _, state := range domain.AllTurnStates() {
		allowed, listed := turnTransitions[state]
		if !listed {
			t.Errorf("state %q has no entry in the transition table", state)
			continue
		}
		if state.IsTerminal() && len(allowed) != 0 {
			t.Errorf("terminal state %q must have no outgoing transitions, has %v", state, allowed)
		}
		if !state.IsTerminal() && len(allowed) == 0 {
			t.Errorf("non-terminal state %q must be able to move on", state)
		}
		for _, target := range allowed {
			if !containsState(domain.AllTurnStates(), target) {
				t.Errorf("state %q transitions to unknown state %q", state, target)
			}
		}
	}
}

// TestEveryNonTerminalStateReachesATerminalState proves no turn can get stuck:
// a state that cannot reach completed, interrupted, or failed would hang a
// session until its deadline.
func TestEveryNonTerminalStateReachesATerminalState(t *testing.T) {
	for _, start := range domain.AllTurnStates() {
		if !reachesTerminal(start, map[TurnState]bool{}) {
			t.Errorf("state %q cannot reach a terminal state", start)
		}
	}
}

// TestCancellationIsReachableFromEveryUncommittedState encodes the ordering
// rule that a Ctrl-C must always work until the commit point.
func TestCancellationIsReachableFromEveryUncommittedState(t *testing.T) {
	uncommitted := []TurnState{
		StateReceived, StateQueued, StateContextReady,
		StateGenerating, StateAwaitingTool, StateValidating, StateConflicted,
	}
	for _, state := range uncommitted {
		if !CanTransitionTurn(state, StateInterrupted) {
			t.Errorf("uncommitted state %q cannot be interrupted", state)
		}
	}
}

// TestCancellationAfterCommitCannotRewriteCommittedWork proves the opposite
// ordering rule: once the commit is accepted the mutation is durable, so the
// turn cannot report discarded work.
func TestCancellationAfterCommitCannotRewriteCommittedWork(t *testing.T) {
	for _, state := range []TurnState{StateCommitting, StateEmitting} {
		if CanTransitionTurn(state, StateInterrupted) {
			t.Errorf("state %q must not be interruptible: its commit is already durable", state)
		}
	}
}

// TestEveryTransitionIsSymmetricallyLegalOnlyWhenDeclared is a guard against a
// target state being reachable in one direction only by accident of the map.
func TestNoTransitionIsDeclaredTwice(t *testing.T) {
	for state, allowed := range turnTransitions {
		seen := make(map[TurnState]bool, len(allowed))
		for _, target := range allowed {
			if seen[target] {
				t.Errorf("state %q declares %q twice", state, target)
			}
			seen[target] = true
		}
	}
}

func TestTerminalStatesAreExactlyTheDocumentedThree(t *testing.T) {
	terminal := []TurnState{}
	for _, state := range domain.AllTurnStates() {
		if state.IsTerminal() {
			terminal = append(terminal, state)
		}
	}
	assertStates(t, terminal, []TurnState{StateCompleted, StateInterrupted, StateFailed})
}

// TestDecideRetrySpendsTheMatchingBudget covers the three budgets separately:
// a provider failure buys another attempt, a conflict buys a rebase, and a
// rejected response buys a repair.
func TestDecideRetrySpendsTheMatchingBudget(t *testing.T) {
	limits := testLimits()
	snapshot := testSnapshot()
	provider := TurnFailure{Class: domain.FailureProviderOutage}
	conflict := TurnFailure{Class: domain.FailureWorldConflict}
	rejected := TurnFailure{Class: domain.FailureInvalidResponse}

	cases := []struct {
		name     string
		failure  TurnFailure
		kind     failureKind
		attempts int
		rebases  int
		repairs  int
		want     bool
	}{
		{"provider failure below budget retries", provider, failureProvider, 1, 0, 0, true},
		{"provider failure at budget stops", provider, failureProvider, 2, 0, 0, false},
		{"conflict below budget rebases", conflict, failureRebase, 1, 0, 0, true},
		{"conflict at budget stops", conflict, failureRebase, 1, 1, 0, false},
		{"rejected response below budget repairs", rejected, failureRepair, 1, 0, 0, true},
		{"rejected response at budget stops", rejected, failureRepair, 1, 0, 1, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			decision := decideRetry(testCase.failure, testCase.kind, testCase.attempts, testCase.rebases, testCase.repairs, limits, snapshot)
			if decision.Retry != testCase.want {
				t.Fatalf("retry = %v (%s), want %v", decision.Retry, decision.Reason, testCase.want)
			}
			if decision.Reason == "" {
				t.Fatal("a retry decision must explain itself in the journal")
			}
		})
	}
}

// TestCancellationAndContentRejectionNeverRetry encodes PLAN 9.1: a user
// cancellation and a content rejection are not provider faults.
func TestCancellationAndContentRejectionNeverRetry(t *testing.T) {
	limits := testLimits()
	for _, class := range []domain.FailureClass{domain.FailureUserCancelled, domain.FailureContentRejected} {
		failure := TurnFailure{Class: class}
		if failure.IsRetryable() {
			t.Errorf("failure class %q must not be retryable", class)
		}
		for _, kind := range []failureKind{failureProvider, failureRebase, failureRepair} {
			decision := decideRetry(failure, kind, 0, 0, 0, limits, testSnapshot())
			if decision.Retry {
				t.Errorf("failure class %q must not retry as %v", class, kind)
			}
		}
	}
}

// TestSnapshotMayOnlyLowerTheRetryBudget proves a pinned configuration can
// tighten a turn's budget but never raise it above the coordinator's own bound.
func TestSnapshotMayOnlyLowerTheRetryBudget(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxAttempts = 5
	limits.MaxRebases = 5
	tightened := testSnapshot()
	tightened.MaxAttempts = 1
	tightened.MaxRebases = 0

	failure := TurnFailure{Class: domain.FailureProviderOutage}
	if decision := decideRetry(failure, failureProvider, 1, 0, 0, limits, tightened); decision.Retry {
		t.Fatal("the pinned attempt budget of 1 must stop a second attempt")
	}
	if decision := decideRetry(failure, failureRebase, 1, 0, 0, limits, tightened); decision.Retry {
		t.Fatal("a pinned rebase budget of 0 must stop a rebase")
	}

	relaxed := testSnapshot()
	relaxed.MaxAttempts = 99
	relaxed.MaxRebases = 99
	if decision := decideRetry(failure, failureProvider, 4, 0, 0, limits, relaxed); !decision.Retry {
		t.Fatal("a pinned attempt budget above the coordinator limit must not shrink the coordinator budget")
	}
}

// TestFatalFailuresNeverRetry keeps an internal failure from spending a budget
// that only provider faults should spend.
func TestFatalFailuresNeverRetry(t *testing.T) {
	failure := TurnFailure{Class: domain.FailureUnknown}
	decision := decideRetry(failure, failureFatal, 0, 0, 0, testLimits(), testSnapshot())
	if decision.Retry {
		t.Fatal("a fatal failure must not retry")
	}
}

// TestClassifyErrorKeepsTheFailureCategory proves an unknown error is never
// treated as success and never silently reclassified as a retryable provider
// fault when the dependency already said what happened.
func TestClassifyErrorKeepsTheFailureCategory(t *testing.T) {
	if got := classifyError(nil, failureProvider); got.Class != "" {
		t.Fatalf("a nil error must classify as no failure, got %q", got.Class)
	}

	envelope := classifyError(domain.ErrorEnvelope{Class: domain.FailureRateLimited, Message: "slow down"}, failureProvider)
	if envelope.Class != domain.FailureRateLimited {
		t.Errorf("envelope class not preserved: got %q", envelope.Class)
	}

	cancelled := classifyError(domain.NewCancelledError(domain.CodeUserCancelled, "user pressed ctrl-c", nil), failureProvider)
	if cancelled.Class != domain.FailureUserCancelled {
		t.Errorf("a cancelled domain error must classify as user cancelled, got %q", cancelled.Class)
	}
	if cancelled.IsRetryable() {
		t.Error("a cancelled domain error must not be retryable")
	}

	conflict := classifyError(domain.NewConflictError(domain.CodeConcurrentModification, "stale revision", nil), failureRebase)
	if conflict.Class != domain.FailureWorldConflict {
		t.Errorf("a conflict domain error must classify as a world conflict, got %q", conflict.Class)
	}

	unknown := classifyError(errors.New("socket closed"), failureProvider)
	if unknown.Class != domain.FailureUnknown {
		t.Errorf("an untyped error must stay a diagnostic unknown failure, got %q", unknown.Class)
	}
	if !unknown.Attempted {
		t.Error("an untyped provider error must record that this turn attempted the call")
	}

	// A TurnFailure returned by a dependency travels unchanged.
	original := TurnFailure{Class: domain.FailureContextTooLong, Code: domain.CodeContextTooLong, Message: "too long"}
	if got := classifyError(original, failureProvider); got != original {
		t.Errorf("a TurnFailure must travel unchanged: got %+v want %+v", got, original)
	}
}

// TestTurnFailureBecomesASharedTypedError proves adapters can classify a failed
// turn with errors.Is/errors.As instead of string matching.
func TestTurnFailureBecomesASharedTypedError(t *testing.T) {
	cases := []struct {
		class    domain.FailureClass
		category domain.ErrorCategory
		code     string
	}{
		{domain.FailureUserCancelled, domain.CategoryCancelled, domain.CodeUserCancelled},
		{domain.FailureWorldConflict, domain.CategoryConflict, domain.CodeConcurrentModification},
		{domain.FailureQuotaExhausted, domain.CategoryLimit, domain.CodeRateLimited},
		{domain.FailureRateLimited, domain.CategoryLimit, domain.CodeRateLimited},
		{domain.FailureContextTooLong, domain.CategoryLimit, domain.CodeContextTooLong},
		{domain.FailureProviderOutage, domain.CategoryUnavailable, domain.CodeModelUnavailable},
		{domain.FailureUnknown, domain.CategoryInternal, ""},
	}
	for _, testCase := range cases {
		converted := TurnFailure{Class: testCase.class, Message: "detail"}.AsDomainError()
		if converted.Category != testCase.category {
			t.Errorf("class %q became category %q, want %q", testCase.class, converted.Category, testCase.category)
		}
		if testCase.code != "" && converted.Code != testCase.code {
			t.Errorf("class %q became code %q, want %q", testCase.class, converted.Code, testCase.code)
		}
		if converted.Message != "detail" {
			t.Errorf("class %q lost its message", testCase.class)
		}
	}
}

// TestFirstControlRuneFindsOnlyRealControls proves the output-safety gate
// separates ordinary output text from sequences that would drive the terminal.
func TestFirstControlRuneFindsOnlyRealControls(t *testing.T) {
	safe := []string{"plain text", "tab\tseparated", "line\nbreak", "carriage\rreturn", "café – ok", ""}
	for _, text := range safe {
		if got := firstControlRune(text); got != 0 {
			t.Errorf("text %q reported unsafe rune %U", text, got)
		}
	}
	unsafe := []rune{0x00, 0x07, 0x1b, 0x1f, 0x7f}
	for _, r := range unsafe {
		if got := firstControlRune("ok" + string(r) + "rest"); got != r {
			t.Errorf("text containing %U reported %U", r, got)
		}
	}
}

// TestValidateCandidateRejectsUnsafeOutcomes covers the gate between a proposed
// outcome and the commit point. Each case is one way a model response could
// cause damage if it were trusted without validation.
func TestValidateCandidateRejectsUnsafeOutcomes(t *testing.T) {
	turn := mustTurnID("trn_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	limits := testLimits()
	snapshot := testSnapshot()
	cwd := domain.ValidPath("/home/alice")
	emptyCWD := domain.ValidPath("")
	mutation := domain.Mutation{
		Type:        domain.MutationCreate,
		NamespaceID: testNamespace,
		Path:        "/home/alice/notes.txt",
		Kind:        domain.NodeKindFile,
	}

	cases := []struct {
		name      string
		candidate *TurnCandidate
		code      string
	}{
		{
			name:      "missing candidate",
			candidate: nil,
			code:      domain.CodeInvalidInput,
		},
		{
			name:      "mutations without a logical commit key",
			candidate: &TurnCandidate{CommitKey: CommitKey{Turn: turn}, Changes: domain.ChangeSet{Mutations: []domain.Mutation{mutation}}},
			code:      domain.CodeInvalidMutation,
		},
		{
			name:      "candidate that names no turn",
			candidate: &TurnCandidate{Output: CandidateOutput{Text: "orphan"}},
			code:      domain.CodeInvalidIdentity,
		},
		{
			name: "change set that belongs to another turn",
			candidate: &TurnCandidate{
				CommitKey: CommitKey{Turn: turn, Logical: "stage-1"},
				Changes:   domain.ChangeSet{TurnID: mustOtherTurnID(), Mutations: []domain.Mutation{mutation}},
			},
			code: domain.CodeInvalidMutation,
		},
		{
			name: "mutation without a namespace",
			candidate: &TurnCandidate{
				CommitKey: CommitKey{Turn: turn, Logical: "stage-1"},
				Changes: domain.ChangeSet{Mutations: []domain.Mutation{{
					Type: domain.MutationCreate, Path: "/home/alice/x", Kind: domain.NodeKindFile,
				}}},
			},
			code: domain.CodeInvalidScope,
		},
		{
			name:      "mutation without a path",
			candidate: &TurnCandidate{CommitKey: CommitKey{Turn: turn, Logical: "stage-1"}, Changes: domain.ChangeSet{Mutations: []domain.Mutation{{NamespaceID: testNamespace, Kind: domain.NodeKindFile}}}},
			code:      domain.CodeInvalidPath,
		},
		{
			name:      "invalid full-screen view mode",
			candidate: &TurnCandidate{CommitKey: CommitKey{Turn: turn}, Output: CandidateOutput{View: &domain.AppView{Mode: "arbitrary"}}},
			code:      domain.CodeInvalidAppView,
		},
		{
			name:      "text carrying a terminal control sequence",
			candidate: &TurnCandidate{CommitKey: CommitKey{Turn: turn}, Output: CandidateOutput{Text: "before\x1b[2Jafter"}},
			code:      domain.CodeInvalidInput,
		},
		{
			name:      "exit status outside the simulated range",
			candidate: &TurnCandidate{CommitKey: CommitKey{Turn: turn}, ExitStatus: 999},
			code:      domain.CodeInvalidInput,
		},
		{
			name:      "foreground patch that names no accepted version",
			candidate: &TurnCandidate{CommitKey: CommitKey{Turn: turn}, SessionPatch: SessionPatch{Foreground: &ForegroundState{Kind: ForegroundApp}}},
			code:      domain.CodeInvalidInput,
		},
		{
			name:      "session patch with an empty working directory",
			candidate: &TurnCandidate{CommitKey: CommitKey{Turn: turn}, SessionPatch: SessionPatch{CWD: &emptyCWD}},
			code:      domain.CodeInvalidPath,
		},
		{
			name:      "app prompt without an app id",
			candidate: &TurnCandidate{CommitKey: CommitKey{Turn: turn}, SessionPatch: SessionPatch{AppPrompt: &AppPrompt{Name: "moon-orchard"}}},
			code:      domain.CodeInvalidInput,
		},
		{
			name: "app prompt beyond the prompt bound",
			candidate: &TurnCandidate{CommitKey: CommitKey{Turn: turn}, SessionPatch: SessionPatch{
				AppPrompt: &AppPrompt{AppID: mustAppID("app_01ARZ3NDEKTSV4RRFFQ69G5FAV"), Prompt: strings.Repeat(">", domain.MaxAppPromptBytes+1)},
			}},
			code: domain.CodeOutputTooLarge,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := validateCandidate(testCase.candidate, snapshot, limits)
			if err == nil {
				t.Fatal("expected the candidate to be rejected")
			}
			if got := errorsReported(t, err); got != testCase.code {
				t.Fatalf("rejection code = %q, want %q", got, testCase.code)
			}
		})
	}

	t.Run("accepted candidate", func(t *testing.T) {
		candidate := mutatingCandidate(turn, "/home/alice/notes.txt", "hello", cwd)
		candidate.Output.View = &domain.AppView{Mode: domain.AppViewModeText, StatusLine: "notes"}
		candidate.ExitStatus = 0
		if err := validateCandidate(candidate, snapshot, limits); err != nil {
			t.Fatalf("a well-formed candidate was rejected: %v", err)
		}
	})
}

// TestValidateCandidateRejectsOversizedOutput proves the output bound is a
// limit on a single item rather than on the session.
func TestValidateCandidateRejectsOversizedOutput(t *testing.T) {
	turn := mustTurnID("trn_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	limits := testLimits()
	oversized := make([]byte, limits.MaxOutputBytes+1)
	for i := range oversized {
		oversized[i] = 'a'
	}
	candidate := &TurnCandidate{CommitKey: CommitKey{Turn: turn}, Output: CandidateOutput{Text: string(oversized)}}
	err := validateCandidate(candidate, testSnapshot(), limits)
	if got := errorsReported(t, err); got != domain.CodeOutputTooLarge {
		t.Fatalf("rejection code = %q, want %q", got, domain.CodeOutputTooLarge)
	}
}

// TestLimitsValidationRejectsAnUnboundedCoordinator proves the composition
// boundary fails early rather than letting a session run unbounded.
func TestLimitsValidationRejectsAnUnboundedCoordinator(t *testing.T) {
	if err := DefaultLimits().Validate(); err != nil {
		t.Fatalf("the operational defaults must be valid: %v", err)
	}

	cases := map[string]func(Limits) Limits{
		"no output queue":         func(l Limits) Limits { l.OutputQueue = 0; return l },
		"no queued turns":         func(l Limits) Limits { l.MaxQueuedTurns = 0; return l },
		"no attempts":             func(l Limits) Limits { l.MaxAttempts = 0; return l },
		"negative rebases":        func(l Limits) Limits { l.MaxRebases = -1; return l },
		"no repairs":              func(l Limits) Limits { l.MaxRepairs = 0; return l },
		"no output byte bound":    func(l Limits) Limits { l.MaxOutputBytes = 0; return l },
		"no inline payload bound": func(l Limits) Limits { l.MaxInlinePayloadBytes = 0; return l },
		"no turn deadline":        func(l Limits) Limits { l.TurnDeadline = 0; return l },
		"no journal timeout":      func(l Limits) Limits { l.JournalTimeout = 0; return l },
		"implausible queue":       func(l Limits) Limits { l.OutputQueue = 100_000; return l },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if err := mutate(DefaultLimits()).Validate(); err == nil {
				t.Fatal("expected the invalid limit to be rejected")
			}
		})
	}
}

// TestConfigSnapshotValidationRejectsAnUnboundedTurn proves a configuration
// revision that cannot bound a turn is refused instead of silently ignored.
func TestConfigSnapshotValidationRejectsAnUnboundedTurn(t *testing.T) {
	if err := testSnapshot().Validate(); err != nil {
		t.Fatalf("the test snapshot must be valid: %v", err)
	}
	cases := map[string]func(ConfigSnapshot) ConfigSnapshot{
		"no deadline":       func(s ConfigSnapshot) ConfigSnapshot { s.TurnDeadlineMs = 0; return s },
		"negative deadline": func(s ConfigSnapshot) ConfigSnapshot { s.TurnDeadlineMs = -1; return s },
		"no attempts":       func(s ConfigSnapshot) ConfigSnapshot { s.MaxAttempts = 0; return s },
		"negative rebases":  func(s ConfigSnapshot) ConfigSnapshot { s.MaxRebases = -1; return s },
	}
	// A zero rebase budget is legal: it is a policy a configuration revision may
	// legitimately choose, so it must not be rejected here.
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if err := mutate(testSnapshot()).Validate(); err == nil {
				t.Fatal("expected the invalid snapshot to be rejected")
			}
		})
	}
}

// TestNewCoordinatorReportsEveryMissingDependency proves the composition
// boundary names what is absent, so a miswired composition is actionable
// instead of a nil dereference later.
func TestNewCoordinatorReportsEveryMissingDependency(t *testing.T) {
	if _, err := NewCoordinator(CoordinatorOptions{}); err == nil {
		t.Fatal("an empty composition must be rejected")
	}

	complete := CoordinatorOptions{
		Engine:    newScriptedEngine(newOrderLog()),
		Tools:     newStubTools(),
		Events:    newStubEventStore(),
		World:     newStubWorldStore(newOrderLog()),
		Content:   newStubContentStore(),
		Renderer:  newStubRenderer(newOrderLog()),
		Clock:     newStubClock(0),
		Random:    &countingRandom{},
		Snapshots: newStubSnapshots(),
		Limits:    DefaultLimits(),
	}
	if _, err := NewCoordinator(complete); err != nil {
		t.Fatalf("a complete composition must be accepted: %v", err)
	}

	partial := complete
	partial.Renderer = nil
	partial.Clock = nil
	_, err := NewCoordinator(partial)
	if err == nil {
		t.Fatal("an incomplete composition must be rejected")
	}
	var domainErr *domain.DomainError
	if !errors.As(err, &domainErr) {
		t.Fatalf("expected a *domain.DomainError, got %T: %v", err, err)
	}
	// The missing names travel as structured detail so a miswired composition
	// says what is absent instead of failing later inside a session goroutine.
	for _, missing := range []string{"Renderer", "Clock"} {
		if !containsSubstring(domainErr.Details["missing"], missing) {
			t.Errorf("composition error %v does not name the missing %s", err, missing)
		}
	}
}

// TestEncodeBase32MatchesTheIdentityFormat proves the coordinator mints
// identities the domain parser accepts, rather than values that only look right.
func TestEncodeBase32MatchesTheIdentityFormat(t *testing.T) {
	raw := make([]byte, identityBytes)
	for i := range raw {
		raw[i] = byte(i * 17)
	}
	encoded := encodeBase32(raw)
	if len(encoded) != 26 {
		t.Fatalf("16 bytes encoded to %d characters, want 26", len(encoded))
	}
	for _, r := range encoded {
		if !strings.ContainsRune(crockford, r) {
			t.Fatalf("encoded identity contains %q outside the Crockford alphabet", r)
		}
	}
}

func containsState(states []TurnState, want TurnState) bool {
	for _, state := range states {
		if state == want {
			return true
		}
	}
	return false
}

// reachesTerminal walks the transition table to prove a terminal state is
// reachable from start.
func reachesTerminal(state TurnState, visited map[TurnState]bool) bool {
	if state.IsTerminal() {
		return true
	}
	if visited[state] {
		return false
	}
	visited[state] = true
	for _, next := range turnTransitions[state] {
		if reachesTerminal(next, visited) {
			return true
		}
	}
	return false
}

func containsSubstring(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

func mustTurnID(s string) domain.TurnID {
	id, err := domain.ParseTurnID(s)
	if err != nil {
		panic(err)
	}
	return id
}

// mustOtherTurnID is a second turn identity, so a test can prove the coordinator
// never confuses two turns' work.
func mustOtherTurnID() domain.TurnID {
	return mustTurnID("trn_01ARZ3NDEKTSV4RRFFQ69G5FBB")
}

func mustAttemptID(s string) domain.AttemptID {
	id, err := domain.ParseAttemptID(s)
	if err != nil {
		panic(err)
	}
	return id
}
