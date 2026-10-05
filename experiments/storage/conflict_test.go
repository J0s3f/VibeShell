package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestGuardedUpdateHasExactlyOneWinner is the core of the optimistic protocol:
// two writers that read the same revision both issue
// `UPDATE ... WHERE revision = ?`, and exactly one of them changes a row.
func TestGuardedUpdateHasExactlyOneWinner(t *testing.T) {
	db, path := newSpikeDB(t, "optimistic")
	seedWorld(t, db, "ns-opt", "root-opt")
	insertDirectory(t, db, "dir", "root-opt", "dir")
	seedNodes(t, db, "dir", 1)
	if revision := queryInt(t, db, `SELECT revision FROM nodes WHERE id = 'seed-0000'`); revision != 1 {
		t.Fatalf("seeded revision is %d, want 1", revision)
	}

	receipt := newReport("Gate 5a: optimistic revision check")
	receipt.add("captured (UTC)", time.Now().UTC().Format(time.RFC3339))
	receipt.add("node", "seed-0000")
	receipt.add("revision both writers read", 1)

	// Two independent single-connection pools, so the two writers are separate
	// connections that cannot see each other's uncommitted work.
	writers := make([]*sql.DB, 2)
	for i := range writers {
		pool, err := OpenWriter(path, Options{})
		if err != nil {
			t.Fatalf("open writer %d: %v", i, err)
		}
		defer pool.Close()
		writers[i] = pool
	}

	var winners, losers counter
	start := make(chan struct{})
	var done sync.WaitGroup
	for i, pool := range writers {
		done.Add(1)
		go func(index int, pool *sql.DB) {
			defer done.Done()
			tx, err := pool.Begin()
			if err != nil {
				t.Errorf("writer %d begin: %v", index, err)
				return
			}
			defer tx.Rollback()
			<-start
			result, err := tx.Exec(
				`UPDATE nodes SET revision = revision + 1 WHERE id = 'seed-0000' AND revision = 1`,
			)
			if err != nil {
				t.Errorf("writer %d update: %v", index, err)
				return
			}
			affected, err := result.RowsAffected()
			if err != nil {
				t.Errorf("writer %d rows affected: %v", index, err)
				return
			}
			switch affected {
			case 1:
				winners.add(1)
			case 0:
				losers.add(1)
			default:
				t.Errorf("writer %d changed %d rows, want 0 or 1", index, affected)
				return
			}
			if affected == 1 {
				if err := tx.Commit(); err != nil {
					t.Errorf("writer %d commit: %v", index, err)
				}
			}
		}(i, pool)
	}
	close(start)
	done.Wait()

	receipt.add("writers that changed a row", winners.value())
	receipt.add("writers that changed nothing", losers.value())
	receipt.add("revision afterwards", queryInt(t, db, `SELECT revision FROM nodes WHERE id = 'seed-0000'`))

	if winners.value() != 1 || losers.value() != 1 {
		t.Fatalf("expected exactly one winner and one loser, got %d and %d", winners.value(), losers.value())
	}
	if revision := queryInt(t, db, `SELECT revision FROM nodes WHERE id = 'seed-0000'`); revision != 2 {
		t.Errorf("revision is %d, want 2: one increment, no lost update", revision)
	}
	receipt.add("conclusion", "the guarded UPDATE matches one row for exactly one writer; the other observes zero rows and can refresh")
	receipt.write(t, "gate5-one-winner")
}

