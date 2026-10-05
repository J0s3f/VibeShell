package sqlite

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"j0s.at/vibeshell/internal/domain"
)

// Change-set bounds mirror schemas/world-changeset.schema.json so oversized
// turns fail fast at the adapter boundary instead of inside the transaction.
const (
	maxMutations        = 256
	maxReadDependencies = 1024
	// maxListLimit bounds one directory page; larger histories page instead.
	maxListLimit     = 1000
	defaultListLimit = 100
	// maxPathDepth guards parent-chain walks against corrupt cycles.
	maxPathDepth = 4096
)

// crockfordBase32 is the Crockford alphabet without I, L, O, U, matching the
// identity regex in internal/domain/identity.go.
const crockfordBase32 = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// encodeLower128 encodes 16 random bytes as 26 Crockford characters: the
// 128 bits sit behind two leading zero bits, giving 130 bits in 26 groups.
func encodeLower128(raw []byte) string {
	var out [26]byte
	for i := range 26 {
		var v byte
		for b := range 5 {
			pos := i*5 + b // 0..129 over the 130-bit value
			var bit byte
			if pos >= 2 {
				p := pos - 2 // 0..127 into raw, MSB first
				bit = (raw[p/8] >> (7 - (p % 8))) & 1
			}
			v = (v << 1) | bit
		}
		out[i] = crockfordBase32[v]
	}
	return string(out[:])
}

func random128() ([]byte, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// newNodeID mints a globally unique node identity.
func newNodeID() (domain.NodeID, error) {
	raw, err := random128()
	if err != nil {
		return domain.NodeID{}, err
	}
	return domain.ParseNodeID(domain.PrefixNode + "_" + encodeLower128(raw))
}

// newNamespaceID mints a globally unique namespace identity.
func newNamespaceID() (domain.NamespaceID, error) {
	raw, err := random128()
	if err != nil {
		return domain.NamespaceID{}, err
	}
	return domain.ParseNamespaceID(domain.PrefixNamespace + "_" + encodeLower128(raw))
}

// newTestPrefixID mints an identity for ChangeSet turn/attempt references.
// Tests and the application use it for bookkeeping IDs the adapter never
// interprets beyond echoing them into the commits table.
func newTestPrefixID(prefix string) (string, error) {
	raw, err := random128()
	if err != nil {
		return "", err
	}
	return prefix + "_" + encodeLower128(raw), nil
}

// contentIDFor derives the deterministic content identity for exact bytes:
// the first 128 bits of SHA-256, Crockford-encoded. Truncation keeps the
// content-addressed property (identical bytes share one row) while fitting
// the domain identity shape.
func contentIDFor(data []byte) (domain.ContentID, error) {
	sum := sha256.Sum256(data)
	return domain.ParseContentID(domain.PrefixContent + "_" + encodeLower128(sum[:16]))
}

// querier abstracts *sql.DB and *sql.Tx so path resolution and commit share
// one implementation; every Commit runs its reads and writes on the tx.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type namespaceRow struct {
	id        string
	owner     string
	scopeName string
	label     string
	createdAt int64
}

func (n namespaceRow) scope() domain.Scope {
	s, err := domain.ParseScope(n.scopeName)
	if err != nil {
		return domain.ScopeSession
	}
	return s
}

type nodeRow struct {
	id            string
	namespaceID   string
	parentID      string
	name          string
	kind          string
	revision      int64
	mode          uint32
	uid           uint32
	gid           uint32
	modTime       int64
	accessTime    int64
	changeTime    int64
	symlinkTarget string
	contentHash   string
	contentSize   int64
	contentMedia  string
	tombstone     int64
	shadowOf      string
}

const nodeColumns = `id, namespace_id, parent_id, name, kind, revision, mode, uid, gid,
	mod_time, access_time, change_time, symlink_target,
	content_hash, content_size, content_media, tombstone, shadow_of`

func scanNodeRow(r *sql.Row) (nodeRow, error) {
	var n nodeRow
	var mode, uid, gid int64
	err := r.Scan(&n.id, &n.namespaceID, &n.parentID, &n.name, &n.kind, &n.revision,
		&mode, &uid, &gid, &n.modTime, &n.accessTime, &n.changeTime,
		&n.symlinkTarget, &n.contentHash, &n.contentSize, &n.contentMedia,
		&n.tombstone, &n.shadowOf)
	if err != nil {
		return nodeRow{}, err
	}
	n.mode, n.uid, n.gid = uint32(mode), uint32(uid), uint32(gid)
	return n, nil
}

func scanNodeRows(rows *sql.Rows) ([]nodeRow, error) {
	var out []nodeRow
	for rows.Next() {
		var n nodeRow
		var mode, uid, gid int64
		if err := rows.Scan(&n.id, &n.namespaceID, &n.parentID, &n.name, &n.kind, &n.revision,
			&mode, &uid, &gid, &n.modTime, &n.accessTime, &n.changeTime,
			&n.symlinkTarget, &n.contentHash, &n.contentSize, &n.contentMedia,
			&n.tombstone, &n.shadowOf); err != nil {
			return nil, err
		}
		n.mode, n.uid, n.gid = uint32(mode), uint32(uid), uint32(gid)
		out = append(out, n)
	}
	return out, rows.Err()
}

// toNode converts a live row to the domain shape. Tombstoned rows never
// reach this function; reads report them as not found.
func toNode(n nodeRow) (domain.Node, error) {
	id, err := domain.ParseNodeID(n.id)
	if err != nil {
		return domain.Node{}, domain.NewInternalError(domain.CodeInvariantViolation, "stored node id is malformed", err)
	}
	ns, err := domain.ParseNamespaceID(n.namespaceID)
	if err != nil {
		return domain.Node{}, domain.NewInternalError(domain.CodeInvariantViolation, "stored namespace id is malformed", err)
	}
	kind, err := domain.ParseNodeKind(n.kind)
	if err != nil {
		return domain.Node{}, domain.NewInternalError(domain.CodeInvariantViolation, "stored node kind is malformed", err)
	}
	node := domain.Node{
		ID:          id,
		NamespaceID: ns,
		Name:        n.name,
		Kind:        kind,
		Revision:    domain.Revision(n.revision),
		Metadata: domain.NodeMetadata{
			Mode:          n.mode,
			UID:           n.uid,
			GID:           n.gid,
			ModTime:       n.modTime,
			AccessTime:    n.accessTime,
			ChangeTime:    n.changeTime,
			SymlinkTarget: n.symlinkTarget,
		},
	}
	if n.parentID != "" {
		parent, err := domain.ParseNodeID(n.parentID)
		if err != nil {
			return domain.Node{}, domain.NewInternalError(domain.CodeInvariantViolation, "stored parent id is malformed", err)
		}
		node.ParentID = &parent
	}
	if n.contentHash == "" {
		node.Content = domain.EmptyContentRef()
	} else {
		hash, err := domain.ParseContentID(n.contentHash)
		if err != nil {
			return domain.Node{}, domain.NewInternalError(domain.CodeInvariantViolation, "stored content hash is malformed", err)
		}
		node.Content = domain.ContentRef{Hash: hash, Size: n.contentSize, MediaType: n.contentMedia}
	}
	return node, nil
}

