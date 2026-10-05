package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
)

// NodeKind represents the kind of filesystem node.
type NodeKind int

const (
	NodeKindFile NodeKind = iota
	NodeKindDir
	NodeKindSymlink
)

// String returns the canonical node kind name.
func (k NodeKind) String() string {
	switch k {
	case NodeKindFile:
		return "file"
	case NodeKindDir:
		return "dir"
	case NodeKindSymlink:
		return "symlink"
	default:
		return fmt.Sprintf("unknown(%d)", k)
	}
}

// ParseNodeKind parses a node kind name.
func ParseNodeKind(s string) (NodeKind, error) {
	switch strings.ToLower(s) {
	case "file":
		return NodeKindFile, nil
	case "dir", "directory":
		return NodeKindDir, nil
	case "symlink", "link":
		return NodeKindSymlink, nil
	default:
		return NodeKindFile, fmt.Errorf("%w: %q", ErrInvalidNodeKind, s)
	}
}

func (k NodeKind) MarshalJSON() ([]byte, error) {
	return json.Marshal(k.String())
}

func (k *NodeKind) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	parsed, err := ParseNodeKind(str)
	if err != nil {
		return err
	}
	*k = parsed
	return nil
}

// IsValidNodeKind reports whether k is a supported world node kind.
// IsValid reports whether k is a supported world node kind.
func (k NodeKind) IsValid() bool {
	switch k {
	case NodeKindFile, NodeKindDir, NodeKindSymlink:
		return true
	default:
		return false
	}
}

var ErrInvalidNodeKind = errors.New("invalid node kind")

// Revision represents a node's version for optimistic concurrency control.
type Revision int64

// InitialRevision is the starting revision for a newly created node.
const InitialRevision Revision = 1

// Next returns the next revision.
func (r Revision) Next() Revision { return r + 1 }

// IsInitial returns true if this is the initial revision.
func (r Revision) IsInitial() bool { return r == InitialRevision }

// ContentRef references immutable content by hash.
type ContentRef struct {
	Hash      ContentID `json:"hash"`       // content-addressed identifier
	Size      int64     `json:"size"`       // exact byte count
	MediaType string    `json:"media_type"` // MIME type, e.g., "text/plain; charset=utf-8"
}

// EmptyContentRef returns a reference to empty content.
func EmptyContentRef() ContentRef {
	return ContentRef{
		Hash:      ContentID{},
		Size:      0,
		MediaType: "application/octet-stream",
	}
}

// IsEmpty returns true if this references empty content.
func (c ContentRef) IsEmpty() bool { return c.Size == 0 }

// IsZero reports whether the reference is the unset zero value. EmptyContentRef
// carries a media type, so it is a real reference rather than zero and is kept
// by omitzero.
func (c ContentRef) IsZero() bool { return c == ContentRef{} }

// UnmarshalJSON decodes a content reference. JSON null and the empty string
// mean "absent" and decode to the zero value; an object decodes field by field.
func (c *ContentRef) UnmarshalJSON(data []byte) error {
	if string(data) == "null" || string(data) == `""` {
		*c = ContentRef{}
		return nil
	}
	// The alias drops the methods so decoding cannot recurse into this method.
	type contentRef ContentRef
	var ref contentRef
	if err := json.Unmarshal(data, &ref); err != nil {
		return err
	}
	*c = ContentRef(ref)
	return nil
}

// ValidPath represents a validated simulated filesystem path.
// Paths are always absolute, use forward slashes, no trailing slash (except root),
// no ".", "..", or empty components, and no null bytes.
type ValidPath string

// Path length bounds mirror common Unix limits so oversized inputs fail fast
// at the domain boundary instead of inside storage or rendering code.
const (
	MaxPathLen = 4096
	MaxNameLen = 255
)

// ParsePath validates a simulated-world path. Accepted paths are absolute,
// use forward slashes, carry no trailing slash (except root), and contain no
// empty, ".", or ".." components: dot segments are rejected rather than
// silently cleaned so a path can never resolve outside the simulated
// namespace or alias another accepted path.
func ParsePath(s string) (ValidPath, error) {
	if s == "" {
		return "", ErrEmptyPath
	}
	if s == "/" {
		return "/", nil
	}
	if len(s) > MaxPathLen {
		return "", ErrPathTooLong
	}
	if strings.Contains(s, "\x00") {
		return "", ErrPathContainsNull
	}
	if !strings.HasPrefix(s, "/") {
		return "", ErrPathNotAbsolute
	}
	if len(s) > 1 && strings.HasSuffix(s, "/") {
		return "", ErrTrailingSlash
	}
	for _, part := range strings.Split(s, "/")[1:] {
		if part == "" {
			return "", ErrEmptyComponent
		}
		if len(part) > MaxNameLen {
			return "", ErrNameTooLong
		}
		if part == "." || part == ".." {
			return "", ErrDotComponent
		}
		if strings.Contains(part, "\x00") {
			return "", ErrPathContainsNull
		}
	}
	cleaned := path.Clean(s)
	if cleaned != s {
		return "", ErrUncleanPath
	}
	return ValidPath(cleaned), nil
}

