package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Secret holds a resolved secret value in memory. The value
// never appears in errors, logs, or formatted output: Secret
// redacts itself in every formatting path. Callers that need
// the raw value (to authenticate a provider request) read it
// through Value and must not log it.
type Secret struct {
	value string
}

// Value returns the raw secret for the caller that needs it.
// The caller must not log or persist it.
func (s Secret) Value() string { return s.value }

// String redacts the secret in every formatting path.
func (s Secret) String() string { return "[redacted]" }

// Empty reports whether the secret holds no value.
func (s Secret) Empty() bool { return s.value == "" }

// SecretRef is a parsed secret reference: either a file inside
// the allowed secrets directory or an environment variable
// name.
type SecretRef struct {
	Kind string // "file" or "env"
	Name string // file path or environment variable name
}

// String renders the reference (never a value), for example
// {file:accounts/primary.key}.
func (r SecretRef) String() string { return "{" + r.Kind + ":" + r.Name + "}" }

// ParseSecretRef parses {file:...} and {env:...} references.
// Any other form is rejected so that inline secret values
// cannot enter a configuration document.
func ParseSecretRef(ref string) (SecretRef, error) {
	if strings.ContainsAny(ref, "\r\n") {
		return SecretRef{}, fmt.Errorf("secret reference contains a newline")
	}
	if len(ref) < 2 || ref[0] != '{' || ref[len(ref)-1] != '}' {
		return SecretRef{}, fmt.Errorf("secret reference must be {file:...} or {env:...}; inline secret values are not allowed")
	}
	kind, name, ok := strings.Cut(ref[1:len(ref)-1], ":")
	if !ok || (kind != "file" && kind != "env") {
		return SecretRef{}, fmt.Errorf("secret reference must be {file:...} or {env:...}, got %q", ref)
	}
	if strings.TrimSpace(name) == "" {
		return SecretRef{}, fmt.Errorf("secret reference %q has an empty %s target", ref, kind)
	}
	if name == "." || name == ".." {
		return SecretRef{}, fmt.Errorf("secret reference %q has a %s target outside the allowed directory", ref, kind)
	}
	if strings.ContainsAny(name, "{}") {
		return SecretRef{}, fmt.Errorf("secret reference %q contains braces in its %s target", ref, kind)
	}
	return SecretRef{Kind: kind, Name: name}, nil
}

// SecretResolver resolves one secret reference to its in-memory
// value. It is an outbound port so tests can inject failures
// without touching files or the environment.
type SecretResolver interface {
	Resolve(ref string) (Secret, error)
}

// FileEnvResolver resolves {file:...} references inside one
// allowed directory and {env:...} references from the process
// environment. It is the production boundary implementation.
type FileEnvResolver struct {
	// AllowedDir is the only directory {file:...} references
	// may read from. Symlinked targets must resolve inside it
	// too.
	AllowedDir string
}

// Resolve implements SecretResolver. Errors name the reference
// and the reason only; a resolved value is never part of an
// error.
func (r FileEnvResolver) Resolve(ref string) (Secret, error) {
	parsed, err := ParseSecretRef(ref)
	if err != nil {
		return Secret{}, err
	}
	if parsed.Kind == "file" {
		return r.resolveFile(parsed)
	}
	return r.resolveEnv(parsed)
}

func (r FileEnvResolver) resolveFile(ref SecretRef) (Secret, error) {
	allowed := r.allowedDir()
	path := ref.Name
	if !filepath.IsAbs(path) {
		path = filepath.Join(allowed, path)
	}
	path = filepath.Clean(path)
	if !withinDir(allowed, path) {
		return Secret{}, fmt.Errorf("secret file %q is outside the allowed directory %s", ref.Name, allowed)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Secret{}, fmt.Errorf("secret file %q is unreadable: %w", ref.Name, err)
	}
	if !withinDir(allowed, real) {
		return Secret{}, fmt.Errorf("secret file %q escapes the allowed directory through a symlink", ref.Name)
	}
	data, err := os.ReadFile(real)
	if err != nil {
		return Secret{}, fmt.Errorf("secret file %q is unreadable: %w", ref.Name, err)
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return Secret{}, fmt.Errorf("secret file %q is empty", ref.Name)
	}
	return Secret{value: value}, nil
}

func (r FileEnvResolver) resolveEnv(ref SecretRef) (Secret, error) {
	value, ok := os.LookupEnv(ref.Name)
	if !ok {
		return Secret{}, fmt.Errorf("environment variable %q is not set", ref.Name)
	}
	if strings.TrimSpace(value) == "" {
		return Secret{}, fmt.Errorf("environment variable %q is empty", ref.Name)
	}
	return Secret{value: value}, nil
}

// allowedDir resolves the allowed directory itself, so a
// symlinked secrets directory does not reject its own files.
func (r FileEnvResolver) allowedDir() string {
	clean := filepath.Clean(r.AllowedDir)
	if real, err := filepath.EvalSymlinks(clean); err == nil {
		return real
	}
	return clean
}

// withinDir reports whether path is inside dir (or is dir
// itself). Both must be cleaned absolute paths.
func withinDir(dir, path string) bool {
	if path == dir {
		return true
	}
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}