// EnsureNamespace persists ns (matched by scope+owner+label) and its root
// directory, returning the stored record with its assigned ID. The namespace
// table is the only cross-scope knowledge the overlay resolver needs.
func (db *DB) EnsureNamespace(ctx context.Context, ns domain.Namespace) (domain.Namespace, error) {
	if err := ctx.Err(); err != nil {
		return domain.Namespace{}, err
	}
	if ns.Label == "" {
		return domain.Namespace{}, domain.NewValidationError(domain.CodeInvalidInput, "namespace label is required", nil)
	}
	owner := ns.Owner.String()
	if ns.Owner.IsZero() {
		owner = ""
	}
	var existing namespaceRow
	err := db.sql.QueryRowContext(ctx,
		`SELECT id, owner, scope, label, created_at FROM namespaces WHERE scope=? AND owner=? AND label=?`,
		ns.Scope.String(), owner, ns.Label).Scan(
		&existing.id, &existing.owner, &existing.scopeName, &existing.label, &existing.createdAt)
	switch {
	case err == nil:
		id, perr := domain.ParseNamespaceID(existing.id)
		if perr != nil {
			return domain.Namespace{}, domain.NewInternalError(domain.CodeInvariantViolation, "stored namespace id is malformed", perr)
		}
		return domain.Namespace{ID: id, Owner: ns.Owner, Scope: ns.Scope, Label: ns.Label, CreatedAt: existing.createdAt}, nil
	case err != sql.ErrNoRows:
		return domain.Namespace{}, fmt.Errorf("sqlite: lookup namespace: %w", err)
	}
	id, err := newNamespaceID()
	if err != nil {
		return domain.Namespace{}, fmt.Errorf("sqlite: mint namespace id: %w", err)
	}
	now := time.Now().UnixMilli()
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return domain.Namespace{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO namespaces(id, owner, scope, label, created_at) VALUES(?,?,?,?,?)`,
		id.String(), owner, ns.Scope.String(), ns.Label, now); err != nil {
		if isUniqueViolation(err) {
			// Lost a same-key race; re-read the winner.
			tx.Rollback()
			return db.EnsureNamespace(ctx, ns)
		}
		return domain.Namespace{}, fmt.Errorf("sqlite: insert namespace: %w", err)
	}
	rootID, err := newNodeID()
	if err != nil {
		return domain.Namespace{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO nodes(id, namespace_id, parent_id, name, kind, revision, mode, uid, gid,
			mod_time, access_time, change_time, updated_at)
		 VALUES(?,?, '', '', 'dir', 1, 493, 0, 0, ?, ?, ?, ?)`,
		rootID.String(), id.String(), now, now, now, now); err != nil {
		return domain.Namespace{}, fmt.Errorf("sqlite: insert namespace root: %w", err)
	}
	if err := insertVersionTx(ctx, tx, nodeRow{
		id: rootID.String(), namespaceID: id.String(), kind: "dir", revision: 1,
		mode: 493, modTime: now, accessTime: now, changeTime: now,
	}, 0); err != nil {
		return domain.Namespace{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Namespace{}, fmt.Errorf("sqlite: commit namespace: %w", err)
	}
	ns.ID = id
	ns.CreatedAt = now
	return ns, nil
}

func loadNamespace(ctx context.Context, q querier, id string) (namespaceRow, error) {
	var n namespaceRow
	err := q.QueryRowContext(ctx,
		`SELECT id, owner, scope, label, created_at FROM namespaces WHERE id=?`, id).Scan(
		&n.id, &n.owner, &n.scopeName, &n.label, &n.createdAt)
	if err == sql.ErrNoRows {
		return namespaceRow{}, domain.NewNotFoundError(domain.CodeNamespaceNotFound, "namespace not found", map[string]string{"namespace_id": id})
	}
	if err != nil {
		return namespaceRow{}, fmt.Errorf("sqlite: load namespace: %w", err)
	}
	return n, nil
}

// overlayChain returns the namespaces a read in ns overlays, highest scope
// first: user over shared over baseline. Shared and baseline resolve to the
// single well-known namespaces ("shared", first baseline label) when present.
func overlayChain(ctx context.Context, q querier, ns namespaceRow) ([]namespaceRow, error) {
	chain := []namespaceRow{ns}
	scope := ns.scope()
	if scope == domain.ScopeUser || scope == domain.ScopeShared {
		var shared namespaceRow
		err := q.QueryRowContext(ctx,
			`SELECT id, owner, scope, label, created_at FROM namespaces WHERE scope='shared' ORDER BY rowid LIMIT 1`).Scan(
			&shared.id, &shared.owner, &shared.scopeName, &shared.label, &shared.createdAt)
		switch {
		case err == nil:
			if shared.id != ns.id {
				chain = append(chain, shared)
			}
		case err == sql.ErrNoRows:
		default:
			return nil, fmt.Errorf("sqlite: find shared namespace: %w", err)
		}
	}
	if scope == domain.ScopeUser || scope == domain.ScopeShared {
		var base namespaceRow
		err := q.QueryRowContext(ctx,
			`SELECT id, owner, scope, label, created_at FROM namespaces WHERE scope='baseline' ORDER BY label, rowid LIMIT 1`).Scan(
			&base.id, &base.owner, &base.scopeName, &base.label, &base.createdAt)
		switch {
		case err == nil:
			if base.id != ns.id {
				chain = append(chain, base)
			}
		case err == sql.ErrNoRows:
		default:
			return nil, fmt.Errorf("sqlite: find baseline namespace: %w", err)
		}
	}
	return chain, nil
}

func resolveChild(ctx context.Context, q querier, namespaceID, parentID, name string) (nodeRow, bool, error) {
	if err := ctx.Err(); err != nil {
		return nodeRow{}, false, err
	}
	n, err := scanNodeRow(q.QueryRowContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE namespace_id=? AND parent_id=? AND name=?`,
		namespaceID, parentID, name))
	if err == sql.ErrNoRows {
		return nodeRow{}, false, nil
	}
	if err != nil {
		return nodeRow{}, false, fmt.Errorf("sqlite: resolve child: %w", err)
	}
	return n, true, nil
}

// resolveResult is one scope's answer for a path: either a live node, a
// hidden tombstone/foreign subtree (stop, do not fall through), or missing
// (caller may consult the next lower scope).
type resolveResult struct {
	node    nodeRow
	found   bool // live node in this scope
	blocked bool // tombstone or non-dir traversal: hidden, stop the chain
}

func resolveInScope(ctx context.Context, q querier, namespaceID string, p domain.ValidPath) (resolveResult, error) {
	root, found, err := resolveChild(ctx, q, namespaceID, "", "")
	if err != nil || !found {
		return resolveResult{}, err
	}
	if p.IsRoot() {
		if root.tombstone != 0 {
			return resolveResult{blocked: true}, nil
		}
		return resolveResult{node: root, found: true}, nil
	}
	cur := root
	if cur.tombstone != 0 {
		return resolveResult{blocked: true}, nil
	}
	parts := strings.Split(strings.TrimPrefix(p.String(), "/"), "/")
	for i, part := range parts {
		last := i == len(parts)-1
		child, ok, err := resolveChild(ctx, q, namespaceID, cur.id, part)
		if err != nil {
			return resolveResult{}, err
		}
		if !ok {
			return resolveResult{}, nil // missing: fall through to lower scope
		}
		if child.tombstone != 0 {
			return resolveResult{blocked: true}, nil
		}
		if !last && child.kind != "dir" {
			// v1 does not follow symlinks or descend into files; the
			// subtree is unreachable through this scope.
			return resolveResult{blocked: true}, nil
		}
		cur = child
		_ = last
	}
	return resolveResult{node: cur, found: true}, nil
}

// nodePath rebuilds a node's absolute path by walking parents. It reports
// NotFound when the chain is broken and an internal error on a cycle.
func nodePath(ctx context.Context, q querier, namespaceID string, n nodeRow) (domain.ValidPath, error) {
	if n.parentID == "" {
		return domain.RootPath(), nil
	}
	parts := []string{n.name}
	cur := n
	for range maxPathDepth {
		parent, err := scanNodeRow(q.QueryRowContext(ctx,
			`SELECT `+nodeColumns+` FROM nodes WHERE id=?`, cur.parentID))
		if err == sql.ErrNoRows {
			return "", domain.NewNotFoundError(domain.CodeNodeNotFound, "node parent chain is broken", map[string]string{"node_id": cur.id})
		}
		if err != nil {
			return "", fmt.Errorf("sqlite: walk parent chain: %w", err)
		}
		if parent.namespaceID != namespaceID {
			return "", domain.NewInternalError(domain.CodeInvariantViolation, "node parent crosses namespaces", nil)
		}
		if parent.parentID == "" {
			parts = append(parts, "")
			reversed := make([]string, 0, len(parts))
			for i := len(parts) - 1; i >= 0; i-- {
				reversed = append(reversed, parts[i])
			}
			joined := strings.Join(reversed, "/")
			if joined == "" {
				joined = "/"
			}
			return domain.ParsePath(joined)
		}
		parts = append(parts, parent.name)
		cur = parent
	}
	return "", domain.NewInternalError(domain.CodeInvariantViolation, "node parent chain exceeds maximum depth", nil)
}

// GetNode returns one node's metadata at its current revision. Tombstones
// are invisible: they report NotFound so historical markers never leak into
// live reads.
func (db *DB) GetNode(ctx context.Context, ns domain.NamespaceID, id domain.NodeID) (domain.Node, error) {
	if err := ctx.Err(); err != nil {
		return domain.Node{}, err
	}
	tx, err := db.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.Node{}, err
	}
	defer tx.Rollback()
	n, err := scanNodeRow(tx.QueryRowContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE id=? AND namespace_id=?`, id.String(), ns.String()))
	if err == sql.ErrNoRows {
		return domain.Node{}, domain.NewNotFoundError(domain.CodeNodeNotFound, "node not found", map[string]string{"node_id": id.String()})
	}
	if err != nil {
		return domain.Node{}, fmt.Errorf("sqlite: get node: %w", err)
	}
	if n.tombstone != 0 {
		return domain.Node{}, domain.NewNotFoundError(domain.CodeNodeNotFound, "node is deleted", map[string]string{"node_id": id.String()})
	}
	if err := tx.Commit(); err != nil {
		return domain.Node{}, fmt.Errorf("sqlite: get node commit: %w", err)
	}
	return toNode(n)
}

