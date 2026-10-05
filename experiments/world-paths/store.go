// Simulated concurrent first materialization with no real database:
// versioned commit with revision, directory-membership, and absence read
// dependencies. Exactly one racing creator wins; the loser rebases.
package worldpaths

import (
	"sync"

	"j0s.at/vibeshell/internal/domain"
)

// Committed is the authoritative stored version of one shared path.
type Committed struct {
	ID      domain.NodeID
	Rev     domain.Revision
	Content string
}

// SimStore is a deterministic in-memory stand-in for the SQLite world
// commit path: one parent directory revision plus per-path versions.
type SimStore struct {
	mu     sync.Mutex
	files  map[string]Committed
	dirRev map[string]domain.Revision
}

// NewSimStore returns an empty store.
func NewSimStore() *SimStore {
	return &SimStore{files: map[string]Committed{}, dirRev: map[string]domain.Revision{}}
}

// Snapshot records the read dependencies a turn would store: the file
// revision (or absence) plus the parent directory membership revision.
func (s *SimStore) Snapshot(path string) (file Committed, found bool, dirRev domain.Revision, deps []domain.ReadDependency) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := parentDir(path)
	dr := s.dirRev[dir]
	if dr == 0 {
		dr = 1
	}
	f, ok := s.files[path]
	_ = f
	if ok {
		return f, true, dr, []domain.ReadDependency{{Revision: f.Rev, DirMemberOf: fakeDirID(dir)}}
	}
	return Committed{}, false, dr, []domain.ReadDependency{{IsAbsence: true, DirMemberOf: fakeDirID(dir), Revision: dr}}
}

// CommitCreate attempts an atomic first materialization. It fails with a
// conflict when the path already exists or when the caller presents a stale
// directory revision, so a stale listing can never validate an invalid write.
func (s *SimStore) CommitCreate(path string, id domain.NodeID, content string, sawAbsence bool, sawDirRev domain.Revision) (Committed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := parentDir(path)
	cur := s.dirRev[dir]
	if cur == 0 {
		cur = 1
	}
	if _, exists := s.files[path]; exists {
		return Committed{}, domain.NewConflictError(domain.CodeDuplicateKey, "shared path already materialized", map[string]string{"path": path})
	}
	if !sawAbsence {
		return Committed{}, domain.NewConflictError(domain.CodeStaleRead, "expected absence dependency missing", map[string]string{"path": path})
	}
	if sawDirRev != cur {
		return Committed{}, domain.NewConflictError(domain.CodeStaleRead, "stale directory membership revision", map[string]string{"path": path})
	}
	c := Committed{ID: id, Rev: domain.InitialRevision, Content: content}
	s.files[path] = c
	s.dirRev[dir] = cur + 1
	return c, nil
}

// Current returns the committed version after a race.
func (s *SimStore) Current(path string) (Committed, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.files[path]
	return c, ok
}

func parentDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			if i == 0 {
				return "/"
			}
			return p[:i]
		}
	}
	return "/"
}

// fakeDirID derives a deterministic stand-in directory NodeID from the path
// length so ReadDependency values stay stable without randomness.
func fakeDirID(dir string) domain.NodeID {
	const base = "nod_0123456789ABCDEFGHJKMNPQRS"
	_ = dir
	id, _ := domain.ParseNodeID(base)
	return id
}
