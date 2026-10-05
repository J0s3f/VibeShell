package application

import (
	"errors"
	"fmt"
	"strings"

	"j0s.at/vibeshell/internal/domain"
)

// This file holds the turn lifecycle as a pure policy: the legal transitions
// between the domain-owned state names, and the retry decisions. It performs no
// I/O, reads no clock, and takes no lock, so it can be exercised as a table. The
// goroutines that apply these rules live in turnrun.go.

// TurnState is the domain-owned turn state vocabulary (PLAN 7.1), aliased here
// because the transition table below and the durable turn.transition record have
// to name the same states.
type TurnState = domain.TurnState

const (
	StateReceived     = domain.StateReceived
	StateQueued       = domain.StateQueued
	StateContextReady = domain.StateContextReady
	StateGenerating   = domain.StateGenerating
	StateAwaitingTool = domain.StateAwaitingTool
	StateValidating   = domain.StateValidating
	StateCommitting   = domain.StateCommitting
	StateEmitting     = domain.StateEmitting
	StateCompleted    = domain.StateCompleted
	StateInterrupted  = domain.StateInterrupted
	StateFailed       = domain.StateFailed
	StateConflicted   = domain.StateConflicted
)

// turnTransitions is the complete legal transition table.
//
// Five ordering rules are encoded here rather than in the worker:
//
//   - received reaches a terminal state from anywhere, because cancellation and
//     failure must always be reachable.
//   - generating accepts itself: a retry starts a new generation of the same
//     state, distinguished by the generation and attempt IDs.
//   - a rejected response returns to generating (bounded repair), and a failed
//     tool batch may return to queued, because a rebase re-reads the state that
//     changed.
//   - committing and emitting cannot become interrupted. Once the world commit
//     is accepted the mutation is durable, so a cancellation arriving afterwards
//     must not report the turn as discarded work. The worker completes the turn
//     and records the delivery outcome instead (PLAN 4.3).
//   - conflicted re-queues rather than failing, because a concurrent change is
//     not a model-health failure (PLAN 5.3).
var turnTransitions = map[TurnState][]TurnState{
	StateReceived:     {StateQueued, StateInterrupted, StateFailed},
	StateQueued:       {StateContextReady, StateInterrupted, StateFailed},
	StateContextReady: {StateGenerating, StateInterrupted, StateFailed},
	StateGenerating:   {StateGenerating, StateAwaitingTool, StateValidating, StateConflicted, StateInterrupted, StateFailed},
	StateAwaitingTool: {StateGenerating, StateQueued, StateValidating, StateInterrupted, StateFailed},
	StateValidating:   {StateGenerating, StateCommitting, StateConflicted, StateInterrupted, StateFailed},
	StateCommitting:   {StateEmitting, StateConflicted, StateFailed},
	StateEmitting:     {StateCompleted, StateFailed},
	StateConflicted:   {StateQueued, StateInterrupted, StateFailed},
	StateCompleted:    nil,
	StateInterrupted:  nil,
	StateFailed:       nil,
}

