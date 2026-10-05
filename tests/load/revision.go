package load

import (
	"fmt"
	"os/exec"
	"strings"
)

// runGit runs one read-only git command in dir and returns its trimmed output.
// The harness uses it only to stamp receipts with the revision that produced
// them; it never writes to the checkout.
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}