// LookupPath resolves an absolute path through the overlay: user over
// shared over baseline. A tombstone at any level hides the lower scopes
// instead of falling through.
func (db *DB) LookupPath(ctx context.Context, ns domain.NamespaceID, p domain.ValidPath) (domain.Node, error) {
	if err := ctx.Err(); err != nil {
		return domain.Node{}, err
	}
	if p.String() == "" {
		return domain.Node{}, domain.NewValidationError(domain.CodeInvalidPath, "path is empty", nil)
	}
	tx, err := db.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.Node{}, err
	}
	defer tx.Rollback()
	self, err := loadNamespace(ctx, tx, ns.String())
	if err != nil {
		return domain.Node{}, err
	}
	chain, err := overlayChain(ctx, tx, self)
	if err != nil {
		return domain.Node{}, err
	}
	for _, scope := range chain {
		res, err := resolveInScope(ctx, tx, scope.id, p)
		if err != nil {
			return domain.Node{}, err
		}
		switch {
		case res.found:
			if err := tx.Commit(); err != nil {
				return domain.Node{}, fmt.Errorf("sqlite: lookup commit: %w", err)
			}
			return toNode(res.node)
		case res.blocked:
			return domain.Node{}, domain.NewNotFoundError(domain.CodeNodeNotFound, "path is deleted in this view", map[string]string{"path": p.String()})
		}
	}
	return domain.Node{}, domain.NewNotFoundError(domain.CodeNodeNotFound, "path not found", map[string]string{"path": p.String()})
}

