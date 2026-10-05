package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseSecretRef(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want SecretRef
	}{
		{"file reference", "{file:accounts/primary.key}", SecretRef{Kind: "file", Name: "accounts/primary.key"}},
		{"env reference", "{env:OPENCODE_API_KEY}", SecretRef{Kind: "env", Name: "OPENCODE_API_KEY"}},
		{"absolute file reference", "{file:/run/secrets/primary.key}", SecretRef{Kind: "file", Name: "/run/secrets/primary.key"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseSecretRef(test.ref)
			if err != nil {
				t.Fatalf("ParseSecretRef(%q): %v", test.ref, err)
			}
			if got != test.want {
				t.Fatalf("ParseSecretRef(%q) = %+v, want %+v", test.ref, got, test.want)
			}
			if got.String() != test.ref {
				t.Fatalf("SecretRef.String() = %q, want %q", got.String(), test.ref)
			}
		})
	}

	invalid := []string{
		"sk-12345",
		"{file}",
		"{env}",
		"{token:abc}",
		"{file:}",
		"{env:}",
		"{file:{nested}}",
		"{file:a\nb}",
		"",
		"{file:..}",
	}
	for _, ref := range invalid {
		if _, err := ParseSecretRef(ref); err == nil {
			t.Errorf("ParseSecretRef(%q) accepted an invalid reference", ref)
		}
	}
}

// TestSecretRedaction proves the resolved value never
// appears in any formatting path.
func TestSecretRedaction(t *testing.T) {
	const marker = "FAKE-SECRET-MARKER-7f3a9c"
	secret := Secret{value: marker}
	if secret.String() != "[redacted]" {
		t.Fatalf("Secret.String() = %q, want [redacted]", secret.String())
	}
	if got := fmt.Sprintf("%v", secret); got != "[redacted]" {
		t.Fatalf("fmt = %q, want [redacted]", got)
	}
	if got := fmt.Sprintf("%s", secret); got != "[redacted]" {
		t.Fatalf("fmt string = %q, want [redacted]", got)
	}
	if secret.Value() != marker {
		t.Fatal("Value() must return the raw value to the caller that needs it")
	}
	var empty Secret
	if !empty.Empty() || secret.Empty() {
		t.Fatal("Empty() misreports")
	}
}

// TestResolvedSecretNeverAppearsInErrors runs every
// resolver failure path (plus one validation failure
// after a successful resolution) and asserts neither
// secret value ever appears in an error message.
func TestResolvedSecretNeverAppearsInErrors(t *testing.T) {
	const fileMarker = "FAKE-FILE-SECRET-0001"
	const envMarker = "FAKE-ENV-SECRET-0002"

	dir := t.TempDir()
	secretsDir := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, "primary.key"), []byte(fileMarker), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, "empty.key"), []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(secretsDir, "adir"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A file outside the allowed directory, used by the
	// traversal and symlink-escape cases.
	if err := os.WriteFile(filepath.Join(dir, "outside.key"), []byte(fileMarker), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VIBESHELL_FAKE_SECRET", envMarker)
	t.Setenv("VIBESHELL_FAKE_EMPTY", "   ")

	resolver := FileEnvResolver{AllowedDir: secretsDir}

	// Success paths resolve and redact.
	secret, err := resolver.Resolve("{file:primary.key}")
	if err != nil {
		t.Fatalf("file resolution failed: %v", err)
	}
	if secret.Value() != fileMarker {
		t.Fatal("file resolution returned the wrong value")
	}
	envSecret, err := resolver.Resolve("{env:VIBESHELL_FAKE_SECRET}")
	if err != nil {
		t.Fatalf("env resolution failed: %v", err)
	}
	if envSecret.Value() != envMarker {
		t.Fatal("env resolution returned the wrong value")
	}

	failures := []struct {
		name string
		ref  string
	}{
		{"missing file", "{file:missing.key}"},
		{"empty file", "{file:empty.key}"},
		{"directory as secret file", "{file:adir}"},
		{"parent traversal", "{file:../outside.key}"},
		{"absolute path outside the allowed directory", "{file:/etc/hostname}"},
		{"missing environment variable", "{env:VIBESHELL_FAKE_MISSING}"},
		{"empty environment variable", "{env:VIBESHELL_FAKE_EMPTY}"},
		{"inline secret value", fileMarker},
		{"malformed reference", "{file}"},
	}
	for _, failure := range failures {
		_, err := resolver.Resolve(failure.ref)
		if err == nil {
			t.Errorf("%s: expected failure", failure.name)
			continue
		}
		assertNoSecret(t, failure.name, err, fileMarker, envMarker)
	}

	// A symlink inside the allowed directory that
	// points outside it must be rejected.
	link := filepath.Join(secretsDir, "link.key")
	if err := os.Symlink(filepath.Join(dir, "outside.key"), link); err != nil {
		t.Skipf("symlinks unavailable on this filesystem: %v", err)
	}
	if _, err := resolver.Resolve("{file:link.key}"); err == nil {
		t.Error("symlink escape resolved")
	} else {
		assertNoSecret(t, "symlink escape", err, fileMarker, envMarker)
	}

	// A successful resolution followed by an unrelated
	// validation failure must not leak either value.
	cfg := validConfig()
	cfg.Accounts[0].SecretRef = "{file:primary.key}"
	cfg.Accounts = append(slices.Clone(cfg.Accounts), cfg.Accounts[0])
	err = Validate(cfg, ExampleCatalogue())
	if err == nil {
		t.Fatal("expected a duplicate account ID error")
	}
	assertNoSecret(t, "validation failure", err, fileMarker, envMarker)
}

func assertNoSecret(t *testing.T, name string, err error, markers ...string) {
	t.Helper()
	for _, marker := range markers {
		if strings.Contains(err.Error(), marker) {
			t.Errorf("%s: error leaks a secret value: %v", name, err)
		}
	}
}
