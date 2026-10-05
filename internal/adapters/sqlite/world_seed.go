package sqlite

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"j0s.at/vibeshell/internal/domain"
)

// Seeding defaults for nodes that carry no explicit mode. A seeded home tree
// must look like an ordinary user tree: traversable directories and
// read/write files.
const (
	seedDirMode  uint32 = 0o755
	seedFileMode uint32 = 0o644
)

// seedTextMediaType labels seeded file bytes. The seeder has no file-type
// knowledge, so non-empty content is recorded as UTF-8 text; an empty file
// carries domain.EmptyContentRef, whose media type is the binary default.
const seedTextMediaType = "text/plain; charset=utf-8"

// seedCommitSeq is the commit sequence recorded for seeded versions. Seeding
// is an adapter operation outside any turn, so its history rows belong to no
// commit — the same marker EnsureNamespace gives the namespace root.
const seedCommitSeq = 0

// SeedNode is one node to create in a namespace tree.
type SeedNode struct {
	Path    domain.ValidPath
	Kind    domain.NodeKind // NodeKindDir or NodeKindFile
	Content []byte          // files only
	Mode    uint32          // optional; default 0o755 dir, 0o644 file
}

// EnsureTree idempotently creates every missing node in nodes within ns,
// parents before children, and returns how many it created. Existing nodes are
// left unchanged.
//
// Seeding is an adapter operation rather than a turn mutation, so created
// nodes start at revision 1 with no commit record, exactly like the namespace
// root. Every insert lands in one transaction, so a rejected tree never
// becomes visible in part.
func (db *DB) EnsureTree(ctx context.Context, ns domain.NamespaceID, nodes []SeedNode) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(nodes) == 0 {
		return 0, nil
	}
	// A seeded node is semantic world state: refuse it once permanent
	// recording is out of service instead of growing a tree the store cannot
	// durably record.
	if err := db.guard.RefuseRecording(); err != nil {
		return 0, err
	}
	ordered, err := orderSeedNodes(nodes)
	if err != nil {
		return 0, err
	}
	// Planning happens before the write transaction opens. This store holds a
	// single connection, so the existence lookups and content writes the plan
	// needs could not run inside it.
	inserts, err := db.planSeedTree(ctx, ns, ordered)
	if err != nil {
		return 0, err
	}
	if len(inserts) == 0 {
		return 0, nil
	}

	now := time.Now().UnixMilli()
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for i, ins := range inserts {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		row := nodeRow{
			id: ins.id.String(), namespaceID: ns.String(), parentID: ins.parent.String(),
			name: ins.path.Base(), kind: ins.kind.String(), revision: 1,
			// Ownership matches the namespace root: seeding establishes no user
			// mapping, so rows stay at uid/gid 0.
			uid: 0, gid: 0,
			mode: ins.mode, modTime: now, accessTime: now, changeTime: now,
			contentHash:  contentHashOf(ins.kind, ins.content),
			contentSize:  contentSizeOf(ins.kind, ins.content),
			contentMedia: contentMediaOf(ins.kind, ins.content),
		}
		if err := insertNodeTx(ctx, tx, row, seedCommitSeq, now); err != nil {
			return 0, fmt.Errorf("sqlite: seed node %d (%s): %w", i, ins.path, err)
		}
	}
	if err := tx.Commit(); err != nil {
		if isBusy(err) {
			return 0, domain.NewUnavailableError(domain.CodeDatabaseUnavailable, "world store is busy", nil, err)
		}
		if refusal := db.guard.ObserveWriteErr(err); refusal != nil {
			return 0, refusal
		}
		return 0, fmt.Errorf("sqlite: commit seed tree: %w", err)
	}
	db.guard.ObserveWriteSuccess()
	return len(inserts), nil
}

// seedInsert is one planned node row. The write pass performs it verbatim, so
// every decision — identity, parent, mode, content — is made before the
// transaction opens.
type seedInsert struct {
	id      domain.NodeID
	parent  domain.NodeID
	path    domain.ValidPath
	kind    domain.NodeKind
	mode    uint32
	content domain.ContentRef
}

// seedParent is the node a seed node attaches to. Identity, owning namespace,
// and kind all matter: a child may only attach to a directory that lives in
// the namespace being seeded.
type seedParent struct {
	id        domain.NodeID
	namespace domain.NamespaceID
	kind      domain.NodeKind
}

// orderSeedNodes rejects unusable seed entries and returns a copy sorted so a
// parent always precedes its children. The path breaks ties within one depth,
// which makes the plan independent of the order the caller listed.
func orderSeedNodes(nodes []SeedNode) ([]SeedNode, error) {
	ordered := make([]SeedNode, len(nodes))
	copy(ordered, nodes)
	seen := make(map[domain.ValidPath]bool, len(ordered))
	for _, seed := range ordered {
		if _, err := domain.ParsePath(seed.Path.String()); err != nil {
			return nil, domain.NewValidationError(domain.CodeInvalidPath,
				"seed path is not a valid world path", map[string]string{"path": seed.Path.String()})
		}
		if seed.Kind != domain.NodeKindDir && seed.Kind != domain.NodeKindFile {
			return nil, domain.NewValidationError(domain.CodeInvalidInput,
				"seed kind must be a directory or a file",
				map[string]string{"path": seed.Path.String(), "kind": seed.Kind.String()})
		}
		if seen[seed.Path] {
			return nil, domain.NewValidationError(domain.CodeInvalidInput,
				"seed path is listed more than once", map[string]string{"path": seed.Path.String()})
		}
		seen[seed.Path] = true
	}
	sort.Slice(ordered, func(i, j int) bool {
		depthI, depthJ := pathDepth(ordered[i].Path), pathDepth(ordered[j].Path)
		if depthI != depthJ {
			return depthI < depthJ
		}
		return ordered[i].Path.String() < ordered[j].Path.String()
	})
	return ordered, nil
}

