package presentation

import (
	"fmt"
	"sort"
	"strings"

	"j0s.at/vibeshell/internal/domain"
)

// Baseline seed facts (PLAN 5.4): a small versioned directory
// skeleton with identity and structural facts only. File and
// symlink contents are never seeded here; the simulation
// materializes plausible contents on demand and commits them
// before they are shown as durable facts.
const (
	// BaselineSeedVersion identifies the seed layout.
	BaselineSeedVersion = "baseline-v1"
	// RootUID/GID are the owner of the seed directories.
	RootUID = 0
	RootGID = 0
)

// BaselineEntry describes one seeded directory. Mode carries
// Unix-style mode bits (for example 0o1777 for the sticky /tmp);
// UID/GID mirror domain.NodeMetadata so a materializing adapter
// can construct node metadata directly from the seed.
type BaselineEntry struct {
	Path    domain.ValidPath `json:"path"`
	Kind    domain.NodeKind  `json:"kind"`
	Mode    uint32           `json:"mode"`
	UID     uint32           `json:"uid"`
	GID     uint32           `json:"gid"`
	Purpose string           `json:"purpose"`
}

// BaselineSeed is the immutable, versioned clean-install seed
// description. It is pure data: sharing it, rendering it, or
// comparing it performs no I/O and never reaches another user's
// world.
type BaselineSeed struct {
	Version     string          `json:"version"`
	Identity    SystemIdentity  `json:"identity"`
	Directories []BaselineEntry `json:"directories"`
}

// DefaultBaselineSeed returns the GNU/Hurd-style baseline seed
// for a fresh VibeOS installation (PLAN 5.4): the root plus the
// standard directory skeleton, each with minimal facts. /dev and
// /proc are simulated views whose contents are generated on
// demand and never report real host devices or processes.
func DefaultBaselineSeed() BaselineSeed {
	return BaselineSeed{
		Version:  BaselineSeedVersion,
		Identity: DefaultSystemIdentity(),
		Directories: []BaselineEntry{
			{Path: domain.MustParsePath("/"), Kind: domain.NodeKindDir, Mode: 0o755, UID: RootUID, GID: RootGID,
				Purpose: "simulated root filesystem"},
			{Path: domain.MustParsePath("/bin"), Kind: domain.NodeKindDir, Mode: 0o755, UID: RootUID, GID: RootGID,
				Purpose: "essential user commands"},
			{Path: domain.MustParsePath("/dev"), Kind: domain.NodeKindDir, Mode: 0o755, UID: RootUID, GID: RootGID,
				Purpose: "simulated device views, generated on demand"},
			{Path: domain.MustParsePath("/etc"), Kind: domain.NodeKindDir, Mode: 0o755, UID: RootUID, GID: RootGID,
				Purpose: "system configuration"},
			{Path: domain.MustParsePath("/home"), Kind: domain.NodeKindDir, Mode: 0o755, UID: RootUID, GID: RootGID,
				Purpose: "user home directories, /home/<username>"},
			{Path: domain.MustParsePath("/proc"), Kind: domain.NodeKindDir, Mode: 0o555, UID: RootUID, GID: RootGID,
				Purpose: "simulated process and system information view, never real host processes"},
			{Path: domain.MustParsePath("/root"), Kind: domain.NodeKindDir, Mode: 0o700, UID: RootUID, GID: RootGID,
				Purpose: "simulated superuser home directory; root is a simulated identity with no host privileges"},
			{Path: domain.MustParsePath("/tmp"), Kind: domain.NodeKindDir, Mode: 0o1777, UID: RootUID, GID: RootGID,
				Purpose: "temporary files with the sticky bit"},
			{Path: domain.MustParsePath("/usr"), Kind: domain.NodeKindDir, Mode: 0o755, UID: RootUID, GID: RootGID,
				Purpose: "user programs and data"},
		},
	}
}

// Validate reports whether the seed is internally consistent:
// every entry is a unique, canonical, absolute directory path
// with a plausible mode and a non-empty purpose.
func (s BaselineSeed) Validate() error {
	if s.Version == "" {
		return fmt.Errorf("invalid baseline seed: version is empty")
	}
	if err := s.Identity.Validate(); err != nil {
		return fmt.Errorf("invalid baseline seed: %w", err)
	}
	if len(s.Directories) == 0 {
		return fmt.Errorf("invalid baseline seed: no directories")
	}
	seen := make(map[string]bool, len(s.Directories))
	paths := make([]string, 0, len(s.Directories))
	for _, entry := range s.Directories {
		if entry.Kind != domain.NodeKindDir {
			return fmt.Errorf("invalid baseline seed: %s is not a directory", entry.Path)
		}
		if seen[string(entry.Path)] {
			return fmt.Errorf("invalid baseline seed: duplicate path %s", entry.Path)
		}
		seen[string(entry.Path)] = true
		paths = append(paths, string(entry.Path))
		if entry.Purpose == "" {
			return fmt.Errorf("invalid baseline seed: %s has no purpose", entry.Path)
		}
		if entry.Mode&^uint32(0o7777) != 0 {
			return fmt.Errorf("invalid baseline seed: %s mode %#o has bits outside the mode range", entry.Path, entry.Mode)
		}
	}
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	if strings.Join(paths, "\n") != strings.Join(sorted, "\n") {
		return fmt.Errorf("invalid baseline seed: directories are not sorted")
	}
	return nil
}