// TestStagedChangeSetDetectsAStaleDirectoryRevision covers the read dependency
// that a node's own revision cannot express: the listing the turn read.
func TestStagedChangeSetDetectsAStaleDirectoryRevision(t *testing.T) {
	db, _ := newSpikeDB(t, "stale-listing")
	seedWorld(t, db, "ns-listing", "root-listing")
	insertDirectory(t, db, "dir", "root-listing", "dir")

	receipt := newReport("Gate 5b: directory membership dependency")
	observed := directoryRevision(t, db, "dir")
	receipt.add("revision the turn read", observed)

	// Another session commits a change in the same directory first.
	otherBytes := []byte("other session content")
	if err := runCommit(t, db, ChangeSet{
		NamespaceID: "ns-listing",
		Contents:    []Content{{Hash: ContentHash(otherBytes), Bytes: otherBytes}},
		Changes: []StagedChange{{
			NodeID: "other-session-node", ParentID: "dir", Name: "other.txt",
			Kind: "file", ContentHash: ContentHash(otherBytes),
			ExpectAbsent: true, ExpectedParentRevision: observed,
		}},
		Events: []AcceptedEvent{{
			ID: "ev-other", SessionID: "session-other", Sequence: 1, Kind: "accepted",
			OccurredAt: fixedTimestamp(1), Command: "touch other.txt", Path: "/dir/other.txt",
		}},
	}); err != nil {
		t.Fatalf("the other session's commit failed: %v", err)
	}
	after := directoryRevision(t, db, "dir")
	receipt.add("revision after the other commit", after)
	if after == observed {
		t.Fatal("the other session's commit did not advance the membership revision")
	}

	// The stale turn now commits against the listing it read.
	err := runCommit(t, db, ChangeSet{
		NamespaceID: "ns-listing",
		Changes: []StagedChange{{
			NodeID: "stale-node", ParentID: "dir", Name: "stale.txt",
			Kind: "file", ExpectAbsent: true, ExpectedParentRevision: observed,
		}},
		Events: []AcceptedEvent{{
			ID: "ev-stale", SessionID: "session-stale", Sequence: 1, Kind: "accepted",
			OccurredAt: fixedTimestamp(1), Command: "touch stale.txt", Path: "/dir/stale.txt",
		}},
	})
	receipt.add("stale commit", describeErr(err))

	if !errors.Is(err, ErrConflict) {
		t.Fatalf("a stale commit returned %v, want a conflict", err)
	}
	var conflictErr *ConflictError
	if !errors.As(err, &conflictErr) || conflictErr.Reason != "directory listing" {
		t.Fatalf("conflict is %#v, want reason %q", conflictErr, "directory listing")
	}
	if count := queryInt(t, db, `SELECT COUNT(*) FROM nodes WHERE id = 'stale-node'`); count != 0 {
		t.Error("the refused change set still created its node")
	}
	if count := queryInt(t, db, `SELECT COUNT(*) FROM events WHERE id = 'ev-stale'`); count != 0 {
		t.Error("the refused change set still recorded its event")
	}

	// The rebase: refresh the revision and commit again.
	rebaseErr := runCommit(t, db, ChangeSet{
		NamespaceID: "ns-listing",
		Changes: []StagedChange{{
			NodeID: "stale-node", ParentID: "dir", Name: "stale.txt",
			Kind: "file", ExpectAbsent: true, ExpectedParentRevision: after,
		}},
		Events: []AcceptedEvent{{
			ID: "ev-stale", SessionID: "session-stale", Sequence: 1, Kind: "accepted",
			OccurredAt: fixedTimestamp(1), Command: "touch stale.txt", Path: "/dir/stale.txt",
		}},
	})
	receipt.add("rebase after refresh", describeErr(rebaseErr))
	if rebaseErr != nil {
		t.Fatalf("rebased commit failed: %v", rebaseErr)
	}
	if count := queryInt(t, db, `SELECT COUNT(*) FROM nodes WHERE id = 'stale-node'`); count != 1 {
		t.Error("the rebased commit did not create its node")
	}
	receipt.add("conclusion", "a listing that changed underneath a turn is a conflict, and the rebase is a refresh plus a second commit")
	receipt.write(t, "gate5-stale-listing")
}

