package config

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// redacted is the single replacement shown wherever a secret would
// otherwise be formatted.
const redacted = "[REDACTED]"

const (
	// MaxSecretRefLen bounds a reference string. It bounds the reference,
	// never the value.
	MaxSecretRefLen = 4096
	// DefaultMaxSecretBytes bounds one resolved secret value so a mistaken
	// mount (a whole file, not a key) cannot balloon process memory.
	DefaultMaxSecretBytes = 64 * 1024
)

// Secret is a resolved credential. Its formatting is always redacted for
// %v, %s, %#v, and JSON, so a secret cannot leak through a log line, an
// error chain, or a marshaled snapshot by accident.
type Secret []byte

// String implements fmt.Stringer with a redacted placeholder.
func (s Secret) String() string { return redacted }

// GoString implements fmt.GoStringer for %#v formatting.
func (s Secret) GoString() string { return redacted }

// MarshalJSON emits a redacted placeholder; secrets never leave memory.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// Bytes returns a copy of the secret for handing to a provider adapter.
func (s Secret) Bytes() []byte { return append([]byte(nil), s...) }

// Len reports the secret's length without revealing its content.
func (s Secret) Len() int { return len(s) }

// SecretRef is a parsed secret reference: an environment variable name or a
// mounted file path. It carries no secret value, only where the value lives.
type SecretRef struct {
	raw  string
	env  string
	file string
}

// String returns the reference as written in the configuration.
func (r SecretRef) String() string { return r.raw }

// IsEnv reports whether the reference points at the container environment.
func (r SecretRef) IsEnv() bool { return r.env != "" }

// IsFile reports whether the reference points at a mounted file.
func (r SecretRef) IsFile() bool { return r.file != "" }

// EnvName returns the environment variable name (empty for file references).
func (r SecretRef) EnvName() string { return r.env }

// FilePath returns the absolute container path (empty for env references).
func (r SecretRef) FilePath() string { return r.file }

// ParseSecretRef validates reference syntax. References are either
// "{file:...}" (an absolute, cleaned POSIX container path) or "{env:...}"
// (a container environment variable name). An inline secret value is
// rejected outright: secrets travel as references, never as content.
func ParseSecretRef(raw string) (SecretRef, error) {
	if raw == "" {
		return SecretRef{}, fmt.Errorf("secret reference is empty")
	}
	if len(raw) > MaxSecretRefLen {
		return SecretRef{}, fmt.Errorf("secret reference is longer than %d bytes", MaxSecretRefLen)
	}
	if strings.ContainsRune(raw, '\n') || strings.ContainsRune(raw, '\r') {
		return SecretRef{}, fmt.Errorf("secret reference contains a newline")
	}
	var kind, target string
	switch {
	case strings.HasPrefix(raw, "{file:") && strings.HasSuffix(raw, "}"):
		kind, target = "file", raw[len("{file:"):len(raw)-1]
	case strings.HasPrefix(raw, "{env:") && strings.HasSuffix(raw, "}"):
		kind, target = "env", raw[len("{env:"):len(raw)-1]
	default:
		return SecretRef{}, fmt.Errorf("secret reference must be {file:...} or {env:...}; inline secret values are not allowed")
	}
	if strings.ContainsRune(target, 0) {
		return SecretRef{}, fmt.Errorf("secret reference contains a NUL byte")
	}
	if strings.ContainsAny(target, "{}") {
		return SecretRef{}, fmt.Errorf("secret reference %q contains braces in its %s target", raw, kind)
	}
	if target == "" {
		return SecretRef{}, fmt.Errorf("secret reference %q has an empty %s target", raw, kind)
	}
	switch kind {
	case "env":
		if !envNamePattern.MatchString(target) {
			return SecretRef{}, fmt.Errorf("environment secret reference %q must name a valid environment variable", raw)
		}
		return SecretRef{raw: raw, env: target}, nil
	default:
		if !path.IsAbs(target) {
			return SecretRef{}, fmt.Errorf("file secret reference target must be an absolute container path (got %q)", target)
		}
		if cleaned := path.Clean(target); cleaned != target {
			return SecretRef{}, fmt.Errorf("file secret reference target must be cleaned without %q or %q segments (want %q)", ".", "..", cleaned)
		}
		return SecretRef{raw: raw, file: target}, nil
	}
}

