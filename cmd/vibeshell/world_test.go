package main

import (
	"context"
	"path/filepath"
	"testing"

	"j0s.at/vibeshell/internal/adapters/sqlite"
	"j0s.at/vibeshell/internal/application"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/system"
)

// fakeNamespaceResolver returns one fixed namespace.
type fakeNamespaceResolver struct{ ns domain.NamespaceID }

func (f fakeNamespaceResolver) namespaceFor(context.Context, application.TurnRequest) (domain.NamespaceID, error) {
	return f.ns, nil
}

// fakeWorldStore is a minimal in-memory world for the shell's read commands.
type fakeWorldStore struct {
	nodes map[domain.ValidPath]domain.Node
	dirs  map[domain.NodeID][]domain.Node
}

func (f *fakeWorldStore) GetNode(_ context.Context, _ domain.NamespaceID, id domain.NodeID) (domain.Node, error) {
	for _, n := range f.nodes {
		if n.ID == id {
			return n, nil
		}
	}
	return domain.Node{}, domain.NewNotFoundError(domain.CodeNodeNotFound, "node not found", nil)
}

func (f *fakeWorldStore) LookupPath(_ context.Context, _ domain.NamespaceID, p domain.ValidPath) (domain.Node, error) {
	if n, ok := f.nodes[p]; ok {
		return n, nil
	}
	return domain.Node{}, domain.NewNotFoundError(domain.CodeNodeNotFound, "node not found", nil)
}

func (f *fakeWorldStore) ListDirectory(_ context.Context, _ domain.NamespaceID, dir domain.NodeID, _ int, _ string) ([]domain.Node, string, error) {
	return f.dirs[dir], "", nil
}

func (f *fakeWorldStore) Commit(context.Context, domain.ChangeSet, domain.ScopePolicy) (domain.Revision, error) {
	return 0, nil
}

// fakeContentStore serves content by hash from memory.
type fakeContentStore struct{ blobs map[domain.ContentID][]byte }

func (f *fakeContentStore) Put(context.Context, []byte, string) (domain.ContentRef, error) {
	return domain.ContentRef{}, nil
}

func (f *fakeContentStore) Get(_ context.Context, ref domain.ContentRef, offset, length int64) ([]byte, error) {
	data := f.blobs[ref.Hash]
	if offset < 0 {
		offset = 0
	}
	if offset > int64(len(data)) {
		return nil, nil
	}
	end := offset + length
	if length <= 0 || end > int64(len(data)) {
		end = int64(len(data))
	}
	return data[offset:end], nil
}