// TestStagedCreateRejectsAConcurrentCreation covers the absence check: two turns
// materializing the same path, where only one may win.
func TestStagedCreateRejectsAConcurrentCreation(t *testing.T) {
	db, _ := newSpikeDB(t, "absence")
	seedWorld(t, db, "ns-absence", "root-absence")
	insertDirectory(t, db, "dir", "root-absence", "dir")

	receipt := newReport("Gate 5c: absence check")
	revision := directoryRevision(t, db, "dir")
	receipt.add("directory revision", revision)

	create := func(nodeID, eventID, sessionID string) error {
		bytes := []byte(nodeID)
		return runCommit(t, db, ChangeSet{
			NamespaceID: "ns-absence",
			Contents:    []Content{{Hash: ContentHash(bytes), Bytes: bytes}},
			Changes: []StagedChange{{
				NodeID: nodeID, ParentID: "dir", Name: "notes.txt",
				Kind: "file", ContentHash: ContentHash(bytes),
				ExpectAbsent: true, ExpectedParentRevision: revision,
			}},
			Events: []AcceptedEvent{{
				ID: eventID, SessionID: sessionID, Sequence: 1, Kind: "accepted",
				OccurredAt: fixedTimestamp(1), Command: "edit notes.txt", Path: "/dir/notes.txt",
			}},
		})
	}

	// Two sessions materialize the same name from the same listing.
	var accepted, refused counter
	start := make(chan struct{})
	var done sync.WaitGroup
	for i, spec := range []struct{ node, event, session string }{
		{"node-a", "ev-a", "session-a"},
		{"node-b", "ev-b", "session-b"},
	} {
		done.Add(1)
		go func(index int, node, event, session string) {
			defer done.Done()
			<-start
			err := create(node, event, session)
			switch {
			case err == nil:
				accepted.add(1)
			case errors.Is(err, ErrConflict):
				refused.add(1)
			default:
				t.Errorf("attempt %d: %v", index, err)
			}
		}(i, spec.node, spec.event, spec.session)
	}
	close(start)
	done.Wait()

	receipt.add("commits accepted", accepted.value())
	receipt.add("commits refused", refused.value())
	receipt.add("nodes named notes.txt", queryInt(t, db,
		`SELECT COUNT(*) FROM nodes WHERE parent_id = 'dir' AND name = 'notes.txt'`))

	if accepted.value() != 1 || refused.value() != 1 {
		t.Fatalf("expected one winner and one refusal, got %d and %d", accepted.value(), refused.value())
	}
	if count := queryInt(t, db, `SELECT COUNT(*) FROM nodes WHERE parent_id = 'dir' AND name = 'notes.txt'`); count != 1 {
		t.Errorf("%d nodes named notes.txt, want 1", count)
	}
	receipt.write(t, "gate5-absence")
}

