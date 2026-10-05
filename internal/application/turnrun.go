package application

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"j0s.at/vibeshell/internal/domain"
)

// This file runs one turn: it pins the configuration snapshot, drives the
// state machine, applies the retry policy, discards late responses, commits
// accepted changes, and only then emits. Ordering here is deliberate:
//
//	pin -> prepare -> generate/tool loop -> validate -> commit -> emit -> done
//
// Nothing reaches the world or the terminal before the commit point, and no
// cancellation can undo an accepted commit.

// CommitLedger records the revisions this turn already committed, keyed by the
// engine's logical commit key. It is what makes a replayed commit a no-op
// instead of a second application of the same mutation.
type CommitLedger map[CommitKey]domain.Revision

// turn is the runtime of one turn. The worker goroutine is its only mutator;
// cancellation and status reads take the lock. Transition journaling happens
// outside the lock so a slow store cannot delay a Ctrl-C.
type turn struct {
	session *session
	id      domain.TurnID
	input   SessionInput
	// principal is copied from the accepted session so building a request never
	// needs the session lock while the turn lock is held.
	principal domain.UserID

	mu          sync.Mutex
	state       TurnState
	generation  uint64
	attempt     domain.AttemptID
	attempts    int
	repairs     int
	rebases     int
	snapshot    ConfigSnapshot
	prepared    PreparedTurn
	commits     CommitLedger
	failure     *TurnFailure
	exitStatus  int
	revision    domain.Revision
	undelivered int
	ended       bool
	cancel      context.CancelFunc
	deadlineMs  int64
}

// status returns the read-only view of the running turn.
func (t *turn) status() ActiveTurn {
	t.mu.Lock()
	defer t.mu.Unlock()
	return ActiveTurn{
		ID:         t.id,
		State:      t.state,
		Generation: t.generation,
		Attempts:   t.attempts,
		Rebases:    t.rebases,
	}
}

// setCancel records the turn's cancellation function.
func (t *turn) setCancel(cancel context.CancelFunc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cancel = cancel
}

// interrupt marks the turn cancelled, cancels its scope, and publishes the
// interrupted outcome once. It is called from the session goroutine so a Ctrl-C
// is recorded immediately, even if the provider call ignores cancellation.
func (t *turn) interrupt(reason string) bool {
	t.mu.Lock()
	if t.ended {
		t.mu.Unlock()
		return false
	}
	cancel := t.cancel
	t.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if err := t.advance(StateInterrupted, reason); err != nil {
		// Already terminal, or already past the commit point: the mutation is
		// durable, so the turn must finish as committed rather than report
		// discarded work.
		return false
	}
	t.finish(OutcomeInterrupted)
	return true
}

// finish publishes the turn's terminal outcome exactly once.
func (t *turn) finish(kind OutcomeKind) {
	t.mu.Lock()
	if t.ended {
		t.mu.Unlock()
		return
	}
	t.ended = true
	outcome := SessionOutcome{
		Session:     t.session.id,
		Turn:        t.id,
		Kind:        kind,
		State:       t.state,
		ExitStatus:  t.exitStatus,
		Revision:    t.revision,
		Undelivered: t.undelivered,
		Failure:     t.failure,
	}
	t.mu.Unlock()

	t.session.deliverOutcome(t.session.scope, outcome)
}

// fail records the terminal failure and publishes it. A turn that already
// reached a terminal state keeps that state: a commit already accepted is not
// undone by a later failure.
func (t *turn) fail(failure TurnFailure, reason string) {
	if err := t.advance(StateFailed, reason); err != nil {
		t.mu.Lock()
		t.failure = &failure
		t.mu.Unlock()
		return
	}
	t.mu.Lock()
	t.failure = &failure
	t.mu.Unlock()
	t.finish(OutcomeFailed)
}

// interruptIfCancelled records the interruption when the turn scope ended. It
// reports whether the turn is finished and the caller must stop.
func (t *turn) interruptIfCancelled(scope context.Context, reason string) bool {
	if t.isTerminal() {
		return true
	}
	if scope.Err() == nil {
		return false
	}
	return t.interrupt(reason)
}

// isTerminal reports whether the turn already reached a terminal state.
func (t *turn) isTerminal() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state.IsTerminal()
}

// isCurrentGeneration reports whether a response belongs to the running
// generation. A response from an older generation is late: the turn was
// cancelled, replaced, or already finished, and the response must not reach the
// world or the terminal.
func (t *turn) isCurrentGeneration(generation uint64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.generation == generation && !t.state.IsTerminal()
}

// setExitStatus records the accepted exit status for the turn outcome.
func (t *turn) setExitStatus(status int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.exitStatus = status
}

// note records a lifecycle note without a state change, for example a discarded
// late response, a skipped replayed commit, or a spent retry budget.
func (t *turn) note(reason string) {
	t.mu.Lock()
	snapshot := t.snapshot
	state := t.state
	generation := t.generation
	attempt := t.attempt
	t.mu.Unlock()

	_, err := t.session.journal.append(t.session.scope, domain.EventKindTurnTransition, domain.TurnTransitionPayload{
		From:             state,
		To:               state,
		Generation:       generation,
		Attempt:          attemptRef(attempt),
		ConfigVersion:    snapshot.ConfigVersion,
		PromptVersion:    snapshot.PromptVersion,
		CatalogueVersion: snapshot.CatalogueVersion,
		Reason:           reason,
	}, journalRefs{Turn: &t.id, Attempt: attemptRef(attempt)})
	if err != nil {
		t.session.markRecordingUnhealthy()
	}
}

