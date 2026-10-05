package application

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"j0s.at/vibeshell/internal/domain"
)

// This file holds the concurrency and lifecycle machinery of one session: a
// single writer goroutine, explicit cancellation scopes, bounded output queues,
// and the command channel that inbound adapters talk to. Business rules live in
// turnstate.go and sessionstate.go; the business flow lives in turnrun.go.

// commandKind selects which session operation a command performs.
type commandKind int

const (
	commandInput commandKind = iota
	commandEnd
)

// sessionCommand is one request from an adapter to the session goroutine. Every
// adapter call is answered through reply, so Accept never returns before the
// input is durably recorded.
type sessionCommand struct {
	kind   commandKind
	input  SessionInput
	reason EndReason
	reply  chan commandReply
}

type commandReply struct {
	turn        domain.TurnID
	interrupted bool
	err         error
}

// session is the runtime of one accepted shell session.
type session struct {
	coord   *Coordinator
	journal *eventJournal
	id      domain.SessionID

	// commands carries adapter requests to the single writer goroutine.
	commands chan sessionCommand
	// finished carries worker completion notices back to the writer.
	finished chan *turn
	// outputs and outcomes are the bounded delivery queues the adapter drains.
	outputs  chan SessionOutput
	outcomes chan SessionOutcome

	// scope is cancelled when the session ends; every worker and every delivery
	// observes it, so a closed session cannot keep work or bytes in flight.
	scope       context.Context
	cancelScope context.CancelFunc
	// workers tracks the running turn workers so the session can drain them
	// before it reports completion.
	workers sync.WaitGroup
	done    chan struct{}

	mu                sync.RWMutex
	state             sessionState
	active            *turn
	pending           []*turn
	recordingHealthy  bool
	ended             bool
	endReason         EndReason
	outputSequence    uint64
	lastInputSequence uint64
	lastInputTurn     domain.TurnID
	// policyRevision is the scope-policy revision of the last turn this
	// session pinned. A live sharing change bumps the revision; the session
	// then records the change and rebuilds cross-scope context for the next
	// turn rather than reusing context assembled under the old policy
	// (PLAN 5.2, 7.3).
	policyRevision int64
}

// start launches the session's single writer goroutine.
func (s *session) start() {
	s.scope, s.cancelScope = context.WithCancel(context.Background())
	go s.run()
}

// run is the session's only writer. It ends when the session scope is
// cancelled, after every turn worker has drained.
func (s *session) run() {
	defer close(s.done)
	defer close(s.outcomes)
	defer close(s.outputs)
	for {
		select {
		case <-s.scope.Done():
			s.workers.Wait()
			return
		case cmd := <-s.commands:
			switch cmd.kind {
			case commandInput:
				s.handleInput(cmd)
			case commandEnd:
				s.handleEnd(cmd)
			}
		case finished := <-s.finished:
			s.onTurnFinished(finished)
		}
	}
}

// ID implements Shell.
func (s *session) ID() domain.SessionID { return s.id }

// Principal implements Shell.
func (s *session) Principal() domain.UserID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.principal
}

// Outputs implements Shell.
func (s *session) Outputs() <-chan SessionOutput { return s.outputs }

// Outcomes implements Shell.
func (s *session) Outcomes() <-chan SessionOutcome { return s.outcomes }

// Done implements Shell.
func (s *session) Done() <-chan struct{} { return s.done }

// Snapshot implements Shell.
func (s *session) Snapshot() SessionSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot := SessionSnapshot{
		ID:               s.state.id,
		Principal:        s.state.principal,
		AuthMode:         s.state.authMode,
		Terminal:         s.state.terminal,
		CWD:              s.state.cwd,
		Foreground:       s.state.foreground,
		ExitStatus:       s.state.exitStatus,
		TurnsCompleted:   s.state.turnsCompleted,
		QueuedTurns:      len(s.pending),
		RecordingHealthy: s.recordingHealthy,
		Ended:            s.ended,
		EndReason:        s.endReason,
		PolicyRevision:   s.policyRevision,
	}
	if s.active != nil {
		active := s.active.status()
		snapshot.ActiveTurn = &active
	}
	return snapshot
}