// TestStagedChangeSetAndEventsCommitAtomically proves that a world change and
// the accepted records describing it share one transaction, in both directions:
// they appear together or not at all.
func TestStagedChangeSetAndEventsCommitAtomically(t *testing.T) {
	db, path := newSpikeDB(t, "atomic")
	seedWorld(t, db, "ns-atomic", "root-atomic")
	insertDirectory(t, db, "dir", "root-atomic", "dir")
	applySearchSchema(t, db)

	receipt := newReport("Gate 5d: staged change set and accepted events in one transaction")
	ctx := context.Background()

	// The accepted case: content, node, event, and the search projection land
	// together.
	revision := directoryRevision(t, db, "dir")
	bytes := []byte("exact\x00bytes\xff\x80")
	hash := ContentHash(bytes)
	accepted := NewWriterQueue(db, 4)
	if err := accepted.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := CommitChangeSet(ctx, tx, ChangeSet{
			NamespaceID: "ns-atomic",
			Contents:    []Content{{Hash: hash, Bytes: bytes, MediaType: "text/plain"}},
			Changes: []StagedChange{{
				NodeID: "node-ok", ParentID: "dir", Name: "ok.txt",
				Kind: "file", ContentHash: hash,
				ExpectAbsent: true, ExpectedParentRevision: revision,
			}},
			Events: []AcceptedEvent{{
				ID: "ev-ok", SessionID: "session-1", Sequence: 1, Kind: "accepted",
				OccurredAt: fixedTimestamp(1), Command: "write ok.txt", Path: "/dir/ok.txt",
				Payload: []byte{0x00, 0xff},
			}},
		}); err != nil {
			return err
		}
		return IndexEventForSearch(ctx, tx, "ev-ok", "write ok.txt", "/dir/ok.txt")
	}); err != nil {
		t.Fatalf("submit the accepted commit: %v", err)
	}
	accepted.Close()

	reader, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer reader.Close()

	var got []byte
	if err := reader.QueryRow(`SELECT bytes FROM contents WHERE hash = ?`, hash).Scan(&got); err != nil {
		t.Fatalf("read the committed content from another connection: %v", err)
	}
	if string(got) != string(bytes) {
		t.Errorf("content is %v, want %v", got, bytes)
	}
	for _, check := range []struct {
		label string
		query string
	}{
		{"content", `SELECT COUNT(*) FROM contents WHERE hash = '` + hash + `'`},
		{"node", `SELECT COUNT(*) FROM nodes WHERE id = 'node-ok'`},
		{"event", `SELECT COUNT(*) FROM events WHERE id = 'ev-ok'`},
		{"command index", `SELECT COUNT(*) FROM command_index WHERE event_id = 'ev-ok'`},
		{"path index", `SELECT COUNT(*) FROM path_index WHERE event_id = 'ev-ok'`},
		{"search projection", `SELECT COUNT(*) FROM search_docs WHERE event_id = 'ev-ok'`},
	} {
		count := queryInt(t, reader, check.query)
		receipt.add(check.label+" rows", count)
		if count != 1 {
			t.Errorf("%s rows after the accepted commit: %d, want 1", check.label, count)
		}
	}
	if err := accepted.LastError(); err != nil {
		t.Fatalf("the accepted commit reported an error: %v", err)
	}

	// The refused case: the second event violates the per-session sequence
	// uniqueness, so the whole change set must roll back.
	rollbackBytes := []byte("must not survive")
	rollbackHash := ContentHash(rollbackBytes)
	revision = directoryRevision(t, reader, "dir")
	refused := NewWriterQueue(db, 4)
	err = refused.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := CommitChangeSet(ctx, tx, ChangeSet{
			NamespaceID: "ns-atomic",
			Contents:    []Content{{Hash: rollbackHash, Bytes: rollbackBytes}},
			Changes: []StagedChange{{
				NodeID: "node-lost", ParentID: "dir", Name: "lost.txt",
				Kind: "file", ContentHash: rollbackHash,
				ExpectAbsent: true, ExpectedParentRevision: revision,
			}},
			Events: []AcceptedEvent{
				{ID: "ev-lost-a", SessionID: "session-2", Sequence: 1, Kind: "accepted",
					OccurredAt: fixedTimestamp(1), Command: "write lost.txt", Path: "/dir/lost.txt"},
				// The same session and sequence again: the constraint rejects this
				// row, and with it everything the transaction had already done.
				{ID: "ev-lost-b", SessionID: "session-2", Sequence: 1, Kind: "accepted",
					OccurredAt: fixedTimestamp(2), Command: "write lost.txt", Path: "/dir/lost.txt"},
			},
		}); err != nil {
			return err
		}
		return IndexEventForSearch(ctx, tx, "ev-lost-a", "write lost.txt", "/dir/lost.txt")
	})
	if err != nil {
		t.Fatalf("submit the failing commit: %v", err)
	}
	refused.Close()

	receipt.add("failing commit recorded", describeErr(refused.LastError()))
	for _, check := range []struct {
		label string
		query string
	}{
		{"content after rollback", `SELECT COUNT(*) FROM contents WHERE hash = '` + rollbackHash + `'`},
		{"node after rollback", `SELECT COUNT(*) FROM nodes WHERE id = 'node-lost'`},
		{"event after rollback", `SELECT COUNT(*) FROM events WHERE id LIKE 'ev-lost%'`},
		{"search projection after rollback", `SELECT COUNT(*) FROM search_docs WHERE event_id LIKE 'ev-lost%'`},
	} {
		count := queryInt(t, reader, check.query)
		receipt.add(check.label, count)
		if count != 0 {
			t.Errorf("%s: %d, want 0", check.label, count)
		}
	}
	after := directoryRevision(t, reader, "dir")
	receipt.add("directory revision before", revision)
	receipt.add("directory revision after", after)
	if after != revision {
		t.Errorf("the directory revision moved to %d after a rollback, want %d", after, revision)
	}
	if refused.LastError() == nil {
		t.Error("the failing commit reported no error")
	}
	receipt.addAll("integrity", integrityReport(t, reader))
	receipt.write(t, "gate5-atomic-commit")
}

