package config

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// Password file constraints (PLAN 4.2). The size bound keeps a hostile or
// mistaken file from turning login into an unbounded parse; the mode
// requirement keeps group/world-readable files out of the authentication
// path.
const (
	// PasswordFileVersion is the only password file format this build
	// accepts.
	PasswordFileVersion  = 1
	maxPasswordFileBytes = 1 << 20
	maxPasswordBytes     = 1024
	maxUsernameBytes     = 64
	// passwordFileMode is the only mode a password file may carry: the
	// owner can read and write it, nobody else can read it.
	passwordFileMode os.FileMode = 0o600
)

// PasswordHash is an encoded Argon2id hash stored in the password file.
// Every string form is redacted so a password file loaded into memory
// cannot leak a hash through a log line or a marshaled structure; saving
// the file goes through private wire types that carry the real value.
type PasswordHash string

// String implements fmt.Stringer with a redacted placeholder.
func (h PasswordHash) String() string { return redacted }

// GoString implements fmt.GoStringer for %#v formatting.
func (h PasswordHash) GoString() string { return redacted }

// MarshalJSON emits a redacted placeholder; use PasswordFile encoding via
// the password store to write the real value back to disk.
func (h PasswordHash) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// User is one password file entry: the login name, its encoded hash, the
// enabled flag, and the stable user identity a successful login maps to.
type User struct {
	Username string       `json:"username"`
	Hash     PasswordHash `json:"hash"`
	Enabled  bool         `json:"enabled"`
	Identity string       `json:"identity"`
}

// PasswordFile is the versioned password file document (PLAN 4.2).
type PasswordFile struct {
	Version int    `json:"version"`
	Users   []User `json:"users"`
}

// Lookup returns the entry for username.
func (f *PasswordFile) Lookup(username string) (User, bool) {
	for _, u := range f.Users {
		if u.Username == username {
			return u, true
		}
	}
	return User{}, false
}

// passwordFileWire and passwordUserWire are the on-disk shapes. They exist
// so saving writes real hash strings while the in-memory User type stays
// redacted under every formatting verb.
type passwordFileWire struct {
	Version int                `json:"version"`
	Users   []passwordUserWire `json:"users"`
}

type passwordUserWire struct {
	Username string `json:"username"`
	Hash     string `json:"hash"`
	Enabled  bool   `json:"enabled"`
	Identity string `json:"identity"`
}

// checkPasswordFile verifies that the configured secure-mode password file
// exists, is a regular file with owner-only permissions, and parses
// strictly with every entry valid. Loader.Validate calls it so a broken
// password file fails at startup instead of at the first login.
func checkPasswordFile(file string) error {
	_, err := ReadPasswordFile(file)
	return err
}

// ReadPasswordFile reads, strictly decodes, and validates the password
// file at path. Problems are reported as a ValidationError with the same
// JSON-path diagnostics the configuration document uses.
func ReadPasswordFile(path string) (*PasswordFile, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("password file %q does not exist (create it before enabling auth.mode \"secure\")", path)
		}
		return nil, fmt.Errorf("cannot stat password file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("password file %q is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("password file %q has mode %04o; it must not grant group or world access (chmod 600)", path, perm)
	}
	if info.Size() > maxPasswordFileBytes {
		return nil, fmt.Errorf("password file %q is larger than the %d byte limit", path, maxPasswordFileBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read password file %q: %w", path, err)
	}
	f, _, problems := decodePasswordFile(raw)
	if len(problems) > 0 {
		return nil, &ValidationError{Problems: problems}
	}
	return f, nil
}

// decodePasswordFile applies the same strict decoding as the configuration
// document: unknown fields, duplicate keys, wrong scalar kinds, missing
// required fields, and out-of-bounds hash parameters all fail together in
// one problem list.
func decodePasswordFile(raw []byte) (*PasswordFile, map[string]bool, []Problem) {
	var f PasswordFile
	present, problems := decodeStrict(raw, &f)
	if len(problems) == 0 {
		problems = validatePasswordFile(&f, present)
	}
	return &f, present, problems
}

