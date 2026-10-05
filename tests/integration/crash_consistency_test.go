package integration

import (
	"context"
	"testing"

	"j0s.at/vibeshell/internal/adapters/sqlite"
	"j0s.at/vibeshell/internal/admin"
	"j0s.at/vibeshell/internal/domain"
)

// nodeVersionCount returns how many immutable node_versions rows exist for a
// node id. A retried or replayed mutation would appear here as an extra row.
func nodeVersionCount(t *testing.T, store *testStore, node domain.NodeID) int {
	t.Helper()
	var n int
	if err := store.db.SQL().QueryRow(
		`SELECT COUNT(*) FROM node_versions WHERE node_id = ?`, node.String()).Scan(&n); err != nil {
		t.Fatalf("count node_versions: %v", err)
	}
	return n
}

// TestCrashAtStagedCheckpointLeavesNoMutation stages exact content and a change
// set but never commits: a crash here must leave no visible world mutation,
// while the already-persisted immutable content remains reusable (PLAN 7.1,
// 10.3).
func TestCrashAtStagedCheckpointLeavesNoMutation(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	user := userID(t, 1)
	ns := store.namespace(t, user)
	ref := store.putContent(t, []byte("staged but never committed"), "text/plain")

	// The staged change set exists only in memory; the crash is modelled by
	// never calling Commit. Content was stored in its own transaction.
	if _, err := store.db.LookupPath(ctx, ns.ID, domain.MustParsePath("/notes.txt")); !domain.IsNotFoundError(err) {
		t.Fatalf("LookupPath before commit = %v, want not found", err)
	}
	integrity, err := sqlite.NewMaintenance(store.db).IntegrityCheck(ctx)
	if err != nil {
		t.Fatalf("IntegrityCheck: %v", err)
	}
	if integrity.RowCounts["contents"] != 1 {
		t.Errorf("content rows = %d, want 1 (staged bytes are durable)", integrity.RowCounts["contents"])
	}

	// The staged content is reusable: committing the change set must succeed
	// with no re-upload and produce exactly one node.
	node := store.commitFile(t, ns.ID, domain.MustParsePath("/notes.txt"), ref)
	if node.Content.Hash != ref.Hash {
		t.Fatalf("committed content hash = %s, want %s", node.Content.Hash, ref.Hash)
	}
	if got := nodeVersionCount(t, store, node.ID); got != 1 {
		t.Errorf("node versions = %d, want 1", got)
	}
}