// fileRefWithinDirs reports whether an absolute, cleaned file path lies
// strictly inside one of the allowed directories.
func fileRefWithinDirs(file string, dirs []string) bool {
	for _, dir := range dirs {
		if !path.IsAbs(dir) {
			continue
		}
		if strings.HasPrefix(file, strings.TrimSuffix(dir, "/")+"/") {
			return true
		}
	}
	return false
}

// SecretResolver reads secret values from mounted files or the container
// environment. AllowedDirs is fail-closed: with no allowed directory every
// file reference is refused, and a symlink that escapes an allowed
// directory is refused too.
type SecretResolver struct {
	// AllowedDirs are cleaned absolute container directories.
	AllowedDirs []string
	// LookupEnv resolves environment references; nil means os.LookupEnv.
	LookupEnv func(string) (string, bool)
	// MaxSecretBytes bounds one value; zero means DefaultMaxSecretBytes.
	MaxSecretBytes int
}

// CheckRef validates syntax and allowed-directory containment without
// touching the filesystem or the environment.
func (r *SecretResolver) CheckRef(raw string) error {
	ref, err := ParseSecretRef(raw)
	if err != nil {
		return err
	}
	if ref.IsFile() && !fileRefWithinDirs(ref.FilePath(), r.AllowedDirs) {
		return fmt.Errorf("file secret reference %q is outside the allowed secret directories", ref.FilePath())
	}
	return nil
}

// Resolve reads the referenced secret. Error messages name the reference
// and the reason, never the value.
func (r *SecretResolver) Resolve(raw string) (Secret, error) {
	if err := r.CheckRef(raw); err != nil {
		return nil, err
	}
	ref, _ := ParseSecretRef(raw)
	limit := r.MaxSecretBytes
	if limit <= 0 {
		limit = DefaultMaxSecretBytes
	}

	if ref.IsEnv() {
		lookup := r.LookupEnv
		if lookup == nil {
			lookup = os.LookupEnv
		}
		value, ok := lookup(ref.EnvName())
		if !ok {
			return nil, fmt.Errorf("environment variable %s referenced by %q is not set", ref.EnvName(), ref.raw)
		}
		return normalizeSecret([]byte(value), limit, ref.raw)
	}

	// Resolve symlinks first: a symlink inside the allowed directory must
	// not become a way to read from outside it.
	resolved, err := filepath.EvalSymlinks(ref.FilePath())
	if err != nil {
		return nil, fmt.Errorf("cannot resolve file secret reference %q: %v", ref.raw, err)
	}
	cleanResolved := path.Clean(filepath.ToSlash(resolved))
	if !fileRefWithinDirs(cleanResolved, r.AllowedDirs) {
		return nil, fmt.Errorf("file secret reference %q resolves outside the allowed secret directories", ref.raw)
	}
	info, err := os.Stat(cleanResolved)
	if err != nil {
		return nil, fmt.Errorf("cannot stat file secret reference %q: %v", ref.raw, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("file secret reference %q is not a regular file", ref.raw)
	}
	if info.Size() > int64(limit) {
		return nil, fmt.Errorf("file secret reference %q is larger than the %d byte secret limit", ref.raw, limit)
	}
	value, err := os.ReadFile(cleanResolved)
	if err != nil {
		return nil, fmt.Errorf("cannot read file secret reference %q: %v", ref.raw, err)
	}
	return normalizeSecret(value, limit, ref.raw)
}

// normalizeSecret trims the trailing newline that mounted secret files
// almost always carry, enforces the value bounds, and rejects empty values
// so a mis-mounted secret fails closed.
func normalizeSecret(value []byte, limit int, ref string) (Secret, error) {
	value = bytes.TrimSuffix(value, []byte("\n"))
	value = bytes.TrimSuffix(value, []byte("\r"))
	if len(value) == 0 {
		return nil, fmt.Errorf("secret reference %q resolved to an empty value", ref)
	}
	if len(value) > limit {
		return nil, fmt.Errorf("secret reference %q resolved to more than the %d byte secret limit", ref, limit)
	}
	return Secret(value), nil
}
