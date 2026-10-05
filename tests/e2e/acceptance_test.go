// Package e2e holds the SSH/world/app acceptance suite for the VibeShell
// service (PLAN 15.5 D04).
//
// The suite itself lives in ssh-acceptance.sh so an operator can run it
// directly and read its PASS/FAIL output. This test runs that script, which
// means `go test ./...` and the script execute the same checks against the same
// service build.
//
// It requires the development container: a Go toolchain and a real OpenSSH
// client. The script binds 127.0.0.1 inside the container's own network
// namespace, so no host port is published. The test fails rather than skips
// when the client is missing, because a silently skipped acceptance suite is
// indistinguishable from a passing one.
package e2e

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// suiteScript is the acceptance driver, relative to the repository root.
const suiteScript = "tests/e2e/ssh-acceptance.sh"

// TestSSHAcceptanceSuite builds the service, starts it on a container-local
// loopback port, and drives it with the real OpenSSH client.
func TestSSHAcceptanceSuite(t *testing.T) {
	root := repositoryRoot(t)
	script := filepath.Join(root, suiteScript)
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("acceptance script %s is missing: %v", suiteScript, err)
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Fatalf("the acceptance suite needs a real OpenSSH client on PATH: %v", err)
	}

	// Ports derive from this process so two concurrent runs in one container
	// do not collide on the loopback listener.
	port := 20000 + (os.Getpid() % 10000)

	work, err := os.MkdirTemp("", "vibeshell-e2e-test-")
	if err != nil {
		t.Fatalf("create acceptance work directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(work) })

	command := exec.Command("sh", suiteScript, "-work", work, "-keep")
	command.Dir = root
	command.Env = append(os.Environ(),
		"E2E_WORKDIR="+work,
		fmt.Sprintf("E2E_PORT=%d", port),
		fmt.Sprintf("E2E_SECURE_PORT=%d", port+1),
	)
	output, runErr := command.CombinedOutput()
	if testing.Verbose() || runErr != nil {
		t.Logf("acceptance suite output:\n%s", output)
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			t.Fatalf("SSH acceptance suite failed (exit %d)", exitErr.ExitCode())
		}
		t.Fatalf("run SSH acceptance suite: %v", runErr)
	}
	if !strings.Contains(string(output), "=== ACCEPTANCE: PASS ===") {
		t.Fatalf("SSH acceptance suite exited zero without reporting PASS:\n%s", output)
	}
	if strings.Contains(string(output), "FAIL ") {
		t.Fatalf("SSH acceptance suite reported a failed check:\n%s", output)
	}
}

// repositoryRoot walks up from the test's working directory to the module root,
// so the suite runs against the checkout the test was compiled from.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod above the test working directory")
		}
		dir = parent
	}
}
