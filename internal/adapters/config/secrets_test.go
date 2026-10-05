package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretFormattingIsRedacted(t *testing.T) {
	s := Secret([]byte("FAKE-PRIMARY-KEY-0001"))
	for name, rendered := range map[string]string{
		"String()":   s.String(),
		"GoString()": s.GoString(),
		"%v":         fmt.Sprintf("%v", s),
		"%s":         fmt.Sprintf("%s", s),
		"%#v":        fmt.Sprintf("%#v", s),
	} {
		if strings.Contains(rendered, "FAKE-PRIMARY-KEY-0001") || rendered != "[REDACTED]" {
			t.Fatalf("%s leaks or mistexts: %q", name, rendered)
		}
	}
	jsonForm, err := s.MarshalJSON()
	if err != nil || strings.Contains(string(jsonForm), "FAKE") {
		t.Fatalf("MarshalJSON leak: %q (%v)", jsonForm, err)
	}
	if got := s.Bytes(); string(got) != "FAKE-PRIMARY-KEY-0001" {
		t.Fatalf("Bytes changed the value: %q", got)
	}
}

func TestParseSecretRefGrammar(t *testing.T) {
	if ref, err := ParseSecretRef("{file:/etc/vibeshell/secrets/k}"); err != nil || !ref.IsFile() || ref.FilePath() != "/etc/vibeshell/secrets/k" {
		t.Fatalf("file ref: %v %v", ref, err)
	}
	if ref, err := ParseSecretRef("{env:VIBESHELL_API_KEY}"); err != nil || !ref.IsEnv() || ref.EnvName() != "VIBESHELL_API_KEY" {
		t.Fatalf("env ref: %v %v", ref, err)
	}
	for _, bad := range []string{
		"",
		"FAKE-PRIMARY-KEY-0001", // inline values are never allowed
		"env:VIBESHELL_API_KEY", // missing braces
		"{env:}",
		"{file:}",
		"{file:relative/key}",
		"{file:/etc/../etc/key}",
		"{env:1bad}",
		"{url:/etc/key}",
	} {
		if _, err := ParseSecretRef(bad); err == nil {
			t.Fatalf("ParseSecretRef(%q) accepted", bad)
		}
	}
}

func TestSecretResolveNoLeak(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "k")
	if err := os.WriteFile(secretPath, []byte("FAKE-PRIMARY-KEY-0001\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &SecretResolver{AllowedDirs: []string{dir}, LookupEnv: func(string) (string, bool) { return "", false }}
	value, err := r.Resolve("{file:" + filepath.ToSlash(secretPath) + "}")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if string(value.Bytes()) != "FAKE-PRIMARY-KEY-0001" {
		t.Fatalf("value = %q", value.Bytes())
	}
	if strings.Contains(fmt.Sprintf("%v", value), "FAKE") {
		t.Fatal("resolved secret leaks through formatting")
	}
	// Errors for other references must never carry the resolved value.
	if _, err := r.Resolve("{env:MISSING}"); err == nil || strings.Contains(err.Error(), "FAKE") {
		t.Fatalf("env error leaks or missing: %v", err)
	}
	if _, err := r.Resolve("{file:/does/not/exist}"); err == nil || strings.Contains(err.Error(), "FAKE") {
		t.Fatalf("file error leaks or missing: %v", err)
	}
}

func TestSecretResolveMissingAndUnreadable(t *testing.T) {
	dir := t.TempDir()
	r := &SecretResolver{AllowedDirs: []string{dir}, LookupEnv: os.LookupEnv}

	if _, err := r.Resolve("{file:" + filepath.ToSlash(filepath.Join(dir, "missing")) + "}"); err == nil {
		t.Fatal("missing file accepted")
	}
	if _, err := r.Resolve("{file:" + filepath.ToSlash(dir) + "}"); err == nil {
		t.Fatal("directory ref accepted")
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve("{file:" + filepath.ToSlash(empty) + "}"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty secret: %v", err)
	}
	if _, err := r.Resolve("{file:/etc/hostname}"); err == nil || !strings.Contains(err.Error(), "outside the allowed") {
		t.Fatalf("outside-allowed ref accepted: %v", err)
	}
}

func TestSecretResolveSymlinkEscape(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "k")
	if err := os.WriteFile(outsideFile, []byte("FAKE-PRIMARY-KEY-0001"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(allowed, "link")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	r := &SecretResolver{AllowedDirs: []string{allowed}}
	if _, err := r.Resolve("{file:" + filepath.ToSlash(link) + "}"); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("symlink escape accepted: %v", err)
	}
}
