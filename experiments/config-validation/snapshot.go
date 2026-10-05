package config

import (
	"fmt"
	"sync"
)

// Snapshot is one immutable, fully validated configuration
// state. A Store publishes snapshots atomically; a turn
// pins the snapshot it started with and keeps seeing
// that view however the active snapshot changes
// afterwards. The only state a snapshot holds is set at
// publication and never written again.
//
// A snapshot holds resolved secrets in memory. Its
// String method redacts them, so formatting a snapshot
// never prints a value; note that fmt prints unexported
// fields raw when a snapshot is nested inside another
// struct, so snapshots must be logged through their
// String method, never as raw struct fields.
type Snapshot struct {
	version int
	config  Config
	secrets map[string]Secret   // account ID -> resolved secret
	routes  map[string][]string // tier name -> expanded route IDs
}

// Version is the snapshot's publication sequence number,
// starting at 1 for the first published snapshot.
func (s *Snapshot) Version() int { return s.version }

// AuthMode returns the validated authentication mode.
func (s *Snapshot) AuthMode() string { return s.config.Auth.Mode }

// SharingEnabled reports whether cross-user access is
// enabled.
func (s *Snapshot) SharingEnabled() bool { return *s.config.Sharing.Enabled }

// TierNames returns the tier names in policy order.
func (s *Snapshot) TierNames() []string {
	names := make([]string, 0, len(s.config.Tiers))
	for _, tier := range s.config.Tiers {
		names = append(names, tier.Name)
	}
	return names
}

// TierRoutes returns a copy of the expanded route IDs of
// one tier, in policy order, and whether the tier exists.
func (s *Snapshot) TierRoutes(tier string) ([]string, bool) {
	routes, ok := s.routes[tier]
	if !ok {
		return nil, false
	}
	return append([]string(nil), routes...), true
}

// AccountIDs returns every configured account ID.
func (s *Snapshot) AccountIDs() []string {
	ids := make([]string, 0, len(s.secrets))
	for _, account := range s.config.Accounts {
		ids = append(ids, account.ID)
	}
	return ids
}

// Secret returns the resolved secret of one account. The
// value must never be logged; Secret redacts itself in
// every formatting path.
func (s *Snapshot) Secret(accountID string) (Secret, bool) {
	secret, ok := s.secrets[accountID]
	return secret, ok
}

// String renders a redacted summary. Formatting a
// snapshot must never expose resolved secret values,
// so every formatting verb that reaches this method
// prints the summary instead of the raw state. The
// value receiver makes both Snapshot and *Snapshot
// Stringers.
func (s Snapshot) String() string {
	return fmt.Sprintf("snapshot %d: auth=%s sharing=%t tiers=%v accounts=%v secrets=%d [redacted]",
		s.version, s.config.Auth.Mode, s.SharingEnabled(), s.TierNames(), s.AccountIDs(), len(s.secrets))
}

// Store owns the active configuration snapshot. LoadFile
// validates a candidate completely before publication: an
// invalid document never replaces the active snapshot, and
// publication is a single pointer swap under a lock, so
// concurrent readers always see one whole snapshot.
type Store struct {
	loader Loader

	mu     sync.RWMutex
	active *Snapshot
	next   int
}

// NewStore returns an empty store that loads through the
// given loader. Active is nil until the first successful
// publication.
func NewStore(loader Loader) *Store {
	return &Store{loader: loader, next: 1}
}

// Active returns the snapshot turns should use right now,
// or nil before the first successful publication. The
// returned snapshot is immutable and remains valid after
// later publications.
func (s *Store) Active() *Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.active
}

// LoadFile loads and validates a configuration document
// and, only when it is fully valid, publishes it as the
// new active snapshot. On any error the active snapshot
// is untouched.
func (s *Store) LoadFile(path string) (*Snapshot, error) {
	candidate, err := s.loader.Load(path)
	if err != nil {
		return nil, err
	}
	return s.publish(candidate), nil
}

// publish swaps in a validated candidate. It is the only
// place the active snapshot changes.
func (s *Store) publish(candidate Candidate) *Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := &Snapshot{
		version: s.next,
		config:  candidate.Config,
		secrets: candidate.secrets,
		routes:  candidate.routes,
	}
	s.active = snapshot
	s.next++
	return snapshot
}
