// Simulated Unix permission interpretation over stored mode/owner/group
// facts, plus the sharing-off boundary that rejects cross-user/shared
// access regardless of any model request.
package worldpaths

import (
	"j0s.at/vibeshell/internal/domain"
)

// Access flags requested on a node.
type Access uint8

const (
	AccessRead Access = 1 << iota
	AccessWrite
	AccessExec
)

// Identity is the simulated credential making the request.
type Identity struct {
	EUID       uint32
	EGID       uint32
	Groups     []uint32
	Simulated  bool // always true: root here is a simulated identity
	IsRootUser bool // EUID == 0 convenience
}

// CheckUnix applies standard owner/group/other dispatch over the low 9 mode
// bits. Simulated root (EUID 0) bypasses read/write checks and needs any
// execute bit for exec; it grants no host privilege and never bypasses the
// scope-policy gate (checked separately in Enforce).
func CheckUnix(meta domain.NodeMetadata, id Identity, want Access) bool {
	mode := meta.Mode & 0o777
	isRoot := id.EUID == 0 || id.IsRootUser
	if isRoot {
		if want&AccessExec != 0 && mode&0o111 == 0 {
			return false
		}
		return true
	}
	var bits uint32
	switch {
	case id.EUID == meta.UID:
		bits = (mode >> 6) & 7
	case id.EGID == meta.GID || inGroups(id.Groups, meta.GID):
		bits = (mode >> 3) & 7
	default:
		bits = mode & 7
	}
	if want&AccessRead != 0 && bits&4 == 0 {
		return false
	}
	if want&AccessWrite != 0 && bits&2 == 0 {
		return false
	}
	if want&AccessExec != 0 && bits&1 == 0 {
		return false
	}
	return true
}

func inGroups(groups []uint32, gid uint32) bool {
	for _, g := range groups {
		if g == gid {
			return true
		}
	}
	return false
}

// Enforce first applies the trusted scope-policy boundary (sharing-off
// denies cross-user/shared access even when Unix bits would allow it), then
// the Unix interpretation. Model input cannot override the first step:
// policy and sameUser come from trusted application context.
func Enforce(policy domain.ScopePolicy, target domain.Scope, sameUser bool, meta domain.NodeMetadata, id Identity, want Access, forWrite bool) error {
	var err error
	if forWrite {
		err = policy.AuthorizeWrite(target, sameUser)
	} else {
		err = policy.AuthorizeRead(target, sameUser)
	}
	if err != nil {
		return err
	}
	if !CheckUnix(meta, id, want) {
		return domain.NewDeniedError(domain.CodePermissionDenied, "simulated unix permission denied", nil)
	}
	return nil
}
