package sqlite

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// seedTestNamespace creates the user namespace a seeded home tree lives in,
// using the same resolution the server does on first use.
func seedTestNamespace(t *testing.T, ctx context.Context, db *DB) domain.Namespace {
	t.Helper()
	user := testUserID(t)
	ns, err := db.EnsureNamespace(ctx, domain.NamespaceForUser(user, "user:"+user.Value()))
	if err != nil {
		t.Fatalf("user namespace: %v", err)
	}
	return ns
}

func lookupSeedNode(t *testing.T, ctx context.Context, db *DB, ns domain.NamespaceID, raw string) domain.Node {
	t.Helper()
	node, err := db.LookupPath(ctx, ns, mustPath(t, raw))
	if err != nil {
		t.Fatalf("LookupPath %s: %v", raw, err)
	}
	return node
}

// listSeedDir returns one complete directory page, failing when the listing
// needed a second page.
func listSeedDir(t *testing.T, ctx context.Context, db *DB, ns domain.NamespaceID, dir domain.Node) []domain.Node {
	t.Helper()
	page, next, err := db.ListDirectory(ctx, ns, dir.ID, defaultListLimit, "")
	if err != nil {
		t.Fatalf("ListDirectory %s: %v", dir.Name, err)
	}
	if next != "" {
		t.Fatalf("ListDirectory %s paged unexpectedly, cursor %q", dir.Name, next)
	}
	return page
}

// seedChildNames renders one directory's child names for a failure message.
func seedChildNames(t *testing.T, ctx context.Context, db *DB, ns domain.NamespaceID, dir domain.Node) string {
	t.Helper()
	entries := listSeedDir(t, ctx, db, ns, dir)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return strings.Join(names, ",")
}

func TestEnsureTreeCreatesDirectoryTreeAndFiles(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})
	ns := seedTestNamespace(t, ctx, db)

	notesBody := []byte("hello from vibeshell\n")
	reportBody := []byte("quarterly report\n")
	// Listed children-first and across depths: the seeder, not the caller,
	// establishes the parents-before-children order.
	created, err := db.EnsureTree(ctx, ns.ID, []SeedNode{
		{Path: mustPath(t, "/home/alice/Documents/report.txt"), Kind: domain.NodeKindFile, Content: reportBody},
		{Path: mustPath(t, "/home/alice/notes.txt"), Kind: domain.NodeKindFile, Content: notesBody},
		{Path: mustPath(t, "/home/alice/empty.log"), Kind: domain.NodeKindFile},
		{Path: mustPath(t, "/home/alice/Documents"), Kind: domain.NodeKindDir},
		{Path: mustPath(t, "/home/alice"), Kind: domain.NodeKindDir},
		{Path: mustPath(t, "/home"), Kind: domain.NodeKindDir},
	})
	if err != nil {
		t.Fatalf("EnsureTree: %v", err)
	}
	if created != 6 {
		t.Fatalf("created %d nodes, want 6", created)
	}

	wantNodes := []struct {
		path string
		kind domain.NodeKind
		mode uint32
	}{
		{"/home", domain.NodeKindDir, seedDirMode},
		{"/home/alice", domain.NodeKindDir, seedDirMode},
		{"/home/alice/Documents", domain.NodeKindDir, seedDirMode},
		{"/home/alice/notes.txt", domain.NodeKindFile, seedFileMode},
		{"/home/alice/empty.log", domain.NodeKindFile, seedFileMode},
		{"/home/alice/Documents/report.txt", domain.NodeKindFile, seedFileMode},
	}
	for _, want := range wantNodes {
		node := lookupSeedNode(t, ctx, db, ns.ID, want.path)
		if node.Kind != want.kind {
			t.Errorf("%s kind = %v, want %v", want.path, node.Kind, want.kind)
		}
		if node.Metadata.Mode != want.mode {
			t.Errorf("%s mode = %#o, want %#o", want.path, node.Metadata.Mode, want.mode)
		}
		if node.NamespaceID != ns.ID {
			t.Errorf("%s namespace = %v, want %v", want.path, node.NamespaceID, ns.ID)
		}
		if node.Revision != 1 {
			t.Errorf("%s revision = %d, want 1", want.path, node.Revision)
		}
	}

	// The tree hangs off the namespace root, so a read from the top finds it.
	rootEntries := listSeedDir(t, ctx, db, ns.ID, lookupSeedNode(t, ctx, db, ns.ID, "/"))
	if len(rootEntries) != 1 || rootEntries[0].Name != "home" || rootEntries[0].Kind != domain.NodeKindDir {
		t.Fatalf("namespace root lists %+v, want the single home directory", rootEntries)
	}

	home := lookupSeedNode(t, ctx, db, ns.ID, "/home")
	if names := seedChildNames(t, ctx, db, ns.ID, home); names != "alice" {
		t.Errorf("/home lists %s, want alice", names)
	}

	alice := lookupSeedNode(t, ctx, db, ns.ID, "/home/alice")
	homeEntries := listSeedDir(t, ctx, db, ns.ID, alice)
	if len(homeEntries) != 3 {
		t.Fatalf("/home/alice lists %d entries (%s), want 3", len(homeEntries), seedChildNames(t, ctx, db, ns.ID, alice))
	}
	if homeEntries[0].Name != "Documents" || homeEntries[0].Kind != domain.NodeKindDir || homeEntries[0].Content.Size != 0 {
		t.Errorf("/home/alice[0] = %+v, want the empty Documents directory", homeEntries[0])
	}
	if homeEntries[1].Name != "empty.log" || homeEntries[1].Kind != domain.NodeKindFile ||
		homeEntries[1].Content.Size != 0 || homeEntries[1].Content.MediaType != "application/octet-stream" {
		t.Errorf("/home/alice[1] = %+v, want an empty file with the binary default media type", homeEntries[1])
	}
	if homeEntries[2].Name != "notes.txt" || homeEntries[2].Kind != domain.NodeKindFile ||
		homeEntries[2].Content.Size != int64(len(notesBody)) {
		t.Errorf("/home/alice[2] = %+v, want notes.txt of %d bytes", homeEntries[2], len(notesBody))
	}

	documents := listSeedDir(t, ctx, db, ns.ID, lookupSeedNode(t, ctx, db, ns.ID, "/home/alice/Documents"))
	if len(documents) != 1 || documents[0].Name != "report.txt" || documents[0].Content.Size != int64(len(reportBody)) {
		t.Errorf("/home/alice/Documents lists %+v, want report.txt of %d bytes", documents, len(reportBody))
	}
}

