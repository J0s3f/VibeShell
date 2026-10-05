package application

import (
	"context"
	"errors"
	"strconv"

	"j0s.at/vibeshell/internal/domain"
)

// errDiscarded marks a response that arrived too late to be applied. It never
// becomes a turn failure: the turn already ended or moved on, and the late
// response must not reach the world or the terminal.
var errDiscarded = errors.New("late response discarded")

// This file is the turn's business flow. It applies the pure policies from
// turnstate.go to the injected engine, tool executor, world store, and
// renderer, in the order PLAN 7.1 requires:
//
//	pin -> queue -> prepare -> generate/tool -> validate -> commit -> emit
//
// Nothing reaches the world or the terminal before the commit point, and no
// cancellation can undo an accepted commit.

// executeTurn runs one turn to a terminal outcome.
func (s *session) executeTurn(t *turn) {
	if err := t.pinSnapshot(s.scope); err != nil {
		t.fail(classifyError(err, failureFatal), "configuration snapshot unavailable")
		return
	}

	turnCtx, cancel := context.WithTimeout(s.scope, t.effectiveDeadline(s.coord.limits))
	t.setCancel(cancel)
	defer cancel()

	s.runTurnLoop(turnCtx, t)
}

// runTurnLoop prepares the turn, then drives generations until the turn reaches
// a terminal state.
func (s *session) runTurnLoop(ctx context.Context, t *turn) {
	prepare := true
	for {
		if t.isTerminal() {
			return
		}
		if prepare {
			if err := s.prepareGeneration(ctx, t); err != nil {
				if t.interruptIfCancelled(ctx, cancellationReason(ctx)) {
					return
				}
				t.fail(classifyError(err, failureFatal), "turn context could not be assembled")
				return
			}
			prepare = false
		}
		if t.interruptIfCancelled(ctx, cancellationReason(ctx)) {
			return
		}

		step, kind, err := s.runGeneration(ctx, t)
		if err != nil {
			if t.isTerminal() {
				return
			}
			if errors.Is(err, errDiscarded) || t.interruptIfCancelled(ctx, cancellationReason(ctx)) {
				return
			}
			retry, _, reason := s.retryOrFail(t, classifyError(err, kind), kind)
			if !retry {
				t.fail(classifyError(err, kind), reason)
				return
			}
			continue
		}
		if step.Phase != StepCandidate || step.Candidate == nil {
			t.fail(TurnFailure{
				Class:   domain.FailureInvalidResponse,
				Code:    domain.CodeInvariantViolation,
				Message: "engine returned neither tool work nor a candidate",
			}, "unknown step phase")
			return
		}

		finished, err := s.acceptCandidate(ctx, t, step.Candidate)
		if err != nil {
			if t.isTerminal() {
				return
			}
			if t.interruptIfCancelled(ctx, cancellationReason(ctx)) {
				return
			}
			kind := failureRebase
			if domain.IsValidationError(err) {
				kind = failureRepair
			}
			retry, reprepare, reason := s.retryOrFail(t, classifyError(err, kind), kind)
			if !retry {
				t.fail(classifyError(err, kind), reason)
				return
			}
			prepare = reprepare
			continue
		}
		if finished {
			return
		}
	}
}

// prepareGeneration assembles the bounded turn context. It runs once per round:
// the first round, and after every conflict rebase, because a rebase must
// re-read the state that changed.
func (s *session) prepareGeneration(ctx context.Context, t *turn) error {
	if err := t.advance(StateQueued, "assembling turn context"); err != nil {
		return err
	}
	prepared, err := s.coord.engine.Prepare(ctx, t.request(s.sessionContext()))
	if err != nil {
		return err
	}
	t.setPrepared(prepared)
	return t.advance(StateContextReady, "turn context assembled")
}

// runGeneration performs one generation attempt. Tool work is executed inside
// it so awaiting-tool is a real recorded state, and every response is checked
// against the generation that requested it before it can be used.
func (s *session) runGeneration(ctx context.Context, t *turn) (StepOutcome, failureKind, error) {
	generation, request, err := s.beginGenerating(t)
	if err != nil {
		return StepOutcome{}, failureFatal, err
	}

	step, err := s.coord.engine.Generate(ctx, GenerationInput{
		Request:  request,
		Prepared: t.preparedOf(),
	})
	if err != nil {
		return StepOutcome{}, failureProvider, err
	}
	if !t.isCurrentGeneration(generation) {
		t.note("response from a replaced generation was discarded")
		return StepOutcome{}, failureFatal, errDiscarded
	}
	if step.Phase != StepAwaitingTool {
		return step, failureProvider, nil
	}
	return s.runToolBatch(ctx, t, request, step)
}

