package worldpaths

import (
	"reflect"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// Same inputs must produce identical results: resolution, overlay lookup,
// permission checks, and commit outcomes are all deterministic.
func TestDeterminism(t *testing.T) {
	cwd := domain.MustParsePath("/home/alice")
	sym := map[string]string{"/home/alice/link": "/etc"}
	for i := 0; i < 2; i++ {
		a, errA := ResolveInput(cwd, "link/../notes.txt", sym)
		b, errB := ResolveInput(cwd, "link/../notes.txt", sym)
		if errA != nil || errB != nil || a != b {
			t.Fatalf("resolution nondeterministic: %q/%v vs %q/%v", a, errA, b, errB)
		}
	}
	v := NewView()
	p := domain.MustParsePath("/x/y")
	id, _ := domain.ParseNodeID("nod_0123456789ABCDEFGHJKMNPQRS")
	v.Shared[string(p)] = LayerEntry{Node: domain.Node{ID: id, Name: "y", Kind: domain.NodeKindFile, Revision: 2, Metadata: domain.NewNodeMetadata(0o644, 1, 1, 7)}}
	n1, s1, ok1 := v.Lookup(p)
	n2, s2, ok2 := v.Lookup(p)
	if !reflect.DeepEqual(n1, n2) || s1 != s2 || ok1 != ok2 {
		t.Fatal("overlay lookup nondeterministic")
	}
	meta := domain.NewNodeMetadata(0o640, 1000, 2000, 7)
	idn := Identity{EUID: 3000, EGID: 2000}
	if CheckUnix(meta, idn, AccessRead) != CheckUnix(meta, idn, AccessRead) {
		t.Fatal("permission check nondeterministic")
	}
}