// CanTransitionTurn reports whether a turn may move from one state to another.
// An unknown state is never a legal target.
func CanTransitionTurn(from, to TurnState) bool {
	for _, allowed := range turnTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// TurnFailure is the typed reason a turn did not complete.
type TurnFailure struct {
	Class   domain.FailureClass `json:"class"`
	Code    string              `json:"code"`
	Message string              `json:"message"`
	// Attempted marks a failure produced by this turn's own work rather than
	// by an external dependency.
	Attempted bool `json:"attempted"`
}

// IsRetryable reports whether a failure may be retried at all. Cancellation is
// never retryable and never carries a provider-health penalty (PLAN 9.1).
func (f TurnFailure) IsRetryable() bool {
	return f.Class != domain.FailureUserCancelled && f.Class != domain.FailureContentRejected
}

func (f TurnFailure) error() string {
	return fmt.Sprintf("%s: %s", f.Class, f.Message)
}

// Error implements error so a failure can travel like any other error.
func (f TurnFailure) Error() string { return f.error() }

// AsDomainError converts the failure into the shared typed error vocabulary so
// adapters and callers can classify it with errors.Is/errors.As.
func (f TurnFailure) AsDomainError() *domain.DomainError {
	switch f.Class {
	case domain.FailureUserCancelled:
		return domain.NewCancelledError(domain.CodeUserCancelled, f.Message, map[string]string{"class": string(f.Class)})
	case domain.FailureWorldConflict:
		return domain.NewConflictError(domain.CodeConcurrentModification, f.Message, map[string]string{"class": string(f.Class)})
	case domain.FailureQuotaExhausted, domain.FailureRateLimited:
		return domain.NewLimitError(domain.CodeRateLimited, f.Message, map[string]string{"class": string(f.Class)})
	case domain.FailureContextTooLong:
		return domain.NewLimitError(domain.CodeContextTooLong, f.Message, map[string]string{"class": string(f.Class)})
	case domain.FailureProviderOutage, domain.FailureNetworkTimeout, domain.FailureModelNotFound:
		return domain.NewUnavailableError(domain.CodeModelUnavailable, f.Message, map[string]string{"class": string(f.Class)}, nil)
	default:
		return domain.NewInternalError(f.Code, f.Message, f)
	}
}

// failureKind separates the three budgets a turn can spend.
type failureKind int

const (
	// failureProvider consumes the attempt budget: a retry uses another
	// provider attempt.
	failureProvider failureKind = iota
	// failureRebase consumes the rebase budget: a retry re-reads the changed
	// state and re-evaluates the turn.
	failureRebase
	// failureRepair consumes the repair budget: a retry re-asks the model once
	// with validation feedback.
	failureRepair
	// failureFatal never retries.
	failureFatal
)

// retryDecision is the pure outcome of a failed attempt.
type retryDecision struct {
	// Retry reports whether a new generation may start.
	Retry bool
	// Kind is the budget a retry consumes; meaningful only when Retry is set.
	Kind failureKind
	// Reason explains the decision in the durable journal.
	Reason string
}

// decideRetry is the whole retry policy as a pure function: a failure class
// plus the remaining budget decides whether a new generation starts, which
// budget it spends, and why. No state is mutated here.
func decideRetry(f TurnFailure, kind failureKind, attempts, rebases, repairs int, limits Limits, snapshot ConfigSnapshot) retryDecision {
	if kind == failureFatal || !f.IsRetryable() {
		return retryDecision{Reason: "failure is not retryable"}
	}
	switch kind {
	case failureProvider:
		budget := min(limits.MaxAttempts, snapshot.MaxAttempts)
		if attempts >= budget {
			return retryDecision{Reason: fmt.Sprintf("attempt budget exhausted after %d attempts", attempts)}
		}
		return retryDecision{Retry: true, Kind: failureProvider, Reason: "retrying provider attempt"}
	case failureRebase:
		budget := min(limits.MaxRebases, snapshot.MaxRebases)
		if rebases >= budget {
			return retryDecision{Reason: fmt.Sprintf("rebase budget exhausted after %d rebases", rebases)}
		}
		return retryDecision{Retry: true, Kind: failureRebase, Reason: "rebasing onto refreshed state"}
	case failureRepair:
		if repairs >= limits.MaxRepairs {
			return retryDecision{Reason: "repair budget exhausted"}
		}
		return retryDecision{Retry: true, Kind: failureRepair, Reason: "retrying with repair feedback"}
	default:
		return retryDecision{Reason: "unknown failure kind"}
	}
}

// classifyError maps an error returned by a dependency onto a turn failure.
// Errors that are already domain-typed keep their category; unknown errors are
// never treated as success.
func classifyError(err error, kind failureKind) TurnFailure {
	if err == nil {
		return TurnFailure{}
	}
	if failure, ok := err.(TurnFailure); ok {
		return failure
	}
	var failure TurnFailure
	if errors.As(err, &failure) {
		return failure
	}
	var envelope domain.ErrorEnvelope
	if errors.As(err, &envelope) {
		return TurnFailure{
			Class:   envelope.Class,
			Code:    string(envelope.Class),
			Message: envelope.Message,
		}
	}
	var domainErr *domain.DomainError
	if errors.As(err, &domainErr) {
		return TurnFailureFromDomainError(domainErr)
	}
	switch kind {
	case failureProvider:
		return TurnFailure{Class: domain.FailureUnknown, Code: domain.CodeModelUnavailable, Message: err.Error(), Attempted: true}
	case failureRebase:
		return TurnFailure{Class: domain.FailureWorldConflict, Code: domain.CodeRevisionMismatch, Message: err.Error(), Attempted: true}
	default:
		return TurnFailure{Class: domain.FailureInvalidResponse, Code: domain.CodeInvariantViolation, Message: err.Error(), Attempted: true}
	}
}

// TurnFailureFromDomainError converts a shared typed error into a turn failure
// without losing its category.
func TurnFailureFromDomainError(err *domain.DomainError) TurnFailure {
	failure := TurnFailure{Code: err.Code, Message: err.Message}
	switch err.Category {
	case domain.CategoryCancelled:
		failure.Class = domain.FailureUserCancelled
	case domain.CategoryConflict:
		failure.Class = domain.FailureWorldConflict
	case domain.CategoryLimit:
		failure.Class = domain.FailureRateLimited
	case domain.CategoryUnavailable:
		failure.Class = domain.FailureProviderOutage
	case domain.CategoryValidation:
		failure.Class = domain.FailureInvalidResponse
	default:
		failure.Class = domain.FailureUnknown
	}
	return failure
}

// firstControlRune returns the first terminal control character in text, or
// zero when the text is safe to write verbatim. Tab, newline, and carriage
// return are ordinary output; every other C0 control plus DEL is a terminal
// control that would let model text drive the user's screen.
func firstControlRune(text string) rune {
	for _, r := range text {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
		case r < 0x20 || r == 0x7f:
			return r
		}
	}
	return 0
}