// validatePasswordFile checks required-field presence (when present is
// non-nil) and every value. A nil present map means the caller constructed
// the document itself, so every field counts as present and only the value
// rules run; the password store passes nil after each mutation.
func validatePasswordFile(f *PasswordFile, present map[string]bool) []Problem {
	v := &passwordValidator{f: f, present: present}
	v.run()
	return v.problems
}

type passwordValidator struct {
	f       *PasswordFile
	present map[string]bool

	problems []Problem
}

func (v *passwordValidator) add(path, message string) {
	v.problems = append(v.problems, Problem{Path: path, Message: message})
}

// required reports whether path is present, recording a problem when it is
// not, so a missing field never doubles as an invalid value.
func (v *passwordValidator) required(path string) bool {
	if v.present == nil || v.present[path] {
		return true
	}
	v.add(path, "required field is missing")
	return false
}

func (v *passwordValidator) run() {
	if v.required("version") && v.f.Version != PasswordFileVersion {
		v.add("version", fmt.Sprintf("unsupported password file version %d (this build accepts %d)", v.f.Version, PasswordFileVersion))
	}
	if !v.required("users") {
		return
	}
	seen := make(map[string]int, len(v.f.Users))
	for i := range v.f.Users {
		u := &v.f.Users[i]
		p := fmt.Sprintf("users[%d]", i)
		if v.required(p + ".username") {
			if err := validateUsername(u.Username); err != nil {
				v.add(p+".username", err.Error())
			} else if first, dup := seen[u.Username]; dup {
				v.add(p+".username", fmt.Sprintf("duplicate username %q (first declared at users[%d])", u.Username, first))
			} else {
				seen[u.Username] = i
			}
		}
		if v.required(p + ".hash") {
			if _, _, _, err := decodePHC(string(u.Hash)); err != nil {
				v.add(p+".hash", err.Error())
			}
		}
		v.required(p + ".enabled")
		if v.required(p + ".identity") {
			if _, err := domain.ParseUserID(u.Identity); err != nil {
				v.add(p+".identity", fmt.Sprintf("must be a user identity: %s", err))
			}
		}
	}
}

// validateUsername enforces the login-name rules: bounded, valid UTF-8,
// no control characters, no path separators, and no outer whitespace
// (inner spaces stay legal so a display name can be a login name).
func validateUsername(name string) error {
	if name == "" {
		return errors.New("username is empty")
	}
	if len(name) > maxUsernameBytes {
		return fmt.Errorf("username must be at most %d bytes (got %d)", maxUsernameBytes, len(name))
	}
	if !utf8.ValidString(name) {
		return errors.New("username must be valid UTF-8")
	}
	if strings.TrimSpace(name) != name {
		return errors.New("username must not start or end with whitespace")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("username must not contain control characters")
		}
		if r == '/' || r == '\\' {
			return errors.New("username must not contain path separators")
		}
	}
	return nil
}

// PasswordStore maintains one password file. Mutations validate the whole
// next document, write it atomically (temporary file plus rename, mode
// 0600), and only then publish it in memory, so a failed write never
// leaves the store holding a state disk does not have.
type PasswordStore struct {
	path string

	mu   sync.Mutex
	file PasswordFile
}

// OpenPasswordStore reads and validates an existing password file.
func OpenPasswordStore(path string) (*PasswordStore, error) {
	f, err := ReadPasswordFile(path)
	if err != nil {
		return nil, err
	}
	return &PasswordStore{path: path, file: *f}, nil
}

// CreatePasswordStore starts a new password file with no users. It refuses
// to touch an existing file: password administration never overwrites
// state it did not just create.
func CreatePasswordStore(path string) (*PasswordStore, error) {
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("password file %q already exists; refusing to overwrite it", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("cannot inspect password file %q: %w", path, err)
	}
	s := &PasswordStore{path: path, file: PasswordFile{Version: PasswordFileVersion}}
	if err := s.persist(&s.file); err != nil {
		return nil, err
	}
	return s, nil
}