// ListDirectory returns the merged direct children of the directory at the
// same path across the overlay, paged by child name. Higher scopes win by
// name; tombstones hide lower-scope names. The cursor is the last returned
// name, empty when the listing is complete.
func (db *DB) ListDirectory(ctx context.Context, ns domain.NamespaceID, dir domain.NodeID, limit int, cursor string) ([]domain.Node, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	tx, err := db.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	self, err := loadNamespace(ctx, tx, ns.String())
	if err != nil {
		return nil, "", err
	}
	dirRow, err := scanNodeRow(tx.QueryRowContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE id=? AND namespace_id=?`, dir.String(), ns.String()))
	if err == sql.ErrNoRows {
		return nil, "", domain.NewNotFoundError(domain.CodeNodeNotFound, "directory not found", map[string]string{"node_id": dir.String()})
	}
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: load directory: %w", err)
	}
	if dirRow.tombstone != 0 {
		return nil, "", domain.NewNotFoundError(domain.CodeNodeNotFound, "directory is deleted", map[string]string{"node_id": dir.String()})
	}
	if dirRow.kind != "dir" {
		return nil, "", domain.NewValidationError(domain.CodeInvalidInput, "node is not a directory", map[string]string{"node_id": dir.String()})
	}
	dirPath, err := nodePath(ctx, tx, self.id, dirRow)
	if err != nil {
		return nil, "", err
	}
	chain, err := overlayChain(ctx, tx, self)
	if err != nil {
		return nil, "", err
	}
	// Merge lowest scope first so higher scopes overwrite by name.
	merged := map[string]nodeRow{}
	live := map[string]bool{}
	for i := len(chain) - 1; i >= 0; i-- {
		scope := chain[i]
		res, err := resolveInScope(ctx, tx, scope.id, dirPath)
		if err != nil {
			return nil, "", err
		}
		if !res.found || res.node.kind != "dir" {
			continue
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT `+nodeColumns+` FROM nodes WHERE namespace_id=? AND parent_id=?`, scope.id, res.node.id)
		if err != nil {
			return nil, "", fmt.Errorf("sqlite: list children: %w", err)
		}
		children, err := scanNodeRows(rows)
		rows.Close()
		if err != nil {
			return nil, "", err
		}
		for _, child := range children {
			if child.tombstone != 0 {
				delete(merged, child.name)
				live[child.name] = false
				continue
			}
			merged[child.name] = child
			live[child.name] = true
		}
	}
	names := make([]string, 0, len(merged))
	for name, ok := range live {
		if ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	page := make([]domain.Node, 0, limit)
	next := ""
	for _, name := range names {
		if cursor != "" && name <= cursor {
			continue
		}
		if len(page) >= limit {
			break
		}
		node, err := toNode(merged[name])
		if err != nil {
			return nil, "", err
		}
		page = append(page, node)
	}
	if len(page) > 0 && len(page) < countAfter(names, cursor) {
		next = page[len(page)-1].Name
	}
	if err := tx.Commit(); err != nil {
		return nil, "", fmt.Errorf("sqlite: list commit: %w", err)
	}
	return page, next, nil
}

func countAfter(names []string, cursor string) int {
	n := 0
	for _, name := range names {
		if cursor == "" || name > cursor {
			n++
		}
	}
	return n
}

// Commit applies every mutation in cs atomically after verifying read
// dependencies and expected revisions in one short transaction. A stale
// expected revision fails with a conflict DomainError and applies nothing;
// commits never silently overwrite another session's changes.
//
// Scope policy is enforced per mutation: baseline targets are denied (the
// seed is immutable through turns), shared targets require an enabled
// sharing policy, and user/session targets are the caller's own namespaces
// as routed by the trusted application layer.
func (db *DB) Commit(ctx context.Context, cs domain.ChangeSet, policy domain.ScopePolicy) (domain.Revision, error) {
	return db.commit(ctx, cs, policy, true)
}

// Provision applies a ChangeSet with the same atomic machinery as Commit
// but without scope-policy authorization. It exists for first-install
// baseline seeding only: the runtime turn path must use Commit so the
// baseline stays immutable after provisioning. Restores use it the same
// way; every provisioned commit is still recorded in the commits table.
func (db *DB) Provision(ctx context.Context, cs domain.ChangeSet) (domain.Revision, error) {
	return db.commit(ctx, cs, domain.DefaultScopePolicy(), false)
}

func (db *DB) commit(ctx context.Context, cs domain.ChangeSet, policy domain.ScopePolicy, enforcePolicy bool) (domain.Revision, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// Once permanent recording has failed, refuse new semantic work with a
	// typed error rather than attempting another write that may be lost.
	if err := db.guard.RefuseRecording(); err != nil {
		return 0, err
	}
	if len(cs.Mutations) > maxMutations {
		return 0, domain.NewLimitError(domain.CodeOutputTooLarge, "change set exceeds mutation bound", map[string]string{"count": itoa(len(cs.Mutations))})
	}
	if len(cs.ReadDependencies) > maxReadDependencies {
		return 0, domain.NewLimitError(domain.CodeOutputTooLarge, "change set exceeds read-dependency bound", map[string]string{"count": itoa(len(cs.ReadDependencies))})
	}
	if cs.TurnID.IsZero() || cs.AttemptID.IsZero() {
		return 0, domain.NewValidationError(domain.CodeInvalidInput, "change set requires turn_id and attempt_id", nil)
	}
	now := time.Now().UnixMilli()
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO commits(turn_id, attempt_id, timestamp, mutation_count) VALUES(?,?,?,?)`,
		cs.TurnID.String(), cs.AttemptID.String(), cs.Timestamp, len(cs.Mutations))
	if err != nil {
		if refusal := db.guard.ObserveWriteErr(err); refusal != nil {
			return 0, refusal
		}
		return 0, fmt.Errorf("sqlite: record commit: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("sqlite: commit sequence: %w", err)
	}
	for i, dep := range cs.ReadDependencies {
		if err := checkReadDep(ctx, tx, dep); err != nil {
			return 0, fmt.Errorf("sqlite: read dependency %d: %w", i, err)
		}
	}
	for i, m := range cs.Mutations {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if err := applyMutation(ctx, tx, m, policy, enforcePolicy, seq, now); err != nil {
			if refusal := db.guard.ObserveWriteErr(err); refusal != nil {
				return 0, refusal
			}
			return 0, fmt.Errorf("sqlite: mutation %d (%s %s): %w", i, m.Type, m.Path, err)
		}
	}
	if err := tx.Commit(); err != nil {
		if isBusy(err) {
			return 0, domain.NewUnavailableError(domain.CodeDatabaseUnavailable, "world store is busy", nil, err)
		}
		// A durable-write failure (read-only, full disk, I/O, corruption)
		// takes permanent recording out of service; report the typed refusal
		// so semantic work stops instead of continuing without logs.
		if refusal := db.guard.ObserveWriteErr(err); refusal != nil {
			return 0, refusal
		}
		return 0, fmt.Errorf("sqlite: commit: %w", err)
	}
	db.guard.ObserveWriteSuccess()
	return domain.Revision(seq), nil
}

// checkReadDep verifies one optimistic-concurrency read: presence
// dependencies re-check revision, kind, and directory membership; absence
// dependencies fail when the checked node has since appeared.
func checkReadDep(ctx context.Context, q querier, dep domain.ReadDependency) error {
	if dep.NodeID.IsZero() {
		return domain.NewValidationError(domain.CodeInvalidInput, "read dependency requires node_id", nil)
	}
	n, err := scanNodeRow(q.QueryRowContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE id=?`, dep.NodeID.String()))
	switch {
	case err == sql.ErrNoRows:
		if dep.IsAbsence {
			return checkDepParent(ctx, q, dep)
		}
		return domain.NewConflictError(domain.CodeStaleRead, "read node no longer exists", map[string]string{"node_id": dep.NodeID.String()})
	case err != nil:
		return fmt.Errorf("sqlite: read dependency lookup: %w", err)
	}
	if n.tombstone != 0 {
		if dep.IsAbsence {
			return checkDepParent(ctx, q, dep)
		}
		return domain.NewConflictError(domain.CodeStaleRead, "read node was deleted", map[string]string{"node_id": dep.NodeID.String()})
	}
	if dep.IsAbsence {
		return domain.NewConflictError(domain.CodeStaleRead, "absent node has since appeared", map[string]string{"node_id": dep.NodeID.String()})
	}
	if domain.Revision(n.revision) != dep.Revision {
		return domain.NewConflictError(domain.CodeRevisionMismatch, "node revision changed under the turn",
			map[string]string{"node_id": dep.NodeID.String(), "expected": itoa(int(dep.Revision)), "actual": itoa(int(n.revision))})
	}
	if kind, kerr := domain.ParseNodeKind(n.kind); kerr != nil || kind != dep.Kind {
		return domain.NewConflictError(domain.CodeStaleRead, "node kind changed under the turn", map[string]string{"node_id": dep.NodeID.String()})
	}
	if !dep.DirMemberOf.IsZero() && n.parentID != dep.DirMemberOf.String() {
		return domain.NewConflictError(domain.CodeStaleRead, "node moved directories under the turn", map[string]string{"node_id": dep.NodeID.String()})
	}
	return nil
}