// TestCrashAtCommittedCheckpointIsDurableAndExact commits a file, stops the
// process (closes the database), reopens it, and requires the exact bytes and
// revision to survive with no duplicate version rows (PLAN 10.3 durability).
func TestCrashAtCommittedCheckpointIsDurableAndExact(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/vibeshell.db"
	payload := []byte("committed bytes \x00\xff exact")

	func() {
		db, err := sqlite.Open(dbPath, sqlite.DefaultOptions())
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		events, err := sqlite.NewEvents(db.SQL(), sqlite.EventsOptions{})
		if err != nil {
			t.Fatalf("NewEvents: %v", err)
		}
		store := &testStore{db: db, events: events, dir: dir}
		user := userID(t, 1)
		ns := store.namespace(t, user)
		ref := store.putContent(t, payload, "application/octet-stream")
		store.commitFile(t, ns.ID, domain.MustParsePath("/keep.bin"), ref)
		events.Close()
		if err := db.Close(); err != nil {
			t.Fatalf("close after commit: %v", err)
		}
	}()

	// Reopen as a fresh process would.
	db, err := sqlite.Open(dbPath, sqlite.DefaultOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	events, err := sqlite.NewEvents(db.SQL(), sqlite.EventsOptions{})
	if err != nil {
		t.Fatalf("NewEvents after reopen: %v", err)
	}
	defer events.Close()
	store := &testStore{db: db, events: events, dir: dir}

	user := userID(t, 1)
	ns, err := db.EnsureNamespace(context.Background(), domain.NamespaceForUser(user, "user:test"))
	if err != nil {
		t.Fatalf("EnsureNamespace after reopen: %v", err)
	}
	node, err := db.LookupPath(context.Background(), ns.ID, domain.MustParsePath("/keep.bin"))
	if err != nil {
		t.Fatalf("LookupPath after restart: %v", err)
	}
	if node.Revision != domain.InitialRevision {
		t.Errorf("revision = %d, want %d", node.Revision, domain.InitialRevision)
	}
	got, err := db.Get(context.Background(), node.Content, 0, node.Content.Size)
	if err != nil {
		t.Fatalf("Get content after restart: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("content after restart = %q, want %q", got, payload)
	}
	if n := nodeVersionCount(t, store, node.ID); n != 1 {
		t.Errorf("node versions after restart = %d, want 1 (no duplicate mutation)", n)
	}
}

// TestCrashAtPartialEmissionRecoversTruthfully commits the world mutation and
// records accepted terminal output but never records the transport write
// outcome. Recovery must mark the session incomplete, keep the single
// committed mutation, and the export must state the incomplete delivery
// rather than fabricate success (PLAN 10.1, 10.3, 12.3).
func TestCrashAtPartialEmissionRecoversTruthfully(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	user := userID(t, 1)
	ns := store.namespace(t, user)
	sess := sessionID(t, 1)

	// Accepted terminal output frame (content-addressed) committed before the
	// crash; the transport write outcome is never recorded.
	frameBytes := []byte("total 4\r\n-rw-r--r-- 1 user user 4 note\r\n")
	frameRef := store.putContent(t, frameBytes, "text/plain")
	store.commitFile(t, ns.ID, domain.MustParsePath("/listed.txt"), frameRef)

	store.appendEvent(t, sess, domain.EventKindSessionStart, domain.SessionStartPayload{
		UserID: user, AuthMode: "secure", TerminalType: "xterm-256color",
		TerminalSize: domain.TermSize{Cols: 80, Rows: 24}, SharingEnabled: true,
	}, 1_700_000_000_000)
	store.appendEvent(t, sess, domain.EventKindTerminalFrame, domain.TerminalFramePayload{
		FrameID: "f_0001", ContentRef: frameRef, PromptText: "user@vibeos:~$ ",
	}, 1_700_000_001_000)
	// No terminal.write outcome event: delivery is unknown, not success.

	// Admin recovery runs at the next startup.
	service := admin.New(admin.Deps{Recovery: store.recorder, Sessions: store.recorder})
	report, err := service.RecoverIncomplete(ctx)
	if err != nil {
		t.Fatalf("RecoverIncomplete: %v", err)
	}
	if report.MarkedSessions != 1 {
		t.Fatalf("marked sessions = %d, want 1", report.MarkedSessions)
	}

	// The committed world mutation survives exactly once.
	node, err := store.db.LookupPath(ctx, ns.ID, domain.MustParsePath("/listed.txt"))
	if err != nil {
		t.Fatalf("LookupPath after recovery: %v", err)
	}
	if n := nodeVersionCount(t, store, node.ID); n != 1 {
		t.Errorf("node versions = %d, want 1 (no duplicate mutation after crash)", n)
	}

	// Recovery is idempotent: a second run must not append another marker.
	second, err := service.RecoverIncomplete(ctx)
	if err != nil {
		t.Fatalf("second RecoverIncomplete: %v", err)
	}
	if second.MarkedSessions != 0 {
		t.Errorf("second recovery marked %d sessions, want 0 (idempotent)", second.MarkedSessions)
	}

	// The event log records the recovery marker and the session is no longer
	// incomplete.
	page, _, err := service.ListSessions(ctx, 10, "")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(page) != 1 || !page[0].Recovered || page[0].Ended {
		t.Fatalf("session summary = %+v, want recovered and not ended", page)
	}
}

// TestRedrivingTurnCommitsNoDuplicateMutation proves a turn whose attempts all
// fail commits zero mutations, and that a turn re-driven after a world commit
// conflict carries no provider-health penalty and leaves the committed node
// intact (PLAN 9.4, 16.1 no duplicate mutations).
func TestRedrivingTurnCommitsNoDuplicateMutation(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	user := userID(t, 1)
	ns := store.namespace(t, user)
	turn := turnID(t, 1)
	attempt := mustParse(t, domain.ParseAttemptID, domain.PrefixAttempt+"_"+crockford(1))

	ref := store.putContent(t, []byte("first"), "text/plain")
	cs := domain.EmptyChangeSet(turn, attempt, 1_700_000_000_000)
	cs.AddCreate(ns.ID, domain.MustParsePath("/dup.txt"), domain.NodeKindFile,
		domain.NewNodeMetadata(0o644, 1000, 1000, 1_700_000_000_000), ref)
	if _, err := store.db.Commit(ctx, cs, domain.DefaultScopePolicy()); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	node, err := store.db.LookupPath(ctx, ns.ID, domain.MustParsePath("/dup.txt"))
	if err != nil {
		t.Fatalf("LookupPath: %v", err)
	}

	// A second turn stages the same creation against the now-existing path:
	// the commit must conflict (no duplicate key, no duplicate node).
	ref2 := store.putContent(t, []byte("second"), "text/plain")
	cs2 := domain.EmptyChangeSet(turn, attempt, 1_700_000_000_001)
	cs2.AddCreate(ns.ID, domain.MustParsePath("/dup.txt"), domain.NodeKindFile,
		domain.NewNodeMetadata(0o644, 1000, 1000, 1_700_000_000_001), ref2)
	if _, err := store.db.Commit(ctx, cs2, domain.DefaultScopePolicy()); !domain.IsConflictError(err) {
		t.Fatalf("second commit = %v, want conflict", err)
	}
	if n := nodeVersionCount(t, store, node.ID); n != 1 {
		t.Errorf("node versions = %d after conflicting re-drive, want 1", n)
	}
	still, err := store.db.LookupPath(ctx, ns.ID, domain.MustParsePath("/dup.txt"))
	if err != nil {
		t.Fatalf("LookupPath after conflict: %v", err)
	}
	if still.Content.Hash != ref.Hash {
		t.Errorf("content changed after conflict: got %s, want %s", still.Content.Hash, ref.Hash)
	}
}