func TestEnsureTreeIsIdempotentAndLeavesExistingNodesAlone(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})
	ns := seedTestNamespace(t, ctx, db)

	first := []SeedNode{
		{Path: mustPath(t, "/home"), Kind: domain.NodeKindDir},
		{Path: mustPath(t, "/home/alice"), Kind: domain.NodeKindDir},
		{Path: mustPath(t, "/home/alice/notes.txt"), Kind: domain.NodeKindFile, Content: []byte("original\n")},
	}
	if created, err := db.EnsureTree(ctx, ns.ID, first); err != nil || created != 3 {
		t.Fatalf("first EnsureTree = %d nodes, %v; want 3, nil", created, err)
	}
	before := lookupSeedNode(t, ctx, db, ns.ID, "/home/alice/notes.txt")

	// A second pass offers different content and modes for a node that
	// already exists, plus one genuinely new directory, and lists neither
	// ancestor: a resumed seed resolves those from the store. Only the new
	// node may be created, and the existing file must survive byte for byte.
	created, err := db.EnsureTree(ctx, ns.ID, []SeedNode{
		{Path: mustPath(t, "/home/alice/notes.txt"), Kind: domain.NodeKindFile, Content: []byte("replaced\n"), Mode: 0o600},
		{Path: mustPath(t, "/home/alice/Documents"), Kind: domain.NodeKindDir, Mode: 0o700},
	})
	if err != nil {
		t.Fatalf("second EnsureTree: %v", err)
	}
	if created != 1 {
		t.Fatalf("second EnsureTree created %d nodes, want 1", created)
	}

	after := lookupSeedNode(t, ctx, db, ns.ID, "/home/alice/notes.txt")
	if after.ID != before.ID || after.Revision != before.Revision || after.Content != before.Content {
		t.Fatalf("existing node changed: before %+v, after %+v", before, after)
	}
	if after.Metadata.Mode != before.Metadata.Mode {
		t.Errorf("existing node mode = %#o, want the unchanged %#o", after.Metadata.Mode, before.Metadata.Mode)
	}
	got, err := db.Get(ctx, after.Content, 0, -1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "original\n" {
		t.Errorf("stored bytes = %q, want the original content", got)
	}
}

