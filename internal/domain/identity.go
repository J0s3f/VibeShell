package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Identity type prefixes for human-readable debugging and log correlation.
const (
	PrefixUser      = "usr"
	PrefixSession   = "ses"
	PrefixTurn      = "trn"
	PrefixAttempt   = "att"
	PrefixEvent     = "evt"
	PrefixContent   = "cnt"
	PrefixNode      = "nod"
	PrefixNamespace = "nsp"
	PrefixApp       = "app"
	PrefixAppVer    = "av"
	PrefixRoute     = "rte"
	PrefixAccount   = "acc"
	PrefixKeyRef    = "key"
)

// Identity encoding: prefix + base32 (Crockford) of 128-bit random value.
// This gives ~26 chars, URL-safe, no padding, case-insensitive decoding.
// Example: "usr_01ARZ3NDEKTSV4RRFFQ69G5FAV"

var (
	// ErrInvalidIdentity is returned when an identity string fails to parse or validate.
	ErrInvalidIdentity = errors.New("invalid identity")
	// ErrInvalidPrefix is returned when an identity has an unexpected prefix.
	ErrInvalidPrefix = errors.New("invalid identity prefix")
	// identityRegex validates the format: 2-3 letter prefix "_" 26-char base32.
	// Two-letter prefixes exist (e.g. "av" for app versions).
	identityRegex = regexp.MustCompile(`^[a-z]{2,3}_[0-9A-HJKMNP-TV-Z]{26}$`)
)

// ParseIdentity parses an identity string into its prefix and raw value.
// It does not validate the prefix against known types; use the typed Parse* functions for that.
func ParseIdentity(s string) (prefix, value string, err error) {
	if !identityRegex.MatchString(s) {
		return "", "", fmt.Errorf("%w: %q", ErrInvalidIdentity, s)
	}
	parts := strings.SplitN(s, "_", 2)
	return parts[0], parts[1], nil
}

// MustParseIdentity panics if the identity is invalid. For tests and constants only.
func MustParseIdentity(s string) (prefix, value string) {
	p, v, err := ParseIdentity(s)
	if err != nil {
		panic(err)
	}
	return p, v
}

// Identity is the base interface for all strongly-typed identities.
type Identity interface {
	String() string
	Prefix() string
	Value() string
	Equal(Identity) bool
}

// baseIdentity holds the common implementation.
type baseIdentity struct {
	prefix string
	value  string
}

func (id baseIdentity) String() string { return id.prefix + "_" + id.value }
func (id baseIdentity) Prefix() string { return id.prefix }
func (id baseIdentity) Value() string  { return id.value }
func (id baseIdentity) Equal(other Identity) bool {
	if other == nil {
		return false
	}
	return id.String() == other.String()
}

// UserID identifies a durable user identity (internal, not the displayed username).
type UserID baseIdentity

func ParseUserID(s string) (UserID, error) {
	p, v, err := ParseIdentity(s)
	if err != nil {
		return UserID{}, err
	}
	if p != PrefixUser {
		return UserID{}, fmt.Errorf("%w: expected %q got %q", ErrInvalidPrefix, PrefixUser, p)
	}
	return UserID{prefix: p, value: v}, nil
}

func (id UserID) String() string            { return baseIdentity(id).String() }
func (id UserID) Prefix() string            { return baseIdentity(id).Prefix() }
func (id UserID) Value() string             { return baseIdentity(id).Value() }
func (id UserID) Equal(other Identity) bool { return baseIdentity(id).Equal(other) }

// MustParseUserID panics if the identity is invalid. For tests and constants only.
func MustParseUserID(s string) UserID {
	u, err := ParseUserID(s)
	if err != nil {
		panic(err)
	}
	return u
}

// SessionID identifies a single SSH shell session (connection + channel).
type SessionID baseIdentity

func ParseSessionID(s string) (SessionID, error) {
	p, v, err := ParseIdentity(s)
	if err != nil {
		return SessionID{}, err
	}
	if p != PrefixSession {
		return SessionID{}, fmt.Errorf("%w: expected %q got %q", ErrInvalidPrefix, PrefixSession, p)
	}
	return SessionID{prefix: p, value: v}, nil
}

func (id SessionID) String() string            { return baseIdentity(id).String() }
func (id SessionID) Prefix() string            { return baseIdentity(id).Prefix() }
func (id SessionID) Value() string             { return baseIdentity(id).Value() }
func (id SessionID) Equal(other Identity) bool { return baseIdentity(id).Equal(other) }

