package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Scope identifies the durability and visibility scope of world state.
type Scope int

const (
	ScopeSession  Scope = iota // session: cwd, shell vars, editor buffers, terminal state
	ScopeUser                  // user: home files, personal modifications, user-associated facts
	ScopeShared                // shared: common machine facts, globally adopted changes (when enabled)
	ScopeBaseline              // baseline: versioned clean-install seed, immutable
)

// String returns the canonical scope name.
func (s Scope) String() string {
	switch s {
	case ScopeSession:
		return "session"
	case ScopeUser:
		return "user"
	case ScopeShared:
		return "shared"
	case ScopeBaseline:
		return "baseline"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

// ParseScope parses a scope name (case-insensitive).
func ParseScope(s string) (Scope, error) {
	switch strings.ToLower(s) {
	case "session":
		return ScopeSession, nil
	case "user":
		return ScopeUser, nil
	case "shared":
		return ScopeShared, nil
	case "baseline":
		return ScopeBaseline, nil
	default:
		return ScopeSession, fmt.Errorf("%w: %q", ErrInvalidScope, s)
	}
}

// MarshalJSON implements json.Marshaler.
func (s Scope) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// UnmarshalJSON implements json.Unmarshaler.
func (s *Scope) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	parsed, err := ParseScope(str)
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// ErrInvalidScope is returned for unknown scope names.
var ErrInvalidScope = errors.New("invalid scope")

// ScopePolicy defines the effective sharing policy for a session/user.
type ScopePolicy struct {
	// SharingEnabled controls whether ScopeShared and other-user reads/writes are permitted.
	SharingEnabled bool `json:"sharing_enabled"`
	// PolicyRevision increments on each policy change for cache invalidation.
	PolicyRevision int64 `json:"policy_revision"`
	// CrossUserReadAllowed permits reading other users' user-scoped data when sharing is enabled.
	CrossUserReadAllowed bool `json:"cross_user_read_allowed"`
	// CrossUserWriteAllowed permits writing to other users' user-scoped data when sharing is enabled.
	CrossUserWriteAllowed bool `json:"cross_user_write_allowed"`
	// SharedWriteAllowed permits writing to shared scope when sharing is enabled.
	SharedWriteAllowed bool `json:"shared_write_allowed"`
}

// DefaultScopePolicy returns the baseline policy (sharing enabled, permissive).
func DefaultScopePolicy() ScopePolicy {
	return ScopePolicy{
		SharingEnabled:        true,
		PolicyRevision:        1,
		CrossUserReadAllowed:  true,
		CrossUserWriteAllowed: false,
		SharedWriteAllowed:    true,
	}
}

// RestrictedScopePolicy returns a policy with sharing disabled.
func RestrictedScopePolicy() ScopePolicy {
	return ScopePolicy{
		SharingEnabled:        false,
		PolicyRevision:        1,
		CrossUserReadAllowed:  false,
		CrossUserWriteAllowed: false,
		SharedWriteAllowed:    false,
	}
}

// Namespace identifies a world namespace (owner + scope).
type Namespace struct {
	ID        NamespaceID `json:"id"`
	Owner     UserID      `json:"owner"`      // owning user (or zero for shared/baseline)
	Scope     Scope       `json:"scope"`      // session/user/shared/baseline
	Label     string      `json:"label"`      // human-readable: "user:alice", "shared", "baseline:v1"
	CreatedAt int64       `json:"created_at"` // unix milliseconds
}

// NamespaceForUser returns the user-scoped namespace for a user.
func NamespaceForUser(user UserID, label string) Namespace {
	return Namespace{
		ID:        NamespaceID{}, // assigned on persistence
		Owner:     user,
		Scope:     ScopeUser,
		Label:     label,
		CreatedAt: 0, // set on persist
	}
}

// NamespaceForSession returns the session-scoped namespace for a session.
func NamespaceForSession(session SessionID) Namespace {
	return Namespace{
		ID:        NamespaceID{},
		Owner:     UserID{},
		Scope:     ScopeSession,
		Label:     "session:" + session.Value(),
		CreatedAt: 0,
	}
}

// NamespaceShared returns the shared namespace.
func NamespaceShared() Namespace {
	return Namespace{
		ID:        NamespaceID{},
		Owner:     UserID{},
		Scope:     ScopeShared,
		Label:     "shared",
		CreatedAt: 0,
	}
}

// NamespaceBaseline returns the baseline namespace for a version.
func NamespaceBaseline(version string) Namespace {
	return Namespace{
		ID:        NamespaceID{},
		Owner:     UserID{},
		Scope:     ScopeBaseline,
		Label:     "baseline:" + version,
		CreatedAt: 0,
	}
}

// IsUser returns true if this is a user-scoped namespace.
func (n Namespace) IsUser() bool { return n.Scope == ScopeUser }

// IsShared returns true if this is the shared namespace.
func (n Namespace) IsShared() bool { return n.Scope == ScopeShared }

// IsBaseline returns true if this is a baseline namespace.
func (n Namespace) IsBaseline() bool { return n.Scope == ScopeBaseline }

// IsSession returns true if this is a session-scoped namespace.
func (n Namespace) IsSession() bool { return n.Scope == ScopeSession }

// AuthorizeRead is the pure scope-policy check for reads. sameUser reports
// whether the target namespace belongs to the requesting user. It returns nil
// when the read is permitted and a denied DomainError otherwise. Sharing-off
// denies every cross-user and shared read regardless of model requests; scope
// filters are injected by trusted application context, never taken from
// model-supplied IDs.
func (p ScopePolicy) AuthorizeRead(target Scope, sameUser bool) error {
	switch target {
	case ScopeSession:
		return nil // session state is always caller-local
	case ScopeBaseline:
		return nil // baseline seed is world-readable
	case ScopeUser:
		if sameUser {
			return nil
		}
		if p.SharingEnabled && p.CrossUserReadAllowed {
			return nil
		}
		return NewDeniedError(CodeSharingDisabled, "cross-user read denied by scope policy", nil)
	case ScopeShared:
		if p.SharingEnabled {
			return nil
		}
		return NewDeniedError(CodeSharingDisabled, "shared read denied while sharing is disabled", nil)
	default:
		return NewDeniedError(CodeInvalidScope, "unknown scope", nil)
	}
}

// AuthorizeWrite is the pure scope-policy check for writes. Baseline is
// immutable; user writes stay with the owning user unless cross-user writes
// are explicitly enabled; shared writes require sharing plus the shared-write
// flag. Denials return a denied DomainError for errors.Is/As matching.
func (p ScopePolicy) AuthorizeWrite(target Scope, sameUser bool) error {
	switch target {
	case ScopeSession:
		return nil
	case ScopeBaseline:
		return NewDeniedError(CodePermissionDenied, "baseline scope is immutable", nil)
	case ScopeUser:
		if sameUser {
			return nil
		}
		if p.SharingEnabled && p.CrossUserWriteAllowed {
			return nil
		}
		return NewDeniedError(CodeSharingDisabled, "cross-user write denied by scope policy", nil)
	case ScopeShared:
		if p.SharingEnabled && p.SharedWriteAllowed {
			return nil
		}
		return NewDeniedError(CodeSharingDisabled, "shared write denied by scope policy", nil)
	default:
		return NewDeniedError(CodeInvalidScope, "unknown scope", nil)
	}
}
