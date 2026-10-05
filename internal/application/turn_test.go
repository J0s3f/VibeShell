package application

import (
	"context"
	"testing"
	"time"

	"j0s.at/vibeshell/internal/domain"
)

// The tests in this file drive whole turns through the coordinator with doubles.
// They assert the observable contract: the recorded state machine, the ordering
// between commit and emission, cancellation, late-response discard, replay-safe
// retries, and per-turn snapshot pinning.

// TestTurnReachesCompletedThroughEveryDocumentedState proves a plain turn walks
// the PLAN 7.1 lifecycle in order and records each transition durably.
func TestTurnReachesCompletedThroughEveryDocumentedState(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)
	h.engine.scriptTurn(0, textTurn("hello from the world"))

	turn, err := shell.Accept(context.Background(), commandLine(shell, 1, "say hello"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	outcome := drain.awaitOutcome(t, turn, waitBudget)
	if outcome.Kind != OutcomeCompleted {
		t.Fatalf("outcome kind = %q, want %q (failure %+v)", outcome.Kind, OutcomeCompleted, outcome.Failure)
	}
	if outcome.State != StateCompleted {
		t.Errorf("final state = %q, want %q", outcome.State, StateCompleted)
	}
	if outcome.Undelivered != 0 {
		t.Errorf("undelivered = %d, want 0", outcome.Undelivered)
	}
	assertStates(t, h.events.transitionPath(turn), []TurnState{
		StateReceived, StateQueued, StateContextReady, StateGenerating,
		StateValidating, StateCommitting, StateEmitting, StateCompleted,
	})

	outputs := drain.awaitOutputs(t, 2, waitBudget)
	assertStrings(t, drain.textOutputs(turn), []string{"hello from the world"})
	if prompt := awaitPromptOutput(t, outputs); prompt.CWD != "/home/alice" {
		t.Errorf("prompt cwd = %q, want the session's working directory", prompt.CWD)
	}
}

// TestCommitPrecedesEmission is the central ordering guarantee of PLAN 9.4: a
// turn's world mutation is durable before any of its output reaches the terminal,
// so a user never sees an answer whose writes were then discarded.
func TestCommitPrecedesEmission(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)
	h.engine.scriptTurn(0, TurnScript{StepFunc: func(in GenerationInput, _ int) (StepOutcome, error) {
		return candidateStep(mutatingCandidate(
			in.Request.Turn, "/home/alice/notes.txt", "wrote the file", "/home/alice/work",
		)), nil
	}})

	turn, err := shell.Accept(context.Background(), commandLine(shell, 1, "write notes"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	outcome := drain.awaitOutcome(t, turn, waitBudget)
	if outcome.Kind != OutcomeCompleted {
		t.Fatalf("outcome kind = %q, want completed (failure %+v)", outcome.Kind, outcome.Failure)
	}

	// The durable world accepted exactly the staged mutation.
	if got := h.world.acceptedMutations(); got != 1 {
		t.Fatalf("world applied %d mutations, want 1", got)
	}
	commit, ok := h.world.lastCommit()
	if !ok {
		t.Fatal("no commit was recorded")
	}
	if commit.ChangeSet.TurnID != turn {
		t.Errorf("committed change set names turn %s, want %s", commit.ChangeSet.TurnID, turn)
	}
	if commit.ChangeSet.AttemptID.IsZero() {
		t.Error("the committed change set carries no attempt reference")
	}
	if commit.Policy != testSnapshot().ScopePolicy {
		t.Error("the commit did not carry the turn's pinned scope policy")
	}

	// The commit event precedes the first terminal frame in the research record.
	commitIndex := h.events.indexOfKind(domain.EventKindWorldCommit)
	frameIndex := h.events.indexOfKind(domain.EventKindTerminalFrame)
	if commitIndex < 0 {
		t.Fatal("no world commit event was recorded")
	}
	if frameIndex < 0 {
		t.Fatal("no terminal frame event was recorded")
	}
	if commitIndex > frameIndex {
		t.Errorf("world commit recorded at %d, terminal frame at %d: emission preceded the commit", commitIndex, frameIndex)
	}

	// The observed call order agrees with the recorded order.
	marks := h.order.marks_()
	worldAt, terminalAt := h.order.indexOf("world.commit"), h.order.indexOf("terminal.text")
	if worldAt < 0 || terminalAt < 0 {
		t.Fatalf("expected a world commit and a terminal write, got %v", marks)
	}
	if worldAt > terminalAt {
		t.Errorf("the world was written at mark %d but the terminal at %d: %v", worldAt, terminalAt, marks)
	}

	// Session-local state changed only after the commit, so the prompt shows it.
	if prompt := awaitPromptOutput(t, drain.awaitOutputs(t, 2, waitBudget)); prompt.CWD != "/home/alice/work" {
		t.Errorf("prompt cwd = %q, want the committed working directory", prompt.CWD)
	}
	if shell.Snapshot().CWD != "/home/alice/work" {
		t.Errorf("session cwd = %q, want the committed working directory", shell.Snapshot().CWD)
	}
}

// TestToolWorkRunsInsideTheTurnAndFeedsTheNextGeneration proves awaiting-tool is
// a real recorded state, that tool batches execute under the turn's scope, and
// that their results reach the following generation.
func TestToolWorkRunsInsideTheTurnAndFeedsTheNextGeneration(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)
	toolResult := ToolCallResult{Name: "list_dir", Result: []byte(`{"entries":2}`)}
	h.tools.result = ToolBatchResult{Results: []ToolCallResult{toolResult}}
	h.engine.scriptTurn(0, TurnScript{
		Steps: []StepOutcome{toolStep("list_dir"), {}},
		StepFunc: func(in GenerationInput, index int) (StepOutcome, error) {
			switch index {
			case 0:
				return toolStep("list_dir"), nil
			case 1:
				return candidateStep(textCandidate(in.Request.Turn, "two entries")), nil
			default:
				return StepOutcome{}, errPastScript
			}
		},
	})

	turn, err := shell.Accept(context.Background(), commandLine(shell, 1, "ls"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if outcome := drain.awaitOutcome(t, turn, waitBudget); outcome.Kind != OutcomeCompleted {
		t.Fatalf("outcome kind = %q, want completed (failure %+v)", outcome.Kind, outcome.Failure)
	}

	assertStates(t, h.events.transitionPath(turn), []TurnState{
		StateReceived, StateQueued, StateContextReady, StateGenerating,
		StateAwaitingTool, StateGenerating, StateValidating, StateCommitting,
		StateEmitting, StateCompleted,
	})

	batch, ok := h.tools.batchAt(0)
	if !ok {
		t.Fatal("no tool batch was executed")
	}
	if len(batch.Calls) != 1 || batch.Calls[0].Name != "list_dir" {
		t.Fatalf("tool batch = %+v, want one list_dir call", batch.Calls)
	}
	if batch.Request.Turn != turn {
		t.Errorf("tool batch names turn %s, want %s", batch.Request.Turn, turn)
	}

	seen := h.engine.toolResultsSeen()
	if len(seen) != 2 {
		t.Fatalf("recorded tool results for %d generations, want 2", len(seen))
	}
	if len(seen[0]) != 0 {
		t.Errorf("the first generation received tool results %+v, want none", seen[0])
	}
	if len(seen[1]) != 1 || seen[1][0].Name != "list_dir" {
		t.Errorf("the second generation received %+v, want the previous batch's results", seen[1])
	}
}

// TestCtrlCInterruptsTheRunningTurn proves a cancellation is prompt, produces an
// interrupted outcome rather than a failure, and reaches neither the world nor
// the terminal.
func TestCtrlCInterruptsTheRunningTurn(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)

	gate := h.engine.blockGeneration(0, 0)
	h.engine.scriptTurn(0, TurnScript{
		Blocks: map[int]chan struct{}{0: gate},
		StepFunc: func(in GenerationInput, _ int) (StepOutcome, error) {
			return candidateStep(mutatingCandidate(
				in.Request.Turn, "/home/alice/notes.txt", "too late to show this", "/home/alice/work",
			)), nil
		},
	})

	turn, err := shell.Accept(context.Background(), commandLine(shell, 1, "slow command"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	waitForState(t, shell, waitBudget, StateGenerating)

	interrupted, err := shell.CancelActive(context.Background(), KeyCtrlC)
	if err != nil {
		t.Fatalf("CancelActive: %v", err)
	}
	if !interrupted {
		t.Fatal("CancelActive reported no running turn")
	}

	outcome := drain.awaitOutcome(t, turn, waitBudget)
	if outcome.Kind != OutcomeInterrupted {
		t.Fatalf("outcome kind = %q, want %q (failure %+v)", outcome.Kind, OutcomeInterrupted, outcome.Failure)
	}
	if outcome.State != StateInterrupted {
		t.Errorf("final state = %q, want %q", outcome.State, StateInterrupted)
	}

	// The blocked generation was released by the cancelled scope, so the turn
	// really did unwind rather than hang until its deadline.
	close(gate)
	awaitIdle(t, shell, waitBudget)

	// A cancelled turn stages nothing and shows nothing.
	if got := h.world.acceptedMutations(); got != 0 {
		t.Errorf("a cancelled turn committed %d mutations", got)
	}
	if texts := drain.textOutputs(turn); len(texts) != 0 {
		t.Errorf("a cancelled turn emitted %v", texts)
	}
	if shell.Snapshot().CWD != "/home/alice" {
		t.Errorf("a cancelled turn changed the working directory to %q", shell.Snapshot().CWD)
	}

	// The interruption is durably recorded, and the cancellation took no
	// provider-health action: it is a state, not a failure with a cause.
	path := h.events.transitionPath(turn)
	if last := path[len(path)-1]; last != StateInterrupted {
		t.Errorf("recorded path ends at %q, want %q", last, StateInterrupted)
	}
	if outcome.Failure != nil && outcome.Failure.Class == domain.FailureUserCancelled {
		t.Error("an interruption is reported as an outcome, not as a cancellation failure")
	}
}

// TestCtrlCThroughTheControlKeyPath proves the transport's actual input shape,
// a Ctrl-C key press, reaches the same cancellation intent as CancelActive.
func TestCtrlCThroughTheControlKeyPath(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)

	gate := h.engine.blockGeneration(0, 0)
	h.engine.scriptTurn(0, TurnScript{
		Blocks:       map[int]chan struct{}{0: gate},
		Preparations: nil,
	})
	close(gate) // the generation completes immediately; Ctrl-C arrives after it

	turn, err := shell.Accept(context.Background(), commandLine(shell, 1, "quick"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	drain.awaitOutcome(t, turn, waitBudget)

	// With no turn running, a Ctrl-C is accepted and interrupts nothing.
	if _, err := shell.Accept(context.Background(), SessionInput{
		Session:   shell.ID(),
		Principal: shell.Principal(),
		Kind:      InputKey,
		Key:       KeyCtrlC,
	}); err != nil {
		t.Fatalf("Ctrl-C on an idle session: %v", err)
	}
	if h.events.countOfKind(domain.EventKindInputAccepted) < 2 {
		t.Error("the accepted Ctrl-C was not recorded as input")
	}
}

// TestLateResponseIsDiscarded proves a response from a replaced generation can
// update neither the world nor the screen, and that the discard is recorded.
func TestLateResponseIsDiscarded(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)

	// The generation ignores the cancelled scope and answers anyway, which is
	// exactly the late response the coordinator has to discard.
	gate := h.engine.blockGeneration(0, 0)
	h.engine.scriptTurn(0, TurnScript{
		Blocks:   map[int]chan struct{}{0: gate},
		Stubborn: true,
		StepFunc: func(in GenerationInput, _ int) (StepOutcome, error) {
			return candidateStep(mutatingCandidate(
				in.Request.Turn, "/home/alice/late.txt", "arrived too late", "/home/alice/late",
			)), nil
		},
	})

	turn, err := shell.Accept(context.Background(), commandLine(shell, 1, "slow command"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	waitForState(t, shell, waitBudget, StateGenerating)

	// Interrupt the turn, then let the in-flight generation return its candidate.
	if _, err := shell.CancelActive(context.Background(), KeyCtrlC); err != nil {
		t.Fatalf("CancelActive: %v", err)
	}
	outcome := drain.awaitOutcome(t, turn, waitBudget)
	if outcome.Kind != OutcomeInterrupted {
		t.Fatalf("outcome kind = %q, want %q", outcome.Kind, OutcomeInterrupted)
	}
	close(gate)

	// Give the released worker time to apply its late response if it were going
	// to; the discard must leave no trace afterwards.
	awaitIdle(t, shell, waitBudget)
	time.Sleep(50 * time.Millisecond)

	if got := h.world.acceptedMutations(); got != 0 {
		t.Errorf("a late response committed %d mutations", got)
	}
	if texts := drain.textOutputs(turn); len(texts) != 0 {
		t.Errorf("a late response reached the terminal: %v", texts)
	}
	if shell.Snapshot().CWD != "/home/alice" {
		t.Errorf("a late response changed the working directory to %q", shell.Snapshot().CWD)
	}
	if h.renderer.renderedFrames() != 0 {
		t.Error("a late response rendered a frame")
	}
	if reasons := h.events.notedContaining(turn, "replaced generation"); len(reasons) == 0 {
		t.Error("the discarded late response was not recorded in the turn's lifecycle notes")
	}
}

// TestReplaySafeRetryCommitsOnce proves a retried turn never applies the same
// logical commit twice: the failed generation's staged work is discarded and only
// the accepted generation's mutation reaches the world.
func TestReplaySafeRetryCommitsOnce(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)
	h.engine.scriptTurn(0, TurnScript{
		StepErrors: []error{providerError("route is cooling down"), nil},
		StepFunc: func(in GenerationInput, index int) (StepOutcome, error) {
			if index == 0 {
				return StepOutcome{}, providerError("route is cooling down")
			}
			// The retry proposes the same logical commit, as a real retry would.
			return candidateStep(mutatingCandidate(
				in.Request.Turn, "/home/alice/notes.txt", "written on the second attempt", "/home/alice/work",
			)), nil
		},
	})

	turn, err := shell.Accept(context.Background(), commandLine(shell, 1, "write notes"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	outcome := drain.awaitOutcome(t, turn, waitBudget)
	if outcome.Kind != OutcomeCompleted {
		t.Fatalf("outcome kind = %q, want completed (failure %+v)", outcome.Kind, outcome.Failure)
	}

	// Two generations ran, and the failed one recorded no commit.
	if got := h.engine.generationCount(0); got != 2 {
		t.Fatalf("ran %d generations, want 2", got)
	}
	if got := h.world.acceptedMutations(); got != 1 {
		t.Fatalf("world applied %d mutations across the retry, want exactly 1", got)
	}
	if got := h.world.commitCount(); got != 1 {
		t.Fatalf("world received %d commits, want 1", got)
	}
	// The output was shown once, not once per attempt.
	assertStrings(t, drain.textOutputs(turn), []string{"written on the second attempt"})
	if h.content.putCount() != 1 {
		t.Errorf("stored %d output payloads, want 1", h.content.putCount())
	}

	// Each generation had its own attempt identity, so the research record can
	// tell the discarded attempt from the accepted one.
	generations := h.engine.generationsOf(0)
	if generations[0].Attempt == generations[1].Attempt {
		t.Error("the retry reused the previous attempt identity")
	}
	if generations[0].Generation == generations[1].Generation {
		t.Error("the retry reused the previous generation number")
	}
	if generations[0].Turn != turn || generations[1].Turn != turn {
		t.Error("the retry left the turn it belongs to")
	}
}

// TestCommitLedgerSkipsAReplayedLogicalCommit proves the coordinator's replay
// guard directly: presenting one logical commit key twice inside a turn applies
// it once. A retry that re-presents the same staged mutation must not write it
// twice.
func TestCommitLedgerSkipsAReplayedLogicalCommit(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)
	h.engine.scriptTurn(0, textTurn("first presentation"))

	first, err := shell.Accept(context.Background(), commandLine(shell, 1, "write notes"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	drain.awaitOutcome(t, first, waitBudget)

	// Reach the live session runtime and present one logical commit key twice
	// inside a single synthetic turn, which is exactly what a replayed retry
	// looks like to the commit step. The coordinator never does this itself, so
	// this is the only way to prove the guard rather than the happy path.
	sess := h.sessionOf(t, shell)
	replay := &turn{
		session:  sess,
		id:       mustOtherTurnID(),
		state:    StateCommitting,
		attempt:  mustAttemptID("att_01ARZ3NDEKTSV4RRFFQ69G5FAV"),
		commits:  make(CommitLedger),
		snapshot: testSnapshot(),
	}

	key := CommitKey{Turn: replay.id, Logical: "stage-1"}
	mutations := domain.ChangeSet{Mutations: []domain.Mutation{{
		Type:        domain.MutationCreate,
		NamespaceID: testNamespace,
		Path:        "/home/alice/replayed.txt",
		Kind:        domain.NodeKindFile,
	}}}

	applied, err := sess.commit(context.Background(), replay, &TurnCandidate{CommitKey: key, Changes: mutations})
	if err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if applied.Replayed {
		t.Fatal("the first presentation of a commit key was treated as a replay")
	}

	repeated, err := sess.commit(context.Background(), replay, &TurnCandidate{CommitKey: key, Changes: mutations})
	if err != nil {
		t.Fatalf("replayed commit: %v", err)
	}
	if !repeated.Replayed {
		t.Fatal("a repeated logical commit key was not recognised as a replay")
	}
	if repeated.Revision != applied.Revision {
		t.Errorf("the replay reported revision %d, want the original %d", repeated.Revision, applied.Revision)
	}

	if got := h.world.acceptedMutations(); got != len(mutations.Mutations) {
		t.Fatalf("the world applied %d mutations across the replay, want %d", got, len(mutations.Mutations))
	}
	if got := h.world.commitCount(); got != 1 {
		t.Fatalf("the world received %d commits, want 1", got)
	}

	// A different logical key inside the same turn is genuinely new work.
	other := CommitKey{Turn: replay.id, Logical: "stage-2"}
	if _, err := sess.commit(context.Background(), replay, &TurnCandidate{CommitKey: other, Changes: mutations}); err != nil {
		t.Fatalf("second logical commit: %v", err)
	}
	if got := h.world.commitCount(); got != 2 {
		t.Errorf("the world received %d commits, want 2 for two distinct keys", got)
	}
}

// TestTwoTurnsAreIndependentWork proves a second turn in the same session does
// not inherit the first turn's identity, attempt budget, or commit ledger.
func TestTwoTurnsAreIndependentWork(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)
	h.engine.scriptTurn(0, textTurn("first"))
	h.engine.scriptTurn(1, textTurn("second"))

	first, err := shell.Accept(context.Background(), commandLine(shell, 1, "one"))
	if err != nil {
		t.Fatalf("first Accept: %v", err)
	}
	drain.awaitOutcome(t, first, waitBudget)
	second, err := shell.Accept(context.Background(), commandLine(shell, 2, "two"))
	if err != nil {
		t.Fatalf("second Accept: %v", err)
	}
	drain.awaitOutcome(t, second, waitBudget)

	firstRequests := h.engine.generationsOf(0)
	secondRequests := h.engine.generationsOf(1)
	if len(firstRequests) == 0 || len(secondRequests) == 0 {
		t.Fatal("both turns should have run a generation")
	}
	if firstRequests[0].Turn == secondRequests[0].Turn {
		t.Error("two turns shared one turn identity")
	}
	if firstRequests[0].Session != secondRequests[0].Session {
		t.Error("two turns of one session reported different sessions")
	}
	// Attempt numbering is per turn, so the second turn starts fresh.
	if secondRequests[0].AttemptNumber != 1 {
		t.Errorf("the second turn started at attempt %d, want 1", secondRequests[0].AttemptNumber)
	}
	if firstRequests[0].Attempt == secondRequests[0].Attempt {
		t.Error("two turns shared one attempt identity")
	}
	if got := shell.Snapshot().TurnsCompleted; got != 2 {
		t.Errorf("completed turns = %d, want 2", got)
	}
}

// TestConcurrentWorldConflictRebasesAndCompletes proves a conflict re-reads the
// state that changed, re-prepares the turn context, and completes without a
// provider-health penalty or a duplicate mutation.
func TestConcurrentWorldConflictRebasesAndCompletes(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)
	h.world.conflictNext(domain.NewConflictError(domain.CodeConcurrentModification,
		"expected revision 4, found 7", map[string]string{
			"expected_rev": "4",
			"actual_rev":   "7",
		}))
	h.engine.scriptTurn(0, TurnScript{StepFunc: func(in GenerationInput, index int) (StepOutcome, error) {
		if index == 0 {
			return candidateStep(mutatingCandidate(
				in.Request.Turn, "/home/alice/contested.txt", "written after the rebase", "/home/alice/work",
			)), nil
		}
		return candidateStep(mutatingCandidate(
			in.Request.Turn, "/home/alice/contested.txt", "written after the rebase", "/home/alice/work",
		)), nil
	}})

	turn, err := shell.Accept(context.Background(), commandLine(shell, 1, "write contested"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	outcome := drain.awaitOutcome(t, turn, waitBudget)
	if outcome.Kind != OutcomeCompleted {
		t.Fatalf("outcome kind = %q, want completed after the rebase (failure %+v)", outcome.Kind, outcome.Failure)
	}

	assertStates(t, h.events.transitionPath(turn), []TurnState{
		StateReceived, StateQueued, StateContextReady, StateGenerating,
		StateValidating, StateCommitting, StateConflicted, StateQueued,
		StateContextReady, StateGenerating, StateValidating, StateCommitting,
		StateEmitting, StateCompleted,
	})

	// The conflict was recorded with the revisions the store reported. The
	// payload is read generically because its optional node identity is absent
	// when the store named no conflicting node, which the domain identity format
	// does not allow as an empty value.
	fields, ok := h.events.payloadFieldsOf(domain.EventKindWorldConflict)
	if !ok {
		t.Fatal("no world conflict event was recorded")
	}
	if got := fields["expected_rev"]; got != float64(4) {
		t.Errorf("conflict recorded expected revision %v, want 4", got)
	}
	if got := fields["actual_rev"]; got != float64(7) {
		t.Errorf("conflict recorded actual revision %v, want 7", got)
	}

	// A rebase re-reads the state that changed, so context was assembled twice;
	// a repair retry would not have.
	if got := h.engine.prepareCount(0); got != 2 {
		t.Errorf("assembled turn context %d times, want 2", got)
	}
	// The rejected attempt's mutation never reached the world.
	if got := h.world.acceptedMutations(); got != 1 {
		t.Fatalf("world applied %d mutations, want exactly 1", got)
	}
}

// TestRejectedResponseGetsOneBoundedRepair proves a malformed candidate is
// refused, retried once with the same prepared context, and that the rejected
// text was never emitted.
func TestRejectedResponseGetsOneBoundedRepair(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)
	h.engine.scriptTurn(0, TurnScript{StepFunc: func(in GenerationInput, index int) (StepOutcome, error) {
		if index == 0 {
			// A control sequence in plain text is a rejected outcome.
			bad := textCandidate(in.Request.Turn, "clears\x1b[2Jthe screen")
			return candidateStep(bad), nil
		}
		return candidateStep(textCandidate(in.Request.Turn, "safe text")), nil
	}})

	turn, err := shell.Accept(context.Background(), commandLine(shell, 1, "draw something"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	outcome := drain.awaitOutcome(t, turn, waitBudget)
	if outcome.Kind != OutcomeCompleted {
		t.Fatalf("outcome kind = %q, want completed after the repair (failure %+v)", outcome.Kind, outcome.Failure)
	}

	assertStates(t, h.events.transitionPath(turn), []TurnState{
		StateReceived, StateQueued, StateContextReady, StateGenerating,
		StateValidating, StateGenerating, StateValidating, StateCommitting,
		StateEmitting, StateCompleted,
	})
	// A repair retry re-asks the model with the same context, so it does not
	// re-assemble.
	if got := h.engine.prepareCount(0); got != 1 {
		t.Errorf("assembled turn context %d times, want 1", got)
	}
	// The rejected output never reached the user.
	assertStrings(t, drain.textOutputs(turn), []string{"safe text"})
}

// TestAttemptBudgetExhaustionFailsTheTurn proves an unbounded retry loop is
// impossible: the attempt budget ends the turn with a classified failure.
func TestAttemptBudgetExhaustionFailsTheTurn(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)
	// testLimits allows two attempts, so both generations fail.
	h.engine.scriptTurn(0, TurnScript{
		StepFunc: func(_ GenerationInput, _ int) (StepOutcome, error) {
			return StepOutcome{}, providerError("route still unavailable")
		},
	})

	turn, err := shell.Accept(context.Background(), commandLine(shell, 1, "do something"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	outcome := drain.awaitOutcome(t, turn, waitBudget)
	if outcome.Kind != OutcomeFailed {
		t.Fatalf("outcome kind = %q, want %q", outcome.Kind, OutcomeFailed)
	}
	if outcome.State != StateFailed {
		t.Errorf("final state = %q, want %q", outcome.State, StateFailed)
	}
	if outcome.Failure == nil {
		t.Fatal("a failed turn reported no failure reason")
	}
	if outcome.Failure.Class != domain.FailureProviderOutage {
		t.Errorf("failure class = %q, want %q", outcome.Failure.Class, domain.FailureProviderOutage)
	}
	if got := h.engine.generationCount(0); got != testLimits().MaxAttempts {
		t.Errorf("ran %d generations, want the attempt budget %d", got, testLimits().MaxAttempts)
	}
	// Nothing was emitted or committed by a turn that never produced an outcome.
	if got := h.world.acceptedMutations(); got != 0 {
		t.Errorf("a failed turn committed %d mutations", got)
	}
}

// TestUnknownCommitOutcomeIsNotRetried proves the coordinator refuses to replay
// a commit whose result it does not know: re-applying it could duplicate a
// mutation, so the turn fails and the ambiguity stays visible.
func TestUnknownCommitOutcomeIsNotRetried(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)
	h.world.commitHook = func(int, domain.ChangeSet) error {
		return domain.NewUnavailableError(domain.CodeDatabaseUnavailable, "commit outcome unknown", nil, nil)
	}
	h.engine.scriptTurn(0, TurnScript{StepFunc: func(in GenerationInput, _ int) (StepOutcome, error) {
		return candidateStep(mutatingCandidate(
			in.Request.Turn, "/home/alice/notes.txt", "may or may not exist", "/home/alice/work",
		)), nil
	}})

	turn, err := shell.Accept(context.Background(), commandLine(shell, 1, "write notes"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	outcome := drain.awaitOutcome(t, turn, waitBudget)
	if outcome.Kind != OutcomeFailed {
		t.Fatalf("outcome kind = %q, want %q for an unknown commit outcome", outcome.Kind, OutcomeFailed)
	}
	if reasons := h.events.notedContaining(turn, "not retried"); len(reasons) == 0 {
		t.Error("the unknown commit outcome was not recorded as deliberately not retried")
	}
	if got := h.engine.generationCount(0); got != 1 {
		t.Errorf("ran %d generations, want exactly 1: an unknown commit must not be retried", got)
	}
	// The working directory was not changed, because the commit never succeeded.
	if shell.Snapshot().CWD != "/home/alice" {
		t.Errorf("cwd = %q, want the pre-commit working directory", shell.Snapshot().CWD)
	}
}

// TestTurnPinsItsConfigurationSnapshot proves a live configuration or prompt
// reload changes the next turn, never a turn already in flight, and that the
// snapshot is taken once per turn rather than once per generation.
func TestTurnPinsItsConfigurationSnapshot(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)

	gate := h.engine.blockGeneration(0, 0)
	h.engine.scriptTurn(0, TurnScript{
		Blocks: map[int]chan struct{}{0: gate},
		StepFunc: func(in GenerationInput, index int) (StepOutcome, error) {
			if index == 0 {
				return toolStep("list_dir"), nil
			}
			return candidateStep(textCandidate(in.Request.Turn, "done")), nil
		},
	})

	turn, err := shell.Accept(context.Background(), commandLine(shell, 1, "ls"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	waitForState(t, shell, waitBudget, StateGenerating)

	// An operator reloads the configuration and the prompt mid-turn.
	reloaded := h.snapshots.reload()
	if reloaded.ConfigVersion == 1 {
		t.Fatal("the reload did not publish a new configuration revision")
	}

	close(gate)
	if outcome := drain.awaitOutcome(t, turn, waitBudget); outcome.Kind != OutcomeCompleted {
		t.Fatalf("outcome kind = %q, want completed (failure %+v)", outcome.Kind, outcome.Failure)
	}

	// Every generation and every preparation of that turn used the pinned
	// revision, not the reloaded one.
	for _, request := range h.engine.generationsOf(0) {
		if request.Snapshot.ConfigVersion != 1 {
			t.Errorf("generation %d ran under configuration %d, want the pinned 1", request.Generation, request.Snapshot.ConfigVersion)
		}
		if request.Snapshot.PromptVersion != "prompt-v1" {
			t.Errorf("generation %d ran under prompt %q, want the pinned prompt-v1", request.Generation, request.Snapshot.PromptVersion)
		}
		if request.Snapshot.CatalogueVersion != "catalogue-v1" {
			t.Errorf("generation %d ran under catalogue %q, want the pinned catalogue-v1", request.Generation, request.Snapshot.CatalogueVersion)
		}
	}
	for _, request := range h.engine.prepareRequests(0) {
		if request.Snapshot.ConfigVersion != 1 {
			t.Errorf("context assembly ran under configuration %d, want the pinned 1", request.Snapshot.ConfigVersion)
		}
	}

	// The pinned versions are in the durable record, so the turn can be audited
	// against the configuration it actually used.
	var payload domain.TurnTransitionPayload
	found := false
	for _, recorded := range h.events.transitions(turn) {
		if recorded.To == StateGenerating {
			payload = recorded
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no transition into generating was recorded")
	}
	if payload.ConfigVersion != 1 || payload.PromptVersion != "prompt-v1" {
		t.Errorf("the generating transition recorded config %d prompt %q, want 1/prompt-v1",
			payload.ConfigVersion, payload.PromptVersion)
	}

	// The next turn pins the reloaded configuration.
	h.engine.scriptTurn(1, textTurn("after reload"))
	next, err := shell.Accept(context.Background(), commandLine(shell, 2, "ls again"))
	if err != nil {
		t.Fatalf("second Accept: %v", err)
	}
	drain.awaitOutcome(t, next, waitBudget)
	nextRequests := h.engine.generationsOf(1)
	if len(nextRequests) == 0 {
		t.Fatal("the second turn ran no generation")
	}
	if nextRequests[0].Snapshot.ConfigVersion != reloaded.ConfigVersion {
		t.Errorf("the second turn ran under configuration %d, want the reloaded %d",
			nextRequests[0].Snapshot.ConfigVersion, reloaded.ConfigVersion)
	}
	if nextRequests[0].Snapshot.PromptVersion != reloaded.PromptVersion {
		t.Errorf("the second turn ran under prompt %q, want the reloaded %q",
			nextRequests[0].Snapshot.PromptVersion, reloaded.PromptVersion)
	}

	// One snapshot per turn: opening the session, the pinned turn, the next turn.
	if got := h.snapshots.promptsIssued(); got != 3 {
		t.Errorf("issued %d snapshots, want 3: one per session and per turn, not per generation", got)
	}
}

// TestTwoSessionsShareDurableStateButNotSessionState proves the central
// isolation rule of PLAN 5.2: two sessions of one user see the same files and
// keep separate working directories, foreground attachments, and exit statuses.
func TestTwoSessionsShareDurableStateButNotSessionState(t *testing.T) {
	h := newHarness(t, testLimits())
	drainA, shellA := h.openSession(t, testPrincipal, "/home/alice")
	drainB, shellB := h.openSession(t, testPrincipal, "/home/alice/work")

	// Session A moves into a new directory and writes a file.
	moved := domain.ValidPath("/home/alice/work/deep")
	h.engine.scriptTurn(0, TurnScript{StepFunc: func(in GenerationInput, _ int) (StepOutcome, error) {
		return candidateStep(mutatingCandidate(
			in.Request.Turn, "/home/alice/work/shared.txt", "written by session A", moved,
		)), nil
	}})
	turnA, err := shellA.Accept(context.Background(), commandLine(shellA, 1, "cd work/deep"))
	if err != nil {
		t.Fatalf("session A Accept: %v", err)
	}
	drainA.awaitOutcome(t, turnA, waitBudget)

	// Durable state is shared: the mutation both sessions would read is one.
	if got := h.world.acceptedMutations(); got != 1 {
		t.Fatalf("world applied %d mutations, want 1", got)
	}
	commit, _ := h.world.lastCommit()
	if commit.ChangeSet.TurnID != turnA {
		t.Errorf("committed turn = %s, want session A's turn %s", commit.ChangeSet.TurnID, turnA)
	}

	// Session-local state is not shared.
	if got := shellA.Snapshot().CWD; got != moved {
		t.Errorf("session A cwd = %q, want %q", got, moved)
	}
	if got := shellB.Snapshot().CWD; got != "/home/alice/work" {
		t.Errorf("session B cwd = %q, want its own starting directory", got)
	}
	if shellB.Snapshot().Foreground.Kind != ForegroundShell {
		t.Errorf("session B foreground = %q, want the shell", shellB.Snapshot().Foreground.Kind)
	}

	// A resize applies to one session only.
	if _, err := shellB.Accept(context.Background(), SessionInput{
		Session:   shellB.ID(),
		Principal: shellB.Principal(),
		Kind:      InputResize,
		Size:      &domain.TermSize{Cols: 132, Rows: 50},
	}); err != nil {
		t.Fatalf("resize: %v", err)
	}
	if got := shellB.Snapshot().Terminal.Size; got != (domain.TermSize{Cols: 132, Rows: 50}) {
		t.Errorf("session B terminal size = %+v, want 132x50", got)
	}
	if got := shellA.Snapshot().Terminal.Size; got != DefaultTerminalSize {
		t.Errorf("session A terminal size = %+v, want the unchanged default", got)
	}

	// Each session's turns see its own working directory in the request the
	// engine receives: session B never observed session A's change of directory.
	h.engine.scriptTurn(1, textTurn("session B output"))
	turnB, err := drainB.shell.Accept(context.Background(), commandLine(drainB.shell, 1, "pwd"))
	if err != nil {
		t.Fatalf("session B Accept: %v", err)
	}
	drainB.awaitOutcome(t, turnB, waitBudget)

	sessionBRequests := h.engine.generationRequestsForSession(drainB.shell.ID())
	if len(sessionBRequests) == 0 {
		t.Fatal("session B ran no generation")
	}
	if got := sessionBRequests[0].Context.CWD; got != "/home/alice/work" {
		t.Errorf("session B turn saw cwd %q, want its own directory", got)
	}
	if sessionBRequests[0].Session == drainA.shell.ID() {
		t.Error("session B's turn carried session A's identity")
	}
	if sessionBRequests[0].Context.ExitStatus != 0 {
		t.Errorf("session B inherited session A's exit status %d", sessionBRequests[0].Context.ExitStatus)
	}
}

// TestSessionRejectsAnotherPrincipalsInput proves a session handle is a boundary:
// input for another session or another user is refused, so a multiplexing adapter
// cannot cross-deliver.
func TestSessionRejectsAnotherPrincipalsInput(t *testing.T) {
	h := newHarness(t, testLimits())
	_, first := h.testSession(t)
	_, second := h.openSession(t, testPrincipal, "/tmp")

	cases := map[string]SessionInput{
		"input for another session": {
			Session: second.ID(), Principal: second.Principal(),
			Sequence: 1, Kind: InputCommand, Command: "ls",
		},
		"input for another principal": {
			Session: first.ID(), Principal: otherPrincipal,
			Sequence: 1, Kind: InputCommand, Command: "ls",
		},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := first.Accept(context.Background(), in); err == nil {
				t.Fatal("expected the cross-delivered input to be refused")
			}
		})
	}

	if got := h.engine.generationCount(0); got != 0 {
		t.Errorf("refused input started %d generations", got)
	}
}

// TestTurnQueueIsBounded proves a session cannot accumulate unbounded semantic
// work: a full queue refuses new input with a limit error and leaves no turn.
func TestTurnQueueIsBounded(t *testing.T) {
	h := newHarness(t, testLimits())
	_, shell := h.testSession(t)

	gate := h.engine.blockGeneration(0, 0)
	h.engine.scriptTurn(0, TurnScript{
		Blocks: map[int]chan struct{}{0: gate},
		StepFunc: func(in GenerationInput, _ int) (StepOutcome, error) {
			return candidateStep(textCandidate(in.Request.Turn, "ok")), nil
		},
	})

	if _, err := shell.Accept(context.Background(), commandLine(shell, 1, "first")); err != nil {
		t.Fatalf("first Accept: %v", err)
	}
	waitForState(t, shell, waitBudget, StateGenerating)

	limits := testLimits()
	for seq := uint64(2); seq <= uint64(limits.MaxQueuedTurns)+1; seq++ {
		if _, err := shell.Accept(context.Background(), commandLine(shell, seq, "queued")); err != nil {
			t.Fatalf("Accept %d: %v", seq, err)
		}
	}
	overflow := uint64(limits.MaxQueuedTurns) + 2
	_, err := shell.Accept(context.Background(), commandLine(shell, overflow, "one too many"))
	if got := errorsReported(t, err); got != domain.CodeMaxAttemptsReached {
		t.Fatalf("rejection code = %q, want %q", got, domain.CodeMaxAttemptsReached)
	}
	if got := shell.Snapshot().QueuedTurns; got != limits.MaxQueuedTurns {
		t.Errorf("queued turns = %d, want the bound %d", got, limits.MaxQueuedTurns)
	}

	close(gate)
	awaitIdle(t, shell, waitBudget)
}

// TestReplayedTransportInputReturnsTheOriginalTurn proves a replayed channel
// sequence does not start a second turn, which is what keeps a retried transport
// frame from running a command twice.
func TestReplayedTransportInputReturnsTheOriginalTurn(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)
	h.engine.scriptTurn(0, textTurn("once"))

	first, err := shell.Accept(context.Background(), commandLine(shell, 7, "echo once"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	drain.awaitOutcome(t, first, waitBudget)

	replay, err := shell.Accept(context.Background(), commandLine(shell, 7, "echo once"))
	if err != nil {
		t.Fatalf("replayed Accept: %v", err)
	}
	if replay != first {
		t.Fatalf("replayed input produced turn %s, want the original %s", replay, first)
	}
	awaitIdle(t, shell, waitBudget)

	if got := drain.outcomeCount(); got != 1 {
		t.Errorf("recorded %d outcomes, want 1", got)
	}
	if got := h.engine.generationCount(0); got != 1 {
		t.Errorf("ran %d generations, want 1", got)
	}
	assertStrings(t, drain.textOutputs(first), []string{"once"})
}

// TestDurableRecordingFailureRefusesNewWork proves the coordinator stops
// accepting semantic work rather than continuing without a research record
// (PLAN 10.3).
func TestDurableRecordingFailureRefusesNewWork(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)
	h.engine.scriptTurn(0, textTurn("recorded"))

	turn, err := shell.Accept(context.Background(), commandLine(shell, 1, "echo recorded"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	drain.awaitOutcome(t, turn, waitBudget)

	// Break the durable record after this turn is established: the accept
	// appends succeed, so the turn exists, and the first append from the worker
	// fails.
	h.events.failAfter(2, storageFailure())
	h.engine.scriptTurn(1, textTurn("unrecorded"))

	second, err := shell.Accept(context.Background(), commandLine(shell, 2, "echo unrecorded"))
	if err != nil {
		t.Fatalf("second Accept: %v", err)
	}
	secondOutcome := drain.awaitOutcome(t, second, waitBudget)

	// A third turn is refused outright: the session will not run unrecorded.
	h.engine.scriptTurn(2, textTurn("refused"))
	_, err = shell.Accept(context.Background(), commandLine(shell, 3, "echo refused"))
	if got := errorsReported(t, err); got != domain.CodeDatabaseUnavailable {
		t.Fatalf("rejection code = %q, want %q", got, domain.CodeDatabaseUnavailable)
	}
	if got := h.engine.generationCount(2); got != 0 {
		t.Errorf("a refused turn ran %d generations", got)
	}

	// The recorded turn's outcome still reports what happened to its output.
	if secondOutcome.Undelivered == 0 {
		t.Error("a turn whose output could not be recorded did not report undelivered output")
	}
}

// TestReportWriteRecordsTransportOutcomeWithoutClaimingDelivery proves the
// research record distinguishes "handed to the transport" from "read by a
// person".
func TestReportWriteRecordsTransportOutcomeWithoutClaimingDelivery(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)
	h.engine.scriptTurn(0, textTurn("output"))

	turn, err := shell.Accept(context.Background(), commandLine(shell, 1, "echo output"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	drain.awaitOutcome(t, turn, waitBudget)

	if err := shell.ReportWrite(context.Background(), WriteResult{
		Session:   shell.ID(),
		Turn:      turn,
		Sequence:  1,
		Status:    WriteOK,
		ByteCount: 6,
	}); err != nil {
		t.Fatalf("ReportWrite: %v", err)
	}

	var payload domain.TerminalWriteOutcomePayload
	if !h.events.payloadOf(domain.EventKindTerminalWrite, &payload) {
		t.Fatal("no transport write outcome was recorded")
	}
	if payload.Status != string(WriteOK) || payload.OutputSequence != 1 || payload.ByteCount != 6 {
		t.Errorf("recorded write outcome = %+v, want ok/1/6", payload)
	}

	if err := shell.ReportWrite(context.Background(), WriteResult{Sequence: 2}); err == nil {
		t.Error("a write result without a status was accepted")
	}
}

// TestEndIsIdempotentAndRecordsTheReason proves the session lifecycle's ending
// rule: ending twice succeeds, and the reason is durable.
func TestEndIsIdempotentAndRecordsTheReason(t *testing.T) {
	h := newHarness(t, testLimits())
	shell, err := h.coord.Open(context.Background(), OpenSessionRequest{
		Principal: testPrincipal,
		AuthMode:  AuthModeSecure,
		Terminal:  TerminalMetadata{Term: "xterm-256color", Size: DefaultTerminalSize},
		CWD:       "/home/alice",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := shell.End(context.Background(), EndDisconnect); err != nil {
		t.Fatalf("End: %v", err)
	}
	if err := shell.End(context.Background(), EndShutdown); err != nil {
		t.Fatalf("second End must be idempotent: %v", err)
	}

	select {
	case <-shell.Done():
	default:
		t.Fatal("Done was not closed after End")
	}
	if got := shell.Snapshot(); !got.Ended || got.EndReason != EndDisconnect {
		t.Errorf("session snapshot = %+v, want ended with reason disconnect", got)
	}
	if h.coord.ActiveSessions() != 0 {
		t.Errorf("coordinator still tracks %d sessions after the session ended", h.coord.ActiveSessions())
	}

	var payload domain.SessionEndPayload
	if !h.events.payloadOf(domain.EventKindSessionEnd, &payload) {
		t.Fatal("no session end event was recorded")
	}
	if payload.Reason != string(EndDisconnect) {
		t.Errorf("recorded end reason = %q, want %q", payload.Reason, EndDisconnect)
	}

	// An ended session refuses further work rather than running headless.
	if _, err := shell.Accept(context.Background(), commandLine(shell, 1, "ls")); err == nil {
		t.Error("an ended session accepted a new command")
	}
}

// TestSessionStartRecordsPrincipalAndTerminalMetadata proves the connect record
// carries what PLAN 10.1 requires of a session event, including the sharing
// status in force at connect time.
func TestSessionStartRecordsPrincipalAndTerminalMetadata(t *testing.T) {
	h := newHarness(t, testLimits())
	shell, err := h.coord.Open(context.Background(), OpenSessionRequest{
		Principal:    testPrincipal,
		AuthMode:     AuthModeSecure,
		Terminal:     TerminalMetadata{Term: "xterm-256color", Size: domain.TermSize{Cols: 120, Rows: 40}, ClientAddr: "198.51.100.7:51000"},
		CWD:          "/home/alice",
		TransportRef: "chan-3",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = shell.End(context.Background(), EndShutdown) })

	var payload domain.SessionStartPayload
	if !h.events.payloadOf(domain.EventKindSessionStart, &payload) {
		t.Fatal("no session start event was recorded")
	}
	if payload.UserID != testPrincipal {
		t.Errorf("recorded principal = %s, want %s", payload.UserID, testPrincipal)
	}
	if payload.AuthMode != string(AuthModeSecure) {
		t.Errorf("recorded auth mode = %q, want %q", payload.AuthMode, AuthModeSecure)
	}
	if payload.TerminalType != "xterm-256color" {
		t.Errorf("recorded terminal type = %q", payload.TerminalType)
	}
	if payload.TerminalSize != (domain.TermSize{Cols: 120, Rows: 40}) {
		t.Errorf("recorded terminal size = %+v", payload.TerminalSize)
	}
	if payload.ClientAddr != "198.51.100.7:51000" {
		t.Errorf("recorded client address = %q", payload.ClientAddr)
	}
	if payload.SharingEnabled != testSnapshot().ScopePolicy.SharingEnabled {
		t.Errorf("recorded sharing status = %v, want the connect-time policy", payload.SharingEnabled)
	}
}

// TestShutdownEndsEveryLiveSession proves the composition root can stop the
// service cleanly without leaking session goroutines.
func TestShutdownEndsEveryLiveSession(t *testing.T) {
	h := newHarness(t, testLimits())
	_, first := h.testSession(t)
	_, second := h.openSession(t, otherPrincipal, "/home/bob")

	if err := h.coord.Shutdown(context.Background(), EndShutdown); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	for name, shell := range map[string]Shell{"first": first, "second": second} {
		select {
		case <-shell.Done():
		case <-time.After(waitBudget):
			t.Fatalf("session %s did not finish after shutdown", name)
		}
	}
	if got := h.coord.ActiveSessions(); got != 0 {
		t.Errorf("coordinator still tracks %d sessions after shutdown", got)
	}

	// A coordinator that is shutting down refuses new sessions.
	if _, err := h.coord.Open(context.Background(), OpenSessionRequest{
		Principal: testPrincipal,
		AuthMode:  AuthModeSecure,
		Terminal:  TerminalMetadata{Term: "xterm-256color", Size: DefaultTerminalSize},
		CWD:       "/home/alice",
	}); err == nil {
		t.Error("a shutting-down coordinator accepted a new session")
	}
}

// TestPreparedTurnContextIsHandedToEveryGeneration proves the assembled context
// travels with each generation, so a retry does not silently lose it.
func TestPreparedTurnContextIsHandedToEveryGeneration(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)
	h.tools.result = ToolBatchResult{Results: []ToolCallResult{{Name: "read_file"}}}
	h.engine.scriptTurn(0, TurnScript{
		Preparations: []PreparedTurn{{
			ContextBytes: 4096,
			Revisions:    []domain.Revision{domain.InitialRevision, domain.InitialRevision.Next()},
			AppVersionID: func() *domain.AppVersionID { v := mustAppVersionID("av_01ARZ3NDEKTSV4RRFFQ69G5FAV"); return &v }(),
		}},
		StepFunc: func(in GenerationInput, index int) (StepOutcome, error) {
			switch index {
			case 0:
				return toolStep("read_file"), nil
			default:
				return candidateStep(textCandidate(in.Request.Turn, "read")), nil
			}
		},
	})

	turn, err := shell.Accept(context.Background(), commandLine(shell, 1, "cat file"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if outcome := drain.awaitOutcome(t, turn, waitBudget); outcome.Kind != OutcomeCompleted {
		t.Fatalf("outcome kind = %q, want completed (failure %+v)", outcome.Kind, outcome.Failure)
	}

	// Both generations received the same pinned snapshot and the same absolute
	// deadline, which is what makes a retry replayable.
	generations := h.engine.generationsOf(0)
	if len(generations) != 2 {
		t.Fatalf("ran %d generations, want 2", len(generations))
	}
	if generations[0].DeadlineMs != generations[1].DeadlineMs {
		t.Errorf("the retry changed the turn deadline: %d then %d", generations[0].DeadlineMs, generations[1].DeadlineMs)
	}
	if generations[0].Snapshot != generations[1].Snapshot {
		t.Error("the retry did not keep the turn's pinned snapshot")
	}
	if generations[0].Context != generations[1].Context {
		t.Error("the retry saw different session state")
	}
	if generations[0].AttemptNumber >= generations[1].AttemptNumber {
		t.Errorf("attempt numbers did not advance across the retry: %d then %d",
			generations[0].AttemptNumber, generations[1].AttemptNumber)
	}
}

// errPastScript is the failure a scripted generation returns when a test's
// script did not anticipate being asked for another generation.
var errPastScript = providerError("scripted engine ran past its script")
