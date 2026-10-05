package simulation

import (
	"context"
	"sort"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// WorldReader resolves app world-read requests against the world store.
type WorldReader struct {
	World        ports.WorldStore
	Content      ports.ContentStore
	MaxReadBytes int64 // default MaxContentReadBytes
}

// WorldEntry is one direct child of a directory in a world read outcome.
type WorldEntry struct {
	Name string          `json:"name"`
	Kind domain.NodeKind `json:"kind"`
	Size int64           `json:"size,omitempty"`
}

// WorldReadOutcome is the fulfillment of one world read request as handed
// back to the app: a file carries its bounded content, a directory its
// bounded, name-sorted entries.
type WorldReadOutcome struct {
	RequestID string           `json:"request_id"`
	Path      domain.ValidPath `json:"path"`
	Found     bool             `json:"found"`
	// Kind is always present when Found: the zero value is NodeKindFile, so it
	// must not be omitted or a file would look like a missing path.
	Kind domain.NodeKind `json:"kind"`
	// Content is the file's text. It is a string, not []byte, so the app
	// receives readable text rather than a base64 blob.
	Content   string       `json:"content,omitempty"`
	Entries   []WorldEntry `json:"entries,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`
}

// Resolve fulfills one request within a namespace. A missing path is not an
// error: it returns Found=false.
func (r *WorldReader) Resolve(ctx context.Context, ns domain.NamespaceID, req domain.WorldReadRequest) (WorldReadOutcome, error) {
	outcome := WorldReadOutcome{RequestID: req.RequestID, Path: req.Path}
	node, err := r.lookup(ctx, ns, req)
	if err != nil {
		if domain.IsNotFoundError(err) {
			return outcome, nil
		}
		return WorldReadOutcome{}, err
	}
	outcome.Found = true
	outcome.Kind = node.Kind
	switch {
	case node.IsFile():
		data, err := r.Content.Get(ctx, node.Content, 0, r.readLimit(req.MaxBytes))
		if err != nil {
			return WorldReadOutcome{}, err
		}
		outcome.Content = string(data)
		outcome.Truncated = int64(len(data)) < node.Content.Size
	case node.IsDir():
		entries, err := r.entries(ctx, ns, node)
		if err != nil {
			return WorldReadOutcome{}, err
		}
		outcome.Entries = entries
	}
	return outcome, nil
}

// lookup resolves the request's node: an explicit node ID wins over a path.
func (r *WorldReader) lookup(ctx context.Context, ns domain.NamespaceID, req domain.WorldReadRequest) (domain.Node, error) {
	if req.NodeID != nil {
		return r.World.GetNode(ctx, ns, *req.NodeID)
	}
	return r.World.LookupPath(ctx, ns, req.Path)
}

// readLimit picks the byte bound for one file read: the configured reader
// bound is the ceiling, and a smaller per-request max narrows it further.
func (r *WorldReader) readLimit(requested int64) int64 {
	limit := int64(MaxContentReadBytes)
	if r.MaxReadBytes > 0 {
		limit = r.MaxReadBytes
	}
	if requested > 0 && requested < limit {
		limit = requested
	}
	return limit
}

// entries lists one bounded page of a directory's children, sorted by name.
// The page bound is the package's single result-page cap.
func (r *WorldReader) entries(ctx context.Context, ns domain.NamespaceID, node domain.Node) ([]WorldEntry, error) {
	children, _, err := r.World.ListDirectory(ctx, ns, node.ID, MaxResultLimit, "")
	if err != nil {
		return nil, err
	}
	entries := make([]WorldEntry, 0, len(children))
	for _, child := range children {
		entries = append(entries, WorldEntry{Name: child.Name, Kind: child.Kind, Size: child.Content.Size})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}
