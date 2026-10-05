package simulation

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// newWorldReader builds a WorldReader over fresh fakes with the user's home
// tree seeded, and returns it with the user namespace and both fakes.
func newWorldReader(t *testing.T) (*WorldReader, domain.NamespaceID, *fakeWorld, *fakeContent) {
	t.Helper()
	world := newFakeWorld()
	content := newFakeContent()
	ns := testNamespaces().User
	world.add(ns, domain.RootPath(), domain.NodeKindDir, domain.InitialRevision)
	for _, dir := range []string{"/home", "/home/alice"} {
		world.add(ns, domain.MustParsePath(dir), domain.NodeKindDir, domain.InitialRevision)
	}
	return &WorldReader{World: world, Content: content}, ns, world, content
}

func TestWorldReaderResolveFile(t *testing.T) {
	reader, ns, world, content := newWorldReader(t)
	seedFile(t, world, content, ns, "/home/alice/notes.txt", []byte("hello notes"))

	outcome, err := reader.Resolve(context.Background(), ns, domain.WorldReadRequest{
		RequestID: "r1",
		Path:      domain.MustParsePath("/home/alice/notes.txt"),
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !outcome.Found {
		t.Errorf("Found = false, want true")
	}
	if outcome.Kind != domain.NodeKindFile {
		t.Errorf("Kind = %v, want file", outcome.Kind)
	}
	if string(outcome.Content) != "hello notes" {
		t.Errorf("Content = %q, want %q", outcome.Content, "hello notes")
	}
	if outcome.Truncated {
		t.Errorf("Truncated = true, want false")
	}
	if outcome.RequestID != "r1" {
		t.Errorf("RequestID = %q, want %q", outcome.RequestID, "r1")
	}
}

func TestWorldReaderResolveMissingPath(t *testing.T) {
	reader, ns, _, _ := newWorldReader(t)
	missing := domain.MustParsePath("/home/alice/missing.txt")

	outcome, err := reader.Resolve(context.Background(), ns, domain.WorldReadRequest{
		RequestID: "r2",
		Path:      missing,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if outcome.Found {
		t.Errorf("Found = true, want false")
	}
	if outcome.RequestID != "r2" {
		t.Errorf("RequestID = %q, want %q", outcome.RequestID, "r2")
	}
	if outcome.Path != missing {
		t.Errorf("Path = %q, want %q", outcome.Path, missing)
	}
}

func TestWorldReaderResolveDirectoryListsSortedEntries(t *testing.T) {
	reader, ns, world, content := newWorldReader(t)
	seedFile(t, world, content, ns, "/home/alice/zeta.txt", []byte("12345"))
	seedFile(t, world, content, ns, "/home/alice/alpha.txt", []byte("12"))
	world.add(ns, domain.MustParsePath("/home/alice/docs"), domain.NodeKindDir, domain.InitialRevision)

	outcome, err := reader.Resolve(context.Background(), ns, domain.WorldReadRequest{
		RequestID: "r3",
		Path:      domain.MustParsePath("/home/alice"),
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !outcome.Found {
		t.Fatalf("Found = false, want true")
	}
	if outcome.Kind != domain.NodeKindDir {
		t.Errorf("Kind = %v, want dir", outcome.Kind)
	}
	want := []WorldEntry{
		{Name: "alpha.txt", Kind: domain.NodeKindFile, Size: 2},
		{Name: "docs", Kind: domain.NodeKindDir},
		{Name: "zeta.txt", Kind: domain.NodeKindFile, Size: 5},
	}
	if !reflect.DeepEqual(outcome.Entries, want) {
		t.Errorf("Entries = %+v, want %+v", outcome.Entries, want)
	}
}

func TestWorldReaderResolveTruncatesLargeFile(t *testing.T) {
	reader, ns, world, content := newWorldReader(t)
	reader.MaxReadBytes = 8
	seedFile(t, world, content, ns, "/home/alice/big.txt", bytes.Repeat([]byte("x"), 32))

	outcome, err := reader.Resolve(context.Background(), ns, domain.WorldReadRequest{
		RequestID: "r4",
		Path:      domain.MustParsePath("/home/alice/big.txt"),
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !outcome.Found {
		t.Fatalf("Found = false, want true")
	}
	if !outcome.Truncated {
		t.Errorf("Truncated = false, want true")
	}
	if len(outcome.Content) != 8 {
		t.Errorf("len(Content) = %d, want %d", len(outcome.Content), 8)
	}
}

func TestWorldReaderResolveByNodeID(t *testing.T) {
	reader, ns, world, content := newWorldReader(t)
	node := seedFile(t, world, content, ns, "/home/alice/byid.txt", []byte("by id"))

	outcome, err := reader.Resolve(context.Background(), ns, domain.WorldReadRequest{
		RequestID: "r5",
		NodeID:    &node.ID,
	})
	if err != nil {
		t.Fatalf("Resolve by node ID: %v", err)
	}
	if !outcome.Found {
		t.Fatalf("Found = false, want true")
	}
	if string(outcome.Content) != "by id" {
		t.Errorf("Content = %q, want %q", outcome.Content, "by id")
	}

	missing := fakeNodeID(12345)
	outcome, err = reader.Resolve(context.Background(), ns, domain.WorldReadRequest{
		RequestID: "r6",
		NodeID:    &missing,
	})
	if err != nil {
		t.Fatalf("Resolve missing node ID: %v", err)
	}
	if outcome.Found {
		t.Errorf("Found = true, want false for a missing node")
	}
}