// Path returns the file this store maintains.
func (s *PasswordStore) Path() string { return s.path }

// Pin returns a copy of the current password file, so a reader can hold
// one consistent view across several lookups while administration runs.
func (s *PasswordStore) Pin() PasswordFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyPasswordFile(s.file)
}

// Reload re-reads the file from disk, for the case where an out-of-band
// administrator command edited it. A failed reload keeps the current
// state.
func (s *PasswordStore) Reload() error {
	f, err := ReadPasswordFile(s.path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.file = *f
	return nil
}

// Verify checks a password in constant time relative to the stored hash.
// An unknown or disabled username still performs one full verification
// against a stored hash (or the package dummy) and discards the result, so
// response time does not reveal whether the account exists.
func (s *PasswordStore) Verify(username string, password []byte) (User, bool, error) {
	s.mu.Lock()
	f := copyPasswordFile(s.file)
	s.mu.Unlock()

	target, found := f.Lookup(username)
	usable := found && target.Enabled
	hash := string(target.Hash)
	if !usable {
		var err error
		if hash, err = dummyHashFor(&f); err != nil {
			return User{}, false, err
		}
	}
	match, err := VerifyHash(hash, password)
	if err != nil {
		return User{}, false, fmt.Errorf("verify password: %w", err)
	}
	if !usable || !match {
		return User{}, false, nil
	}
	return target, true, nil
}

// AddUser creates a user with a fresh stable identity and saves the file.
// The hash is computed before the duplicate check so an existing username
// and a new one cost the same to submit.
func (s *PasswordStore) AddUser(random ports.Random, username string, password []byte, params Params) (domain.UserID, error) {
	if err := validateUsername(username); err != nil {
		return domain.UserID{}, err
	}
	identity, err := NewUserID(random)
	if err != nil {
		return domain.UserID{}, err
	}
	hash, err := GenerateHash(password, params)
	if err != nil {
		return domain.UserID{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := copyPasswordFile(s.file)
	if _, exists := next.Lookup(username); exists {
		return domain.UserID{}, fmt.Errorf("user %q already exists", username)
	}
	next.Users = append(next.Users, User{
		Username: username,
		Hash:     PasswordHash(hash),
		Enabled:  true,
		Identity: identity.String(),
	})
	if err := s.commit(&next); err != nil {
		return domain.UserID{}, err
	}
	return identity, nil
}

// SetPassword replaces an existing user's hash. An unknown username still
// pays the full hashing cost before failing, so it cannot be told apart
// from a known one by timing.
func (s *PasswordStore) SetPassword(username string, password []byte, params Params) error {
	if err := validateUsername(username); err != nil {
		return err
	}
	hash, err := GenerateHash(password, params)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := copyPasswordFile(s.file)
	idx := userIndex(next.Users, username)
	if idx < 0 {
		return fmt.Errorf("user %q does not exist", username)
	}
	next.Users[idx].Hash = PasswordHash(hash)
	return s.commit(&next)
}

// SetEnabled enables or disables login for an existing user.
func (s *PasswordStore) SetEnabled(username string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := copyPasswordFile(s.file)
	idx := userIndex(next.Users, username)
	if idx < 0 {
		return fmt.Errorf("user %q does not exist", username)
	}
	next.Users[idx].Enabled = enabled
	return s.commit(&next)
}

// RemoveUser deletes a user. Removing the last user is allowed: an empty
// user list fails closed (nobody can log in) and is repairable only by an
// operator with file access, which is the same access that maintains the
// file in the first place.
func (s *PasswordStore) RemoveUser(username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := copyPasswordFile(s.file)
	idx := userIndex(next.Users, username)
	if idx < 0 {
		return fmt.Errorf("user %q does not exist", username)
	}
	next.Users = append(next.Users[:idx], next.Users[idx+1:]...)
	return s.commit(&next)
}

// commit validates next as a whole, persists it atomically, and only then
// publishes it. Callers hold s.mu.
func (s *PasswordStore) commit(next *PasswordFile) error {
	if problems := validatePasswordFile(next, nil); len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	if err := s.persist(next); err != nil {
		return err
	}
	s.file = *next
	return nil
}

// persist writes the file atomically: a temporary file in the same
// directory, owner-only permissions, flushed, then renamed over the
// destination. Readers see either the old file or the new one.
func (s *PasswordStore) persist(f *PasswordFile) error {
	data, err := encodePasswordFile(f)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".vibeshell-password-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary password file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(passwordFileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set mode on temporary password file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary password file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary password file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary password file: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace password file %q: %w", s.path, err)
	}
	tmpName = ""
	return nil
}

// encodePasswordFile renders the document with usernames in sorted order
// (stable output for review and diffs) and real hash values through the
// private wire types.
func encodePasswordFile(f *PasswordFile) ([]byte, error) {
	users := append([]User(nil), f.Users...)
	sort.Slice(users, func(i, j int) bool { return users[i].Username < users[j].Username })
	wire := passwordFileWire{
		Version: f.Version,
		Users:   make([]passwordUserWire, 0, len(users)),
	}
	for _, u := range users {
		wire.Users = append(wire.Users, passwordUserWire{
			Username: u.Username,
			Hash:     string(u.Hash),
			Enabled:  u.Enabled,
			Identity: u.Identity,
		})
	}
	data, err := json.MarshalIndent(wire, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode password file: %w", err)
	}
	return append(data, '\n'), nil
}

func copyPasswordFile(f PasswordFile) PasswordFile {
	out := f
	out.Users = append([]User(nil), f.Users...)
	return out
}

func userIndex(users []User, username string) int {
	for i, u := range users {
		if u.Username == username {
			return i
		}
	}
	return -1
}

// dummyHashFor returns a hash of the same cost class as the file's stored
// hashes for the discard-verification of unknown or disabled usernames.
// With no usable stored hash it falls back to a package-level dummy built
// with the default parameters.
func dummyHashFor(f *PasswordFile) (string, error) {
	for _, u := range f.Users {
		if _, _, _, err := decodePHC(string(u.Hash)); err == nil {
			return string(u.Hash), nil
		}
	}
	return fallbackDummyHash()
}

var (
	dummyHashOnce sync.Once
	dummyHash     string
	dummyHashErr  error
)

// fallbackDummyHash is generated once per process. It exists only to make
// an empty password file cost the same to query as a populated one.
func fallbackDummyHash() (string, error) {
	dummyHashOnce.Do(func() {
		dummyHash, dummyHashErr = GenerateHash([]byte("vibeshell-dummy-verification-password"), DefaultParams)
	})
	return dummyHash, dummyHashErr
}

// ReadPassword reads one password line from r (a TTY or stdin) with a hard
// bound on its length. Passwords deliberately have no argv API: command
// lines end up in process listings and shell history.
func ReadPassword(r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, errors.New("password reader is nil")
	}
	br := bufio.NewReader(io.LimitReader(r, maxPasswordBytes+2))
	line, err := br.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read password: %w", err)
	}
	if len(line) > maxPasswordBytes+1 {
		return nil, fmt.Errorf("password exceeds the %d byte limit", maxPasswordBytes)
	}
	line = bytes.TrimRight(line, "\r\n")
	if len(line) > maxPasswordBytes {
		return nil, fmt.Errorf("password exceeds the %d byte limit", maxPasswordBytes)
	}
	if len(line) == 0 {
		return nil, errors.New("password is empty")
	}
	return line, nil
}

// GenerateHashFromReader reads a password from r and returns its encoded
// Argon2id hash, which is what an administrator command writes into a
// password file entry.
func GenerateHashFromReader(r io.Reader, p Params) (string, error) {
	password, err := ReadPassword(r)
	if err != nil {
		return "", err
	}
	return GenerateHash(password, p)
}
