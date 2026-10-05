package ssh_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	cryptossh "golang.org/x/crypto/ssh"

	adapter "j0s.at/vibeshell/internal/adapters/ssh"
)

// TestNoProcessSpawningPrimitives enforces the PLAN 4.3 gate mechanically:
// the adapter must never start a process, a pseudo-terminal, or a shell, so
// no Go file under this directory may import the process-spawning package.
// The real-client path lives in testdata/ssh_e2e.sh, outside the Go sources.
//
// The forbidden import is spelled indirectly below so this check does not
// match its own needle.
func TestNoProcessSpawningPrimitives(t *testing.T) {
	needle := "os" + "/exec"
	var files []string
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk package dir: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found")
	}
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if strings.Contains(string(content), needle) {
			t.Errorf("%s references %s: the adapter must never spawn processes", file, needle)
		}
	}
}

func TestHostKeyPersistsAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "ssh_host_ed25519_key")
	first, err := adapter.LoadOrGenerateHostKey(path)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat host key: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("host key mode = %o, want 600", info.Mode().Perm())
	}
	second, err := adapter.LoadOrGenerateHostKey(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	firstFP := cryptossh.FingerprintSHA256(first.PublicKey())
	if got := cryptossh.FingerprintSHA256(second.PublicKey()); got != firstFP {
		t.Errorf("reloaded fingerprint = %s, want %s", got, firstFP)
	}
	// A server started with the reloaded key keeps its identity.
	h := start(t, adapter.Options{Mode: adapter.ModePublic, HostKey: second})
	_ = h
}

func TestHostKeyRefusesCorruptFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad_key")
	if err := os.WriteFile(path, []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write corrupt key: %v", err)
	}
	if _, err := adapter.LoadOrGenerateHostKey(path); err == nil {
		t.Error("LoadOrGenerateHostKey(corrupt) = nil, want an error")
	}
}

func TestEphemeralHostKeyForEmptyPath(t *testing.T) {
	first, err := adapter.LoadOrGenerateHostKey("")
	if err != nil {
		t.Fatalf("ephemeral: %v", err)
	}
	second, err := adapter.LoadOrGenerateHostKey("")
	if err != nil {
		t.Fatalf("ephemeral: %v", err)
	}
	if cryptossh.FingerprintSHA256(first.PublicKey()) == cryptossh.FingerprintSHA256(second.PublicKey()) {
		t.Error("two ephemeral keys share a fingerprint, want fresh randomness")
	}
}