// runToolBatch executes the engine's bounded tool batch and asks for the next
// generation with its results.
func (s *session) runToolBatch(ctx context.Context, t *turn, request TurnRequest, step StepOutcome) (StepOutcome, failureKind, error) {
	if err := t.advance(StateAwaitingTool, "executing tool batch"); err != nil {
		return StepOutcome{}, failureFatal, err
	}
	result, err := s.coord.tools.Execute(ctx, ToolBatch{Request: request, Calls: step.ToolCalls})
	if err != nil {
		return StepOutcome{}, failureRebase, err
	}
	if t.isTerminal() {
		t.note("tool result from a cancelled turn was discarded")
		return StepOutcome{}, failureFatal, errDiscarded
	}

	nextGeneration, nextRequest, err := s.beginGenerating(t)
	if err != nil {
		return StepOutcome{}, failureFatal, err
	}
	stepOutcome, err := s.coord.engine.Generate(ctx, GenerationInput{
		Request:     nextRequest,
		Prepared:    t.preparedOf(),
		ToolResults: result.Results,
	})
	if err != nil {
		return StepOutcome{}, failureProvider, err
	}
	if !t.isCurrentGeneration(nextGeneration) {
		t.note("response from a replaced generation was discarded")
		return StepOutcome{}, failureFatal, errDiscarded
	}
	return stepOutcome, failureProvider, nil
}

// beginGenerating opens a generation and records the transition into it.
func (s *session) beginGenerating(t *turn) (uint64, TurnRequest, error) {
	generation, _, err := t.beginGeneration()
	if err != nil {
		return 0, TurnRequest{}, err
	}
	if err := t.advance(StateGenerating, "generating"); err != nil {
		return 0, TurnRequest{}, err
	}
	return generation, t.request(s.sessionContext()), nil
}

// acceptCandidate validates, commits, emits, and completes one candidate. It
// reports whether the turn reached a terminal state.
func (s *session) acceptCandidate(ctx context.Context, t *turn, candidate *TurnCandidate) (bool, error) {
	if err := t.advance(StateValidating, "validating accepted outcome"); err != nil {
		return true, err
	}
	if err := validateCandidate(candidate, t.snapshotOf(), s.coord.limits); err != nil {
		// A rejected candidate is a repairable response: the next generation
		// re-asks the model with the validation reason.
		return false, err
	}
	if err := t.advance(StateCommitting, "committing accepted changes"); err != nil {
		return true, err
	}
	committed, err := s.commit(ctx, t, candidate)
	if err != nil {
		if domain.IsConflictError(err) {
			s.recordConflict(ctx, t, err)
			if advanceErr := t.advance(StateConflicted, "concurrent world change"); advanceErr != nil {
				return true, advanceErr
			}
			return false, err
		}
		// The commit outcome is unknown. The turn never retries an unknown
		// commit: re-applying the same mutation could duplicate it, so the turn
		// fails and the ambiguity stays visible to the operator.
		t.note("commit outcome unknown: not retried to avoid a duplicate mutation")
		return true, err
	}
	if committed.Replayed {
		t.note("replayed commit skipped: its mutation was already applied")
	}

	s.applySessionPatch(candidate.SessionPatch)
	t.setExitStatus(candidate.ExitStatus)

	if err := t.advance(StateEmitting, "emitting accepted output"); err != nil {
		return true, err
	}
	s.emitAccepted(ctx, t, candidate)

	if err := t.advance(StateCompleted, ""); err != nil {
		return true, err
	}
	t.finish(OutcomeCompleted)
	return true, nil
}

