package worldpaths

import (
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

func testNodeID(t *testing.T, s string) domain.NodeID {
	t.Helper()
	id, err := domain.ParseNodeID(s)
	if err != nil {
		t.Fatalf("ParseNodeID(%q): %v", s, err)
	}
	return id
}

func meta644() domain.NodeMetadata {
	return domain.NewNodeMetadata(0o644, 1000, 1000, 1798732800000)
}

func TestOverlayPrecedenceUserOverSharedOverBaseline(t *testing.T) {
	v := NewView()
	p := domain.MustParsePath("/etc/motd")
	baseID := testNodeID(t, "nod_0123456789ABCDEFGHJKMNPQRS")
	sharedID := testNodeID(t, "nod_0123456789ABCDEFGHJKMNPQRT")
	userID := testNodeID(t, "nod_0123456789ABCDEFGHJKMNPQRV")
	v.Baseline[string(p)] = LayerEntry{Node: domain.Node{ID: baseID, Name: "motd", Kind: domain.NodeKindFile, Revision: 3, Metadata: meta644()}}
	v.Shared[string(p)] = LayerEntry{Node: domain.Node{ID: sharedID, Name: "motd", Kind: domain.NodeKindFile, Revision: 5, Metadata: meta644()}}
	v.User[string(p)] = LayerEntry{Node: domain.Node{ID: userID, Name: "motd", Kind: domain.NodeKindFile, Revision: 7, Metadata: meta644()}}

	n, scope, ok := v.Lookup(p)
	if !ok || scope != domain.ScopeUser || n.ID != userID || n.Revision != 7 {
		t.Fatalf("want user layer, got scope=%v ok=%v id=%v", scope, ok, n.ID)
	}
	delete(v.User, string(p))
	n, scope, ok = v.Lookup(p)
	if !ok || scope != domain.ScopeShared || n.ID != sharedID {
		t.Fatalf("want shared layer, got scope=%v ok=%v", scope, ok)
	}
	delete(v.Shared, string(p))
	n, scope, ok = v.Lookup(p)
	if !ok || scope != domain.ScopeBaseline || n.ID != baseID {
		t.Fatalf("want baseline layer, got scope=%v ok=%v", scope, ok)
	}
}

func TestUserDeleteHidesWithoutErasingAndRematerializesAsNew(t *testing.T) {
	v := NewView()
	p := domain.MustParsePath("/shared/notes.txt")
	sharedID := testNodeID(t, "nod_0123456789ABCDEFGHJKMNPQRS")
	v.Shared[string(p)] = LayerEntry{Node: domain.Node{ID: sharedID, Name: "notes.txt", Kind: domain.NodeKindFile, Revision: 4, Metadata: meta644()}}

	tombID := testNodeID(t, "nod_0123456789ABCDEFGHJKMNPQRT")
	m := v.DeleteUser(p, tombID, nil, "notes.txt")
	if m.Type != domain.MutationTombstone {
		t.Fatalf("delete must record a tombstone mutation, got %v", m.Type)
	}
	if _, _, ok := v.Lookup(p); ok {
		t.Fatal("tombstoned path still resolves, want hidden")
	}
	// Lower layer is intact, not erased.
	if e, ok := v.Shared[string(p)]; !ok || e.Node.ID != sharedID {
		t.Fatal("shared entry was erased instead of hidden")
	}
	// Re-materialization is a NEW creation: fresh ID, InitialRevision,
	// recorded as MutationCreate.
	freshID := testNodeID(t, "nod_0123456789ABCDEFGHJKMNPQRV")
	n, cm := v.MaterializeUser(p, freshID, domain.NodeKindFile, meta644(), domain.ContentRef{Size: 3, MediaType: "text/plain"})
	if cm.Type != domain.MutationCreate {
		t.Fatalf("re-materialization must be create, got %v", cm.Type)
	}
	if n.ID == sharedID {
		t.Fatal("re-materialization reused the hidden ID; must be new")
	}
	if n.Revision != domain.InitialRevision {
		t.Fatalf("re-materialization revision = %v, want initial", n.Revision)
	}
	got, scope, ok := v.Lookup(p)
	if !ok || scope != domain.ScopeUser || got.ID != freshID {
		t.Fatalf("lookup after re-create: scope=%v ok=%v", scope, ok)
	}
}

func TestSharedTombstoneHidesBaseline(t *testing.T) {
	v := NewView()
	p := domain.MustParsePath("/bin/tool")
	baseID := testNodeID(t, "nod_0123456789ABCDEFGHJKMNPQRS")
	v.Baseline[string(p)] = LayerEntry{Node: domain.Node{ID: baseID, Name: "tool", Kind: domain.NodeKindFile, Revision: 1, Metadata: meta644()}}
	v.Shared[string(p)] = LayerEntry{Tombstone: true}
	if _, _, ok := v.Lookup(p); ok {
		t.Fatal("shared tombstone should hide baseline")
	}
	if e, ok := v.Baseline[string(p)]; !ok || e.Node.ID != baseID {
		t.Fatal("baseline entry was erased instead of hidden")
	}
}