// checkDepParent keeps absence checks honest: the directory that was listed
// must still exist, otherwise the listing the turn validated against is gone.
func checkDepParent(ctx context.Context, q querier, dep domain.ReadDependency) error {
	if dep.DirMemberOf.IsZero() {
		return nil
	}
	parent, err := scanNodeRow(q.QueryRowContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE id=?`, dep.DirMemberOf.String()))
	switch {
	case err == sql.ErrNoRows:
		return domain.NewConflictError(domain.CodeStaleRead, "listed directory no longer exists", map[string]string{"dir": dep.DirMemberOf.String()})
	case err != nil:
		return fmt.Errorf("sqlite: read dependency parent: %w", err)
	}
	if parent.tombstone != 0 || parent.kind != "dir" {
		return domain.NewConflictError(domain.CodeStaleRead, "listed directory is gone", map[string]string{"dir": dep.DirMemberOf.String()})
	}
	return nil
}

func applyMutation(ctx context.Context, tx *sql.Tx, m domain.Mutation, policy domain.ScopePolicy, enforcePolicy bool, seq, now int64) error {
	if m.NamespaceID.IsZero() {
		return domain.NewValidationError(domain.CodeInvalidMutation, "mutation requires namespace_id", nil)
	}
	if m.Path.String() == "" {
		return domain.NewValidationError(domain.CodeInvalidPath, "mutation requires path", nil)
	}
	if _, err := domain.ParsePath(m.Path.String()); err != nil {
		return domain.NewValidationError(domain.CodeInvalidPath, "mutation path is invalid", map[string]string{"path": m.Path.String()})
	}
	target, err := loadNamespace(ctx, tx, m.NamespaceID.String())
	if err != nil {
		return err
	}
	if enforcePolicy {
		if err := policy.AuthorizeWrite(target.scope(), true); err != nil {
			return err
		}
	}
	switch m.Type {
	case domain.MutationCreate:
		return applyCreate(ctx, tx, target, m, seq, now)
	case domain.MutationUpdate:
		return applyUpdate(ctx, tx, target, m, seq, now)
	case domain.MutationDelete:
		return applyDelete(ctx, tx, target, m, seq, now)
	case domain.MutationTombstone:
		return applyTombstone(ctx, tx, target, m, seq, now)
	default:
		return domain.NewValidationError(domain.CodeInvalidMutation, "unknown mutation type", nil)
	}
}

// resolveParentForWrite returns the target-namespace parent directory for
// p, materializing overlay copies of lower-scope ancestors (mkdir -p) so a
// user write under a shared-only directory lands in the overlay.
func resolveParentForWrite(ctx context.Context, tx *sql.Tx, target namespaceRow, chain []namespaceRow, p domain.ValidPath, seq, now int64) (nodeRow, error) {
	if p.IsRoot() {
		return nodeRow{}, domain.NewValidationError(domain.CodeInvalidMutation, "the namespace root cannot be created or replaced", nil)
	}
	parentPath := p.Parent()
	// Walk the parent prefix inside the target namespace, filling gaps.
	if parentPath.IsRoot() {
		root, found, err := resolveChild(ctx, tx, target.id, "", "")
		if err != nil || !found {
			return nodeRow{}, domain.NewInternalError(domain.CodeInvariantViolation, "namespace root is missing", nil)
		}
		if root.kind != "dir" || root.tombstone != 0 {
			return nodeRow{}, domain.NewInternalError(domain.CodeInvariantViolation, "namespace root is unusable", nil)
		}
		return root, nil
	}
	parts := strings.Split(strings.TrimPrefix(parentPath.String(), "/"), "/")
	root, found, err := resolveChild(ctx, tx, target.id, "", "")
	if err != nil {
		return nodeRow{}, err
	}
	if !found {
		return nodeRow{}, domain.NewInternalError(domain.CodeInvariantViolation, "namespace root is missing", nil)
	}
	cur := root
	prefix := ""
	for _, part := range parts {
		if prefix == "" {
			prefix = "/" + part
		} else {
			prefix = prefix + "/" + part
		}
		child, ok, err := resolveChild(ctx, tx, target.id, cur.id, part)
		if err != nil {
			return nodeRow{}, err
		}
		if ok {
			if child.tombstone != 0 {
				return nodeRow{}, domain.NewConflictError(domain.CodeStaleRead, "parent path is deleted in this view", map[string]string{"path": prefix})
			}
			if child.kind != "dir" {
				return nodeRow{}, domain.NewValidationError(domain.CodeInvalidMutation, "parent path is not a directory", map[string]string{"path": prefix})
			}
			cur = child
			continue
		}
		// Gap in the overlay: mirror the lower-scope directory when there
		// is one, otherwise fail so the turn stages the missing parent
		// explicitly instead of inventing hierarchy silently.
		prefixPath, perr := domain.ParsePath(prefix)
		if perr != nil {
			return nodeRow{}, domain.NewValidationError(domain.CodeInvalidPath, "parent path is invalid", nil)
		}
		var mirror *nodeRow
		for _, scope := range chain[1:] {
			res, rerr := resolveInScope(ctx, tx, scope.id, prefixPath)
			if rerr != nil {
				return nodeRow{}, rerr
			}
			if res.found && res.node.kind == "dir" {
				mirror = &res.node
				break
			}
			if res.blocked {
				break
			}
		}
		if mirror == nil {
			return nodeRow{}, domain.NewValidationError(domain.CodeInvalidMutation, "parent directory does not exist; stage it first", map[string]string{"path": prefix})
		}
		overlayID, idErr := newNodeID()
		if idErr != nil {
			return nodeRow{}, idErr
		}
		overlay := nodeRow{
			id: overlayID.String(), namespaceID: target.id, parentID: cur.id, name: part,
			kind: "dir", revision: 1, mode: mirror.mode, uid: mirror.uid, gid: mirror.gid,
			modTime: now, accessTime: now, changeTime: now,
		}
		if err := insertNodeTx(ctx, tx, overlay, seq, now); err != nil {
			return nodeRow{}, err
		}
		cur = overlay
	}
	return cur, nil
}

func applyCreate(ctx context.Context, tx *sql.Tx, target namespaceRow, m domain.Mutation, seq, now int64) error {
	if !m.Kind.IsValid() {
		return domain.NewValidationError(domain.CodeInvalidMutation, "create requires a valid kind", nil)
	}
	chain, err := overlayChain(ctx, tx, target)
	if err != nil {
		return err
	}
	parent, err := resolveParentForWrite(ctx, tx, target, chain, m.Path, seq, now)
	if err != nil {
		return err
	}
	if existing, ok, err := resolveChild(ctx, tx, target.id, parent.id, m.Path.Base()); err != nil {
		return err
	} else if ok {
		if existing.tombstone == 0 {
			return domain.NewConflictError(domain.CodeDuplicateKey, "node already exists at path", map[string]string{"path": m.Path.String()})
		}
		// A whiteout marker occupies the slot: upcycle it into the live
		// overlay copy so re-creation after a tombstone keeps one history
		// chain for the path instead of colliding with its own marker.
		if err := checkContentRef(ctx, tx, m.Kind, m.Content); err != nil {
			return err
		}
		symlinkTarget := firstNonEmpty(m.SymlinkTarget, m.Metadata.SymlinkTarget)
		if m.Kind == domain.NodeKindSymlink && symlinkTarget == "" {
			return domain.NewValidationError(domain.CodeInvalidMutation, "symlink requires a target", nil)
		}
		meta := m.Metadata
		if meta.ModTime == 0 {
			meta.ModTime, meta.AccessTime, meta.ChangeTime = now, now, now
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE nodes SET kind=?, revision=?, mode=?, uid=?, gid=?, mod_time=?, access_time=?,
			 change_time=?, symlink_target=?, content_hash=?, content_size=?, content_media=?,
			 tombstone=0, updated_at=? WHERE id=?`,
			m.Kind.String(), existing.revision+1,
			meta.Mode, meta.UID, meta.GID, meta.ModTime, meta.AccessTime, meta.ChangeTime,
			symlinkTarget, contentHashOf(m.Kind, m.Content), contentSizeOf(m.Kind, m.Content),
			contentMediaOf(m.Kind, m.Content), now, existing.id); err != nil {
			return fmt.Errorf("sqlite: revive tombstone: %w", err)
		}
		existing.kind = m.Kind.String()
		existing.revision++
		existing.mode, existing.uid, existing.gid = meta.Mode, meta.UID, meta.GID
		existing.modTime, existing.accessTime, existing.changeTime = meta.ModTime, meta.AccessTime, meta.ChangeTime
		existing.symlinkTarget = symlinkTarget
		existing.contentHash = contentHashOf(m.Kind, m.Content)
		existing.contentSize = contentSizeOf(m.Kind, m.Content)
		existing.contentMedia = contentMediaOf(m.Kind, m.Content)
		existing.tombstone = 0
		return insertVersionTx(ctx, tx, existing, seq)
	}
	if err := checkContentRef(ctx, tx, m.Kind, m.Content); err != nil {
		return err
	}
	symlinkTarget := m.SymlinkTarget
	if symlinkTarget == "" {
		symlinkTarget = m.Metadata.SymlinkTarget
	}
	if m.Kind == domain.NodeKindSymlink && symlinkTarget == "" {
		return domain.NewValidationError(domain.CodeInvalidMutation, "symlink requires a target", nil)
	}
	id, err := newNodeID()
	if err != nil {
		return err
	}
	n := nodeRow{
		id: id.String(), namespaceID: target.id, parentID: parent.id, name: m.Path.Base(),
		kind: m.Kind.String(), revision: 1,
		mode: m.Metadata.Mode, uid: m.Metadata.UID, gid: m.Metadata.GID,
		modTime: m.Metadata.ModTime, accessTime: m.Metadata.AccessTime, changeTime: m.Metadata.ChangeTime,
		symlinkTarget: symlinkTarget,
		contentHash:   contentHashOf(m.Kind, m.Content),
		contentSize:   contentSizeOf(m.Kind, m.Content),
		contentMedia:  contentMediaOf(m.Kind, m.Content),
	}
	if n.modTime == 0 {
		n.modTime, n.accessTime, n.changeTime = now, now, now
	}
	return insertNodeTx(ctx, tx, n, seq, now)
}