// planSeedTree resolves which nodes already exist, rejects a node whose parent
// is neither stored nor part of the tree, and stores the bytes of every new
// file. ordered must already place parents before their children, so a parent
// is always known before its children need it.
func (db *DB) planSeedTree(ctx context.Context, ns domain.NamespaceID, ordered []SeedNode) ([]seedInsert, error) {
	// Every seeded tree hangs off the namespace root, which EnsureNamespace
	// created with this namespace.
	root, err := db.LookupPath(ctx, ns, domain.RootPath())
	if err != nil {
		return nil, err
	}
	known := map[domain.ValidPath]seedParent{
		domain.RootPath(): {id: root.ID, namespace: ns, kind: domain.NodeKindDir},
	}
	// findParent resolves the directory a new node attaches to: from the tree
	// when the caller listed it, otherwise from the store, because an earlier
	// seeding pass may already have created it.
	findParent := func(seed SeedNode) (seedParent, error) {
		parentPath := seed.Path.Parent()
		parent, ok := known[parentPath]
		if !ok {
			node, err := db.LookupPath(ctx, ns, parentPath)
			switch {
			case err == nil:
				parent = seedParentOf(node)
				known[parentPath] = parent
			case errors.Is(err, domain.CategoryNotFound):
				return seedParent{}, domain.NewValidationError(domain.CodeInvalidInput,
					"seed node has no parent; include the parent directory in the tree",
					map[string]string{"path": seed.Path.String(), "parent": parentPath.String()})
			default:
				return seedParent{}, fmt.Errorf("sqlite: lookup seed parent %s: %w", parentPath, err)
			}
		}
		if err := checkSeedParent(parent, ns, seed); err != nil {
			return seedParent{}, err
		}
		return parent, nil
	}

	var inserts []seedInsert
	for _, seed := range ordered {
		node, err := db.LookupPath(ctx, ns, seed.Path)
		switch {
		case err == nil:
			// The overlay may answer with a node from a lower scope. It stays
			// untouched here, but only one from this namespace may become a
			// parent, which checkSeedParent enforces.
			known[seed.Path] = seedParentOf(node)
			continue
		case !errors.Is(err, domain.CategoryNotFound):
			return nil, fmt.Errorf("sqlite: lookup seed path %s: %w", seed.Path, err)
		}
		parent, err := findParent(seed)
		if err != nil {
			return nil, err
		}
		id, err := newNodeID()
		if err != nil {
			return nil, err
		}
		content, err := seedContent(ctx, db, seed)
		if err != nil {
			return nil, err
		}
		known[seed.Path] = seedParent{id: id, namespace: ns, kind: seed.Kind}
		inserts = append(inserts, seedInsert{
			id: id, parent: parent.id, path: seed.Path,
			kind: seed.Kind, mode: seedMode(seed), content: content,
		})
	}
	return inserts, nil
}

// seedParentOf reads the parent identity a seed node may attach to from a
// stored node.
func seedParentOf(node domain.Node) seedParent {
	return seedParent{id: node.ID, namespace: node.NamespaceID, kind: node.Kind}
}

// checkSeedParent rejects a parent that cannot carry the new seed node. A child
// may only attach to a directory of the namespace being seeded: a lower-scope
// ancestor is not visible to the namespace that would hold the child, and a
// file has no children at all.
func checkSeedParent(parent seedParent, ns domain.NamespaceID, seed SeedNode) error {
	details := map[string]string{"path": seed.Path.String(), "parent": seed.Path.Parent().String()}
	if parent.namespace != ns {
		return domain.NewValidationError(domain.CodeInvalidInput,
			"seed parent belongs to another namespace; seed that tree there first", details)
	}
	if parent.kind != domain.NodeKindDir {
		return domain.NewValidationError(domain.CodeInvalidInput,
			"seed parent is not a directory", details)
	}
	return nil
}

// seedContent stores a file's bytes and returns the reference its node row
// records. Directories and empty files carry no stored content.
func seedContent(ctx context.Context, db *DB, seed SeedNode) (domain.ContentRef, error) {
	if seed.Kind != domain.NodeKindFile || len(seed.Content) == 0 {
		return domain.EmptyContentRef(), nil
	}
	ref, err := db.Put(ctx, seed.Content, seedTextMediaType)
	if err != nil {
		return domain.ContentRef{}, fmt.Errorf("sqlite: store seed content for %s: %w", seed.Path, err)
	}
	return ref, nil
}

// seedMode resolves the optional mode override to the conventional default
// for the node's kind.
func seedMode(seed SeedNode) uint32 {
	if seed.Mode != 0 {
		return seed.Mode
	}
	if seed.Kind == domain.NodeKindDir {
		return seedDirMode
	}
	return seedFileMode
}

// pathDepth is the segment count of an absolute path; the root has depth 0.
func pathDepth(p domain.ValidPath) int {
	if p.IsRoot() {
		return 0
	}
	return strings.Count(p.String(), "/")
}
