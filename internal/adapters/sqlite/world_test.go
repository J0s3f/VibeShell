package sqlite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// testDB opens a file-backed database (WAL-capable, restartable) in a fresh
// temporary directory.
func testDB(t *testing.T, opts Options) *DB {
	t.Helper()
	if opts.MaxContentBytes == 0 && opts.BusyTimeoutMs == 0 {
		opts = DefaultOptions()
	}
	db, err := Open(t.TempDir()+"/world.db", opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return db
}

func testUserID(t *testing.T) domain.UserID {
	t.Helper()
	raw, err := newTestPrefixID(domain.PrefixUser)
	if err != nil {
		t.Fatal(err)
	}
	id, err := domain.ParseUserID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func testChangeIDs(t *testing.T) (domain.TurnID, domain.AttemptID) {
	t.Helper()
	rawTurn, err := newTestPrefixID(domain.PrefixTurn)
	if err != nil {
		t.Fatal(err)
	}
	rawAttempt, err := newTestPrefixID(domain.PrefixAttempt)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := domain.ParseTurnID(rawTurn)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := domain.ParseAttemptID(rawAttempt)
	if err != nil {
		t.Fatal(err)
	}
	return turn, attempt
}

func mustNamespaces(t *testing.T, ctx context.Context, db *DB, user domain.UserID) (baseline, shared, personal domain.Namespace) {
	t.Helper()
	var err error
	baseline, err = db.EnsureNamespace(ctx, domain.Namespace{Scope: domain.ScopeBaseline, Label: "baseline:v1"})
	if err != nil {
		t.Fatalf("baseline namespace: %v", err)
	}
	shared, err = db.EnsureNamespace(ctx, domain.Namespace{Scope: domain.ScopeShared, Label: "shared"})
	if err != nil {
		t.Fatalf("shared namespace: %v", err)
	}
	personal, err = db.EnsureNamespace(ctx, domain.Namespace{Owner: user, Scope: domain.ScopeUser, Label: "user:test"})
	if err != nil {
		t.Fatalf("user namespace: %v", err)
	}
	// Idempotent: the same scope+owner+label resolves to the same ID.
	again, err := db.EnsureNamespace(ctx, domain.Namespace{Owner: user, Scope: domain.ScopeUser, Label: "user:test"})
	if err != nil {
		t.Fatalf("re-ensure namespace: %v", err)
	}
	if again.ID != personal.ID {
		t.Fatalf("namespace not idempotent: %v vs %v", again.ID, personal.ID)
	}
	return baseline, shared, personal
}

func putFile(t *testing.T, ctx context.Context, db *DB, data []byte) domain.ContentRef {
	t.Helper()
	ref, err := db.Put(ctx, data, "application/octet-stream")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return ref
}

// commitCreates stages parent directories (parents before children in one
// atomic ChangeSet) and creates one file.
func commitCreates(t *testing.T, ctx context.Context, db *DB, ns domain.Namespace, dirs []string, filePath string, ref domain.ContentRef) domain.Revision {
	t.Helper()
	turn, attempt := testChangeIDs(t)
	cs := domain.EmptyChangeSet(turn, attempt, 1)
	now := int64(1798732800000)
	for _, d := range dirs {
		cs.AddCreate(ns.ID, domain.MustParsePath(d), domain.NodeKindDir, domain.NewNodeMetadata(0o755, 0, 0, now), domain.EmptyContentRef())
	}
	cs.AddCreate(ns.ID, domain.MustParsePath(filePath), domain.NodeKindFile, domain.NewNodeMetadata(0o644, 1000, 1000, now), ref)
	rev, err := db.Commit(ctx, cs, domain.DefaultScopePolicy())
	if err != nil {
		t.Fatalf("Commit creates: %v", err)
	}
	return rev
}

// provisionCreates is the first-install counterpart of commitCreates for the
// immutable baseline seed, which Commit refuses through the turn path.
func provisionCreates(t *testing.T, ctx context.Context, db *DB, ns domain.Namespace, dirs []string, filePath string, ref domain.ContentRef) domain.Revision {
	t.Helper()
	turn, attempt := testChangeIDs(t)
	cs := domain.EmptyChangeSet(turn, attempt, 1)
	now := int64(1798732800000)
	for _, d := range dirs {
		cs.AddCreate(ns.ID, domain.MustParsePath(d), domain.NodeKindDir, domain.NewNodeMetadata(0o755, 0, 0, now), domain.EmptyContentRef())
	}
	cs.AddCreate(ns.ID, domain.MustParsePath(filePath), domain.NodeKindFile, domain.NewNodeMetadata(0o644, 1000, 1000, now), ref)
	rev, err := db.Provision(ctx, cs)
	if err != nil {
		t.Fatalf("Provision creates: %v", err)
	}
	return rev
}

func TestByteExactRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})
	user := testUserID(t)
	_, _, personal := mustNamespaces(t, ctx, db, user)

	payloads := map[string][]byte{
		"empty":       {},
		"text":        []byte("hello vibeshell\n"),
		"nul":         {0x00, 0x01, 0x02, 0x00, 'a', 0x00},
		"invalid-utf": {0xff, 0xfe, 0x00, 0x80, 0x41, 0xfd},
	}
	big := make([]byte, 1<<16)
	for i := range big {
		big[i] = byte((i*31 + 7) % 251)
	}
	big[1000] = 0x00
	payloads["binary-64k"] = big

	// One shared parent for every subtest file: creating it per subtest
	// would collide on the second iteration.
	turn0, attempt0 := testChangeIDs(t)
	cs0 := domain.EmptyChangeSet(turn0, attempt0, 1)
	cs0.AddCreate(personal.ID, domain.MustParsePath("/roundtrip"), domain.NodeKindDir,
		domain.NewNodeMetadata(0o755, 0, 0, 1), domain.EmptyContentRef())
	if _, err := db.Commit(ctx, cs0, domain.DefaultScopePolicy()); err != nil {
		t.Fatalf("seed /roundtrip: %v", err)
	}

	for name, want := range payloads {
		t.Run(name, func(t *testing.T) {
			ref := putFile(t, ctx, db, want)
			if int64(len(want)) != ref.Size {
				t.Fatalf("ref size = %d, want %d", ref.Size, len(want))
			}
			if len(want) == 0 && !ref.Hash.IsZero() {
				t.Fatalf("empty content must use the zero hash, got %v", ref.Hash)
			}
			path := "/roundtrip/" + name + ".bin"
			commitCreates(t, ctx, db, personal, nil, path, ref)

			node, err := db.LookupPath(ctx, personal.ID, domain.MustParsePath(path))
			if err != nil {
				t.Fatalf("LookupPath: %v", err)
			}
			if node.Content.Size != int64(len(want)) {
				t.Fatalf("node content size = %d, want %d", node.Content.Size, len(want))
			}
			got, err := db.Get(ctx, node.Content, 0, -1)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(got), len(want))
			}
			// A bounded range returns the exact slice.
			if len(want) > 4 {
				part, err := db.Get(ctx, node.Content, 1, 3)
				if err != nil {
					t.Fatalf("Get range: %v", err)
				}
				if !bytes.Equal(part, want[1:4]) {
					t.Fatalf("range mismatch: got %q want %q", part, want[1:4])
				}
			}
			byID, err := db.GetNode(ctx, personal.ID, node.ID)
			if err != nil {
				t.Fatalf("GetNode: %v", err)
			}
			if byID.Revision != node.Revision || byID.Content != node.Content {
				t.Fatalf("GetNode disagrees with LookupPath: %+v vs %+v", byID, node)
			}
		})
	}
}

