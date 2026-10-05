package storage

import (
	"context"
	"database/sql"
	"fmt"
)

// AcceptedEvent is one accepted turn outcome recorded with its world changes.
// Order comes from the session sequence, never from a timestamp, and the caller
// supplies the timestamp so that the recorded clock is the one the service used.
type AcceptedEvent struct {
	ID        string
	SessionID string
	Sequence  int
	Kind      string
	// OccurredAt is the UTC timestamp the service assigned, in a format the
	// export layer understands. It is required rather than defaulted so that no
	// event silently carries the wall clock of a background goroutine.
	OccurredAt string
	Payload    []byte
	// Command and Path feed the lookup indexes and the text projection. An event
	// that touched neither, such as a routing decision, records no lookup rows.
	Command string
	Path    string
}

// appendAcceptedEvent writes one accepted event with its lookup projections in
// the caller's transaction, so the event and the world change it describes become
// durable together.
//
// The projections are written here rather than by a trigger because they are
// part of the same turn record: a session sequence gap would otherwise be
// visible in the transcript but missing from the command history.
func appendAcceptedEvent(ctx context.Context, tx *sql.Tx, event AcceptedEvent) error {
	if event.OccurredAt == "" {
		return fmt.Errorf("event %s has no timestamp", event.ID)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (id, session_id, sequence, occurred_at, kind, payload) VALUES (?, ?, ?, ?, ?, ?)`,
		event.ID, event.SessionID, event.Sequence, event.OccurredAt, event.Kind, event.Payload,
	); err != nil {
		return fmt.Errorf("append event %s: %w", event.ID, err)
	}

	if event.Command == "" && event.Path == "" {
		return nil
	}
	if event.Command != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO command_index (event_id, command, normalized_command, cwd, exit_status)
			 VALUES (?, ?, ?, ?, 0)`,
			event.ID, event.Command, NormalizeCommand(event.Command), "/",
		); err != nil {
			return fmt.Errorf("index command for %s: %w", event.ID, err)
		}
	}
	if event.Path != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO path_index (event_id, path, path_key) VALUES (?, ?, ?)`,
			event.ID, event.Path, PathKey(event.Path),
		); err != nil {
			return fmt.Errorf("index path for %s: %w", event.ID, err)
		}
	}
	return nil
}

// IndexEventForSearch adds the full-text projection row for an accepted event.
// It is separate from appendAcceptedEvent because the projection is derived: it
// lives in an FTS5 table, and the search gate reports separately whether FTS5 is
// available. The caller runs it in the same transaction, so the projection is
// never behind the event it describes.
func IndexEventForSearch(ctx context.Context, tx *sql.Tx, eventID, command, path string) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO search_docs (event_id, command, path) VALUES (?, ?, ?)`,
		eventID, command, path,
	); err != nil {
		return fmt.Errorf("index search projection for %s: %w", eventID, err)
	}
	return nil
}
