package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	cryptossh "golang.org/x/crypto/ssh"
)

// GenerateHostKey returns an ephemeral ed25519 host key signer. It exists for
// tests; production servers load a persistent key with LoadOrGenerateHostKey
// so a restart does not change the server identity (PLAN 4.3).
func GenerateHostKey() (cryptossh.Signer, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ssh: generate ed25519 key: %w", err)
	}
	signer, err := cryptossh.NewSignerFromKey(private)
	if err != nil {
		return nil, fmt.Errorf("ssh: build signer: %w", err)
	}
	return signer, nil
}

// LoadOrGenerateHostKey loads the PEM-encoded ed25519 host key at path,
// creating parent directories and the key file (mode 0600) when the file does
// not exist. An empty path returns an ephemeral key for tests.
//
// The file holds the standard OpenSSH private-key PEM block produced by
// cryptossh.MarshalPrivateKey, so operators can inspect it with
// `ssh-keygen -l -f`. Any other read error, or a file that does not decode as
// a private key, is reported and never silently replaced: replacing a host key
// behind the operator's back would break client trust.
func LoadOrGenerateHostKey(path string) (cryptossh.Signer, error) {
	if path == "" {
		return GenerateHostKey()
	}
	pemBytes, err := os.ReadFile(path)
	switch {
	case err == nil:
		signer, err := cryptossh.ParsePrivateKey(pemBytes)
		if err != nil {
			return nil, fmt.Errorf("ssh: parse host key %s: %w", path, err)
		}
		return signer, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("ssh: read host key %s: %w", path, err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("ssh: create host key directory %s: %w", dir, err)
		}
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ssh: generate ed25519 key: %w", err)
	}
	block, err := cryptossh.MarshalPrivateKey(private, "vibeshell host key")
	if err != nil {
		return nil, fmt.Errorf("ssh: marshal host key: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("ssh: create host key %s: %w", path, err)
	}
	if _, err := f.Write(pem.EncodeToMemory(block)); err != nil {
		f.Close()
		return nil, fmt.Errorf("ssh: write host key %s: %w", path, err)
	}
	f.Close()
	// Verify the mode took effect.
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("ssh: stat host key %s: %w", path, err)
	}
	if info.Mode().Perm() != 0o600 {
		if chmodErr := syscall.Chmod(path, 0o600); chmodErr != nil {
			return nil, fmt.Errorf("ssh: chmod host key %s: %w", path, chmodErr)
		}
		// Retry stat one more time after chmod.
		info, err = os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("ssh: stat host key %s after chmod: %w", path, err)
		}
	}
	if info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("ssh: host key mode = %o, want 600", info.Mode().Perm())
	}
	signer, err := cryptossh.NewSignerFromKey(private)
	if err != nil {
		return nil, fmt.Errorf("ssh: build signer: %w", err)
	}
	return signer, nil
}