func TestOverlayAndTombstone(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})
	user := testUserID(t)
	baseline, shared, personal := mustNamespaces(t, ctx, db, user)
	policy := domain.DefaultScopePolicy()

	baseRef := putFile(t, ctx, db, []byte("base motd\n"))
	provisionCreates(t, ctx, db, baseline, []string{"/etc"}, "/etc/motd", baseRef)

	// Baseline is visible from the user view before anything shadows it.
	node, err := db.LookupPath(ctx, personal.ID, domain.MustParsePath("/etc/motd"))
	if err != nil {
		t.Fatalf("baseline visible via user view: %v", err)
	}
	got, _ := db.Get(ctx, node.Content, 0, -1)
	if string(got) != "base motd\n" {
		t.Fatalf("baseline bytes = %q", got)
	}

	// Shared shadows baseline; user sees the shared version.
	sharedRef := putFile(t, ctx, db, []byte("shared motd\n"))
	commitCreates(t, ctx, db, shared, []string{"/etc"}, "/etc/motd", sharedRef)
	node, err = db.LookupPath(ctx, personal.ID, domain.MustParsePath("/etc/motd"))
	if err != nil {
		t.Fatalf("shared overlay: %v", err)
	}
	if node.NamespaceID != shared.ID {
		t.Fatalf("overlay winner namespace = %v, want shared %v", node.NamespaceID, shared.ID)
	}

	// A user tombstone hides every lower scope without deleting their rows.
	turn, attempt := testChangeIDs(t)
	cs := domain.EmptyChangeSet(turn, attempt, 2)
	cs.AddTombstone(shared.ID, domain.MustParsePath("/etc/motd"), node.ID, node.Revision)
	// Wrong namespace on purpose would hide in the wrong view; target the
	// user namespace instead: re-resolve what the user sees (shared node) and
	// tombstone it from the personal namespace.
	cs.Mutations[0].NamespaceID = personal.ID
	if _, err := db.Commit(ctx, cs, policy); err != nil {
		t.Fatalf("tombstone commit: %v", err)
	}
	if _, err := db.LookupPath(ctx, personal.ID, domain.MustParsePath("/etc/motd")); !errors.Is(err, domain.CategoryNotFound) {
		t.Fatalf("tombstoned path lookup = %v, want not_found", err)
	}
	// Lower scopes are untouched: shared still serves the file.
	if _, err := db.LookupPath(ctx, shared.ID, domain.MustParsePath("/etc/motd")); err != nil {
		t.Fatalf("shared row survived tombstone: %v", err)
	}

	// Creating in the user namespace shadows the tombstone with live bytes.
	userRef := putFile(t, ctx, db, []byte("user motd\n"))
	turn2, attempt2 := testChangeIDs(t)
	cs2 := domain.EmptyChangeSet(turn2, attempt2, 3)
	cs2.AddCreate(personal.ID, domain.MustParsePath("/etc/motd"), domain.NodeKindFile,
		domain.NewNodeMetadata(0o644, 1000, 1000, 3), userRef)
	if _, err := db.Commit(ctx, cs2, policy); err != nil {
		t.Fatalf("user shadow create: %v", err)
	}
	node, err = db.LookupPath(ctx, personal.ID, domain.MustParsePath("/etc/motd"))
	if err != nil {
		t.Fatalf("user shadow lookup: %v", err)
	}
	got, _ = db.Get(ctx, node.Content, 0, -1)
	if string(got) != "user motd\n" {
		t.Fatalf("user bytes = %q", got)
	}

	// Deleting the user copy reveals the shared file again (overlay reveal).
	turn3, attempt3 := testChangeIDs(t)
	cs3 := domain.EmptyChangeSet(turn3, attempt3, 4)
	cs3.AddDelete(personal.ID, domain.MustParsePath("/etc/motd"), node.ID, node.Revision)
	if _, err := db.Commit(ctx, cs3, policy); err != nil {
		t.Fatalf("delete user copy: %v", err)
	}
	node, err = db.LookupPath(ctx, personal.ID, domain.MustParsePath("/etc/motd"))
	if err != nil {
		t.Fatalf("reveal after delete: %v", err)
	}
	got, _ = db.Get(ctx, node.Content, 0, -1)
	if string(got) != "shared motd\n" {
		t.Fatalf("revealed bytes = %q, want shared version", got)
	}

	// Private files never leak into the shared view.
	secretRef := putFile(t, ctx, db, []byte("private\n"))
	commitCreates(t, ctx, db, personal, []string{"/home"}, "/home/secret.txt", secretRef)
	if _, err := db.LookupPath(ctx, shared.ID, domain.MustParsePath("/home/secret.txt")); !errors.Is(err, domain.CategoryNotFound) {
		t.Fatalf("user file visible from shared view: %v", err)
	}
}