// TurnID identifies a single agent turn within a session.
type TurnID baseIdentity

func ParseTurnID(s string) (TurnID, error) {
	p, v, err := ParseIdentity(s)
	if err != nil {
		return TurnID{}, err
	}
	if p != PrefixTurn {
		return TurnID{}, fmt.Errorf("%w: expected %q got %q", ErrInvalidPrefix, PrefixTurn, p)
	}
	return TurnID{prefix: p, value: v}, nil
}

func (id TurnID) String() string            { return baseIdentity(id).String() }
func (id TurnID) Prefix() string            { return baseIdentity(id).Prefix() }
func (id TurnID) Value() string             { return baseIdentity(id).Value() }
func (id TurnID) Equal(other Identity) bool { return baseIdentity(id).Equal(other) }

// AttemptID identifies a single model attempt within a turn (for retries/failover).
type AttemptID baseIdentity

func ParseAttemptID(s string) (AttemptID, error) {
	p, v, err := ParseIdentity(s)
	if err != nil {
		return AttemptID{}, err
	}
	if p != PrefixAttempt {
		return AttemptID{}, fmt.Errorf("%w: expected %q got %q", ErrInvalidPrefix, PrefixAttempt, p)
	}
	return AttemptID{prefix: p, value: v}, nil
}

func (id AttemptID) String() string            { return baseIdentity(id).String() }
func (id AttemptID) Prefix() string            { return baseIdentity(id).Prefix() }
func (id AttemptID) Value() string             { return baseIdentity(id).Value() }
func (id AttemptID) Equal(other Identity) bool { return baseIdentity(id).Equal(other) }

// MustParseAttemptID panics if the identity is invalid. For tests and constants only.
func MustParseAttemptID(s string) AttemptID {
	a, err := ParseAttemptID(s)
	if err != nil {
		panic(err)
	}
	return a
}

// EventID identifies a single immutable research event.
type EventID baseIdentity

func ParseEventID(s string) (EventID, error) {
	p, v, err := ParseIdentity(s)
	if err != nil {
		return EventID{}, err
	}
	if p != PrefixEvent {
		return EventID{}, fmt.Errorf("%w: expected %q got %q", ErrInvalidPrefix, PrefixEvent, p)
	}
	return EventID{prefix: p, value: v}, nil
}

func (id EventID) String() string            { return baseIdentity(id).String() }
func (id EventID) Prefix() string            { return baseIdentity(id).Prefix() }
func (id EventID) Value() string             { return baseIdentity(id).Value() }
func (id EventID) Equal(other Identity) bool { return baseIdentity(id).Equal(other) }

// ContentID identifies immutable content bytes (hash-addressed).
type ContentID baseIdentity

func ParseContentID(s string) (ContentID, error) {
	p, v, err := ParseIdentity(s)
	if err != nil {
		return ContentID{}, err
	}
	if p != PrefixContent {
		return ContentID{}, fmt.Errorf("%w: expected %q got %q", ErrInvalidPrefix, PrefixContent, p)
	}
	return ContentID{prefix: p, value: v}, nil
}

func (id ContentID) String() string            { return baseIdentity(id).String() }
func (id ContentID) Prefix() string            { return baseIdentity(id).Prefix() }
func (id ContentID) Value() string             { return baseIdentity(id).Value() }
func (id ContentID) Equal(other Identity) bool { return baseIdentity(id).Equal(other) }

// NodeID identifies a filesystem node (file, dir, symlink).
type NodeID baseIdentity

func ParseNodeID(s string) (NodeID, error) {
	p, v, err := ParseIdentity(s)
	if err != nil {
		return NodeID{}, err
	}
	if p != PrefixNode {
		return NodeID{}, fmt.Errorf("%w: expected %q got %q", ErrInvalidPrefix, PrefixNode, p)
	}
	return NodeID{prefix: p, value: v}, nil
}

func (id NodeID) String() string            { return baseIdentity(id).String() }
func (id NodeID) Prefix() string            { return baseIdentity(id).Prefix() }
func (id NodeID) Value() string             { return baseIdentity(id).Value() }
func (id NodeID) Equal(other Identity) bool { return baseIdentity(id).Equal(other) }

// NamespaceID identifies a world namespace (user, shared, baseline).
type NamespaceID baseIdentity

func ParseNamespaceID(s string) (NamespaceID, error) {
	p, v, err := ParseIdentity(s)
	if err != nil {
		return NamespaceID{}, err
	}
	if p != PrefixNamespace {
		return NamespaceID{}, fmt.Errorf("%w: expected %q got %q", ErrInvalidPrefix, PrefixNamespace, p)
	}
	return NamespaceID{prefix: p, value: v}, nil
}

