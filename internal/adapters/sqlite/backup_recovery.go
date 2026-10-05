package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"j0s.at/vibeshell/internal/admin"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// Pagination and recovery bounds. The page limit mirrors the directory
// listing bound; the recovered-turn list is bounded so one crash cannot
// produce a recovery payload larger than the inline event limit.
const (
	defaultSessionPageLimit = 100
	maxSessionPageLimit     = 1000
	maxRecoveredTurnIDs     = 16
)

// Recovery derives session/turn state from the append-only event log. The
// merged schema has no sessions or turns table, so a session is complete when
// it has an end event and recovered when it has a recovery marker; a turn is
// interrupted when it belongs to an incomplete session. Recovery marks those
// sessions with a durable session.recovered event, which also makes a second
// recovery run idempotent.
type Recovery struct {
	db     *DB
	events *Events
	clock  ports.Clock
}

// Compile-time proofs that the adapter stays behind the admin ports.
var (
	_ admin.RecoveryOps   = (*Recovery)(nil)
	_ admin.SessionLister = (*Recovery)(nil)
)

// NewRecovery builds the recovery/session adapter. The event store appends
// recovery markers; db supplies the session queries.
func NewRecovery(db *DB, events *Events, clock ports.Clock) *Recovery {
	return &Recovery{db: db, events: events, clock: clock}
}

