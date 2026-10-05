package sshserver

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Username limits. Usernames are data: they are validated here and never
// interpolated into a path, a command, or an environment variable.
const (
	// MaxUsernameBytes bounds an accepted username.
	MaxUsernameBytes = 64
	// MaxPasswordBytes bounds a password accepted from a client. Anything
	// longer is refused without comparison, so an oversized password cannot
	// turn into unbounded hashing work.
	MaxPasswordBytes = 1024
)

// Errors an Authenticator returns. They are wrapped with a reason and are never
// sent to a client verbatim; the transport answers every authentication failure
// with the same SSH message.
var (
	// ErrUnauthenticated refuses an authentication attempt for any reason:
	// unknown user, wrong password, or a disabled account.
	ErrUnauthenticated = errors.New("authentication refused")
	// ErrInvalidUsername refuses a username that cannot be an identity.
	ErrInvalidUsername = errors.New("invalid username")
)

// Principal is the authenticated identity a session runs as.
type Principal struct {
	// Username is the name the client presented, unchanged.
	Username string
}

// ConnMetadata describes the connection during authentication.
type ConnMetadata interface {
	// RemoteAddress is the client's network address.
	RemoteAddress() net.Addr
	// User is the username from the authentication request.
	User() string
}

// PasswordAuthenticator authenticates a username and password.
type PasswordAuthenticator interface {
	// Password authenticates one attempt. Returning an error that wraps
	// ErrUnauthenticated refuses the connection. The same error must be
	// returned for an unknown user and for a wrong password so the client
	// cannot distinguish them.
	Password(meta ConnMetadata, username, password string) (Principal, error)
}

// PasswordEntry is one record of the spike's stand-in for the versioned
// password file. Hashing and file format belong to the secure-password task;
// this spike only needs enabled and disabled accounts with a known secret.
type PasswordEntry struct {
	Password string
	Disabled bool
}

// PasswordFile is an in-memory password file for the spike.
type PasswordFile map[string]PasswordEntry

// Password authenticates against the file. A disabled account is refused even
// when its password is correct.
func (f PasswordFile) Password(_ ConnMetadata, username, password string) (Principal, error) {
	if err := ValidateUsername(username); err != nil {
		return Principal{}, err
	}
	if len(password) > MaxPasswordBytes {
		return Principal{}, fmt.Errorf("%w: password longer than %d bytes", ErrUnauthenticated, MaxPasswordBytes)
	}
	entry, ok := f[username]
	if !ok {
		// A real implementation performs dummy verification here so that an
		// unknown user costs the same as a wrong password.
		return Principal{}, fmt.Errorf("%w: no such user", ErrUnauthenticated)
	}
	if entry.Disabled {
		return Principal{}, fmt.Errorf("%w: account disabled", ErrUnauthenticated)
	}
	if password != entry.Password {
		return Principal{}, fmt.Errorf("%w: wrong password", ErrUnauthenticated)
	}
	return Principal{Username: username}, nil
}

// ValidateUsername reports whether name can be a VibeShell identity.
//
// Validation is deliberately narrow: valid UTF-8, at most MaxUsernameBytes,
// printable, and free of control characters, spaces, and the separators that
// would make a name ambiguous as a path component. Two distinct names are never
// folded onto one identity, so no normalization is applied.
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

// PublicPrincipal validates a username for public mode, where any acceptable
// name becomes an identity and no credential is presented.
func PublicPrincipal(meta ConnMetadata) (Principal, error) {
	name := meta.User()
	if err := ValidateUsername(name); err != nil {
		return Principal{}, err
	}
	return Principal{Username: name}, nil
}
