// Package buildinfo reports the identity of a VibeShell build.
//
// Version and revision are package-level strings so that a build can stamp them
// with `go build -ldflags -X`. The linker paths that stamp them are
// j0s.at/vibeshell/internal/buildinfo.version and
// j0s.at/vibeshell/internal/buildinfo.revision; they are repeated in
// containers/Containerfile and in scripts/dev.ps1, and a test guards the
// Containerfile copy so a typo cannot silently produce an unstamped binary.
package buildinfo

import (
	"fmt"
	"strings"
)

// Public identity of the simulated system and its shell. PLAN.md fixes these
// names, so they are constants rather than configuration.
const (
	SystemName = "VibeOS"
	ShellName  = "VibeShell"
)

// DefaultVersion marks a build that no release build stamped.
const DefaultVersion = "dev"

// unknownRevision marks a build whose VCS revision was not available.
const unknownRevision = "unknown"

// Stamped by the build; see the package comment.
var (
	version  = DefaultVersion
	revision = unknownRevision
)

// Info is the identity of one build.
type Info struct {
	System   string
	Shell    string
	Version  string
	Revision string
}

// Current returns the identity of the running binary.
func Current() Info {
	return Info{
		System:   SystemName,
		Shell:    ShellName,
		Version:  version,
		Revision: revision,
	}
}

// String renders a single line for logs and command output.
func (i Info) String() string {
	line := fmt.Sprintf("%s on %s version %s", i.Shell, i.System, i.Version)
	if isKnownRevision(i.Revision) {
		return line + " (" + i.Revision + ")"
	}
	return line
}

// isKnownRevision reports whether a revision identifies a specific build. A
// build without one still has to name itself, so the revision is left out
// rather than printed as "unknown".
func isKnownRevision(revision string) bool {
	trimmed := strings.TrimSpace(revision)
	return trimmed != "" && trimmed != unknownRevision
}
