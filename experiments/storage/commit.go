package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrConflict reports that a staged change set no longer applies. It is an
// expected outcome of concurrent work, not a failure: PLAN 5.3 requires the
// caller to refresh the affected state and rebase within a bounded retry budget
// rather than overwrite another session.
var ErrConflict = errors.New("storage: staged change set conflicts with committed state")

// ConflictError says which read dependency failed, so the caller can refresh
// exactly what it needs instead of re-reading the whole world.
type ConflictError struct {
	// NodeID is the node whose revision or existence did not match.
	NodeID string
	// Reason is a short, stable description such as "revision" or "directory
	// listing" or "absence".
	Reason string
	// Detail is for logs and receipts, not for policy.
	Detail string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("storage: conflict on %s (%s): %s", e.NodeID, e.Reason, e.Detail)
}

// Is reports every ConflictError as ErrConflict.
func (e *ConflictError) Is(target error) bool { return target == ErrConflict }

func conflict(nodeID, reason, detail string) error {
	return &ConflictError{NodeID: nodeID, Reason: reason, Detail: detail}
}

// Content is one immutable blob referenced by a change set. Content is written in
// the same transaction as the metadata pointing at it, so a file's bytes and its
// existence become durable together.
type Content struct {
	Hash      string
	Bytes     []byte
	MediaType string
}

// StagedChange is one world change a turn wants to apply, together with the
// versions it read while preparing it. The expectations are the read
// dependencies PLAN 5.3 requires: the node's own revision, its parent's
// membership revision, and absence where the turn created something new.
type StagedChange struct {
	// NodeID identifies the node. A create uses a fresh identifier.
	NodeID string
	// ParentID is the containing directory; for a create it is where the new
	// node's membership is claimed.
	ParentID string
	// Name is the entry name inside ParentID.
	Name string
	// Kind is "file" or "directory".
	Kind string
	// ContentHash references content already listed in the change set.
	ContentHash string
	// MediaType is recorded with the content bytes.
	MediaType string
	// ExpectAbsent marks a create. The commit fails if the name is taken.
	ExpectAbsent bool
	// ExpectedRevision is the revision the turn read. For a create it is zero.
	ExpectedRevision int64
	// ExpectedParentRevision is the parent's membership revision the turn read,
	// which is what detects a listing that changed underneath it.
	ExpectedParentRevision int64
}

// ChangeSet is one turn's staged world changes plus the accepted records that
// must become durable with them.
type ChangeSet struct {
	NamespaceID string
	Contents    []Content
	Changes     []StagedChange
	Events      []AcceptedEvent
}

// CommitChangeSet verifies every read dependency and applies the staged changes
// with their accepted events in one transaction.
//
// Every check runs inside the caller's transaction, so a check and the write it
// guards cannot be separated by another writer. The caller supplies the
// transaction; in the service that transaction is the writer queue's.
func CommitChangeSet(ctx context.Context, tx *sql.Tx, set ChangeSet) error {
	if len(set.Changes) == 0 && len(set.Events) == 0 && len(set.Contents) == 0 {
		return errors.New("storage: a commit must apply at least one change, content, or event")
	}

	// Contents are immutable and shared, so a rebased change set can re-present
	// content another turn already stored.
	for _, content := range set.Contents {
		if content.Hash != ContentHash(content.Bytes) {
			return fmt.Errorf("storage: content %s does not match its bytes", content.Hash)
		}
		mediaType := content.MediaType
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO contents (hash, bytes, size, media_type) VALUES (?, ?, ?, ?)
			 ON CONFLICT (hash) DO NOTHING`,
			content.Hash, content.Bytes, len(content.Bytes), mediaType,
		); err != nil {
			return fmt.Errorf("store content %s: %w", content.Hash, err)
		}
	}

	// Verify every read dependency before applying anything, so a conflict is
	// reported before the transaction has done any work.
	touchedParents := map[string]struct{}{}
	for _, change := range set.Changes {
		if err := verifyChange(ctx, tx, set.NamespaceID, change); err != nil {
			return err
		}
		touchedParents[change.ParentID] = struct{}{}
	}

	for _, change := range set.Changes {
		if err := applyChange(ctx, tx, set.NamespaceID, change); err != nil {
			return err
		}
	}

	// Membership changes are recorded once per parent, whatever the number of
	// children a turn added.
	for parentID := range touchedParents {
		if _, err := tx.ExecContext(ctx,
			`UPDATE directory_revisions SET revision = revision + 1 WHERE directory_id = ?`, parentID,
		); err != nil {
			return fmt.Errorf("advance directory revision for %s: %w", parentID, err)
		}
	}

	for _, event := range set.Events {
		if err := appendAcceptedEvent(ctx, tx, event); err != nil {
			return err
		}
	}
	return nil
}

// verifyChange checks one change's read dependencies inside the transaction.
func verifyChange(ctx context.Context, tx *sql.Tx, namespaceID string, change StagedChange) error {
	var directoryRevision int64
	err := tx.QueryRowContext(ctx,
		`SELECT revision FROM directory_revisions WHERE directory_id = ?`, change.ParentID,
	).Scan(&directoryRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return conflict(change.ParentID, "directory listing", "parent directory has no membership revision")
	}
	if err != nil {
		return fmt.Errorf("read directory revision for %s: %w", change.ParentID, err)
	}
	if directoryRevision != change.ExpectedParentRevision {
		return conflict(change.ParentID, "directory listing",
			fmt.Sprintf("membership revision is %d, the turn read %d", directoryRevision, change.ExpectedParentRevision))
	}

	var currentRevision int64
	err = tx.QueryRowContext(ctx,
		`SELECT revision FROM nodes WHERE id = ?`, change.NodeID,
	).Scan(&currentRevision)
	switch {
	case change.ExpectAbsent && err == nil:
		return conflict(change.NodeID, "absence", "a node with this identifier already exists")
	case change.ExpectAbsent && errors.Is(err, sql.ErrNoRows):
		// The name is what must be free. The unique constraint would reject a
		// duplicate name too, but a named conflict is a better answer for the
		// caller than a constraint failure.
		var taken int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM nodes WHERE namespace_id = ? AND parent_id = ? AND name = ?`,
			namespaceID, change.ParentID, change.Name,
		).Scan(&taken); err != nil {
			return fmt.Errorf("check absence of %s in %s: %w", change.Name, change.ParentID, err)
		}
		if taken != 0 {
			return conflict(change.NodeID, "absence", fmt.Sprintf("%s already exists in %s", change.Name, change.ParentID))
		}
	case !change.ExpectAbsent && errors.Is(err, sql.ErrNoRows):
		return conflict(change.NodeID, "revision", "the node was removed by another turn")
	case !change.ExpectAbsent && err != nil:
		return fmt.Errorf("read revision of %s: %w", change.NodeID, err)
	case !change.ExpectAbsent && currentRevision != change.ExpectedRevision:
		return conflict(change.NodeID, "revision",
			fmt.Sprintf("revision is %d, the turn read %d", currentRevision, change.ExpectedRevision))
	}
	return nil
}