func TestEnsureTreeFileContentRoundTrips(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})
	ns := seedTestNamespace(t, ctx, db)

	want := []byte{0x00, 'b', 'i', 'n', 0xff, 0x00, '\n'}
	if _, err := db.EnsureTree(ctx, ns.ID, []SeedNode{
		{Path: mustPath(t, "/home"), Kind: domain.NodeKindDir},
		{Path: mustPath(t, "/home/alice"), Kind: domain.NodeKindDir},
		{Path: mustPath(t, "/home/alice/data.bin"), Kind: domain.NodeKindFile, Content: want},
	}); err != nil {
		t.Fatalf("EnsureTree: %v", err)
	}
	node := lookupSeedNode(t, ctx, db, ns.ID, "/home/alice/data.bin")
	if node.Content.Size != int64(len(want)) {
		t.Errorf("content size = %d, want %d", node.Content.Size, len(want))
	}
	if node.Content.Hash.IsZero() {
		t.Error("non-empty seeded file carries no content hash")
	}
	if node.Content.MediaType != seedTextMediaType {
		t.Errorf("content media type = %q, want %q", node.Content.MediaType, seedTextMediaType)
	}
	got, err := db.Get(ctx, node.Content, 0, -1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("content = %v, want %v", got, want)
	}
}

func TestEnsureTreeRejectsNodeWithoutParent(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})
	ns := seedTestNamespace(t, ctx, db)

	created, err := db.EnsureTree(ctx, ns.ID, []SeedNode{
		{Path: mustPath(t, "/home/alice/Documents/report.txt"), Kind: domain.NodeKindFile, Content: []byte("orphan\n")},
	})
	if created != 0 {
		t.Errorf("created %d nodes, want 0", created)
	}
	if !domain.IsValidationError(err) {
		t.Fatalf("EnsureTree error = %v (%T), want a validation error", err, err)
	}
	var domainErr *domain.DomainError
	if !errors.As(err, &domainErr) {
		t.Fatalf("EnsureTree error = %v (%T), want a *domain.DomainError", err, err)
	}
	// The failure names both the node and the parent it could not resolve, so
	// a caller can fix the tree without guessing which ancestor is absent.
	if got := domainErr.Details["path"]; got != "/home/alice/Documents/report.txt" {
		t.Errorf("error path detail = %q, want the seeded node's path", got)
	}
	if got := domainErr.Details["parent"]; got != "/home/alice/Documents" {
		t.Errorf("error parent detail = %q, want the missing parent path", got)
	}

	// The whole tree is rejected, not just the orphaning node.
	for _, p := range []string{"/home", "/home/alice", "/home/alice/Documents", "/home/alice/Documents/report.txt"} {
		if _, err := db.LookupPath(ctx, ns.ID, mustPath(t, p)); !errors.Is(err, domain.CategoryNotFound) {
			t.Errorf("LookupPath %s = %v, want not_found", p, err)
		}
	}
}

func TestEnsureTreeEmptyInputIsNoOp(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})
	ns := seedTestNamespace(t, ctx, db)

	for _, nodes := range [][]SeedNode{nil, {}} {
		created, err := db.EnsureTree(ctx, ns.ID, nodes)
		if err != nil {
			t.Fatalf("EnsureTree: %v", err)
		}
		if created != 0 {
			t.Fatalf("created %d nodes, want 0", created)
		}
	}
	if entries := listSeedDir(t, ctx, db, ns.ID, lookupSeedNode(t, ctx, db, ns.ID, "/")); len(entries) != 0 {
		t.Errorf("namespace root lists %+v, want nothing", entries)
	}
}

func TestEnsureTreeRefusesWhilePermanentRecordingIsUnavailable(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, DefaultOptions())
	ns := seedTestNamespace(t, ctx, db)

	// One durable write fails, which takes permanent recording out of service.
	if _, err := db.SQL().ExecContext(ctx, `PRAGMA query_only=ON`); err != nil {
		t.Fatalf("enable query_only: %v", err)
	}
	if _, err := db.Put(ctx, []byte("probe"), "text/plain"); domain.GetErrorCode(err) != domain.CodeRecordingUnavailable {
		t.Fatalf("Put while read-only = %v, want %q", err, domain.CodeRecordingUnavailable)
	}

	created, err := db.EnsureTree(ctx, ns.ID, []SeedNode{
		{Path: mustPath(t, "/home/alice"), Kind: domain.NodeKindDir},
	})
	if !errors.Is(err, domain.CategoryUnavailable) {
		t.Fatalf("EnsureTree while recording down = %v, want unavailable", err)
	}
	if created != 0 {
		t.Fatalf("created %d nodes, want 0", created)
	}
	if _, err := db.LookupPath(ctx, ns.ID, mustPath(t, "/home/alice")); !errors.Is(err, domain.CategoryNotFound) {
		t.Errorf("LookupPath after refusal = %v, want not_found", err)
	}
}