// MustParsePath panics if the path is invalid. For tests and constants.
func MustParsePath(s string) ValidPath {
	p, err := ParsePath(s)
	if err != nil {
		panic(err)
	}
	return p
}

// RootPath returns the root path "/".
func RootPath() ValidPath { return "/" }

// String returns the path as a string.
func (p ValidPath) String() string { return string(p) }

// Parent returns the parent directory path.
func (p ValidPath) Parent() ValidPath {
	if p == "/" {
		return "/"
	}
	return ValidPath(path.Dir(string(p)))
}

// Base returns the final component of the path.
func (p ValidPath) Base() string {
	return path.Base(string(p))
}

// Join appends a path component.
func (p ValidPath) Join(elem string) ValidPath {
	if p == "/" {
		return ValidPath("/" + elem)
	}
	return ValidPath(path.Join(string(p), elem))
}

// IsRoot returns true if this is the root path.
func (p ValidPath) IsRoot() bool { return p == "/" }

// MarshalJSON implements json.Marshaler.
func (p ValidPath) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(p))
}

// UnmarshalJSON implements json.Unmarshaler. An absent path (JSON null or the
// empty string) decodes to the zero value so a zero-valued structure round-
// trips; required-path validation still rejects it where a path is mandatory.
func (p *ValidPath) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*p = ""
		return nil
	}
	parsed, err := ParsePath(s)
	if err != nil {
		return err
	}
	*p = parsed
	return nil
}

var (
	ErrEmptyPath        = errors.New("path is empty")
	ErrPathContainsNull = errors.New("path contains null byte")
	ErrPathNotAbsolute  = errors.New("path must be absolute")
	ErrTrailingSlash    = errors.New("path must not have trailing slash (except root)")
	ErrEmptyComponent   = errors.New("path contains an empty component")
	ErrDotComponent     = errors.New("path must not contain . or .. components")
	ErrUncleanPath      = errors.New("path is not in canonical form")
	ErrPathTooLong      = errors.New("path exceeds maximum length")
	ErrNameTooLong      = errors.New("path component exceeds maximum length")
)

// NodeMetadata holds mutable node attributes.
type NodeMetadata struct {
	Mode          uint32 `json:"mode"`                     // Unix-style mode bits
	UID           uint32 `json:"uid"`                      // owner user id
	GID           uint32 `json:"gid"`                      // owner group id
	ModTime       int64  `json:"mod_time"`                 // unix milliseconds
	AccessTime    int64  `json:"access_time"`              // unix milliseconds
	ChangeTime    int64  `json:"change_time"`              // unix milliseconds
	SymlinkTarget string `json:"symlink_target,omitempty"` // for symlinks
}

// NewNodeMetadata creates metadata with an explicit timestamp. The caller
// supplies now from the Clock port so domain construction stays deterministic.
func NewNodeMetadata(mode uint32, uid, gid uint32, nowUnixMilli int64) NodeMetadata {
	return NodeMetadata{
		Mode:       mode,
		UID:        uid,
		GID:        gid,
		ModTime:    nowUnixMilli,
		AccessTime: nowUnixMilli,
		ChangeTime: nowUnixMilli,
	}
}

// Node represents a filesystem node in the world model.
type Node struct {
	ID          NodeID      `json:"id"`
	NamespaceID NamespaceID `json:"namespace_id"`
	// ParentID is nil for the namespace root and set for every other node.
	// A pointer (rather than a zero value) keeps the JSON encoding free of
	// placeholder IDs that would fail identity validation.
	ParentID *NodeID      `json:"parent_id,omitempty"`
	Name     string       `json:"name"` // single path component, no slashes
	Kind     NodeKind     `json:"kind"`
	Revision Revision     `json:"revision"`
	Metadata NodeMetadata `json:"metadata"`
	Content  ContentRef   `json:"content"` // empty for directories/symlinks
}

// Path returns the full path of this node (requires namespace context to resolve).
// This is a helper; the authoritative path comes from the namespace hierarchy.
func (n Node) Path() ValidPath {
	// This is a placeholder; actual path resolution requires the namespace tree.
	// For root node:
	if n.ParentID == nil && n.Name == "" {
		return "/"
	}
	return ValidPath("/" + n.Name) // simplified; real impl walks parent chain
}

// IsDir returns true if this is a directory.
func (n Node) IsDir() bool { return n.Kind == NodeKindDir }

// IsFile returns true if this is a regular file.
func (n Node) IsFile() bool { return n.Kind == NodeKindFile }

// IsSymlink returns true if this is a symlink.
func (n Node) IsSymlink() bool { return n.Kind == NodeKindSymlink }

// ReadDependency records what state was read to validate a mutation.
type ReadDependency struct {
	NodeID      NodeID   `json:"node_id"`       // node that was read
	Revision    Revision `json:"revision"`      // expected revision at read time
	Kind        NodeKind `json:"kind"`          // expected kind
	IsAbsence   bool     `json:"is_absence"`    // true if this was an absence check (node did not exist)
	DirMemberOf NodeID   `json:"dir_member_of"` // for directory membership: parent dir ID
}