func (id NamespaceID) String() string            { return baseIdentity(id).String() }
func (id NamespaceID) Prefix() string            { return baseIdentity(id).Prefix() }
func (id NamespaceID) Value() string             { return baseIdentity(id).Value() }
func (id NamespaceID) Equal(other Identity) bool { return baseIdentity(id).Equal(other) }

// AppID identifies a generated application (immutable artifact).
type AppID baseIdentity

func ParseAppID(s string) (AppID, error) {
	p, v, err := ParseIdentity(s)
	if err != nil {
		return AppID{}, err
	}
	if p != PrefixApp {
		return AppID{}, fmt.Errorf("%w: expected %q got %q", ErrInvalidPrefix, PrefixApp, p)
	}
	return AppID{prefix: p, value: v}, nil
}

func (id AppID) String() string            { return baseIdentity(id).String() }
func (id AppID) Prefix() string            { return baseIdentity(id).Prefix() }
func (id AppID) Value() string             { return baseIdentity(id).Value() }
func (id AppID) Equal(other Identity) bool { return baseIdentity(id).Equal(other) }

// MustParseAppID panics if the identity is invalid. For tests and constants only.
func MustParseAppID(s string) AppID {
	a, err := ParseAppID(s)
	if err != nil {
		panic(err)
	}
	return a
}

// AppVersionID identifies a specific version of a generated application.
type AppVersionID baseIdentity

func ParseAppVersionID(s string) (AppVersionID, error) {
	p, v, err := ParseIdentity(s)
	if err != nil {
		return AppVersionID{}, err
	}
	if p != PrefixAppVer {
		return AppVersionID{}, fmt.Errorf("%w: expected %q got %q", ErrInvalidPrefix, PrefixAppVer, p)
	}
	return AppVersionID{prefix: p, value: v}, nil
}

func (id AppVersionID) String() string            { return baseIdentity(id).String() }
func (id AppVersionID) Prefix() string            { return baseIdentity(id).Prefix() }
func (id AppVersionID) Value() string             { return baseIdentity(id).Value() }
func (id AppVersionID) Equal(other Identity) bool { return baseIdentity(id).Equal(other) }

// RouteID identifies a configured model route (provider+product+model+protocol).
type RouteID baseIdentity

func ParseRouteID(s string) (RouteID, error) {
	p, v, err := ParseIdentity(s)
	if err != nil {
		return RouteID{}, err
	}
	if p != PrefixRoute {
		return RouteID{}, fmt.Errorf("%w: expected %q got %q", ErrInvalidPrefix, PrefixRoute, p)
	}
	return RouteID{prefix: p, value: v}, nil
}

func (id RouteID) String() string            { return baseIdentity(id).String() }
func (id RouteID) Prefix() string            { return baseIdentity(id).Prefix() }
func (id RouteID) Value() string             { return baseIdentity(id).Value() }
func (id RouteID) Equal(other Identity) bool { return baseIdentity(id).Equal(other) }

// MustParseRouteID panics if the identity is invalid. For tests and constants only.
func MustParseRouteID(s string) RouteID {
	r, err := ParseRouteID(s)
	if err != nil {
		panic(err)
	}
	return r
}

// AccountID identifies a provider account (groups keys, quota).
type AccountID baseIdentity

func ParseAccountID(s string) (AccountID, error) {
	p, v, err := ParseIdentity(s)
	if err != nil {
		return AccountID{}, err
	}
	if p != PrefixAccount {
		return AccountID{}, fmt.Errorf("%w: expected %q got %q", ErrInvalidPrefix, PrefixAccount, p)
	}
	return AccountID{prefix: p, value: v}, nil
}

func (id AccountID) String() string            { return baseIdentity(id).String() }
func (id AccountID) Prefix() string            { return baseIdentity(id).Prefix() }
func (id AccountID) Value() string             { return baseIdentity(id).Value() }
func (id AccountID) Equal(other Identity) bool { return baseIdentity(id).Equal(other) }

// MustParseAccountID panics if the identity is invalid. For tests and constants only.
func MustParseAccountID(s string) AccountID {
	a, err := ParseAccountID(s)
	if err != nil {
		panic(err)
	}
	return a
}

// KeyRef is an opaque reference to a secret credential (never the secret itself).
type KeyRef baseIdentity

