package domain

// TurnState is one explicit turn state (PLAN 7.1). The vocabulary lives in the
// domain because the durable turn record and the lifecycle policy that drives
// the worker must name the same states; a state added here is immediately
// available to both the producer of turn.transition records and their readers.
type TurnState string

const (
	StateReceived     TurnState = "received"
	StateQueued       TurnState = "queued"
	StateContextReady TurnState = "context_ready"
	StateGenerating   TurnState = "generating"
	StateAwaitingTool TurnState = "awaiting_tool"
	StateValidating   TurnState = "validating"
	StateCommitting   TurnState = "committing"
	StateEmitting     TurnState = "emitting"
	StateCompleted    TurnState = "completed"
	StateInterrupted  TurnState = "interrupted"
	StateFailed       TurnState = "failed"
	StateConflicted   TurnState = "conflicted"
)

// AllTurnStates returns every known turn state in lifecycle order.
func AllTurnStates() []TurnState {
	return []TurnState{
		StateReceived, StateQueued, StateContextReady, StateGenerating,
		StateAwaitingTool, StateValidating, StateCommitting, StateEmitting,
		StateCompleted, StateInterrupted, StateFailed, StateConflicted,
	}
}

// IsTerminal reports whether a state ends the turn.
func (s TurnState) IsTerminal() bool {
	switch s {
	case StateCompleted, StateInterrupted, StateFailed:
		return true
	default:
		return false
	}
}

// String makes turn states printable in errors and journals.
func (s TurnState) String() string { return string(s) }