// Scope authorization for mutations happens where namespaces are resolved (the
// tool layer, and again inside the world store): a ChangeSet carries namespace
// IDs, not scopes, so the coordinator passes the pinned policy into Commit and
// lets the store re-verify it at the transaction boundary.

// validateCandidate is the pure gate between a proposed outcome and the commit
// point. It checks the invariants that the world store or the terminal would
// otherwise have to reject after the fact.
func validateCandidate(candidate *TurnCandidate, snapshot ConfigSnapshot, limits Limits) error {
	if candidate == nil {
		return domain.NewValidationError(domain.CodeInvalidInput, "candidate is required", nil)
	}
	if candidate.CommitKey.Turn.IsZero() {
		return domain.NewValidationError(domain.CodeInvalidIdentity, "commit key requires a turn", nil)
	}
	if !candidate.Changes.TurnID.IsZero() && candidate.Changes.TurnID != candidate.CommitKey.Turn {
		return domain.NewValidationError(domain.CodeInvalidMutation, "change set turn does not match the commit key", nil)
	}
	if !candidate.Changes.IsEmpty() && strings.TrimSpace(candidate.CommitKey.Logical) == "" {
		return domain.NewValidationError(domain.CodeInvalidMutation, "mutations require a logical commit key", nil)
	}
	for _, mutation := range candidate.Changes.Mutations {
		if mutation.NamespaceID.IsZero() {
			return domain.NewValidationError(domain.CodeInvalidScope, "mutation requires a namespace", nil)
		}
		if mutation.Path == "" {
			return domain.NewValidationError(domain.CodeInvalidPath, "mutation requires a path", nil)
		}
	}
	if candidate.Output.View != nil {
		if err := domain.ValidateView(*candidate.Output.View); err != nil {
			return domain.WrapError(err, domain.CategoryValidation, domain.CodeInvalidAppView, "candidate view is not valid")
		}
	}
	if len(candidate.Output.Text) > limits.MaxOutputBytes {
		return domain.NewLimitError(domain.CodeOutputTooLarge, "candidate text exceeds the output limit", map[string]string{
			"bytes": fmt.Sprint(len(candidate.Output.Text)),
			"limit": fmt.Sprint(limits.MaxOutputBytes),
		})
	}
	if control := firstControlRune(candidate.Output.Text); control != 0 {
		// Plain text is written verbatim to the transport, so it must not carry
		// terminal control characters. A candidate that needs them must produce
		// a validated view instead, which is rendered by the terminal port.
		return domain.NewValidationError(domain.CodeInvalidInput, "candidate text contains a terminal control character",
			map[string]string{"rune": fmt.Sprintf("%U", control)})
	}
	if candidate.ExitStatus < 0 || candidate.ExitStatus > 255 {
		return domain.NewValidationError(domain.CodeInvalidInput, "exit status out of range", map[string]string{
			"exit_status": fmt.Sprint(candidate.ExitStatus),
		})
	}
	if patch := candidate.SessionPatch; patch.Foreground != nil {
		if err := patch.Foreground.Validate(); err != nil {
			return err
		}
	}
	if patch := candidate.SessionPatch; patch.CWD != nil && *patch.CWD == "" {
		return domain.NewValidationError(domain.CodeInvalidPath, "session patch cwd must be a valid path", nil)
	}
	if patch := candidate.SessionPatch; patch.ExitStatus != nil && (*patch.ExitStatus < 0 || *patch.ExitStatus > 255) {
		return domain.NewValidationError(domain.CodeInvalidInput, "session patch exit status out of range", nil)
	}
	if prompt := candidate.SessionPatch.AppPrompt; prompt != nil {
		if prompt.AppID.IsZero() {
			return domain.NewValidationError(domain.CodeInvalidInput, "session patch app prompt requires an app id", nil)
		}
		if len(prompt.Prompt) > domain.MaxAppPromptBytes {
			return domain.NewLimitError(domain.CodeOutputTooLarge, "session patch app prompt exceeds the prompt limit", map[string]string{
				"bytes": fmt.Sprint(len(prompt.Prompt)),
				"limit": fmt.Sprint(domain.MaxAppPromptBytes),
			})
		}
	}
	return nil
}
