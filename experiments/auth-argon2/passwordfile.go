package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"unicode"
	"unicode/utf8"
)

// Schema identification and field bounds of the versioned password file
// (PLAN 4.2). The file stores only usernames, encoded hashes, enabled state,
// and identity references; plaintext passwords are never persisted.
const (
	PasswordFileSchema  = "vibeshell/password-file"
	PasswordFileVersion = 1
	MaxUsernameBytes    = 64
	MaxIdentityRefBytes = 128
)

var (
	// ErrSchemaMismatch reports an unrecognized schema or version.
	ErrSchemaMismatch = errors.New("unrecognized password file schema or version")
	// ErrDuplicateUsername reports the same username twice in one file.
	// Usernames compare byte-for-byte; nothing is lowercased or normalized.
	ErrDuplicateUsername = errors.New("duplicate username in password file")
	// ErrInvalidEntry reports a structurally invalid password file entry.
	ErrInvalidEntry = errors.New("invalid password file entry")
)

// UserEntry is one account in the password file. Hash is the PHC-encoded
// Argon2id hash; IdentityRef is the stable internal identity this username
// maps to (PLAN 4.1), never a filesystem path.
type UserEntry struct {
	Username    string `json:"username"`
	Hash        string `json:"hash"`
	Enabled     bool   `json:"enabled"`
	IdentityRef string `json:"identityRef"`
}

// PasswordFile is the versioned on-disk format. Entries are immutable once
// parsed; a reload swaps the whole value.
type PasswordFile struct {
	Schema  string      `json:"schema"`
	Version int         `json:"version"`
	Users   []UserEntry `json:"users"`
}

// ParsePasswordFile decodes and fully validates a password file. Decoding is
// strict: unknown fields, trailing JSON, malformed hashes, out-of-bounds
// parameters, invalid usernames, and duplicate usernames are all rejected
// before the value is usable.
func ParsePasswordFile(data []byte) (PasswordFile, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var file PasswordFile
	if err := decoder.Decode(&file); err != nil {
		return PasswordFile{}, fmt.Errorf("decode password file: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return PasswordFile{}, errors.New("decode password file: trailing data after JSON value")
	}
	if err := file.validate(); err != nil {
		return PasswordFile{}, err
	}
	return file, nil
}

// Encode validates the file and returns its canonical JSON encoding.
func (f PasswordFile) Encode() ([]byte, error) {
	if err := f.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(f)
}

// Find returns the entry for username using exact byte comparison.
func (f PasswordFile) Find(username string) (UserEntry, bool) {
	for _, entry := range f.Users {
		if entry.Username == username {
			return entry, true
		}
	}
	return UserEntry{}, false
}

func (f PasswordFile) validate() error {
	if f.Schema != PasswordFileSchema || f.Version != PasswordFileVersion {
		return fmt.Errorf("%w: schema %q version %d", ErrSchemaMismatch, f.Schema, f.Version)
	}
	seen := make(map[string]struct{}, len(f.Users))
	for i, entry := range f.Users {
		if err := entry.validate(); err != nil {
			return fmt.Errorf("%w: user %d: %w", ErrInvalidEntry, i, err)
		}
		if _, dup := seen[entry.Username]; dup {
			return fmt.Errorf("%w: %q", ErrDuplicateUsername, entry.Username)
		}
		seen[entry.Username] = struct{}{}
	}
	return nil
}

func (e UserEntry) validate() error {
	if err := validateIdentifier("username", e.Username, MaxUsernameBytes); err != nil {
		return err
	}
	if _, err := ParseEncoded(e.Hash); err != nil {
		return fmt.Errorf("hash: %w", err)
	}
	if err := validateIdentifier("identityRef", e.IdentityRef, MaxIdentityRefBytes); err != nil {
		return err
	}
	return nil
}

// validateIdentifier applies the length and control-character checks PLAN 4.1
// requires for usernames, also used for identity references.
func validateIdentifier(field, value string, maxBytes int) error {
	if value == "" {
		return fmt.Errorf("%s is empty", field)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%s is %d bytes, limit %d", field, len(value), maxBytes)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", field)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains control character U+%04X", field, r)
		}
	}
	return nil
}

// FileStore holds the currently loaded password file. Load validates the new
// bytes completely before atomically replacing the snapshot; a failed load
// keeps the previous snapshot serving (PLAN 4.2).
type FileStore struct {
	snapshot atomic.Pointer[PasswordFile]
}

// Load validates data and, only on success, makes it the current snapshot.
func (s *FileStore) Load(data []byte) error {
	file, err := ParsePasswordFile(data)
	if err != nil {
		return err
	}
	s.snapshot.Store(&file)
	return nil
}

// Current returns the current snapshot, if any. The returned value must be
// treated as immutable.
func (s *FileStore) Current() (PasswordFile, bool) {
	file := s.snapshot.Load()
	if file == nil {
		return PasswordFile{}, false
	}
	return *file, true
}