// locateForWrite finds the mutation's node: first in the target namespace,
// otherwise the overlay-visible lower-scope row for copy-up/tombstone work.
func locateForWrite(ctx context.Context, tx *sql.Tx, target namespaceRow, m domain.Mutation) (nodeRow, bool, error) {
	if m.NodeID == nil || m.NodeID.IsZero() {
		return nodeRow{}, false, domain.NewValidationError(domain.CodeInvalidMutation, "mutation requires node_id", nil)
	}
	if m.ExpectedRev == 0 {
		return nodeRow{}, false, domain.NewValidationError(domain.CodeInvalidMutation, "mutation requires expected_rev", nil)
	}
	n, err := scanNodeRow(tx.QueryRowContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE id=?`, m.NodeID.String()))
	switch {
	case err == sql.ErrNoRows:
		// It may have existed and been removed: history distinguishes a
		// stale delete (conflict) from an unknown id (not found).
		var count int
		if cerr := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM node_versions WHERE node_id=?`, m.NodeID.String()).Scan(&count); cerr != nil {
			return nodeRow{}, false, fmt.Errorf("sqlite: version history lookup: %w", cerr)
		}
		if count > 0 {
			return nodeRow{}, false, domain.NewConflictError(domain.CodeStaleRead, "node was deleted", map[string]string{"node_id": m.NodeID.String()})
		}
		return nodeRow{}, false, domain.NewNotFoundError(domain.CodeNodeNotFound, "node not found", map[string]string{"node_id": m.NodeID.String()})
	case err != nil:
		return nodeRow{}, false, fmt.Errorf("sqlite: locate node: %w", err)
	}
	if n.tombstone != 0 {
		return nodeRow{}, false, domain.NewConflictError(domain.CodeStaleRead, "node was deleted", map[string]string{"node_id": m.NodeID.String()})
	}
	if domain.Revision(n.revision) != m.ExpectedRev {
		return nodeRow{}, false, domain.NewConflictError(domain.CodeRevisionMismatch, "node revision mismatch",
			map[string]string{"node_id": m.NodeID.String(), "expected": itoa(int(m.ExpectedRev)), "actual": itoa(int(n.revision))})
	}
	actualPath, err := nodePath(ctx, tx, n.namespaceID, n)
	if err != nil {
		return nodeRow{}, false, err
	}
	if actualPath.String() != m.Path.String() {
		return nodeRow{}, false, domain.NewConflictError(domain.CodeStaleRead, "node moved under the turn",
			map[string]string{"node_id": m.NodeID.String(), "path": m.Path.String()})
	}
	return n, n.namespaceID == target.id, nil
}

