package sshserver

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// hostKeyComment identifies a host key file written by this package. The PEM
// block type itself is the OpenSSH one, which is what ssh.ParsePrivateKey
// expects.
const hostKeyComment = "vibeshell-spike-host-key"

// GenerateHostKey returns an ephemeral ed25519 host key signer. The service
// uses LoadOrCreateHostKey instead, because PLAN.md section 4.3 requires a
// persistent host key; this exists so tests do not write files.
func GenerateHostKey() (ssh.Signer, error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ed25519 key: %w", err)
	}
	return ssh.NewSignerFromKey(private)
}

// LoadOrCreateHostKey loads the host key at path, creating it with mode 0600
// when it does not exist. A service restart therefore keeps the same host
// identity, so a returning client does not see a changed host key.
func LoadOrCreateHostKey(path string) (ssh.Signer, error) {
	if path == "" {
		return GenerateHostKey()
	}
	pemBytes, err := os.ReadFile(path)
	switch {
	case err == nil:
		signer, err := ssh.ParsePrivateKey(pemBytes)
		if err != nil {
			return nil, fmt.Errorf("parse host key %s: %w", path, err)
		}
		return signer, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("read host key %s: %w", path, err)
	}
	return createHostKey(path)
}

func createHostKey(path string) (ssh.Signer, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create host key directory %s: %w", dir, err)
		}
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ed25519 key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(private, hostKeyComment)
	if err != nil {
		return nil, fmt.Errorf("marshal host key: %w", err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, fmt.Errorf("write host key %s: %w", path, err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		return nil, fmt.Errorf("build host key signer: %w", err)
	}
	return signer, nil
}