func TestTwoWriterConflictOneWinner(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})
	user := testUserID(t)
	_, shared, _ := mustNamespaces(t, ctx, db, user)
	policy := domain.DefaultScopePolicy()

	v1 := putFile(t, ctx, db, []byte("v1\n"))
	commitCreates(t, ctx, db, shared, []string{"/doc"}, "/doc/note.txt", v1)

	// Both writers snapshot revision 1.
	stale, err := db.LookupPath(ctx, shared.ID, domain.MustParsePath("/doc/note.txt"))
	if err != nil {
		t.Fatal(err)
	}

	// Writer B wins first.
	v2 := putFile(t, ctx, db, []byte("v2 from B\n"))
	turnB, attemptB := testChangeIDs(t)
	csB := domain.EmptyChangeSet(turnB, attemptB, 10)
	csB.AddUpdate(shared.ID, domain.MustParsePath("/doc/note.txt"), stale.ID, stale.Revision,
		domain.NewNodeMetadata(0o644, 1000, 1000, 10), v2)
	if _, err := db.Commit(ctx, csB, policy); err != nil {
		t.Fatalf("writer B commit: %v", err)
	}

	// Writer A replays its staged write against the stale revision: conflict,
	// never a silent overwrite.
	v3 := putFile(t, ctx, db, []byte("v3 from A\n"))
	turnA, attemptA := testChangeIDs(t)
	csA := domain.EmptyChangeSet(turnA, attemptA, 11)
	csA.AddUpdate(shared.ID, domain.MustParsePath("/doc/note.txt"), stale.ID, stale.Revision,
		domain.NewNodeMetadata(0o644, 1000, 1000, 11), v3)
	_, err = db.Commit(ctx, csA, policy)
	if !errors.Is(err, domain.CategoryConflict) {
		t.Fatalf("stale write err = %v, want errors.Is conflict", err)
	}

	// B's bytes are intact; A rebases onto the fresh revision and succeeds.
	current, err := db.LookupPath(ctx, shared.ID, domain.MustParsePath("/doc/note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision != stale.Revision+1 {
		t.Fatalf("revision = %d, want %d", current.Revision, stale.Revision+1)
	}
	got, _ := db.Get(ctx, current.Content, 0, -1)
	if string(got) != "v2 from B\n" {
		t.Fatalf("winner bytes = %q", got)
	}
	turnA2, attemptA2 := testChangeIDs(t)
	csA2 := domain.EmptyChangeSet(turnA2, attemptA2, 12)
	csA2.AddUpdate(shared.ID, domain.MustParsePath("/doc/note.txt"), current.ID, current.Revision,
		domain.NewNodeMetadata(0o644, 1000, 1000, 12), v3)
	if _, err := db.Commit(ctx, csA2, policy); err != nil {
		t.Fatalf("rebased commit: %v", err)
	}
}

