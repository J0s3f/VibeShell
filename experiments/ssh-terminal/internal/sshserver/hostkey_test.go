package sshserver_test

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"

	"j0s.at/vibeshell/experiments/ssh-terminal/internal/sshserver"
)

func TestLoadOrCreateHostKeyIsStableAcrossCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "spike_host_key")

	first, err := sshserver.LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("first LoadOrCreateHostKey: %v", err)
	}
	second, err := sshserver.LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("second LoadOrCreateHostKey: %v", err)
	}
	if got, want := ssh.FingerprintSHA256(second.PublicKey()), ssh.FingerprintSHA256(first.PublicKey()); got != want {
		t.Fatalf("host key fingerprint changed between calls: %s then %s", want, got)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat host key: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("host key mode = %o, want 0600", mode)
	}
}

func TestLoadOrCreateHostKeyRefusesAnUnreadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not_a_key")
	if err := os.WriteFile(path, []byte("this is not a key"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := sshserver.LoadOrCreateHostKey(path); err == nil {
		t.Fatal("a file that is not a key was accepted")
	}
}

func TestEmptyHostKeyPathGeneratesAnEphemeralKey(t *testing.T) {
	first, err := sshserver.LoadOrCreateHostKey("")
	if err != nil {
		t.Fatalf("LoadOrCreateHostKey(\"\"): %v", err)
	}
	second, err := sshserver.LoadOrCreateHostKey("")
	if err != nil {
		t.Fatalf("LoadOrCreateHostKey(\"\"): %v", err)
	}
	if ssh.FingerprintSHA256(first.PublicKey()) == ssh.FingerprintSHA256(second.PublicKey()) {
		t.Fatal("two ephemeral host keys were identical, which is not plausible")
	}
}