// Accept implements Shell. It blocks until the input is durably recorded and
// classified, so the adapter must not call it on its write path.
func (s *session) Accept(ctx context.Context, in SessionInput) (domain.TurnID, error) {
	if in.Session.IsZero() {
		in.Session = s.id
	}
	if in.Principal.IsZero() {
		in.Principal = s.Principal()
	}
	result, err := s.submit(ctx, sessionCommand{kind: commandInput, input: in})
	if err != nil {
		return domain.TurnID{}, err
	}
	return result.turn, result.err
}

// CancelActive implements Shell.
func (s *session) CancelActive(ctx context.Context, reason string) (bool, error) {
	in := SessionInput{
		Session:   s.id,
		Principal: s.Principal(),
		Kind:      InputCancel,
		Key:       reason,
	}
	result, err := s.submit(ctx, sessionCommand{kind: commandInput, input: in})
	if err != nil {
		return false, err
	}
	return result.interrupted, result.err
}

// submit hands one command to the session goroutine and waits for its answer.
//
// The ended check is a separate first select rather than one more case in the
// sending select: once the goroutine has exited, the buffered command channel
// still accepts a send, so a combined select could take the send and then wait
// for a reply that will never come.
func (s *session) submit(ctx context.Context, cmd sessionCommand) (commandReply, error) {
	select {
	case <-s.done:
		return commandReply{}, s.endedError()
	default:
	}

	cmd.reply = make(chan commandReply, 1)
	select {
	case s.commands <- cmd:
	case <-s.done:
		return commandReply{}, s.endedError()
	case <-ctx.Done():
		return commandReply{}, ctx.Err()
	}

	select {
	case result := <-cmd.reply:
		return result, nil
	case <-s.done:
		return commandReply{}, s.endedError()
	case <-ctx.Done():
		return commandReply{}, ctx.Err()
	}
}

// endedError is the refusal an ended session returns to a late caller.
func (s *session) endedError() error {
	return domain.NewUnavailableError(domain.CodeSessionNotFound, "session has ended", nil, nil)
}