func ParseKeyRef(s string) (KeyRef, error) {
	p, v, err := ParseIdentity(s)
	if err != nil {
		return KeyRef{}, err
	}
	if p != PrefixKeyRef {
		return KeyRef{}, fmt.Errorf("%w: expected %q got %q", ErrInvalidPrefix, PrefixKeyRef, p)
	}
	return KeyRef{prefix: p, value: v}, nil
}

func (id KeyRef) String() string            { return baseIdentity(id).String() }
func (id KeyRef) Prefix() string            { return baseIdentity(id).Prefix() }
func (id KeyRef) Value() string             { return baseIdentity(id).Value() }
func (id KeyRef) Equal(other Identity) bool { return baseIdentity(id).Equal(other) }

// JSON encoding represents every identity as its canonical string form
// ("prefix_value"). Methods are defined per concrete type because methods
// on baseIdentity do not promote through the defined-type conversions used
// above; without these, encoding/json would emit empty objects since the
// struct fields are unexported.
//
// The zero identity encodes as the empty JSON string, not as the "_" that
// String() would produce: an unset or absent reference must round-trip back
// to the zero identity instead of failing validation on decode. Callers still
// reject zero where an identity is required (see IsZero and the validators).

// marshalIdentityJSON encodes an identity's prefix and value. The zero
// identity encodes as "" so that it round-trips; every other identity keeps
// the canonical "prefix_value" shape.
func marshalIdentityJSON(prefix, value string) ([]byte, error) {
	if prefix == "" && value == "" {
		return json.Marshal("")
	}
	return json.Marshal(prefix + "_" + value)
}

// unmarshalIdentityJSON decodes an identity from its JSON string form. JSON
// null and the empty string mean "unset" and return the zero identity without
// error; any other value is passed to parse, which enforces the expected
// prefix.
func unmarshalIdentityJSON[T any](data []byte, parse func(string) (T, error), zero T) (T, error) {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return zero, err
	}
	if s == "" {
		return zero, nil
	}
	return parse(s)
}

func (id UserID) MarshalJSON() ([]byte, error)      { return marshalIdentityJSON(id.prefix, id.value) }
func (id SessionID) MarshalJSON() ([]byte, error)   { return marshalIdentityJSON(id.prefix, id.value) }
func (id TurnID) MarshalJSON() ([]byte, error)      { return marshalIdentityJSON(id.prefix, id.value) }
func (id AttemptID) MarshalJSON() ([]byte, error)   { return marshalIdentityJSON(id.prefix, id.value) }
func (id EventID) MarshalJSON() ([]byte, error)     { return marshalIdentityJSON(id.prefix, id.value) }
func (id ContentID) MarshalJSON() ([]byte, error)   { return marshalIdentityJSON(id.prefix, id.value) }
func (id NodeID) MarshalJSON() ([]byte, error)      { return marshalIdentityJSON(id.prefix, id.value) }
func (id NamespaceID) MarshalJSON() ([]byte, error) { return marshalIdentityJSON(id.prefix, id.value) }
func (id AppID) MarshalJSON() ([]byte, error)       { return marshalIdentityJSON(id.prefix, id.value) }
func (id AppVersionID) MarshalJSON() ([]byte, error) {
	return marshalIdentityJSON(id.prefix, id.value)
}
func (id RouteID) MarshalJSON() ([]byte, error)   { return marshalIdentityJSON(id.prefix, id.value) }
func (id AccountID) MarshalJSON() ([]byte, error) { return marshalIdentityJSON(id.prefix, id.value) }
func (id KeyRef) MarshalJSON() ([]byte, error)    { return marshalIdentityJSON(id.prefix, id.value) }