// applyChange writes one verified change.
func applyChange(ctx context.Context, tx *sql.Tx, namespaceID string, change StagedChange) error {
	if change.ExpectAbsent {
		var content any
		if change.ContentHash != "" {
			content = change.ContentHash
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO nodes (id, namespace_id, parent_id, name, kind, revision, content_hash)
			 VALUES (?, ?, ?, ?, ?, 1, ?)`,
			change.NodeID, namespaceID, change.ParentID, change.Name, change.Kind, content,
		); err != nil {
			return fmt.Errorf("create node %s: %w", change.NodeID, err)
		}
		if change.Kind == "directory" {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO directory_revisions (directory_id, revision) VALUES (?, 1)`,
				change.NodeID,
			); err != nil {
				return fmt.Errorf("record membership revision for %s: %w", change.NodeID, err)
			}
		}
		return nil
	}

	// The guarded update is the optimistic check: it names the revision the turn
	// read, so a stale writer matches no row and is told so instead of
	// overwriting the winner.
	result, err := tx.ExecContext(ctx,
		`UPDATE nodes SET revision = revision + 1, content_hash = ? WHERE id = ? AND revision = ?`,
		change.ContentHash, change.NodeID, change.ExpectedRevision,
	)
	if err != nil {
		return fmt.Errorf("update node %s: %w", change.NodeID, err)
	}
	matched, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count rows updated for %s: %w", change.NodeID, err)
	}
	if matched != 1 {
		return conflict(change.NodeID, "revision",
			fmt.Sprintf("the guarded update matched %d rows", matched))
	}
	return nil
}

// rowQuerier is the read surface that *sql.DB and *sql.Tx share, so a revision
// can be read while staging a change outside a transaction as well as inside the
// one that applies it.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ReadDirectoryRevision returns a directory's membership revision, which a turn
// reads before staging a change and again when it rebases.
func ReadDirectoryRevision(ctx context.Context, db rowQuerier, directoryID string) (int64, error) {
	var revision int64
	err := db.QueryRowContext(ctx,
		`SELECT revision FROM directory_revisions WHERE directory_id = ?`, directoryID,
	).Scan(&revision)
	if err != nil {
		return 0, fmt.Errorf("read directory revision for %s: %w", directoryID, err)
	}
	return revision, nil
}

// ReadNodeRevision returns one node's revision.
func ReadNodeRevision(ctx context.Context, db rowQuerier, nodeID string) (int64, error) {
	var revision int64
	err := db.QueryRowContext(ctx,
		`SELECT revision FROM nodes WHERE id = ?`, nodeID,
	).Scan(&revision)
	if err != nil {
		return 0, fmt.Errorf("read revision of %s: %w", nodeID, err)
	}
	return revision, nil
}