// End implements Shell. It is idempotent: ending an ended session succeeds.
func (s *session) End(ctx context.Context, reason EndReason) error {
	if _, err := s.submit(ctx, sessionCommand{kind: commandEnd, reason: reason}); err != nil {
		// An already ended session is the requested end state, not a failure.
		var domainErr *domain.DomainError
		if errors.As(err, &domainErr) && domainErr.Code == domain.CodeSessionNotFound {
			return nil
		}
		return err
	}
	// Wait for Done so the caller knows no further output or outcome follows.
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReportWrite implements Shell. A reported success means the bytes were handed
// to the transport, never that a person read them.
func (s *session) ReportWrite(ctx context.Context, result WriteResult) error {
	if result.Session.IsZero() {
		result.Session = s.id
	}
	if result.Status == "" {
		return domain.NewValidationError(domain.CodeInvalidInput, "write result requires a status", nil)
	}
	payload := domain.TerminalWriteOutcomePayload{
		OutputSequence: result.Sequence,
		Status:         string(result.Status),
		ByteCount:      result.ByteCount,
		Error:          truncateDetail(result.Error),
	}
	var refs journalRefs
	if !result.Turn.IsZero() {
		refs.Turn = &result.Turn
	}
	_, err := s.journal.append(ctx, domain.EventKindTerminalWrite, payload, refs)
	return err
}

// handleInput processes one accepted semantic input.
func (s *session) handleInput(cmd sessionCommand) {
	reply := func(turn domain.TurnID, interrupted bool, err error) {
		cmd.reply <- commandReply{turn: turn, interrupted: interrupted, err: err}
	}

	in, err := normalizeInput(cmd.input)
	if err != nil {
		reply(domain.TurnID{}, false, err)
		return
	}
	if in.Session != s.id || in.Principal != s.Principal() {
		reply(domain.TurnID{}, false, domain.NewValidationError(domain.CodeInvalidInput,
			"input does not belong to this session", nil))
		return
	}
	if s.ended {
		reply(domain.TurnID{}, false, domain.NewUnavailableError(domain.CodeSessionNotFound, "session has ended", nil, nil))
		return
	}
	if replay, ok := s.replayedInput(in); ok {
		reply(replay, false, nil)
		return
	}
	// Durable recording failed: refuse semantic work rather than run unrecorded.
	if in.StartsTurn() && !s.isRecordingHealthy() {
		reply(domain.TurnID{}, false, domain.NewUnavailableError(domain.CodeDatabaseUnavailable,
			"research recording is unavailable; session refuses new work", nil, nil))
		return
	}

	switch in.Kind {
	case InputResize:
		size := *in.Size
		payload := domain.InputAcceptedPayload{Action: "resize", Resize: &size}
		if _, err := s.journal.append(s.scope, domain.EventKindInputAccepted, payload, journalRefs{}); err != nil {
			reply(domain.TurnID{}, false, err)
			return
		}
		s.mu.Lock()
		s.state.resize(size)
		s.mu.Unlock()
		reply(domain.TurnID{}, false, nil)
	case InputCancel:
		payload := domain.InputAcceptedPayload{Action: InputActionCancel, Key: in.Key}
		if _, err := s.journal.append(s.scope, domain.EventKindInputAccepted, payload, journalRefs{}); err != nil {
			reply(domain.TurnID{}, false, err)
			return
		}
		active := s.currentTurn()
		interrupted := false
		if active != nil {
			interrupted = active.interrupt(in.Key)
		}
		reply(domain.TurnID{}, interrupted, nil)
	case InputEOF:
		payload := domain.InputAcceptedPayload{Action: "eof"}
		if _, err := s.journal.append(s.scope, domain.EventKindInputAccepted, payload, journalRefs{}); err != nil {
			reply(domain.TurnID{}, false, err)
			return
		}
		if s.appIsForeground() {
			// End of input leaves the foreground application and returns to the
			// shell; the session itself stays alive. A turn still in flight
			// applies its own patch when it commits, exactly as for any other
			// session patch, so the last accepted result wins.
			shell := shellForeground()
			s.applySessionPatch(SessionPatch{Foreground: &shell})
			reply(domain.TurnID{}, false, nil)
			s.emitShellPrompt()
			return
		}
		reply(domain.TurnID{}, false, nil)
		s.shutdown(EndDisconnect)
	default:
		turn, err := s.beginTurn(in)
		if err != nil {
			reply(domain.TurnID{}, false, err)
			return
		}
		reply(turn.id, false, nil)
	}
}

// handleEnd ends the session with the caller's reason.
func (s *session) handleEnd(cmd sessionCommand) {
	reason := cmd.reason
	if reason == "" {
		reason = EndExit
	}
	err := s.shutdown(reason)
	cmd.reply <- commandReply{err: err}
}

// appIsForeground reports whether a generated application is foreground for
// this session. It is read on the session goroutine when an input has to
// decide between app-facing and shell-facing behaviour.
func (s *session) appIsForeground() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.foreground.Kind == ForegroundApp
}