// MutationType represents the type of world mutation.
type MutationType int

const (
	MutationCreate MutationType = iota
	MutationUpdate
	MutationDelete
	MutationTombstone // explicit deletion marker for overlay scopes
)

func (m MutationType) String() string {
	switch m {
	case MutationCreate:
		return "create"
	case MutationUpdate:
		return "update"
	case MutationDelete:
		return "delete"
	case MutationTombstone:
		return "tombstone"
	default:
		return fmt.Sprintf("unknown(%d)", m)
	}
}

func (m MutationType) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.String())
}

func (m *MutationType) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	switch strings.ToLower(s) {
	case "create":
		*m = MutationCreate
	case "update":
		*m = MutationUpdate
	case "delete":
		*m = MutationDelete
	case "tombstone":
		*m = MutationTombstone
	default:
		return fmt.Errorf("invalid mutation type: %q", s)
	}
	return nil
}

// Mutation represents a proposed change to the world.
type Mutation struct {
	Type        MutationType `json:"type"`
	NamespaceID NamespaceID  `json:"namespace_id"`
	Path        ValidPath    `json:"path"`
	// NodeID names the existing node for update/delete/tombstone and is nil
	// for create. Pointers keep absent references out of the encoding instead
	// of emitting placeholder IDs that fail identity validation.
	NodeID        *NodeID      `json:"node_id,omitempty"`
	ExpectedRev   Revision     `json:"expected_rev,omitempty"`   // expected revision (for update/delete/tombstone)
	Kind          NodeKind     `json:"kind"`                     // for create: new node kind
	Metadata      NodeMetadata `json:"metadata"`                 // for create/update
	Content       ContentRef   `json:"content"`                  // for create/update file content
	SymlinkTarget string       `json:"symlink_target,omitempty"` // for create/update symlink
}

// ChangeSet is a collection of mutations to be applied atomically.
type ChangeSet struct {
	Mutations        []Mutation       `json:"mutations"`
	ReadDependencies []ReadDependency `json:"read_dependencies"`
	TurnID           TurnID           `json:"turn_id"`
	AttemptID        AttemptID        `json:"attempt_id"`
	Timestamp        int64            `json:"timestamp"` // unix milliseconds
}

// EmptyChangeSet returns an empty change set with an explicit timestamp.
func EmptyChangeSet(turn TurnID, attempt AttemptID, nowUnixMilli int64) ChangeSet {
	return ChangeSet{
		TurnID:    turn,
		AttemptID: attempt,
		Timestamp: nowUnixMilli,
	}
}

// AddCreate adds a create mutation.
func (c *ChangeSet) AddCreate(ns NamespaceID, p ValidPath, kind NodeKind, meta NodeMetadata, content ContentRef) {
	c.Mutations = append(c.Mutations, Mutation{
		Type:        MutationCreate,
		NamespaceID: ns,
		Path:        p,
		Kind:        kind,
		Metadata:    meta,
		Content:     content,
	})
}

// AddUpdate adds an update mutation.
func (c *ChangeSet) AddUpdate(ns NamespaceID, p ValidPath, nodeID NodeID, expectedRev Revision, meta NodeMetadata, content ContentRef) {
	c.Mutations = append(c.Mutations, Mutation{
		Type:        MutationUpdate,
		NamespaceID: ns,
		Path:        p,
		NodeID:      &nodeID,
		ExpectedRev: expectedRev,
		Metadata:    meta,
		Content:     content,
	})
}

// AddDelete adds a delete mutation.
func (c *ChangeSet) AddDelete(ns NamespaceID, p ValidPath, nodeID NodeID, expectedRev Revision) {
	c.Mutations = append(c.Mutations, Mutation{
		Type:        MutationDelete,
		NamespaceID: ns,
		Path:        p,
		NodeID:      &nodeID,
		ExpectedRev: expectedRev,
	})
}

// AddTombstone adds a tombstone mutation (for overlay scope deletions).
func (c *ChangeSet) AddTombstone(ns NamespaceID, p ValidPath, nodeID NodeID, expectedRev Revision) {
	c.Mutations = append(c.Mutations, Mutation{
		Type:        MutationTombstone,
		NamespaceID: ns,
		Path:        p,
		NodeID:      &nodeID,
		ExpectedRev: expectedRev,
	})
}

// AddReadDep adds a read dependency.
func (c *ChangeSet) AddReadDep(nodeID NodeID, rev Revision, kind NodeKind, isAbsence bool, dirMemberOf NodeID) {
	c.ReadDependencies = append(c.ReadDependencies, ReadDependency{
		NodeID:      nodeID,
		Revision:    rev,
		Kind:        kind,
		IsAbsence:   isAbsence,
		DirMemberOf: dirMemberOf,
	})
}

// IsEmpty returns true if there are no mutations.
func (c ChangeSet) IsEmpty() bool { return len(c.Mutations) == 0 }
