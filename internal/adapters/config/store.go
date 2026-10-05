package config

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"j0s.at/vibeshell/internal/ports"
)

// Snapshot is one fully validated configuration publication. Version is the
// snapshot's own monotonic publication number (not the document's version
// field): a turn pins one snapshot and keeps it for its whole lifetime, so
// mid-turn reloads cannot mix old and new policy.
type Snapshot struct {
	Version           uint64        `json:"version"`
	LoadedAtUnixMilli int64         `json:"loaded_at_unix_ms"`
	Config            *Config       `json:"config"`
	Reload            *ReloadReport `json:"reload,omitempty"` // classified field changes
	secrets           map[string]Secret
}

// Secret returns a copy of the resolved value for a reference. The copy
// keeps callers from mutating the published snapshot.
func (s *Snapshot) Secret(ref string) (Secret, bool) {
	value, ok := s.secrets[ref]
	if !ok {
		return nil, false
	}
	return Secret(value.Bytes()), true
}

// SecretCount reports how many references were resolved, without names.
func (s *Snapshot) SecretCount() int { return len(s.secrets) }

// String renders a safe operator-facing summary. It has a redacted value
// receiver so fmt's default %v of a Snapshot (which would otherwise walk
// every unexported field, secrets included) still shows only placeholders.
func (s Snapshot) String() string {
	auth := "unset"
	if s.Config != nil && s.Config.Auth != nil {
		auth = string(s.Config.Auth.Mode)
	}
	sharing := false
	if s.Config != nil && s.Config.Sharing != nil {
		sharing = s.Config.Sharing.Enabled
	}
	var tiers []string
	if s.Config != nil {
		for _, t := range s.Config.Tiers {
			tiers = append(tiers, t.Name)
		}
	}
	accounts := 0
	if s.Config != nil {
		accounts = len(s.Config.Accounts)
	}
	return fmt.Sprintf("snapshot %d: auth=%s sharing=%v tiers=%v accounts=%d secrets=%d %s",
		s.Version, auth, sharing, tiers, accounts, len(s.secrets), redacted)
}

// Store publishes validated snapshots atomically. Readers only ever observe
// a complete snapshot: the replacement is built and validated before it is
// stored, and a failed reload leaves the previous snapshot and version in
// place.
type Store struct {
	path   string
	loader *Loader
	clock  ports.Clock

	mu        sync.Mutex
	published atomic.Pointer[Snapshot]
}

// OpenStore loads path and publishes its first snapshot. It fails when the
// initial configuration is invalid, so a service never starts half-configured.
func OpenStore(clock ports.Clock, loader *Loader, path string) (*Store, error) {
	if clock == nil {
		return nil, fmt.Errorf("config: store requires a clock")
	}
	if loader == nil {
		return nil, fmt.Errorf("config: store requires a loader")
	}
	s := &Store{path: path, loader: loader, clock: clock}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload re-reads and re-validates the configuration file, then swaps the
// snapshot in one step. On any error the current snapshot stays published.
// Concurrent reloads are serialized so version numbers stay dense.
//
// The published candidate is always fully validated. Its ReloadReport
// classifies what changed: hot-reloadable groups take effect for new
// turns/sessions immediately, while restart-required groups (bind address,
// engine binary, storage) are validated and reported as PendingRestart but do
// not take effect until the process restarts.
func (s *Store) Reload() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("read configuration %q: %w", s.path, err)
	}
	candidate, err := s.loader.Load(raw)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	previous := s.published.Load()
	version := uint64(1)
	var previousConfig *Config
	if previous != nil {
		version = previous.Version + 1
		previousConfig = previous.Config
	}

	candidate.Version = version
	candidate.LoadedAtUnixMilli = s.clock.NowUnixMilli()
	candidate.Reload = newReloadReport(previousConfig, candidate.Config, version)

	// Single pointer swap: readers see either the old complete snapshot or
	// the new complete snapshot, never a partial one.
	s.published.Store(candidate)
	return nil
}

// Pin returns the snapshot a turn must hold for its whole lifetime; it is
// nil only if the store was never successfully opened.
func (s *Store) Pin() *Snapshot {
	return s.published.Load()
}