// TestConflictReasonsAreDistinguishable checks that a caller can tell a stale
// revision from a stale listing from a broken parent, because each needs a
// different response.
func TestConflictReasonsAreDistinguishable(t *testing.T) {
	db, _ := newSpikeDB(t, "conflict-reasons")
	seedWorld(t, db, "ns-unknown", "root-unknown")
	insertDirectory(t, db, "dir", "root-unknown", "dir")
	seedNodes(t, db, "dir", 1)

	receipt := newReport("Gate 5e: distinguishable conflict reasons")
	revision := directoryRevision(t, db, "dir")

	for _, tc := range []struct {
		name   string
		change StagedChange
		reason string
	}{
		{
			name: "revision from the future",
			change: StagedChange{
				NodeID: "seed-0000", ParentID: "dir", Name: "seed-0000",
				Kind: "file", ExpectedRevision: revision + 99, ExpectedParentRevision: revision,
			},
			reason: "revision",
		},
		{
			name: "node that does not exist",
			change: StagedChange{
				NodeID: "absent", ParentID: "dir", Name: "absent.txt",
				Kind: "file", ExpectedRevision: 1, ExpectedParentRevision: revision,
			},
			reason: "revision",
		},
		{
			name: "parent directory without a revision",
			change: StagedChange{
				NodeID: "orphan", ParentID: "no-such-dir", Name: "orphan.txt",
				Kind: "file", ExpectAbsent: true, ExpectedParentRevision: 1,
			},
			reason: "directory listing",
		},
	} {
		err := runCommit(t, db, ChangeSet{
			NamespaceID: "ns-unknown",
			Changes:     []StagedChange{tc.change},
		})
		if !errors.Is(err, ErrConflict) {
			t.Errorf("%s returned %v, want a conflict", tc.name, err)
			continue
		}
		var conflictErr *ConflictError
		if !errors.As(err, &conflictErr) {
			t.Errorf("%s did not yield a ConflictError", tc.name)
			continue
		}
		if conflictErr.Reason != tc.reason {
			t.Errorf("%s reported reason %q, want %q", tc.name, conflictErr.Reason, tc.reason)
		}
		receipt.add(tc.name, conflictErr.Error())
	}
	receipt.write(t, "gate5-conflict-reasons")
}

// TestCommitRejectsMismatchedContent protects the content-addressed invariant at
// the boundary: a blob whose bytes do not hash to its key is a caller bug, not a
// world change.
func TestCommitRejectsMismatchedContent(t *testing.T) {
	db, _ := newSpikeDB(t, "bad-content")
	seedWorld(t, db, "ns-bad", "root-bad")

	err := runCommit(t, db, ChangeSet{
		NamespaceID: "ns-bad",
		Contents:    []Content{{Hash: ContentHash([]byte("one")), Bytes: []byte("two")}},
	})
	if err == nil {
		t.Fatal("content whose bytes do not match its hash was accepted")
	}
	if errors.Is(err, ErrConflict) {
		t.Errorf("mismatched content was reported as a conflict: %v", err)
	}
	if count := queryInt(t, db, `SELECT COUNT(*) FROM contents`); count != 0 {
		t.Errorf("%d content rows after a rejected commit, want 0", count)
	}
	Receipt(t, "gate5-content-hash", fmt.Sprintf("rejected: %v", err))
}

// runCommit applies a change set in its own transaction on db, which is how a
// single session commits outside the writer queue.
func runCommit(t *testing.T, db *sql.DB, set ChangeSet) error {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	if err := CommitChangeSet(context.Background(), tx, set); err != nil {
		return err
	}
	return tx.Commit()
}

// directoryRevision reads a directory's membership revision from db.
func directoryRevision(t *testing.T, db *sql.DB, directoryID string) int64 {
	t.Helper()
	revision, err := ReadDirectoryRevision(context.Background(), db, directoryID)
	if err != nil {
		t.Fatalf("read directory revision: %v", err)
	}
	return revision
}

// counter counts results from the concurrent parts of the protocol.
type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) add(by int) {
	c.mu.Lock()
	c.n += by
	c.mu.Unlock()
}

func (c *counter) value() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}
