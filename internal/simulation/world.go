package simulation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"j0s.at/vibeshell/internal/domain"
)

// nodeInfo is the model-facing view of a world node. It exposes the fields a
// tool result needs without leaking internal zero-valued identities.
type nodeInfo struct {
	ID            domain.NodeID     `json:"id"`
	Scope         domain.Scope      `json:"scope"`
	Path          domain.ValidPath  `json:"path"`
	Kind          domain.NodeKind   `json:"kind"`
	Revision      domain.Revision   `json:"revision"`
	Mode          uint32            `json:"mode"`
	UID           uint32            `json:"uid"`
	GID           uint32            `json:"gid"`
	Size          int64             `json:"size"`
	ContentHash   *domain.ContentID `json:"content_hash,omitempty"`
	MediaType     string            `json:"media_type,omitempty"`
	SymlinkTarget string            `json:"symlink_target,omitempty"`
}

func newNodeInfo(scope domain.Scope, path domain.ValidPath, node domain.Node) nodeInfo {
	info := nodeInfo{
		ID:            node.ID,
		Scope:         scope,
		Path:          path,
		Kind:          node.Kind,
		Revision:      node.Revision,
		Mode:          node.Metadata.Mode,
		UID:           node.Metadata.UID,
		GID:           node.Metadata.GID,
		Size:          node.Content.Size,
		SymlinkTarget: node.Metadata.SymlinkTarget,
	}
	if !node.Content.Hash.IsZero() {
		hash := node.Content.Hash
		info.ContentHash = &hash
		info.MediaType = node.Content.MediaType
	}
	return info
}

// contentRefInfo is the model-facing view of an immutable content reference.
type contentRefInfo struct {
	Size      int64             `json:"size"`
	MediaType string            `json:"media_type,omitempty"`
	Hash      *domain.ContentID `json:"hash,omitempty"`
}

func newContentRefInfo(ref domain.ContentRef) contentRefInfo {
	info := contentRefInfo{Size: ref.Size, MediaType: ref.MediaType}
	if !ref.Hash.IsZero() {
		hash := ref.Hash
		info.Hash = &hash
	}
	return info
}

// resolveNode maps the model-supplied scope name to the trusted namespace,
// authorizes the read against the injected policy, and resolves the path.
func (r *Registry) resolveNode(ctx context.Context, call CallContext, scopeName, pathStr string) (domain.Node, domain.Scope, domain.ValidPath, error) {
	ns, scope, err := call.scopeNamespace(scopeName)
	if err != nil {
		return domain.Node{}, 0, "", err
	}
	path, err := domain.ParsePath(pathStr)
	if err != nil {
		return domain.Node{}, 0, "", err
	}
	if err := call.Policy.AuthorizeRead(scope, scope == domain.ScopeUser); err != nil {
		return domain.Node{}, 0, "", err
	}
	node, err := r.deps.World.LookupPath(ctx, ns, path)
	if err != nil {
		return domain.Node{}, 0, "", err
	}
	return node, scope, path, nil
}

// ---------------------------------------------------------------------------
// world.lookup / world.list
// ---------------------------------------------------------------------------

type worldLookupArgs struct {
	Scope string `json:"scope"`
	Path  string `json:"path"`
}

type worldLookupResult struct {
	Scope domain.Scope `json:"scope"`
	Node  nodeInfo     `json:"node"`
}

func (r *Registry) worldLookup(ctx context.Context, call CallContext, raw json.RawMessage) (json.RawMessage, error) {
	var args worldLookupArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	node, scope, path, err := r.resolveNode(ctx, call, args.Scope, args.Path)
	if err != nil {
		return nil, err
	}
	return marshalResult(worldLookupResult{Scope: scope, Node: newNodeInfo(scope, path, node)})
}