// IncompleteSessions lists sessions that started but neither ended nor were
// already recovered, with their recorded turn counts.
func (r *Recovery) IncompleteSessions(ctx context.Context) ([]admin.SessionSummary, error) {
	if r == nil || r.db == nil {
		return nil, domain.NewUnavailableError(domain.CodeDatabaseUnavailable, "recovery has no database", nil, nil)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := r.db.sql.QueryContext(ctx, `
		SELECT s.session_id, s.started_at, s.last_at, s.last_seq,
		       (SELECT COUNT(DISTINCT t.turn_id) FROM events t
		        WHERE t.session_id = s.session_id AND t.turn_id <> '')
		FROM (
			SELECT session_id,
			       MIN(timestamp) AS started_at,
			       MAX(timestamp) AS last_at,
			       MAX(sequence)  AS last_seq
			FROM events
			WHERE kind = ?
			GROUP BY session_id
		) s
		WHERE NOT EXISTS (
			SELECT 1 FROM events e
			WHERE e.session_id = s.session_id AND e.kind IN (?, ?)
		)
		ORDER BY s.session_id`,
		string(domain.EventKindSessionStart),
		string(domain.EventKindSessionEnd),
		string(domain.EventKindSessionRecovered))
	if err != nil {
		return nil, fmt.Errorf("sqlite: list incomplete sessions: %w", err)
	}
	defer rows.Close()
	var out []admin.SessionSummary
	for rows.Next() {
		var summary admin.SessionSummary
		var session string
		var startedAt, lastAt, lastSeq, turns int64
		if err := rows.Scan(&session, &startedAt, &lastAt, &lastSeq, &turns); err != nil {
			return nil, fmt.Errorf("sqlite: scan incomplete session: %w", err)
		}
		id, err := domain.ParseSessionID(session)
		if err != nil {
			// A malformed stored identity is a data error, not a reason to
			// silently drop the session from recovery.
			return nil, domain.NewInternalError(domain.CodeInvariantViolation, "stored session id is malformed", err)
		}
		summary.SessionID = id
		summary.StartedAtUnixMilli = startedAt
		summary.LastActivityUnixMilli = lastAt
		if lastSeq > 0 {
			summary.LastSequence = uint64(lastSeq)
		}
		summary.TurnCount = int(turns)
		summary.InterruptedTurns = int(turns)
		out = append(out, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate incomplete sessions: %w", err)
	}
	return out, nil
}

// ListSessions pages through sessions derived from the event log, newest
// session id last. The cursor is the last session id returned; the next
// cursor is empty when the listing is complete.
func (r *Recovery) ListSessions(ctx context.Context, limit int, cursor string) ([]admin.SessionSummary, string, error) {
	if r == nil || r.db == nil {
		return nil, "", domain.NewUnavailableError(domain.CodeDatabaseUnavailable, "recovery has no database", nil, nil)
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if limit <= 0 {
		limit = defaultSessionPageLimit
	}
	if limit > maxSessionPageLimit {
		limit = maxSessionPageLimit
	}
	// Fetch one extra row to know whether another page exists without a
	// second query.
	rows, err := r.db.sql.QueryContext(ctx, `
		SELECT session_id,
		       MIN(timestamp) AS started_at,
		       MAX(timestamp) AS last_at,
		       MAX(sequence)  AS last_seq,
		       MAX(CASE WHEN kind = ? THEN 1 ELSE 0 END) AS ended,
		       MAX(CASE WHEN kind = ? THEN 1 ELSE 0 END) AS recovered,
		       COUNT(DISTINCT CASE WHEN turn_id <> '' THEN turn_id END) AS turns
		FROM events
		WHERE session_id > ?
		GROUP BY session_id
		ORDER BY session_id
		LIMIT ?`,
		string(domain.EventKindSessionEnd),
		string(domain.EventKindSessionRecovered),
		cursor, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: list sessions: %w", err)
	}
	defer rows.Close()
	var page []admin.SessionSummary
	for rows.Next() {
		var summary admin.SessionSummary
		var session string
		var startedAt, lastAt, lastSeq, turns, ended, recovered int64
		if err := rows.Scan(&session, &startedAt, &lastAt, &lastSeq, &ended, &recovered, &turns); err != nil {
			return nil, "", fmt.Errorf("sqlite: scan session: %w", err)
		}
		id, err := domain.ParseSessionID(session)
		if err != nil {
			return nil, "", domain.NewInternalError(domain.CodeInvariantViolation, "stored session id is malformed", err)
		}
		summary.SessionID = id
		summary.StartedAtUnixMilli = startedAt
		summary.LastActivityUnixMilli = lastAt
		if lastSeq > 0 {
			summary.LastSequence = uint64(lastSeq)
		}
		summary.Ended = ended != 0
		summary.Recovered = recovered != 0
		summary.TurnCount = int(turns)
		page = append(page, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("sqlite: iterate sessions: %w", err)
	}
	next := ""
	if len(page) > limit {
		page = page[:limit]
		next = page[len(page)-1].SessionID.String()
	}
	return page, next, nil
}

// RecoverIncomplete marks every incomplete session with a session.recovered
// event carrying the interrupted turn count, then returns what it did. The
// marker makes a repeated run a no-op, so a restart loop does not grow the
// event log.
func (r *Recovery) RecoverIncomplete(ctx context.Context) (admin.RecoveryReport, error) {
	if r == nil || r.db == nil {
		return admin.RecoveryReport{}, domain.NewUnavailableError(domain.CodeDatabaseUnavailable, "recovery has no database", nil, nil)
	}
	sessions, err := r.IncompleteSessions(ctx)
	if err != nil {
		return admin.RecoveryReport{}, err
	}
	report := admin.RecoveryReport{
		IncompleteSessions:   len(sessions),
		RecoveredAtUnixMilli: r.now(),
	}
	for _, session := range sessions {
		turns, err := r.interruptedTurnIDs(ctx, session.SessionID, maxRecoveredTurnIDs)
		if err != nil {
			return report, err
		}
		if err := r.markRecovered(ctx, session, turns); err != nil {
			return report, err
		}
		report.MarkedSessions++
		report.InterruptedTurns += session.InterruptedTurns
	}
	return report, nil
}

// interruptedTurnIDs returns up to limit distinct turn IDs recorded for the
// session, for the recovery marker's audit detail.
func (r *Recovery) interruptedTurnIDs(ctx context.Context, session domain.SessionID, limit int) ([]string, error) {
	rows, err := r.db.sql.QueryContext(ctx,
		`SELECT DISTINCT turn_id FROM events WHERE session_id = ? AND turn_id <> '' ORDER BY turn_id LIMIT ?`,
		session.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list interrupted turns: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var turn string
		if err := rows.Scan(&turn); err != nil {
			return nil, fmt.Errorf("sqlite: scan interrupted turn: %w", err)
		}
		out = append(out, turn)
	}
	return out, rows.Err()
}

// sessionRecoveredPayload is the inline payload of a recovery marker. It
// carries no secret and names the interrupted turns so the research record
// can correlate the marker with the work it closed out.
type sessionRecoveredPayload struct {
	Reason               string   `json:"reason"`
	InterruptedTurnCount int      `json:"interrupted_turn_count"`
	InterruptedTurns     []string `json:"interrupted_turns,omitempty"`
	LastSequence         uint64   `json:"last_sequence"`
	RecoveredAtUnixMilli int64    `json:"recovered_at_unix_milli"`
}

func (r *Recovery) markRecovered(ctx context.Context, session admin.SessionSummary, turns []string) error {
	if r.events == nil {
		return domain.NewUnavailableError(domain.CodeDatabaseUnavailable, "recovery has no event store", nil, nil)
	}
	raw, err := json.Marshal(sessionRecoveredPayload{
		Reason:               "startup_recovery",
		InterruptedTurnCount: session.InterruptedTurns,
		InterruptedTurns:     turns,
		LastSequence:         session.LastSequence,
		RecoveredAtUnixMilli: r.now(),
	})
	if err != nil {
		return fmt.Errorf("sqlite: encode recovery payload: %w", err)
	}
	envelope := domain.NewEventEnvelope(
		session.SessionID,
		0, // the writer assigns the next per-session sequence
		domain.EventKindSessionRecovered,
		domain.Provenance{Source: "system", Actor: "startup_recovery"},
		r.now(),
		r.monotonic(),
	)
	envelope.Payload = domain.PayloadRef{Inline: raw}
	if _, err := r.events.Append(ctx, envelope); err != nil {
		return fmt.Errorf("sqlite: append recovery marker: %w", err)
	}
	return nil
}

func (r *Recovery) now() int64 {
	if r.clock != nil {
		return r.clock.NowUnixMilli()
	}
	return time.Now().UnixMilli()
}

func (r *Recovery) monotonic() int64 {
	if r.clock != nil {
		return r.clock.MonotonicNanos()
	}
	return 0
}