// attemptRef returns a pointer to an attempt identity, or nil when the attempt
// has not been minted yet. Transitions before the first generation carry no
// attempt, and the domain identity format rejects a zero value on decode.
func attemptRef(attempt domain.AttemptID) *domain.AttemptID {
	if attempt.IsZero() {
		return nil
	}
	return &attempt
}

// advance applies a state transition and records it durably.
//
// An illegal transition is an invariant violation and is returned as an error.
// A failed journal append is not: the state change is real, so it is applied and
// the session is marked unrecorded. The session then refuses new semantic work
// rather than continuing without a research record (PLAN 10.3).
func (t *turn) advance(to TurnState, reason string) error {
	t.mu.Lock()
	from := t.state
	if !CanTransitionTurn(from, to) {
		t.mu.Unlock()
		return domain.NewInternalError(domain.CodeInvariantViolation, "illegal turn transition",
			fmt.Errorf("cannot move turn %s from %s to %s", t.id, from, to))
	}
	t.state = to
	snapshot := t.snapshot
	generation := t.generation
	attempt := t.attempt
	t.mu.Unlock()

	if _, err := t.session.journal.append(t.session.scope, domain.EventKindTurnTransition, domain.TurnTransitionPayload{
		From:             from,
		To:               to,
		Generation:       generation,
		Attempt:          attemptRef(attempt),
		ConfigVersion:    snapshot.ConfigVersion,
		PromptVersion:    snapshot.PromptVersion,
		CatalogueVersion: snapshot.CatalogueVersion,
		Reason:           reason,
	}, journalRefs{Turn: &t.id, Attempt: attemptRef(attempt)}); err != nil {
		t.session.markRecordingUnhealthy()
	}
	return nil
}

// pinSnapshot captures the configuration snapshot for the whole turn and
// records it, so a later configuration or prompt reload cannot change a turn
// that is already running.
func (t *turn) pinSnapshot(ctx context.Context) error {
	snapshot, err := t.session.coord.snapshots.CurrentSnapshot(ctx)
	if err != nil {
		return t.session.coord.wrapSnapshotError(err)
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	// The policy revision is injected here, from trusted context, and pinned
	// for the whole turn. A change from the session's previous revision is a
	// live sharing change: the session records it and rebuilds cross-scope
	// context for this turn instead of reusing context assembled under the
	// old policy (PLAN 5.2, 7.3).
	revision := snapshot.ScopePolicy.PolicyRevision
	t.mu.Lock()
	t.snapshot = snapshot
	t.deadlineMs = t.session.coord.clock.NowUnixMilli() + snapshot.TurnDeadlineMs
	t.mu.Unlock()

	if t.session.observePolicyRevision(revision) {
		t.note("scope policy revision changed; cross-scope context invalidated")
		_, err := t.session.journal.append(ctx, domain.EventKindConfigChange, domain.ConfigChangePayload{
			ConfigVersion: snapshot.ConfigVersion,
			ChangedKeys:   []string{"scope_policy.policy_revision"},
			Actor:         "session",
		}, journalRefs{Turn: &t.id})
		if err != nil {
			t.session.markRecordingUnhealthy()
		}
	}
	return nil
}

// snapshotOf returns the pinned snapshot.
func (t *turn) snapshotOf() ConfigSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshot
}

// request builds the per-generation request for the current state. The session
// context is captured by the caller, so the turn lock is never held while the
// session lock is taken.
func (t *turn) request(sessionContext SessionContext) TurnRequest {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TurnRequest{
		Session:       t.session.id,
		Principal:     t.principal,
		Turn:          t.id,
		Generation:    t.generation,
		Attempt:       t.attempt,
		Input:         t.input,
		Context:       sessionContext,
		Snapshot:      t.snapshot,
		DeadlineMs:    t.deadlineMs,
		AttemptNumber: t.attempts,
		RebaseNumber:  t.rebases,
	}
}

// preparedOf returns the assembled turn context.
func (t *turn) preparedOf() PreparedTurn {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.prepared
}

// setPrepared stores the assembled turn context.
func (t *turn) setPrepared(prepared PreparedTurn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prepared = prepared
}