type worldListArgs struct {
	Scope  string `json:"scope"`
	Dir    string `json:"dir"`
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

type worldListResult struct {
	Scope      domain.Scope     `json:"scope"`
	Dir        domain.ValidPath `json:"dir"`
	Nodes      []nodeInfo       `json:"nodes"`
	NextCursor string           `json:"next_cursor,omitempty"`
	Truncated  bool             `json:"truncated"`
}

func (r *Registry) worldList(ctx context.Context, call CallContext, raw json.RawMessage) (json.RawMessage, error) {
	var args worldListArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	ns, scope, err := call.scopeNamespace(args.Scope)
	if err != nil {
		return nil, err
	}
	dirPath, err := domain.ParsePath(args.Dir)
	if err != nil {
		return nil, err
	}
	if err := call.Policy.AuthorizeRead(scope, scope == domain.ScopeUser); err != nil {
		return nil, err
	}
	dirNode, err := r.deps.World.LookupPath(ctx, ns, dirPath)
	if err != nil {
		return nil, err
	}
	if !dirNode.IsDir() {
		return nil, domain.NewValidationError(
			domain.CodeInvalidInput, fmt.Sprintf("path %q is not a directory", dirPath), nil)
	}
	limit := clampLimit(args.Limit, DefaultResultLimit, MaxResultLimit)
	nodes, nextCursor, err := r.deps.World.ListDirectory(ctx, ns, dirNode.ID, limit, args.Cursor)
	if err != nil {
		return nil, err
	}
	infos := make([]nodeInfo, 0, len(nodes))
	for _, node := range nodes {
		infos = append(infos, newNodeInfo(scope, dirPath.Join(node.Name), node))
	}
	return marshalResult(worldListResult{
		Scope:      scope,
		Dir:        dirPath,
		Nodes:      infos,
		NextCursor: nextCursor,
		Truncated:  nextCursor != "",
	})
}

// ---------------------------------------------------------------------------
// content.read
// ---------------------------------------------------------------------------

type contentReadArgs struct {
	Scope  string `json:"scope"`
	Path   string `json:"path"`
	Offset int64  `json:"offset,omitempty"`
	Length int64  `json:"length,omitempty"`
}

type contentReadResult struct {
	Scope      domain.Scope     `json:"scope"`
	Path       domain.ValidPath `json:"path"`
	NodeID     domain.NodeID    `json:"node_id"`
	Content    []byte           `json:"content"`
	Ref        contentRefInfo   `json:"ref"`
	Offset     int64            `json:"offset"`
	Truncated  bool             `json:"truncated"`
	NextOffset int64            `json:"next_offset,omitempty"`
}

func (r *Registry) contentRead(ctx context.Context, call CallContext, raw json.RawMessage) (json.RawMessage, error) {
	var args contentReadArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	node, scope, path, err := r.resolveNode(ctx, call, args.Scope, args.Path)
	if err != nil {
		return nil, err
	}
	if !node.IsFile() {
		return nil, domain.NewValidationError(
			domain.CodeInvalidInput, fmt.Sprintf("path %q is not a regular file", path), nil)
	}
	offset := args.Offset
	if offset < 0 {
		offset = 0
	}
	length := args.Length
	if length <= 0 || length > MaxContentReadBytes {
		length = MaxContentReadBytes
	}
	data, err := r.deps.Content.Get(ctx, node.Content, offset, length)
	if err != nil {
		return nil, err
	}
	res := contentReadResult{
		Scope:   scope,
		Path:    path,
		NodeID:  node.ID,
		Content: data,
		Ref:     newContentRefInfo(node.Content),
		Offset:  offset,
	}
	if offset+int64(len(data)) < node.Content.Size {
		res.Truncated = true
		res.NextOffset = offset + int64(len(data))
	}
	return marshalResult(res)
}

// ---------------------------------------------------------------------------
// fact.lookup
// ---------------------------------------------------------------------------

// Fact categories (PLAN 7.2): typed, scoped, provenance-carrying machine
// facts stored as world nodes under /facts/<category>/<key>.
const (
	FactCategoryPackage  = "package"
	FactCategoryProcess  = "process"
	FactCategoryIdentity = "identity"
	FactCategorySystem   = "system"
)

var factCategories = map[string]bool{
	FactCategoryPackage:  true,
	FactCategoryProcess:  true,
	FactCategoryIdentity: true,
	FactCategorySystem:   true,
}

type factLookupArgs struct {
	Category string `json:"category"`
	Key      string `json:"key"`
	Scope    string `json:"scope,omitempty"`
}

type factLookupResult struct {
	Category   string             `json:"category"`
	Key        string             `json:"key"`
	Path       domain.ValidPath   `json:"path"`
	Scope      domain.Scope       `json:"scope"`
	Found      bool               `json:"found"`
	Content    []byte             `json:"content,omitempty"`
	Revision   domain.Revision    `json:"revision,omitempty"`
	Provenance *domain.Provenance `json:"provenance,omitempty"`
}

func (r *Registry) factLookup(ctx context.Context, call CallContext, raw json.RawMessage) (json.RawMessage, error) {
	var args factLookupArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if !factCategories[args.Category] {
		return nil, domain.NewValidationError(
			domain.CodeInvalidInput, fmt.Sprintf("unknown fact category %q", args.Category), nil)
	}
	if args.Key == "" {
		return nil, domain.NewValidationError(domain.CodeInvalidInput, "fact key is required", nil)
	}
	scopeName := args.Scope
	if scopeName == "" {
		scopeName = "shared" // machine facts live in the shared world by default
	}
	ns, scope, err := call.scopeNamespace(scopeName)
	if err != nil {
		return nil, err
	}
	if err := call.Policy.AuthorizeRead(scope, scope == domain.ScopeUser); err != nil {
		return nil, err
	}
	factPath := domain.MustParsePath("/facts/" + args.Category).Join(args.Key)
	if !strings.HasPrefix(factPath.String(), "/facts/") {
		return nil, domain.NewValidationError(
			domain.CodeInvalidInput, "fact key must stay under the facts root", nil)
	}
	node, err := r.deps.World.LookupPath(ctx, ns, factPath)
	if err != nil {
		if domain.IsNotFoundError(err) {
			return marshalResult(factLookupResult{
				Category: args.Category, Key: args.Key, Path: factPath, Scope: scope, Found: false,
			})
		}
		return nil, err
	}
	res := factLookupResult{
		Category: args.Category,
		Key:      args.Key,
		Path:     factPath,
		Scope:    scope,
		Found:    true,
		Revision: node.Revision,
	}
	if node.IsFile() && !node.Content.IsEmpty() {
		data, err := r.deps.Content.Get(ctx, node.Content, 0, MaxContentReadBytes)
		if err != nil {
			return nil, err
		}
		res.Content = data
	}
	provenance, err := r.factProvenance(ctx, call, scope, factPath)
	if err != nil {
		return nil, err
	}
	res.Provenance = provenance
	return marshalResult(res)
}

// factProvenance best-effort finds the origin event for a fact path: the
// latest world.materialize event in the fact's scope that names the path.
// Facts that predate event recording or come from the baseline simply carry
// no provenance.
func (r *Registry) factProvenance(ctx context.Context, call CallContext, scope domain.Scope, path domain.ValidPath) (*domain.Provenance, error) {
	rs := domain.RetrievalScope{Scopes: []domain.Scope{scope}}
	switch scope {
	case domain.ScopeSession:
		rs.SessionIDs = []domain.SessionID{call.SessionID}
	case domain.ScopeUser:
		rs.UserIDs = []domain.UserID{call.UserID}
	case domain.ScopeShared:
		rs.IncludeShared = true
	}
	res, err := r.deps.Retrieval.Query(ctx, domain.RetrievalQuery{
		Scope:  rs,
		Filter: domain.RetrievalFilter{Kinds: []domain.EventKind{domain.EventKindWorldMaterialize}},
		Pagination: domain.Pagination{
			Limit:      20,
			Descending: true,
		},
	}, call.Policy)
	if err != nil {
		return nil, err
	}
	for _, ev := range res.Events {
		var payload domain.WorldMaterializePayload
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			continue
		}
		if payload.Path == path {
			return &ev.Provenance, nil
		}
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// world.stage
// ---------------------------------------------------------------------------

type worldStageArgs struct {
	Scope         string           `json:"scope"`
	Path          string           `json:"path"`
	Create        bool             `json:"create,omitempty"`
	Kind          string           `json:"kind,omitempty"`
	Content       []byte           `json:"content,omitempty"`
	Mode          uint32           `json:"mode,omitempty"`
	SymlinkTarget string           `json:"symlink_target,omitempty"`
	ExpectedRev   *domain.Revision `json:"expected_rev,omitempty"`
}

type worldStageResult struct {
	Scope  domain.Scope     `json:"scope"`
	Path   domain.ValidPath `json:"path"`
	Staged stagedChange     `json:"staged"`
}

// worldStage proposes a file/fact/metadata change as a staged change set.
// It never commits: staged changes take effect only when the turn
// coordinator commits them, so a tool call has no immediate shared effect.
func (r *Registry) worldStage(ctx context.Context, call CallContext, raw json.RawMessage) (json.RawMessage, error) {
	var args worldStageArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	ns, scope, err := call.scopeNamespace(args.Scope)
	if err != nil {
		return nil, err
	}
	path, err := domain.ParsePath(args.Path)
	if err != nil {
		return nil, err
	}
	if err := call.Policy.AuthorizeWrite(scope, scope == domain.ScopeUser); err != nil {
		return nil, err
	}
	cs := domain.EmptyChangeSet(call.TurnID, call.AttemptID, call.NowUnixMilli)
	if args.Create {
		if err := r.stageCreate(ctx, call, ns, path, args, &cs); err != nil {
			return nil, err
		}
	} else {
		if err := r.stageUpdate(ctx, ns, path, args, &cs); err != nil {
			return nil, err
		}
	}
	r.stageChangeSet(call.TurnID, cs)
	count := len(r.StagedChanges(call.TurnID))
	return marshalResult(worldStageResult{Scope: scope, Path: path, Staged: summarizeChangeSet(cs, count)})
}

func (r *Registry) stageCreate(ctx context.Context, call CallContext, ns domain.NamespaceID, path domain.ValidPath, args worldStageArgs, cs *domain.ChangeSet) error {
	kind, err := parseNodeKind(args.Kind)
	if err != nil {
		return err
	}
	parent := path.Parent()
	parentNode, err := r.deps.World.LookupPath(ctx, ns, parent)
	if err != nil {
		if domain.IsNotFoundError(err) {
			return domain.NewValidationError(
				domain.CodeInvalidInput,
				fmt.Sprintf("parent directory %q does not exist; use world.materialize to create missing paths", parent), nil)
		}
		return err
	}
	var content domain.ContentRef
	if kind == domain.NodeKindFile {
		if len(args.Content) > MaxStageBytes {
			return domain.NewLimitError(
				domain.CodeOutputTooLarge,
				fmt.Sprintf("staged content exceeds the %d-byte stage limit", MaxStageBytes), nil)
		}
		ref, err := r.deps.Content.Put(ctx, args.Content, "application/octet-stream")
		if err != nil {
			return err
		}
		content = ref
	}
	meta := domain.NewNodeMetadata(args.Mode, call.UID, call.GID, call.NowUnixMilli)
	cs.AddCreate(ns, path, kind, meta, content)
	cs.AddReadDep(domain.NodeID{}, 0, kind, true, parentNode.ID)
	return nil
}

func (r *Registry) stageUpdate(ctx context.Context, ns domain.NamespaceID, path domain.ValidPath, args worldStageArgs, cs *domain.ChangeSet) error {
	if args.ExpectedRev == nil {
		return domain.NewValidationError(
			domain.CodeInvalidInput, "expected_rev is required for an update", nil)
	}
	node, err := r.deps.World.LookupPath(ctx, ns, path)
	if err != nil {
		return err
	}
	meta := node.Metadata
	if args.Mode != 0 {
		meta.Mode = args.Mode
	}
	var content domain.ContentRef = node.Content
	if args.Content != nil {
		if len(args.Content) > MaxStageBytes {
			return domain.NewLimitError(
				domain.CodeOutputTooLarge,
				fmt.Sprintf("staged content exceeds the %d-byte stage limit", MaxStageBytes), nil)
		}
		ref, err := r.deps.Content.Put(ctx, args.Content, "application/octet-stream")
		if err != nil {
			return err
		}
		content = ref
	}
	cs.AddUpdate(ns, path, node.ID, *args.ExpectedRev, meta, content)
	cs.AddReadDep(node.ID, *args.ExpectedRev, node.Kind, false, domain.NodeID{})
	return nil
}

// ---------------------------------------------------------------------------
// world.materialize
// ---------------------------------------------------------------------------

// defaultMaterializeMode is the Unix mode for generated nodes: owner
// read/write, group and others read (no execute for generated content).
const defaultMaterializeMode uint32 = 0o644

type worldMaterializeArgs struct {
	Scope string `json:"scope"`
	Path  string `json:"path"`
	Kind  string `json:"kind,omitempty"`
	Hint  string `json:"hint,omitempty"`
}

type worldMaterializeResult struct {
	Path    domain.ValidPath `json:"path"`
	Scope   domain.Scope     `json:"scope"`
	Existed bool             `json:"existed"`
	Staged  *stagedChange    `json:"staged,omitempty"`
}

// worldMaterialize resolves an unexplored path by staging the creation of
// the path and every missing parent as one atomic change set. Generation is
// deferred: the change set takes effect only when the turn coordinator
// commits it, and an existing path is returned unchanged rather than
// restaged.
func (r *Registry) worldMaterialize(ctx context.Context, call CallContext, raw json.RawMessage) (json.RawMessage, error) {
	var args worldMaterializeArgs
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	ns, scope, err := call.scopeNamespace(args.Scope)
	if err != nil {
		return nil, err
	}
	path, err := domain.ParsePath(args.Path)
	if err != nil {
		return nil, err
	}
	if err := call.Policy.AuthorizeWrite(scope, scope == domain.ScopeUser); err != nil {
		return nil, err
	}
	if _, err := r.deps.World.LookupPath(ctx, ns, path); err == nil {
		return marshalResult(worldMaterializeResult{Path: path, Scope: scope, Existed: true})
	} else if !domain.IsNotFoundError(err) {
		return nil, err
	}
	kind, err := parseNodeKind(args.Kind)
	if err != nil {
		return nil, err
	}
	// Walk up to the nearest existing ancestor, collecting the missing
	// chain target-first.
	chain := []domain.ValidPath{path}
	ancestor := path.Parent()
	for {
		if _, err := r.deps.World.LookupPath(ctx, ns, ancestor); err == nil {
			break
		} else if !domain.IsNotFoundError(err) {
			return nil, err
		}
		if ancestor.IsRoot() {
			return nil, domain.NewInternalError(
				domain.CodeInvariantViolation, "namespace root does not exist", nil)
		}
		chain = append(chain, ancestor)
		ancestor = ancestor.Parent()
	}
	cs := domain.EmptyChangeSet(call.TurnID, call.AttemptID, call.NowUnixMilli)
	// Create top-down so every parent exists before its child is staged.
	for i := len(chain) - 1; i >= 0; i-- {
		mp := chain[i]
		mk := domain.NodeKindDir
		var content domain.ContentRef
		if mp == path {
			mk = kind
			if kind == domain.NodeKindFile {
				data, err := r.deps.Generator.GenerateContent(ctx, mp, kind, args.Hint)
				if err != nil {
					return nil, err
				}
				if len(data) > MaxStageBytes {
					return nil, domain.NewLimitError(
						domain.CodeOutputTooLarge,
						fmt.Sprintf("generated content exceeds the %d-byte stage limit", MaxStageBytes), nil)
				}
				ref, err := r.deps.Content.Put(ctx, data, "application/octet-stream")
				if err != nil {
					return nil, err
				}
				content = ref
			}
		}
		meta := domain.NewNodeMetadata(defaultMaterializeMode, call.UID, call.GID, call.NowUnixMilli)
		cs.AddCreate(ns, mp, mk, meta, content)
		// The absence dependency names the nearest existing ancestor for the
		// topmost missing path; deeper creates are covered by the atomic
		// change set itself.
		dirMemberOf := domain.NodeID{}
		if i == len(chain)-1 {
			ancestorNode, err := r.deps.World.LookupPath(ctx, ns, ancestor)
			if err != nil {
				return nil, err
			}
			dirMemberOf = ancestorNode.ID
		}
		cs.AddReadDep(domain.NodeID{}, 0, mk, true, dirMemberOf)
	}
	r.stageChangeSet(call.TurnID, cs)
	count := len(r.StagedChanges(call.TurnID))
	summary := summarizeChangeSet(cs, count)
	return marshalResult(worldMaterializeResult{Path: path, Scope: scope, Staged: &summary})
}

func parseNodeKind(s string) (domain.NodeKind, error) {
	if s == "" {
		return domain.NodeKindFile, nil
	}
	kind, err := domain.ParseNodeKind(s)
	if err != nil {
		return 0, domain.NewValidationError(domain.CodeInvalidInput, err.Error(), nil)
	}
	return kind, nil
}
