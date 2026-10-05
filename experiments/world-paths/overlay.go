// Overlay precedence for the PLAN 5.2 effective user/shared/baseline view.
package worldpaths

import (
	"j0s.at/vibeshell/internal/domain"
)

// LayerEntry is one stored path in one scope layer.
type LayerEntry struct {
	Node      domain.Node
	Tombstone bool
}

// View is the three durable layers behind one effective lookup.
// Keys are canonical absolute path strings; maps are never shared across
// View copies in tests so precedence stays deterministic.
type View struct {
	Baseline map[string]LayerEntry
	Shared   map[string]LayerEntry
	User     map[string]LayerEntry
}

// NewView returns an empty layered view.
func NewView() View {
	return View{
		Baseline: map[string]LayerEntry{},
		Shared:   map[string]LayerEntry{},
		User:     map[string]LayerEntry{},
	}
}

// Lookup resolves p with user > shared > baseline precedence. A tombstone in
// a higher layer hides every lower layer without erasing it, modelling
// PLAN 5.2 deletion semantics.
func (v View) Lookup(p domain.ValidPath) (domain.Node, domain.Scope, bool) {
	key := string(p)
	if e, ok := v.User[key]; ok {
		if e.Tombstone {
			return domain.Node{}, 0, false
		}
		return e.Node, domain.ScopeUser, true
	}
	if e, ok := v.Shared[key]; ok {
		if e.Tombstone {
			return domain.Node{}, 0, false
		}
		return e.Node, domain.ScopeShared, true
	}
	if e, ok := v.Baseline[key]; ok {
		if e.Tombstone {
			return domain.Node{}, 0, false
		}
		return e.Node, domain.ScopeBaseline, true
	}
	return domain.Node{}, 0, false
}

// DeleteUser records a user-scope deletion as a tombstone. Lower layers are
// left untouched so the hide is reversible only by a new creation.
func (v *View) DeleteUser(p domain.ValidPath, tombID domain.NodeID, parent *domain.NodeID, name string) domain.Mutation {
	ns := domain.NamespaceID{}
	m := domain.Mutation{
		Type:        domain.MutationTombstone,
		NamespaceID: ns,
		Path:        p,
		NodeID:      &tombID,
	}
	v.User[string(p)] = LayerEntry{Tombstone: true}
	_ = parent
	_ = name
	return m
}

// MaterializeUser creates (or re-creates) a user-layer node as a NEW creation:
// a fresh ID and InitialRevision even when a tombstone previously hid the
// path. Callers pass the fresh ID explicitly so tests stay deterministic.
func (v *View) MaterializeUser(p domain.ValidPath, freshID domain.NodeID, kind domain.NodeKind, meta domain.NodeMetadata, content domain.ContentRef) (domain.Node, domain.Mutation) {
	n := domain.Node{
		ID:       freshID,
		Name:     p.Base(),
		Kind:     kind,
		Revision: domain.InitialRevision,
		Metadata: meta,
		Content:  content,
	}
	v.User[string(p)] = LayerEntry{Node: n}
	m := domain.Mutation{
		Type:        domain.MutationCreate,
		NamespaceID: domain.NamespaceID{},
		Path:        p,
		Kind:        kind,
		Metadata:    meta,
		Content:     content,
	}
	return n, m
}