func applyUpdate(ctx context.Context, tx *sql.Tx, target namespaceRow, m domain.Mutation, seq, now int64) error {
	n, own, err := locateForWrite(ctx, tx, target, m)
	if err != nil {
		return err
	}
	if err := checkContentRef(ctx, tx, domain.NodeKindFile, m.Content); err != nil {
		// Updates to directories carry an empty content ref; only files pay
		// the existence check.
		if n.kind != "dir" || m.Content.Size != 0 {
			return err
		}
	}
	symlinkTarget := m.SymlinkTarget
	if symlinkTarget == "" {
		symlinkTarget = m.Metadata.SymlinkTarget
	}
	if !own {
		// Copy-up: the visible node lives in a lower scope, so the update
		// lands as an overlay copy in the target namespace at rev+1.
		chain, cerr := overlayChain(ctx, tx, target)
		if cerr != nil {
			return cerr
		}
		parent, perr := resolveParentForWrite(ctx, tx, target, chain, m.Path, seq, now)
		if perr != nil {
			return perr
		}
		id, idErr := newNodeID()
		if idErr != nil {
			return idErr
		}
		meta := m.Metadata
		if meta.ModTime == 0 {
			meta.ModTime, meta.AccessTime, meta.ChangeTime = now, now, now
		}
		overlay := nodeRow{
			id: id.String(), namespaceID: target.id, parentID: parent.id, name: n.name,
			kind: n.kind, revision: n.revision + 1,
			mode: meta.Mode, uid: meta.UID, gid: meta.GID,
			modTime: meta.ModTime, accessTime: meta.AccessTime, changeTime: meta.ChangeTime,
			symlinkTarget: firstNonEmpty(symlinkTarget, n.symlinkTarget),
			contentHash:   contentHashOf(domain.NodeKindFile, m.Content),
			contentSize:   contentSizeOf(domain.NodeKindFile, m.Content),
			contentMedia:  contentMediaOf(domain.NodeKindFile, m.Content),
			shadowOf:      n.id,
		}
		if overlay.kind == "dir" {
			overlay.contentHash, overlay.contentSize, overlay.contentMedia = "", 0, ""
		}
		return insertNodeTx(ctx, tx, overlay, seq, now)
	}
	meta := m.Metadata
	if meta.ModTime == 0 {
		meta.ModTime, meta.AccessTime, meta.ChangeTime = now, now, now
	}
	n.revision++
	n.mode, n.uid, n.gid = meta.Mode, meta.UID, meta.GID
	n.modTime, n.accessTime, n.changeTime = meta.ModTime, meta.AccessTime, meta.ChangeTime
	if n.kind == "file" {
		n.contentHash = contentHashOf(domain.NodeKindFile, m.Content)
		n.contentSize = contentSizeOf(domain.NodeKindFile, m.Content)
		n.contentMedia = contentMediaOf(domain.NodeKindFile, m.Content)
	}
	if n.kind == "symlink" && symlinkTarget != "" {
		n.symlinkTarget = symlinkTarget
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE nodes SET revision=?, mode=?, uid=?, gid=?, mod_time=?, access_time=?, change_time=?,
		 symlink_target=?, content_hash=?, content_size=?, content_media=?, updated_at=? WHERE id=?`,
		n.revision, n.mode, n.uid, n.gid, n.modTime, n.accessTime, n.changeTime,
		n.symlinkTarget, n.contentHash, n.contentSize, n.contentMedia, now, n.id); err != nil {
		return fmt.Errorf("sqlite: update node: %w", err)
	}
	return insertVersionTx(ctx, tx, n, seq)
}

func applyDelete(ctx context.Context, tx *sql.Tx, target namespaceRow, m domain.Mutation, seq, _ int64) error {
	n, own, err := locateForWrite(ctx, tx, target, m)
	if err != nil {
		return err
	}
	if !own {
		return domain.NewValidationError(domain.CodeInvalidMutation, "delete targets another scope; use a tombstone to hide shared content", map[string]string{"node_id": m.NodeID.String()})
	}
	if !m.Path.IsRoot() {
		// Refuse to remove a non-empty directory so a stale listing cannot
		// silently orphan children; the turn must delete children first.
		if n.kind == "dir" {
			var count int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM nodes WHERE namespace_id=? AND parent_id=? AND tombstone=0`,
				target.id, n.id).Scan(&count); err != nil {
				return fmt.Errorf("sqlite: count children: %w", err)
			}
			if count > 0 {
				return domain.NewConflictError(domain.CodeStaleRead, "directory is not empty", map[string]string{"node_id": n.id})
			}
		}
	} else {
		return domain.NewValidationError(domain.CodeInvalidMutation, "the namespace root cannot be deleted", nil)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE id=?`, n.id); err != nil {
		return fmt.Errorf("sqlite: delete node: %w", err)
	}
	n.revision++
	n.tombstone = 1
	return insertVersionTx(ctx, tx, n, seq)
}

// applyTombstone hides the overlay-visible node at path behind a whiteout
// marker in the target namespace. Later reads in that view report NotFound
// while lower scopes keep their rows for other views and for history.
func applyTombstone(ctx context.Context, tx *sql.Tx, target namespaceRow, m domain.Mutation, seq, now int64) error {
	n, own, err := locateForWrite(ctx, tx, target, m)
	if err != nil {
		return err
	}
	if own {
		n.revision++
		n.tombstone = 1
		if _, err := tx.ExecContext(ctx,
			`UPDATE nodes SET revision=?, tombstone=1, change_time=?, updated_at=? WHERE id=?`,
			n.revision, now, now, n.id); err != nil {
			return fmt.Errorf("sqlite: mark tombstone: %w", err)
		}
		return insertVersionTx(ctx, tx, n, seq)
	}
	chain, err := overlayChain(ctx, tx, target)
	if err != nil {
		return err
	}
	parent, err := resolveParentForWrite(ctx, tx, target, chain, m.Path, seq, now)
	if err != nil {
		return err
	}
	if _, ok, err := resolveChild(ctx, tx, target.id, parent.id, n.name); err != nil {
		return err
	} else if ok {
		return domain.NewConflictError(domain.CodeDuplicateKey, "overlay entry already exists at path", map[string]string{"path": m.Path.String()})
	}
	id, err := newNodeID()
	if err != nil {
		return err
	}
	marker := nodeRow{
		id: id.String(), namespaceID: target.id, parentID: parent.id, name: n.name,
		kind: n.kind, revision: n.revision + 1,
		mode: n.mode, uid: n.uid, gid: n.gid,
		modTime: n.modTime, accessTime: n.accessTime, changeTime: now,
		symlinkTarget: n.symlinkTarget,
		tombstone:     1,
		shadowOf:      n.id,
	}
	return insertNodeTx(ctx, tx, marker, seq, now)
}

// checkContentRef verifies that a file content reference points at stored
// exact bytes. Directories and symlinks carry no content; empty files carry
// the zero reference without a contents row.
func checkContentRef(ctx context.Context, q querier, kind domain.NodeKind, ref domain.ContentRef) error {
	if kind != domain.NodeKindFile || ref.Size == 0 {
		return nil
	}
	if ref.Hash.IsZero() {
		return domain.NewValidationError(domain.CodeInvalidMutation, "non-empty content requires a hash", nil)
	}
	var size int64
	err := q.QueryRowContext(ctx, `SELECT size FROM contents WHERE hash=?`, ref.Hash.String()).Scan(&size)
	if err == sql.ErrNoRows {
		return domain.NewNotFoundError(domain.CodeContentNotFound, "content not staged", map[string]string{"hash": ref.Hash.String()})
	}
	if err != nil {
		return fmt.Errorf("sqlite: check content: %w", err)
	}
	if size != ref.Size {
		return domain.NewValidationError(domain.CodeInvalidMutation, "content size does not match staged bytes", map[string]string{"hash": ref.Hash.String()})
	}
	return nil
}

func contentHashOf(kind domain.NodeKind, ref domain.ContentRef) string {
	if kind != domain.NodeKindFile || ref.Size == 0 {
		return ""
	}
	return ref.Hash.String()
}

func contentSizeOf(kind domain.NodeKind, ref domain.ContentRef) int64 {
	if kind != domain.NodeKindFile {
		return 0
	}
	return ref.Size
}

func contentMediaOf(kind domain.NodeKind, ref domain.ContentRef) string {
	if kind != domain.NodeKindFile || ref.Size == 0 {
		return ""
	}
	if ref.MediaType == "" {
		return "application/octet-stream"
	}
	return ref.MediaType
}

func insertNodeTx(ctx context.Context, tx *sql.Tx, n nodeRow, seq, now int64) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO nodes(id, namespace_id, parent_id, name, kind, revision, mode, uid, gid,
			mod_time, access_time, change_time, symlink_target,
			content_hash, content_size, content_media, tombstone, shadow_of, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		n.id, n.namespaceID, n.parentID, n.name, n.kind, n.revision,
		n.mode, n.uid, n.gid, n.modTime, n.accessTime, n.changeTime,
		n.symlinkTarget, n.contentHash, n.contentSize, n.contentMedia,
		n.tombstone, n.shadowOf, now); err != nil {
		if isUniqueViolation(err) {
			return domain.NewConflictError(domain.CodeDuplicateKey, "node already exists at path", map[string]string{"path": n.name})
		}
		return fmt.Errorf("sqlite: insert node: %w", err)
	}
	return insertVersionTx(ctx, tx, n, seq)
}

func insertVersionTx(ctx context.Context, tx *sql.Tx, n nodeRow, seq int64) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO node_versions(node_id, revision, namespace_id, parent_id, name, kind, mode, uid, gid,
			mod_time, access_time, change_time, symlink_target,
			content_hash, content_size, content_media, tombstone, shadow_of, commit_seq)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		n.id, n.revision, n.namespaceID, n.parentID, n.name, n.kind,
		n.mode, n.uid, n.gid, n.modTime, n.accessTime, n.changeTime,
		n.symlinkTarget, n.contentHash, n.contentSize, n.contentMedia,
		n.tombstone, n.shadowOf, seq); err != nil {
		return fmt.Errorf("sqlite: insert node version: %w", err)
	}
	return nil
}

// Put stores exact bytes under their content-derived identity and returns
// the reference mutations use. Identical bytes share one row; empty input
// returns the zero reference without touching the table.
func (db *DB) Put(ctx context.Context, data []byte, mediaType string) (domain.ContentRef, error) {
	if err := ctx.Err(); err != nil {
		return domain.ContentRef{}, err
	}
	if int64(len(data)) > db.opts.MaxContentBytes {
		return domain.ContentRef{}, domain.NewLimitError(domain.CodeOutputTooLarge, "content exceeds size bound", map[string]string{"size": itoa(len(data))})
	}
	if len(data) == 0 {
		return domain.EmptyContentRef(), nil
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	id, err := contentIDFor(data)
	if err != nil {
		return domain.ContentRef{}, err
	}
	if _, err := db.sql.ExecContext(ctx,
		`INSERT INTO contents(hash, size, media_type, bytes) VALUES(?,?,?,?)
		 ON CONFLICT(hash) DO NOTHING`,
		id.String(), len(data), mediaType, data); err != nil {
		if refusal := db.guard.ObserveWriteErr(err); refusal != nil {
			return domain.ContentRef{}, refusal
		}
		return domain.ContentRef{}, fmt.Errorf("sqlite: store content: %w", err)
	}
	db.guard.ObserveWriteSuccess()
	return domain.ContentRef{Hash: id, Size: int64(len(data)), MediaType: mediaType}, nil
}

// Get returns the exact stored bytes for ref, or the bounded [offset,
// offset+length) range. A negative length reads to the end; the bytes keep
// NUL and invalid-UTF-8 sequences untouched as opaque BLOB data.
func (db *DB) Get(ctx context.Context, ref domain.ContentRef, offset, length int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ref.IsEmpty() {
		if !ref.Hash.IsZero() {
			return nil, domain.NewValidationError(domain.CodeInvalidInput, "empty content must use the zero hash", nil)
		}
		return []byte{}, nil
	}
	if offset < 0 {
		return nil, domain.NewValidationError(domain.CodeInvalidInput, "content offset cannot be negative", nil)
	}
	var data []byte
	var size int64
	var media string
	err := db.sql.QueryRowContext(ctx, `SELECT bytes, size, media_type FROM contents WHERE hash=?`, ref.Hash.String()).Scan(&data, &size, &media)
	if err == sql.ErrNoRows {
		return nil, domain.NewNotFoundError(domain.CodeContentNotFound, "content not found", map[string]string{"hash": ref.Hash.String()})
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: load content: %w", err)
	}
	if offset > int64(len(data)) {
		return nil, domain.NewValidationError(domain.CodeInvalidInput, "content offset exceeds size", nil)
	}
	end := int64(len(data))
	if length >= 0 && offset+length < end {
		end = offset + length
	}
	out := make([]byte, end-offset)
	copy(out, data[offset:end])
	return out, nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	// modernc.org/sqlite surfaces constraint failures with SQLITE_CONSTRAINT
	// text markers; match case-insensitively without importing the driver.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "constraint") || strings.Contains(msg, "unique")
}

func isBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "busy") || strings.Contains(msg, "locked")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