func TestReadDependenciesEnforced(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})
	user := testUserID(t)
	_, shared, _ := mustNamespaces(t, ctx, db, user)
	policy := domain.DefaultScopePolicy()

	ref := putFile(t, ctx, db, []byte("x\n"))
	commitCreates(t, ctx, db, shared, []string{"/dep"}, "/dep/f.txt", ref)
	node, err := db.LookupPath(ctx, shared.ID, domain.MustParsePath("/dep/f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	parent, err := db.LookupPath(ctx, shared.ID, domain.MustParsePath("/dep"))
	if err != nil {
		t.Fatal(err)
	}

	// Stale revision dependency fails.
	turn, attempt := testChangeIDs(t)
	cs := domain.EmptyChangeSet(turn, attempt, 20)
	cs.AddReadDep(node.ID, node.Revision+5, node.Kind, false, parent.ID)
	if _, err := db.Commit(ctx, cs, policy); !errors.Is(err, domain.CategoryConflict) {
		t.Fatalf("stale read dep err = %v, want conflict", err)
	}

	// Wrong directory membership fails: the node is not a child of itself.
	turn2, attempt2 := testChangeIDs(t)
	cs2 := domain.EmptyChangeSet(turn2, attempt2, 21)
	cs2.AddReadDep(node.ID, node.Revision, node.Kind, false, node.ID)
	if _, err := db.Commit(ctx, cs2, policy); !errors.Is(err, domain.CategoryConflict) {
		t.Fatalf("membership dep err = %v, want conflict", err)
	}

	// A correct presence dependency (with membership) commits cleanly.
	turn3, attempt3 := testChangeIDs(t)
	cs3 := domain.EmptyChangeSet(turn3, attempt3, 22)
	cs3.AddReadDep(node.ID, node.Revision, node.Kind, false, parent.ID)
	if _, err := db.Commit(ctx, cs3, policy); err != nil {
		t.Fatalf("valid read dep: %v", err)
	}

	// Absence violated: the checked node has since appeared.
	ghost, err := newNodeID()
	if err != nil {
		t.Fatal(err)
	}
	ghostRef := putFile(t, ctx, db, []byte("ghost\n"))
	turn4, attempt4 := testChangeIDs(t)
	cs4 := domain.EmptyChangeSet(turn4, attempt4, 23)
	cs4.Mutations = append(cs4.Mutations, domain.Mutation{
		Type: domain.MutationCreate, NamespaceID: shared.ID,
		Path: domain.MustParsePath("/dep/ghost.txt"), Kind: domain.NodeKindFile,
		Metadata: domain.NewNodeMetadata(0o644, 1000, 1000, 23), Content: ghostRef,
	})
	_ = ghost
	created, err := db.Commit(ctx, cs4, policy)
	if err != nil {
		t.Fatalf("ghost create: %v", err)
	}
	_ = created
	ghostNode, err := db.LookupPath(ctx, shared.ID, domain.MustParsePath("/dep/ghost.txt"))
	if err != nil {
		t.Fatal(err)
	}
	turn5, attempt5 := testChangeIDs(t)
	cs5 := domain.EmptyChangeSet(turn5, attempt5, 24)
	cs5.AddReadDep(ghostNode.ID, 1, ghostNode.Kind, true, parent.ID)
	if _, err := db.Commit(ctx, cs5, policy); !errors.Is(err, domain.CategoryConflict) {
		t.Fatalf("violated absence dep err = %v, want conflict", err)
	}
}

func TestParallelSessions(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})
	user := testUserID(t)
	_, _, personal := mustNamespaces(t, ctx, db, user)
	policy := domain.DefaultScopePolicy()

	seedRef, err := db.Put(ctx, []byte("seed\n"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	commitCreates(t, ctx, db, personal, []string{"/parallel"}, "/parallel/seed.txt", seedRef)

	const writers = 16
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			path := fmt.Sprintf("/parallel/w%02d.txt", w)
			data := []byte(fmt.Sprintf("writer %d\n", w))
			ref, err := db.Put(ctx, data, "text/plain")
			if err != nil {
				errs <- err
				return
			}
			turn, attempt := testChangeIDs(t)
			cs := domain.EmptyChangeSet(turn, attempt, int64(100+w))
			cs.AddCreate(personal.ID, domain.MustParsePath(path), domain.NodeKindFile,
				domain.NewNodeMetadata(0o644, 1000, 1000, int64(100+w)), ref)
			if _, err := db.Commit(ctx, cs, policy); err != nil {
				errs <- fmt.Errorf("writer %d: %w", w, err)
				return
			}
			node, err := db.LookupPath(ctx, personal.ID, domain.MustParsePath(path))
			if err != nil {
				errs <- err
				return
			}
			got, err := db.Get(ctx, node.Content, 0, -1)
			if err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(got, data) {
				errs <- fmt.Errorf("writer %d bytes = %q", w, got)
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("parallel session: %v", err)
	}

	// Every writer's file plus the seed lists exactly once.
	root, err := db.LookupPath(ctx, personal.ID, domain.MustParsePath("/parallel"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	cursor := ""
	for {
		page, next, err := db.ListDirectory(ctx, personal.ID, root.ID, 5, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range page {
			names = append(names, n.Name)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(names) != writers+1 {
		t.Fatalf("listed %d children, want %d: %v", len(names), writers+1, names)
	}
}

func TestRestartPersistence(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/world.db"
	db, err := Open(path, DefaultOptions())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	user := testUserID(t)
	_, shared, personal := mustNamespaces(t, ctx, db, user)

	want := []byte{0x00, 'p', 'e', 'r', 's', 'i', 's', 't', 0xff, 0x00}
	ref, err := db.Put(ctx, want, "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	rev1 := commitCreates(t, ctx, db, personal, []string{"/keep"}, "/keep/data.bin", ref)
	sharedRef, err := db.Put(ctx, []byte("shared\n"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	rev2 := commitCreates(t, ctx, db, shared, []string{"/s"}, "/s/x.txt", sharedRef)
	if rev2 <= rev1 {
		t.Fatalf("commit revisions not monotonic across namespaces: %d then %d", rev1, rev2)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen the same file: namespaces, nodes, exact bytes, and the commit
	// sequence all survive the restart.
	db2, err := Open(path, DefaultOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	node, err := db2.LookupPath(ctx, personal.ID, domain.MustParsePath("/keep/data.bin"))
	if err != nil {
		t.Fatalf("lookup after restart: %v", err)
	}
	got, err := db2.Get(ctx, node.Content, 0, -1)
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("bytes after restart = %v, want %v", got, want)
	}
	if _, err := db2.LookupPath(ctx, personal.ID, domain.MustParsePath("/s/x.txt")); err != nil {
		t.Fatalf("shared overlay after restart: %v", err)
	}
	again, err := db2.EnsureNamespace(ctx, domain.Namespace{Owner: user, Scope: domain.ScopeUser, Label: "user:test"})
	if err != nil {
		t.Fatalf("re-ensure after restart: %v", err)
	}
	if again.ID != personal.ID {
		t.Fatalf("namespace id changed across restart: %v vs %v", again.ID, personal.ID)
	}
	rev3 := commitCreates(t, ctx, db2, personal, nil, "/keep/more.txt", sharedRef)
	if rev3 <= rev2 {
		t.Fatalf("commit sequence did not survive restart: %d after %d", rev3, rev2)
	}
}

func TestMigrationVersionAndPragmas(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/world.db"
	db, err := Open(path, DefaultOptions())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if got := CurrentSchemaVersion(); got < 1 {
		t.Fatalf("CurrentSchemaVersion = %d, want at least 1", got)
	}
	v, err := SchemaVersion(ctx, db.SQL())
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	// The applied schema tracks the highest registered migration rather than a
	// literal, so adding a migration (for example B02's events schema) does not
	// make this world-storage test assert a stale number.
	if v != CurrentSchemaVersion() {
		t.Fatalf("applied schema = %d, want %d", v, CurrentSchemaVersion())
	}
	var journal string
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if journal != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journal)
	}
	var syncMode int
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&syncMode); err != nil {
		t.Fatalf("synchronous: %v", err)
	}
	if syncMode != 2 {
		t.Fatalf("synchronous = %d, want 2 (FULL)", syncMode)
	}
	pending, latest, err := NeedsMigration(ctx, db.SQL())
	if err != nil {
		t.Fatalf("NeedsMigration: %v", err)
	}
	if pending || latest != CurrentSchemaVersion() {
		t.Fatalf("NeedsMigration = (%v, %d), want (false, %d)", pending, latest, CurrentSchemaVersion())
	}
}
