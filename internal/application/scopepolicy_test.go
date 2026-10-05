package application

import (
	"context"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// The tests in this file cover the live scope-policy switch (PLAN 5.2, 10.4):
// the policy revision is injected from trusted context and pinned per turn, a
// live sharing change is detected and recorded, and the next turn runs under
// the new policy while a turn already in flight keeps the one it pinned.

// sharingOffRevision is a restricted policy carrying a revision distinct from
// the default, modelling an administrator turning sharing off.
func sharingOffRevision() domain.ScopePolicy {
	policy := domain.RestrictedScopePolicy()
	policy.PolicyRevision = 2
	return policy
}

// TestTurnPinsInjectedScopePolicyRevision proves the scope policy a turn runs
// under comes from the injected snapshot, is pinned for every generation, and
// is observable as the session's current revision.
func TestTurnPinsInjectedScopePolicyRevision(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)
	h.engine.scriptTurn(0, TurnScript{
		StepFunc: func(in GenerationInput, index int) (StepOutcome, error) {
			if index == 0 {
				return toolStep("world.lookup"), nil
			}
			return candidateStep(textCandidate(in.Request.Turn, "done")), nil
		},
	})
	h.tools.result = ToolBatchResult{Results: []ToolCallResult{{Name: "world.lookup"}}}

	turn, err := shell.Accept(context.Background(), commandLine(shell, 1, "cat /etc/motd"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if outcome := drain.awaitOutcome(t, turn, waitBudget); outcome.Kind != OutcomeCompleted {
		t.Fatalf("outcome kind = %q, want completed (failure %+v)", outcome.Kind, outcome.Failure)
	}

	// The injected revision (1) is what every generation and every tool batch
	// received, and it is the session's reported revision.
	if got := shell.Snapshot().PolicyRevision; got != 1 {
		t.Errorf("session policy revision = %d, want the injected 1", got)
	}
	for _, request := range h.engine.generationsOf(0) {
		if request.Snapshot.ScopePolicy.PolicyRevision != 1 {
			t.Errorf("generation ran under policy revision %d, want the injected 1", request.Snapshot.ScopePolicy.PolicyRevision)
		}
		if !request.Snapshot.ScopePolicy.SharingEnabled {
			t.Error("the first turn should run under the sharing-enabled injected policy")
		}
	}
	batch, ok := h.tools.batchAt(0)
	if !ok {
		t.Fatal("no tool batch executed")
	}
	if batch.Request.Snapshot.ScopePolicy.PolicyRevision != 1 {
		t.Errorf("tool batch ran under policy revision %d, want 1", batch.Request.Snapshot.ScopePolicy.PolicyRevision)
	}
}

// TestLiveSharingChangePinsNewRevisionOnTheNextTurn proves a live sharing
// change is detected, recorded, and pinned by the next turn, and that the
// session reports the new revision.
func TestLiveSharingChangePinsNewRevisionOnTheNextTurn(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)

	h.engine.scriptTurn(0, textTurn("sharing on"))
	first, err := shell.Accept(context.Background(), commandLine(shell, 1, "echo one"))
	if err != nil {
		t.Fatalf("first Accept: %v", err)
	}
	drain.awaitOutcome(t, first, waitBudget)

	// An administrator turns sharing off while the session stays open.
	reloaded := h.snapshots.reloadWithPolicy(sharingOffRevision())
	if reloaded.ScopePolicy.SharingEnabled {
		t.Fatal("the test reload did not disable sharing")
	}

	h.engine.scriptTurn(1, textTurn("sharing off"))
	second, err := shell.Accept(context.Background(), commandLine(shell, 2, "echo two"))
	if err != nil {
		t.Fatalf("second Accept: %v", err)
	}
	if outcome := drain.awaitOutcome(t, second, waitBudget); outcome.Kind != OutcomeCompleted {
		t.Fatalf("second outcome kind = %q, want completed (failure %+v)", outcome.Kind, outcome.Failure)
	}

	// The next turn pinned the new restricted revision.
	requests := h.engine.generationsOf(1)
	if len(requests) == 0 {
		t.Fatal("the second turn ran no generation")
	}
	if requests[0].Snapshot.ScopePolicy.SharingEnabled {
		t.Error("the turn after the live change still ran with sharing enabled")
	}
	if got := requests[0].Snapshot.ScopePolicy.PolicyRevision; got != 2 {
		t.Errorf("the turn after the live change pinned revision %d, want 2", got)
	}
	if got := shell.Snapshot().PolicyRevision; got != 2 {
		t.Errorf("session policy revision = %d, want 2 after the live change", got)
	}

	// The change is durably recorded and tied to the turn that rebuilt context
	// under the new policy.
	if got := h.events.countOfKind(domain.EventKindConfigChange); got != 1 {
		t.Errorf("recorded %d config-change events, want 1", got)
	}
	if reasons := h.events.notedContaining(second, "scope policy revision changed"); len(reasons) == 0 {
		t.Error("the turn that rebuilt context after the live change was not noted")
	}
	// The turn before the change is not retroactively noted.
	if reasons := h.events.notedContaining(first, "scope policy revision changed"); len(reasons) != 0 {
		t.Errorf("the pre-change turn carries the change note: %v", reasons)
	}
}

// TestInFlightTurnKeepsPinnedPolicyAcrossLiveChange proves the central ordering
// rule of PLAN 5.2: a turn already running keeps the policy revision it pinned,
// so a reload mid-turn cannot mix old and new sharing policy inside one turn.
func TestInFlightTurnKeepsPinnedPolicyAcrossLiveChange(t *testing.T) {
	h := newHarness(t, testLimits())
	drain, shell := h.testSession(t)

	gate := h.engine.blockGeneration(0, 0)
	h.engine.scriptTurn(0, TurnScript{
		Blocks: map[int]chan struct{}{0: gate},
		StepFunc: func(in GenerationInput, _ int) (StepOutcome, error) {
			return candidateStep(textCandidate(in.Request.Turn, "pinned")), nil
		},
	})

	turn, err := shell.Accept(context.Background(), commandLine(shell, 1, "echo pinned"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	waitForState(t, shell, waitBudget, StateGenerating)

	// Sharing is turned off while the turn is generating.
	h.snapshots.reloadWithPolicy(sharingOffRevision())
	close(gate)

	if outcome := drain.awaitOutcome(t, turn, waitBudget); outcome.Kind != OutcomeCompleted {
		t.Fatalf("outcome kind = %q, want completed (failure %+v)", outcome.Kind, outcome.Failure)
	}

	// The in-flight generation still ran under the pinned sharing-enabled
	// revision 1; the change only applies to the next turn.
	requests := h.engine.generationsOf(0)
	if len(requests) == 0 {
		t.Fatal("the turn ran no generation")
	}
	if !requests[0].Snapshot.ScopePolicy.SharingEnabled {
		t.Error("the in-flight turn picked up the mid-turn sharing change")
	}
	if got := requests[0].Snapshot.ScopePolicy.PolicyRevision; got != 1 {
		t.Errorf("the in-flight turn ran under revision %d, want the pinned 1", got)
	}
}

// TestConfigSnapshotRequiresPolicyRevision proves a pinned policy must carry a
// revision, so a live change is always detectable rather than silently ignored.
func TestConfigSnapshotRequiresPolicyRevision(t *testing.T) {
	snapshot := testSnapshot()
	snapshot.ScopePolicy.PolicyRevision = 0
	if err := snapshot.Validate(); err == nil {
		t.Fatal("a snapshot whose policy carries no revision must be rejected")
	}
	snapshot.ScopePolicy.PolicyRevision = -1
	if err := snapshot.Validate(); err == nil {
		t.Fatal("a negative policy revision must be rejected")
	}
}
