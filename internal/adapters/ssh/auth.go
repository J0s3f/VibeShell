package ssh

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Username and password bounds. Usernames are data: they are validated here
// and never interpolated into a path, a command, or an environment variable.
// The password bound caps hashing work before the authenticator runs, so an
// oversized password cannot turn into unbounded CPU/memory use.
const (
	// MaxUsernameBytes bounds an accepted username.
	MaxUsernameBytes = 64
	// MaxPasswordBytes bounds a password forwarded to the authenticator.
	// Anything longer is refused without comparison.
	MaxPasswordBytes = 1024
)

// Authentication errors. They are never sent to a client verbatim; every
// authentication failure produces the same SSH failure message.
var (
	// ErrUnauthenticated refuses an authentication attempt for any reason:
	// unknown user, wrong password, or a disabled account.
	ErrUnauthenticated = errors.New("authentication refused")
	// ErrInvalidUsername refuses a username that cannot be an identity.
	ErrInvalidUsername = errors.New("invalid username")
)

// Principal is the authenticated identity a session runs as. The transport
// keeps the presented name unchanged; mapping it to a durable internal
// identity (and separating public/secure namespaces per PLAN 4.1) is the
// application layer's job, not the transport's.
type Principal struct {
	// Username is the name the client presented, unchanged.
	Username string
}

// PasswordAuthenticator checks one username/password attempt. It is the seam
// where the secure-password task (B08) plugs in its password-file lookup and
// Argon2id verification; this package owns only the transport callback.
//
// Contract for implementations:
//   - Inputs are pre-bounded: username passed ValidateUsername and password
//     is at most MaxPasswordBytes.
//   - Return the same ErrUnauthenticated-wrapping error for an unknown user,
//     a wrong password, and a disabled account (with dummy verification for
//     unknown users) so a client cannot distinguish them.
//   - Error text must never contain the password or any hash material; the
//     server logs it for operators.
//   - Implementations must be safe for concurrent use: the server
//     authenticates connections concurrently.
type PasswordAuthenticator interface {
	AuthenticatePassword(username, password string) (Principal, error)
}

// PasswordAuthenticatorFunc adapts a function to PasswordAuthenticator.
type PasswordAuthenticatorFunc func(username, password string) (Principal, error)

// AuthenticatePassword calls f.
func (f PasswordAuthenticatorFunc) AuthenticatePassword(username, password string) (Principal, error) {
	return f(username, password)
}

// ValidateUsername reports whether name can be a VibeShell identity.
//
// Validation is deliberately narrow: valid UTF-8, at most MaxUsernameBytes,
// printable, and free of control characters, spaces, and path separators.
// Two distinct names are never folded onto one identity, so no normalization
// or case folding is applied.
func ValidateUsername(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrInvalidUsername)
	}
	if len(name) > MaxUsernameBytes {
		return fmt.Errorf("%w: longer than %d bytes", ErrInvalidUsername, MaxUsernameBytes)
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("%w: not valid UTF-8", ErrInvalidUsername)
	}
	if strings.ContainsRune(name, '/') || strings.ContainsRune(name, '\\') {
		return fmt.Errorf("%w: contains a path separator", ErrInvalidUsername)
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return fmt.Errorf("%w: contains a control or space character", ErrInvalidUsername)
		}
		if r == utf8.RuneError {
			return fmt.Errorf("%w: contains an unpaired surrogate", ErrInvalidUsername)
		}
	}
	return nil
}

// publicPrincipal validates a username for public mode, where any acceptable
// name becomes an identity and no credential is presented.
func publicPrincipal(username string) (Principal, error) {
	if err := ValidateUsername(username); err != nil {
		return Principal{}, err
	}
	return Principal{Username: username}, nil
}