// emitAccepted emits the accepted output and the resulting prompt. Every item
// is durably recorded before it is handed to the transport; an item that cannot
// be handed over inside the turn deadline is counted as undelivered instead of
// being silently dropped.
func (s *session) emitAccepted(ctx context.Context, t *turn, candidate *TurnCandidate) {
	if text := candidate.Output.Text; text != "" {
		ref, err := s.coord.content.Put(ctx, []byte(text), "text/plain; charset=utf-8")
		if err != nil || !s.recordFrame(ctx, t, ref, false, "") {
			t.addUndelivered()
			return
		}
		written, err := s.coord.renderer.WriteText(ctx, s.id, text)
		if err != nil {
			t.addUndelivered()
			return
		}
		if !s.deliverAccepted(ctx, t, SessionOutput{Kind: OutputText, Text: text, ByteCount: written}) {
			return
		}
	}

	for _, content := range candidate.Output.Content {
		if !s.recordFrame(ctx, t, content, false, "") || !s.deliverAccepted(ctx, t, SessionOutput{
			Kind:      OutputContent,
			Content:   content,
			ByteCount: content.Size,
		}) {
			return
		}
	}

	if view := candidate.Output.View; view != nil {
		frame, size, err := s.coord.renderer.RenderView(ctx, s.id, *view)
		if err != nil {
			t.addUndelivered()
			return
		}
		if !s.recordFrame(ctx, t, frame, false, view.Mode) || !s.deliverAccepted(ctx, t, SessionOutput{
			Kind:      OutputFrame,
			Content:   frame,
			View:      view,
			ByteCount: size,
		}) {
			return
		}
	}

	if !s.recordPrompt(ctx, t) {
		t.addUndelivered()
		return
	}
	prompt := s.prompt()
	s.deliverAccepted(ctx, t, SessionOutput{Kind: OutputPrompt, Prompt: &prompt})
}

// deliverAccepted assigns the session-local output sequence and hands one item
// to the adapter.
func (s *session) deliverAccepted(ctx context.Context, t *turn, out SessionOutput) bool {
	out.Session = s.id
	out.Turn = t.id
	out.Sequence = s.nextOutputSequence()
	if !s.deliverOutput(ctx, out) {
		t.addUndelivered()
		return false
	}
	return true
}

// recordFrame records accepted terminal output by content reference.
func (s *session) recordFrame(ctx context.Context, t *turn, ref domain.ContentRef, isPrompt bool, mode string) bool {
	attempt := t.attemptID()
	_, err := s.journal.append(ctx, domain.EventKindTerminalFrame, domain.TerminalFramePayload{
		FrameID:    strconv.FormatUint(s.peekOutputSequence(), 10),
		ContentRef: ref,
		IsPrompt:   isPrompt,
		Mode:       mode,
	}, journalRefs{Turn: &t.id, Attempt: &attempt})
	if err != nil {
		s.markRecordingUnhealthy()
		return false
	}
	return true
}

// recordPrompt records the accepted prompt position of a completed turn.
func (s *session) recordPrompt(ctx context.Context, t *turn) bool {
	attempt := t.attemptID()
	return s.recordPromptAt(ctx, s.prompt(), journalRefs{Turn: &t.id, Attempt: &attempt})
}

// recordPromptAt records a prompt position under the given correlation refs, so
// a prompt emitted outside a turn (leaving a foreground application on end of
// input) is recorded exactly like a turn's prompt.
func (s *session) recordPromptAt(ctx context.Context, prompt PromptState, refs journalRefs) bool {
	_, err := s.journal.append(ctx, domain.EventKindTerminalPrompt, domain.TerminalPromptPayload{
		Prompt:   string(prompt.CWD),
		CWD:      string(prompt.CWD),
		ExitCode: prompt.ExitCode,
	}, refs)
	if err != nil {
		s.markRecordingUnhealthy()
		return false
	}
	return true
}

// retryOrFail applies the pure retry policy and spends the matching budget. It
// reports whether a new generation may start and whether that generation needs
// freshly prepared context.
func (s *session) retryOrFail(t *turn, failure TurnFailure, kind failureKind) (retry, reprepare bool, reason string) {
	attempts, rebases, repairs := t.budget()
	decision := decideRetry(failure, kind, attempts, rebases, repairs, s.coord.limits, t.snapshotOf())
	if !decision.Retry {
		return false, false, decision.Reason
	}
	switch decision.Kind {
	case failureRebase:
		t.spendRebase()
		t.note(decision.Reason)
		return true, true, decision.Reason
	case failureRepair:
		t.spendRepair()
		t.note(decision.Reason)
	}
	return true, false, decision.Reason
}

// cancellationReason explains why a turn scope ended.
func cancellationReason(ctx context.Context) string {
	if err := ctx.Err(); errors.Is(err, context.DeadlineExceeded) {
		return "turn deadline reached"
	}
	return "cancelled"
}