func (id *UserID) UnmarshalJSON(data []byte) error {
	parsed, err := unmarshalIdentityJSON(data, ParseUserID, UserID{})
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

func (id *SessionID) UnmarshalJSON(data []byte) error {
	parsed, err := unmarshalIdentityJSON(data, ParseSessionID, SessionID{})
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

func (id *TurnID) UnmarshalJSON(data []byte) error {
	parsed, err := unmarshalIdentityJSON(data, ParseTurnID, TurnID{})
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

func (id *AttemptID) UnmarshalJSON(data []byte) error {
	parsed, err := unmarshalIdentityJSON(data, ParseAttemptID, AttemptID{})
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

func (id *EventID) UnmarshalJSON(data []byte) error {
	parsed, err := unmarshalIdentityJSON(data, ParseEventID, EventID{})
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

func (id *ContentID) UnmarshalJSON(data []byte) error {
	parsed, err := unmarshalIdentityJSON(data, ParseContentID, ContentID{})
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

func (id *NodeID) UnmarshalJSON(data []byte) error {
	parsed, err := unmarshalIdentityJSON(data, ParseNodeID, NodeID{})
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

func (id *NamespaceID) UnmarshalJSON(data []byte) error {
	parsed, err := unmarshalIdentityJSON(data, ParseNamespaceID, NamespaceID{})
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

func (id *AppID) UnmarshalJSON(data []byte) error {
	parsed, err := unmarshalIdentityJSON(data, ParseAppID, AppID{})
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

func (id *AppVersionID) UnmarshalJSON(data []byte) error {
	parsed, err := unmarshalIdentityJSON(data, ParseAppVersionID, AppVersionID{})
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

func (id *RouteID) UnmarshalJSON(data []byte) error {
	parsed, err := unmarshalIdentityJSON(data, ParseRouteID, RouteID{})
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

func (id *AccountID) UnmarshalJSON(data []byte) error {
	parsed, err := unmarshalIdentityJSON(data, ParseAccountID, AccountID{})
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

func (id *KeyRef) UnmarshalJSON(data []byte) error {
	parsed, err := unmarshalIdentityJSON(data, ParseKeyRef, KeyRef{})
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// IsZero reports whether the identity is the unset zero value.
// Adapters treat zero IDs as absent references, never as valid keys.
func (id UserID) IsZero() bool       { return id == UserID{} }
func (id SessionID) IsZero() bool    { return id == SessionID{} }
func (id TurnID) IsZero() bool       { return id == TurnID{} }
func (id AttemptID) IsZero() bool    { return id == AttemptID{} }
func (id EventID) IsZero() bool      { return id == EventID{} }
func (id ContentID) IsZero() bool    { return id == ContentID{} }
func (id NodeID) IsZero() bool       { return id == NodeID{} }
func (id NamespaceID) IsZero() bool  { return id == NamespaceID{} }
func (id AppID) IsZero() bool        { return id == AppID{} }
func (id AppVersionID) IsZero() bool { return id == AppVersionID{} }
func (id RouteID) IsZero() bool      { return id == RouteID{} }
func (id AccountID) IsZero() bool    { return id == AccountID{} }
func (id KeyRef) IsZero() bool       { return id == KeyRef{} }

// Typed unmarshal helpers for each identity type. As with the JSON methods,
// null and "" decode to the zero identity so absent references stay valid.
func UnmarshalUserID(data []byte) (UserID, error) {
	return unmarshalIdentityJSON(data, ParseUserID, UserID{})
}

func UnmarshalSessionID(data []byte) (SessionID, error) {
	return unmarshalIdentityJSON(data, ParseSessionID, SessionID{})
}

func UnmarshalTurnID(data []byte) (TurnID, error) {
	return unmarshalIdentityJSON(data, ParseTurnID, TurnID{})
}

func UnmarshalAttemptID(data []byte) (AttemptID, error) {
	return unmarshalIdentityJSON(data, ParseAttemptID, AttemptID{})
}

func UnmarshalEventID(data []byte) (EventID, error) {
	return unmarshalIdentityJSON(data, ParseEventID, EventID{})
}

func UnmarshalContentID(data []byte) (ContentID, error) {
	return unmarshalIdentityJSON(data, ParseContentID, ContentID{})
}

func UnmarshalNodeID(data []byte) (NodeID, error) {
	return unmarshalIdentityJSON(data, ParseNodeID, NodeID{})
}

func UnmarshalNamespaceID(data []byte) (NamespaceID, error) {
	return unmarshalIdentityJSON(data, ParseNamespaceID, NamespaceID{})
}

func UnmarshalAppID(data []byte) (AppID, error) {
	return unmarshalIdentityJSON(data, ParseAppID, AppID{})
}

func UnmarshalAppVersionID(data []byte) (AppVersionID, error) {
	return unmarshalIdentityJSON(data, ParseAppVersionID, AppVersionID{})
}

func UnmarshalRouteID(data []byte) (RouteID, error) {
	return unmarshalIdentityJSON(data, ParseRouteID, RouteID{})
}

func UnmarshalAccountID(data []byte) (AccountID, error) {
	return unmarshalIdentityJSON(data, ParseAccountID, AccountID{})
}

func UnmarshalKeyRef(data []byte) (KeyRef, error) {
	return unmarshalIdentityJSON(data, ParseKeyRef, KeyRef{})
}