// beginGeneration opens a new generation: a new generation ID and attempt ID
// for the same turn, keeping the pinned snapshot and session context. Responses
// from the previous generation become late the moment this returns.
func (t *turn) beginGeneration() (uint64, domain.AttemptID, error) {
	attempt, err := t.session.coord.mintAttemptID()
	if err != nil {
		return 0, domain.AttemptID{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.generation++
	t.attempts++
	t.attempt = attempt
	return t.generation, attempt, nil
}

// recordCommitted remembers an applied commit so a replay of the same logical
// commit is skipped instead of applied twice.
func (t *turn) recordCommitted(key CommitKey, revision domain.Revision) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.commits[key] = revision
	t.revision = revision
}

// committedRevision reports an already applied commit.
func (t *turn) committedRevision(key CommitKey) (domain.Revision, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	revision, ok := t.commits[key]
	return revision, ok
}

// commitContext is the outcome of the commit step.
type commitContext struct {
	Revision domain.Revision
	// Replayed reports that the commit key was already applied by this turn,
	// so the stored mutations were deliberately not applied again.
	Replayed bool
}

// commit applies accepted world changes. It never applies the same logical
// commit twice, and it never silently retries an unknown commit outcome.
func (s *session) commit(ctx context.Context, t *turn, candidate *TurnCandidate) (commitContext, error) {
	if revision, ok := t.committedRevision(candidate.CommitKey); ok {
		return commitContext{Revision: revision, Replayed: true}, nil
	}
	if candidate.Changes.IsEmpty() {
		return commitContext{}, nil
	}

	snapshot := t.snapshotOf()
	changes := candidate.Changes
	changes.TurnID = t.id
	if changes.AttemptID.IsZero() {
		changes.AttemptID = t.attemptID()
	}

	if _, err := s.journal.append(ctx, domain.EventKindWorldStage, domain.WorldStagePayload{ChangeSet: changes}, journalRefs{
		Turn:    &t.id,
		Attempt: &changes.AttemptID,
	}); err != nil {
		return commitContext{}, err
	}

	revision, err := s.coord.world.Commit(ctx, changes, snapshot.ScopePolicy)
	if err != nil {
		return commitContext{}, err
	}
	t.recordCommitted(candidate.CommitKey, revision)

	hashes := make([]domain.ContentID, 0, len(changes.Mutations))
	for _, mutation := range changes.Mutations {
		if !mutation.Content.IsEmpty() {
			hashes = append(hashes, mutation.Content.Hash)
		}
	}
	if _, err := s.journal.append(ctx, domain.EventKindWorldCommit, domain.WorldCommitPayload{
		ChangeSet:     changes,
		CommittedRev:  revision,
		ContentHashes: hashes,
	}, journalRefs{Turn: &t.id, Attempt: &changes.AttemptID, Revision: revision}); err != nil {
		return commitContext{}, err
	}
	return commitContext{Revision: revision}, nil
}

// attemptID returns the current attempt identity.
func (t *turn) attemptID() domain.AttemptID {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.attempt
}

// addUndelivered counts output that could not be handed to the transport.
func (t *turn) addUndelivered() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.undelivered++
}

// recordConflict journals a concurrent-change conflict for the turn.
func (s *session) recordConflict(ctx context.Context, t *turn, err error) {
	payload := domain.WorldConflictPayload{
		RebaseAttempt: t.rebaseCount(),
	}
	var conflict *domain.DomainError
	if errors.As(err, &conflict) {
		payload.ExpectedRev = revisionFromDetails(conflict, "expected_rev")
		payload.ActualRev = revisionFromDetails(conflict, "actual_rev")
		if node, parseErr := domain.ParseNodeID(conflict.Details["node_id"]); parseErr == nil {
			payload.ConflictingNode = node
		}
	}
	attempt := t.attemptID()
	_, _ = s.journal.append(ctx, domain.EventKindWorldConflict, payload, journalRefs{
		Turn:    &t.id,
		Attempt: &attempt,
	})
}

// revisionFromDetails reads a revision the store reported in its conflict
// details. Absent or malformed details simply mean an unknown revision.
func revisionFromDetails(err *domain.DomainError, key string) domain.Revision {
	var revision domain.Revision
	if _, scanErr := fmt.Sscanf(err.Details[key], "%d", &revision); scanErr != nil {
		return 0
	}
	return revision
}

// rebaseCount returns how many conflict rebases this turn already spent.
func (t *turn) rebaseCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rebases
}

// spendRebase spends one unit of the turn's rebase budget.
func (t *turn) spendRebase() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rebases++
}

// spendRepair spends one unit of the turn's repair budget.
func (t *turn) spendRepair() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.repairs++
}

// budget returns the current retry counters.
func (t *turn) budget() (attempts, rebases, repairs int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.attempts, t.rebases, t.repairs
}

// effectiveDeadline is the turn's wall-clock budget. The pinned snapshot may
// only lower the coordinator's own limit, never raise it.
func (t *turn) effectiveDeadline(limits Limits) time.Duration {
	t.mu.Lock()
	snapshot := t.snapshot
	t.mu.Unlock()
	if snapshot.TurnDeadlineMs <= 0 {
		return limits.TurnDeadline
	}
	snapshotDeadline := time.Duration(snapshot.TurnDeadlineMs) * time.Millisecond
	if snapshotDeadline < limits.TurnDeadline {
		return snapshotDeadline
	}
	return limits.TurnDeadline
}

// markRecordingUnhealthy flags that durable research recording failed.
func (s *session) markRecordingUnhealthy() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordingHealthy = false
}

// isRecordingHealthy reports whether durable research recording still works.
func (s *session) isRecordingHealthy() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.recordingHealthy
}
