package admin

import (
	"context"

	"j0s.at/vibeshell/internal/domain"
)

// SessionSummary is the operator's view of one session. The merged storage
// schema has no sessions table yet, so the summary is derived from the event
// log: a session with a start event and no end event is incomplete, and a
// session recovered at startup has a session.recovered marker.
type SessionSummary struct {
	SessionID             domain.SessionID
	StartedAtUnixMilli    int64
	LastActivityUnixMilli int64
	LastSequence          uint64
	Ended                 bool
	Recovered             bool
	TurnCount             int
	InterruptedTurns      int
}

// RecoveryReport is the outcome of marking interrupted work at startup.
type RecoveryReport struct {
	IncompleteSessions   int
	MarkedSessions       int
	InterruptedTurns     int
	RecoveredAtUnixMilli int64
}

// RecoveryOps performs startup recovery of incomplete session/turn state from
// the event log (PLAN 12.3). The store cannot know which in-flight turn was
// interrupted from a durable table, so it treats every turn recorded in an
// incomplete session as interrupted and marks the session recovered with a
// durable event; a later recovery run then leaves it alone.
type RecoveryOps interface {
	// IncompleteSessions lists sessions with a start event but neither an
	// end event nor a recovery marker.
	IncompleteSessions(ctx context.Context) ([]SessionSummary, error)
	// RecoverIncomplete marks every incomplete session and returns the count.
	RecoverIncomplete(ctx context.Context) (RecoveryReport, error)
}

// SessionLister lists sessions for operator display, derived from the event
// log. It returns a page and a cursor for the next page (empty when done).
type SessionLister interface {
	ListSessions(ctx context.Context, limit int, cursor string) ([]SessionSummary, string, error)
}