func mustNodeID(t *testing.T, s string) domain.NodeID {
	t.Helper()
	id, err := domain.ParseNodeID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustContentID(t *testing.T, s string) domain.ContentID {
	t.Helper()
	id, err := domain.ParseContentID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// testWorldShell builds a shell over a small fixed world and returns it with the
// home path.
func testWorldShell(t *testing.T) (*worldShell, domain.ValidPath, domain.ValidPath) {
	t.Helper()
	ns, err := domain.ParseNamespaceID("nsp_0123456789ABCDEFGHJKMNPQRS")
	if err != nil {
		t.Fatal(err)
	}
	homeID := mustNodeID(t, "nod_0123456789ABCDEFGHJKMNPQRS")
	docID := mustNodeID(t, "nod_0123456789ABCDEFGHJKMNPQRT")
	picID := mustNodeID(t, "nod_0123456789ABCDEFGHJKMNPQRV")
	notesID := mustNodeID(t, "nod_0123456789ABCDEFGHJKMNPQRW")
	hash := mustContentID(t, "cnt_0123456789ABCDEFGHJKMNPQRS")

	home := domain.MustParsePath("/home/alice")
	world := &fakeWorldStore{
		nodes: map[domain.ValidPath]domain.Node{
			home:                   {ID: homeID, Name: "alice", Kind: domain.NodeKindDir},
			home.Join("Documents"): {ID: docID, Name: "Documents", Kind: domain.NodeKindDir},
			home.Join("Photos"):    {ID: picID, Name: "Photos", Kind: domain.NodeKindDir},
			home.Join("notes.txt"): {ID: notesID, Name: "notes.txt", Kind: domain.NodeKindFile, Content: domain.ContentRef{Hash: hash, Size: 12}},
		},
		dirs: map[domain.NodeID][]domain.Node{
			homeID: {
				{Name: "notes.txt", Kind: domain.NodeKindFile},
				{Name: "Documents", Kind: domain.NodeKindDir},
				{Name: "Photos", Kind: domain.NodeKindDir},
			},
		},
	}
	content := &fakeContentStore{blobs: map[domain.ContentID][]byte{hash: []byte("hello world\n")}}
	shell := newWorldShell(world, content, fakeNamespaceResolver{ns: ns}, system.NewClock())
	return shell, home, home
}

func testRequest(t *testing.T, cwd, home domain.ValidPath) application.TurnRequest {
	t.Helper()
	turn, err := domain.ParseTurnID("trn_0123456789ABCDEFGHJKMNPQRS")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := domain.ParseAttemptID("att_0123456789ABCDEFGHJKMNPQRS")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := domain.ParseUserID("usr_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err != nil {
		t.Fatal(err)
	}
	return application.TurnRequest{
		Principal: principal,
		Turn:      turn,
		Attempt:   attempt,
		Context:   application.SessionContext{CWD: cwd, Home: home},
		Snapshot:  application.ConfigSnapshot{ScopePolicy: domain.DefaultScopePolicy()},
	}
}

func TestWorldShellList(t *testing.T) {
	shell, home, _ := testWorldShell(t)
	out, handled, err := shell.run(context.Background(), testRequest(t, home, home), "ls", nil)
	if err != nil || !handled {
		t.Fatalf("ls: handled=%v err=%v", handled, err)
	}
	if got, want := out.Candidate.Output.Text, "Documents\nPhotos\nnotes.txt"; got != want {
		t.Fatalf("ls = %q, want %q", got, want)
	}
}

func TestWorldShellCat(t *testing.T) {
	shell, home, _ := testWorldShell(t)
	out, handled, err := shell.run(context.Background(), testRequest(t, home, home), "cat", []string{"notes.txt"})
	if err != nil || !handled {
		t.Fatalf("cat: handled=%v err=%v", handled, err)
	}
	if got := out.Candidate.Output.Text; got != "hello world" {
		t.Fatalf("cat = %q, want the file content without the trailing newline", got)
	}
	if out.Candidate.ExitStatus != 0 {
		t.Fatalf("cat exit = %d, want 0", out.Candidate.ExitStatus)
	}
}

func TestWorldShellCatMissing(t *testing.T) {
	shell, home, _ := testWorldShell(t)
	out, handled, err := shell.run(context.Background(), testRequest(t, home, home), "cat", []string{"friends.md"})
	if err != nil || !handled {
		t.Fatalf("cat: handled=%v err=%v", handled, err)
	}
	if got, want := out.Candidate.Output.Text, "cat: friends.md: No such file or directory"; got != want {
		t.Fatalf("cat = %q, want %q", got, want)
	}
	if out.Candidate.ExitStatus == 0 {
		t.Fatal("cat of a missing file must exit non-zero")
	}
}

func TestWorldShellChangeDir(t *testing.T) {
	shell, home, _ := testWorldShell(t)
	out, handled, err := shell.run(context.Background(), testRequest(t, home, home), "cd", []string{"Documents"})
	if err != nil || !handled {
		t.Fatalf("cd: handled=%v err=%v", handled, err)
	}
	if out.Candidate.SessionPatch.CWD == nil {
		t.Fatal("cd must propose a new working directory")
	}
	if got, want := out.Candidate.SessionPatch.CWD.String(), "/home/alice/Documents"; got != want {
		t.Fatalf("cd cwd = %q, want %q", got, want)
	}
}

func TestWorldShellChangeDirNoArgGoesHome(t *testing.T) {
	shell, home, _ := testWorldShell(t)
	// Start elsewhere and ask cd with no argument; it must return home.
	elsewhere := domain.MustParsePath("/")
	out, handled, err := shell.run(context.Background(), testRequest(t, elsewhere, home), "cd", nil)
	if err != nil || !handled {
		t.Fatalf("cd: handled=%v err=%v", handled, err)
	}
	if out.Candidate.SessionPatch.CWD == nil || out.Candidate.SessionPatch.CWD.String() != home.String() {
		t.Fatalf("cd with no argument must go home, got %v", out.Candidate.SessionPatch.CWD)
	}
}

func TestWorldShellIgnoresUnknownCommand(t *testing.T) {
	shell, home, _ := testWorldShell(t)
	_, handled, err := shell.run(context.Background(), testRequest(t, home, home), "moon-orchard", nil)
	if err != nil {
		t.Fatalf("unknown command: %v", err)
	}
	if handled {
		t.Fatal("an unknown command must be reported as unhandled so generation still runs")
	}
}

// TestWorldShellMakeDirStagesACreate proves mkdir stages a directory creation
// in the turn's change set instead of mutating the world directly.
func TestWorldShellMakeDirStagesACreate(t *testing.T) {
	shell, home, _ := testWorldShell(t)
	out, handled, err := shell.run(context.Background(), testRequest(t, home, home), "mkdir", []string{"Projects"})
	if err != nil || !handled {
		t.Fatalf("mkdir: handled=%v err=%v", handled, err)
	}
	mutations := out.Candidate.Changes.Mutations
	if len(mutations) != 1 {
		t.Fatalf("mkdir staged %d mutations, want 1", len(mutations))
	}
	m := mutations[0]
	if m.Type != domain.MutationCreate || m.Kind != domain.NodeKindDir {
		t.Fatalf("mkdir mutation = %+v, want a directory create", m)
	}
	if got := m.Path.String(); got != "/home/alice/Projects" {
		t.Fatalf("mkdir path = %q, want /home/alice/Projects", got)
	}
	deps := out.Candidate.Changes.ReadDependencies
	if len(deps) != 1 || deps[0].NodeID.IsZero() || deps[0].IsAbsence {
		t.Fatalf("mkdir read dependency = %+v, want a presence check on the parent", deps)
	}
}

// TestWorldShellRemoveStagesADelete proves rm stages a delete of an existing
// file and refuses a directory.
func TestWorldShellRemoveStagesADelete(t *testing.T) {
	shell, home, _ := testWorldShell(t)
	out, handled, err := shell.run(context.Background(), testRequest(t, home, home), "rm", []string{"notes.txt"})
	if err != nil || !handled {
		t.Fatalf("rm: handled=%v err=%v", handled, err)
	}
	if len(out.Candidate.Changes.Mutations) != 1 || out.Candidate.Changes.Mutations[0].Type != domain.MutationDelete {
		t.Fatalf("rm mutations = %+v, want one delete", out.Candidate.Changes.Mutations)
	}
	if out.Candidate.ExitStatus != 0 {
		t.Fatalf("rm exit = %d, want 0", out.Candidate.ExitStatus)
	}

	dirOut, _, err := shell.run(context.Background(), testRequest(t, home, home), "rm", []string{"Documents"})
	if err != nil {
		t.Fatalf("rm dir: %v", err)
	}
	if len(dirOut.Candidate.Changes.Mutations) != 0 || dirOut.Candidate.ExitStatus == 0 {
		t.Fatalf("rm of a directory must be refused, got %+v exit=%d", dirOut.Candidate.Changes.Mutations, dirOut.Candidate.ExitStatus)
	}
}

// TestWorldShellTouchStagesACreate proves touch stages an empty file create.
func TestWorldShellTouchStagesACreate(t *testing.T) {
	shell, home, _ := testWorldShell(t)
	out, handled, err := shell.run(context.Background(), testRequest(t, home, home), "touch", []string{"todo.md"})
	if err != nil || !handled {
		t.Fatalf("touch: handled=%v err=%v", handled, err)
	}
	if len(out.Candidate.Changes.Mutations) != 1 || out.Candidate.Changes.Mutations[0].Kind != domain.NodeKindFile {
		t.Fatalf("touch mutations = %+v, want one file create", out.Candidate.Changes.Mutations)
	}
}

// fakeMaterializer records whether it was asked to materialize a path.
type fakeMaterializer struct {
	content []byte
	called  bool
}

func (f *fakeMaterializer) Materialize(context.Context, domain.ValidPath, string) ([]byte, error) {
	f.called = true
	return f.content, nil
}

// TestWorldShellMaterializesMissingFile proves cat on a missing file under an
// existing directory generates plausible content, returns it, and stages the
// creation so it persists.
func TestWorldShellMaterializesMissingFile(t *testing.T) {
	shell, home, _ := testWorldShell(t)
	mat := &fakeMaterializer{content: []byte("generated text")}
	shell.materializer = mat

	out, handled, err := shell.run(context.Background(), testRequest(t, home, home), "cat", []string{"friends.md"})
	if err != nil || !handled {
		t.Fatalf("cat: handled=%v err=%v", handled, err)
	}
	if !mat.called {
		t.Fatal("the materializer was never asked for content")
	}
	if got := out.Candidate.Output.Text; got != "generated text" {
		t.Fatalf("cat output = %q, want the materialized content", got)
	}
	if len(out.Candidate.Changes.Mutations) != 1 || out.Candidate.Changes.Mutations[0].Kind != domain.NodeKindFile {
		t.Fatalf("materialize mutations = %+v, want one file create", out.Candidate.Changes.Mutations)
	}
	if out.Candidate.ExitStatus != 0 {
		t.Fatalf("materialized cat exit = %d, want 0", out.Candidate.ExitStatus)
	}
	deps := out.Candidate.Changes.ReadDependencies
	if len(deps) != 1 || deps[0].NodeID.IsZero() || deps[0].IsAbsence {
		t.Fatalf("materialize read dependency = %+v, want a presence check on the parent", deps)
	}
}

// TestWorldShellWithoutMaterializerReportsMissing proves a nil materializer
// keeps the truthful "no such file" behavior.
func TestWorldShellWithoutMaterializerReportsMissing(t *testing.T) {
	shell, home, _ := testWorldShell(t)
	out, _, err := shell.run(context.Background(), testRequest(t, home, home), "cat", []string{"friends.md"})
	if err != nil {
		t.Fatalf("cat: %v", err)
	}
	if len(out.Candidate.Changes.Mutations) != 0 {
		t.Fatalf("a nil materializer must not stage a create, got %+v", out.Candidate.Changes.Mutations)
	}
	if out.Candidate.ExitStatus == 0 {
		t.Fatal("a missing file with no materializer must exit non-zero")
	}
}

// TestWorldServiceSeedsHomeTree proves the first use of a user namespace creates
// the baseline skeleton and the home tree, and that a second use is idempotent.
func TestWorldServiceSeedsHomeTree(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "world.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("open world: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	user, err := domain.ParseUserID("usr_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err != nil {
		t.Fatal(err)
	}
	home := domain.MustParsePath("/home/alice")
	svc := newWorldService(db, system.NewClock())

	ns, err := svc.userNamespace(ctx, user, home)
	if err != nil {
		t.Fatalf("userNamespace: %v", err)
	}
	node, err := db.LookupPath(ctx, ns, home.Join("notes.txt"))
	if err != nil {
		t.Fatalf("lookup notes.txt: %v", err)
	}
	if !node.IsFile() {
		t.Fatalf("notes.txt kind = %v, want a file", node.Kind)
	}
	data, err := db.Get(ctx, node.Content, 0, 4096)
	if err != nil {
		t.Fatalf("read notes.txt: %v", err)
	}
	if string(data) != homeNotes {
		t.Fatalf("notes.txt content = %q, want the seeded note", data)
	}
	if _, err := db.LookupPath(ctx, ns, home.Join("Documents")); err != nil {
		t.Fatalf("Documents was not seeded: %v", err)
	}
	if _, err := db.LookupPath(ctx, ns, domain.MustParsePath("/etc")); err != nil {
		t.Fatalf("baseline /etc was not seeded: %v", err)
	}
	if again, err := svc.userNamespace(ctx, user, home); err != nil || again != ns {
		t.Fatalf("second userNamespace = %v, %v; want the same namespace", again, err)
	}
}

// TestWorldShellCommitAgainstRealStore proves the change sets mkdir and
// materialization produce are accepted by the real world store, not just the
// fake one. A zero-node read dependency, which the store rejects, is exactly
// the bug this guards against.
func TestWorldShellCommitAgainstRealStore(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "world.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("open world: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	user, err := domain.ParseUserID("usr_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err != nil {
		t.Fatal(err)
	}
	home := domain.MustParsePath("/home/alice")
	svc := newWorldService(db, system.NewClock())
	ns, err := svc.userNamespace(ctx, user, home)
	if err != nil {
		t.Fatalf("userNamespace: %v", err)
	}

	shell := newWorldShell(db, db, svc, system.NewClock())
	shell.materializer = &fakeMaterializer{content: []byte("materialized")}
	req := testRequest(t, home, home)

	mkdirOut, handled, err := shell.run(ctx, req, "mkdir", []string{"Projects"})
	if err != nil || !handled {
		t.Fatalf("mkdir: handled=%v err=%v", handled, err)
	}
	if _, err := db.Commit(ctx, mkdirOut.Candidate.Changes, req.Snapshot.ScopePolicy); err != nil {
		t.Fatalf("commit mkdir: %v", err)
	}
	if _, err := db.LookupPath(ctx, ns, home.Join("Projects")); err != nil {
		t.Fatalf("mkdir did not persist: %v", err)
	}

	catOut, handled, err := shell.run(ctx, req, "cat", []string{"friends.md"})
	if err != nil || !handled {
		t.Fatalf("cat: handled=%v err=%v", handled, err)
	}
	if _, err := db.Commit(ctx, catOut.Candidate.Changes, req.Snapshot.ScopePolicy); err != nil {
		t.Fatalf("commit materialized cat: %v", err)
	}
	node, err := db.LookupPath(ctx, ns, home.Join("friends.md"))
	if err != nil {
		t.Fatalf("materialized file did not persist: %v", err)
	}
	data, err := db.Get(ctx, node.Content, 0, 4096)
	if err != nil || string(data) != "materialized" {
		t.Fatalf("materialized content = %q, %v; want %q", data, err, "materialized")
	}
}