// beginTurn records an accepted input, establishes a turn, and either starts it
// or queues it behind the running one.
func (s *session) beginTurn(in SessionInput) (*turn, error) {
	// Queue capacity is checked before anything is recorded, so a refused
	// input leaves no turn in the research record.
	s.mu.RLock()
	queued := s.active != nil && len(s.pending) >= s.coord.limits.MaxQueuedTurns
	s.mu.RUnlock()
	if queued {
		return nil, domain.NewLimitError(domain.CodeMaxAttemptsReached, "session turn queue is full", map[string]string{
			"queued": strconv.Itoa(s.coord.limits.MaxQueuedTurns),
		})
	}

	action := "command"
	if in.Kind == InputPaste {
		action = "paste"
	}
	payload := domain.InputAcceptedPayload{Action: action, Command: commandOf(in)}
	turnID, err := s.coord.mintTurnID()
	if err != nil {
		return nil, err
	}

	t := &turn{
		session:   s,
		id:        turnID,
		input:     in,
		principal: in.Principal,
		state:     StateReceived,
		commits:   make(map[CommitKey]domain.Revision),
	}

	// The turn is established only after its input is durably recorded, so
	// research and application state agree on which turns exist.
	if _, err := s.journal.append(s.scope, domain.EventKindInputAccepted, payload, journalRefs{Turn: &turnID}); err != nil {
		return nil, err
	}
	if _, err := s.journal.append(s.scope, domain.EventKindTurnTransition, domain.TurnTransitionPayload{
		From: "", To: StateReceived, Generation: 0,
	}, journalRefs{Turn: &turnID}); err != nil {
		return nil, err
	}

	s.mu.Lock()
	if s.active == nil {
		s.active = t
	} else {
		s.pending = append(s.pending, t)
	}
	s.mu.Unlock()

	s.noteInputSequence(in, turnID)
	if t == s.currentTurn() {
		s.startWorker(t)
	}
	return t, nil
}

// startWorker runs one turn outside the session goroutine so a slow provider
// never delays cancellation, another session, or a new input.
func (s *session) startWorker(t *turn) {
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		s.executeTurn(t)
		select {
		case s.finished <- t:
		case <-s.scope.Done():
		}
	}()
}

// onTurnFinished retires a finished turn and starts the next queued one.
func (s *session) onTurnFinished(finished *turn) {
	s.mu.Lock()
	if s.active == finished {
		s.active = nil
		s.state.turnsCompleted++
	}
	var next *turn
	if s.active == nil && len(s.pending) > 0 {
		next, s.pending = s.pending[0], s.pending[1:]
		s.active = next
	}
	s.mu.Unlock()

	if next != nil {
		s.startWorker(next)
	}
}

// currentTurn returns the running turn, if any.
func (s *session) currentTurn() *turn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.active
}

// replayedInput recognises a transport-level replay of the last accepted input
// and returns the turn it already established. Input sequence numbers are
// monotonic per channel, so a repeated number is a replay, not a new input.
func (s *session) replayedInput(in SessionInput) (domain.TurnID, bool) {
	if !in.StartsTurn() {
		return domain.TurnID{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if in.Sequence == s.lastInputSequence && !s.lastInputTurn.IsZero() {
		return s.lastInputTurn, true
	}
	return domain.TurnID{}, false
}

// noteInputSequence records the sequence that established a turn.
func (s *session) noteInputSequence(in SessionInput, turnID domain.TurnID) {
	if !in.StartsTurn() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastInputSequence = in.Sequence
	s.lastInputTurn = turnID
}

// shutdown cancels the session scope, drains the workers, records the ending,
// and deregisters the session. It is idempotent.
func (s *session) shutdown(reason EndReason) error {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return nil
	}
	s.ended = true
	s.endReason = reason
	s.mu.Unlock()

	s.cancelScope()
	s.workers.Wait()

	endedAt := s.coord.clock.NowUnixMilli()
	s.mu.RLock()
	turns := s.state.turnsCompleted
	started := s.state.startedAtMs
	s.mu.RUnlock()

	payload := domain.SessionEndPayload{
		Reason:     string(reason),
		TurnsCount: turns,
		DurationMs: endedAt - started,
	}
	_, err := s.journal.append(context.Background(), domain.EventKindSessionEnd, payload, journalRefs{})
	s.coord.forget(s.id)
	return err
}

// recordStart durably records the accepted session: principal, auth mode,
// terminal metadata, and the sharing status in force at connect time.
func (s *session) recordStart(ctx context.Context, snapshot ConfigSnapshot) error {
	payload := domain.SessionStartPayload{
		UserID:         s.state.principal,
		AuthMode:       string(s.state.authMode),
		TerminalType:   s.state.terminal.Term,
		TerminalSize:   s.state.terminal.Size,
		ClientAddr:     s.state.terminal.ClientAddr,
		SharingEnabled: snapshot.ScopePolicy.SharingEnabled,
	}
	_, err := s.journal.append(ctx, domain.EventKindSessionStart, payload, journalRefs{})
	s.mu.Lock()
	s.recordingHealthy = err == nil
	s.mu.Unlock()
	return err
}

// sessionContext captures the session-local state handed to one turn.
func (s *session) sessionContext() SessionContext {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.context()
}

// observePolicyRevision records the scope-policy revision of a turn that is
// about to run and reports whether it differs from the session's previous
// revision. The first turn establishes the baseline and reports no change; a
// later turn whose revision differs means the live sharing policy changed, so
// any cross-scope context assembled under the old policy must not be reused.
func (s *session) observePolicyRevision(revision int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := s.policyRevision != 0 && s.policyRevision != revision
	s.policyRevision = revision
	return changed
}

// applySessionPatch applies an accepted session-state change. It runs after the
// world commit and before emission, so the emitted prompt shows accepted state.
func (s *session) applySessionPatch(patch SessionPatch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.applyPatch(patch)
}

// prompt returns the accepted prompt position.
func (s *session) prompt() PromptState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.prompt()
}

// emitShellPrompt records and delivers the prompt that follows a foreground
// application on end of input. Recording precedes delivery, as for every
// emitted output item; a prompt that cannot be recorded is not sent.
func (s *session) emitShellPrompt() {
	prompt := s.prompt()
	if !s.recordPromptAt(s.scope, prompt, journalRefs{}) {
		return
	}
	s.deliverOutput(s.scope, SessionOutput{
		Session:  s.id,
		Sequence: s.nextOutputSequence(),
		Kind:     OutputPrompt,
		Prompt:   &prompt,
	})
}

// nextOutputSequence returns the session-local sequence of the next output item.
func (s *session) nextOutputSequence() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outputSequence++
	return s.outputSequence
}

// peekOutputSequence returns the sequence the next output item will carry. A
// frame is recorded before its output item is assigned a sequence, so the frame
// record names the sequence the item is about to receive.
func (s *session) peekOutputSequence() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.outputSequence + 1
}

// deliverOutput hands one accepted output item to the adapter. It applies
// backpressure bounded by the turn deadline and the session scope, so a slow
// client cannot block other sessions or grow memory without limit.
func (s *session) deliverOutput(ctx context.Context, out SessionOutput) bool {
	select {
	case s.outputs <- out:
		return true
	case <-ctx.Done():
		return false
	case <-s.scope.Done():
		return false
	}
}

// deliverOutcome hands one turn outcome to the adapter. Outcomes are
// low-volume and must not be dropped: a lost completion would strand the
// adapter's flow control.
func (s *session) deliverOutcome(ctx context.Context, outcome SessionOutcome) {
	select {
	case s.outcomes <- outcome:
	case <-s.scope.Done():
	}
}

// commandOf returns the recorded text of an accepted input.
func commandOf(in SessionInput) string {
	switch in.Kind {
	case InputCommand:
		return in.Command
	case InputPaste:
		return in.Text
	default:
		return in.Key
	}
}

// truncateDetail bounds operator-facing detail carried in the research record.
func truncateDetail(detail string) string {
	const maxDetail = 256
	if len(detail) <= maxDetail {
		return detail
	}
	return fmt.Sprintf("%s...", detail[:maxDetail])
}
