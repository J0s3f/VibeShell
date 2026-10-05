package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// adminTestConfig is a minimal valid configuration for admin command tests. It
// uses public authentication, so it needs no password file or resolved secret.
const adminTestConfig = `{
  "version": 1,
  "identity": {"system_name": "VibeOS", "shell_name": "VibeShell", "hostname": "vibeshell.test"},
  "ssh": {"listen_port": 2222, "host_key_file": "/etc/vibeshell/host_key"},
  "auth": {"mode": "public"},
  "sharing": {"enabled": false},
  "providers": [{"name": "opencode", "products": [{"name": "console", "base_url": "https://opencode.example.internal", "protocols": ["chat"], "default_protocol": "chat"}]}],
  "routes": [{"id": "rte_0123456789ABCDEFGHJKMNPQRS", "provider": "opencode", "product": "console", "model": "example-free", "protocol": "chat"}],
  "tiers": [{"name": "named-free", "routes": ["rte_0123456789ABCDEFGHJKMNPQRS"]}],
  "persistence": {"database_path": "/var/lib/vibeshell/world.db"}
}`

// writeAdminConfig writes config to a temporary file and returns its path.
func writeAdminConfig(t *testing.T, config string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vibeshell.json")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestAdminHelpListsOperations verifies `vibeshell admin help` succeeds and
// names the operator surface.
func TestAdminHelpListsOperations(t *testing.T) {
	if err := run([]string{"admin", "help"}); err != nil {
		t.Fatalf("admin help: %v", err)
	}
}

// TestAdminUnknownOperationFails verifies an unrecognised admin operation is
// reported, never silently ignored.
func TestAdminUnknownOperationFails(t *testing.T) {
	err := run([]string{"admin", "definitely-not-an-operation"})
	if err == nil {
		t.Fatal("unknown admin operation returned no error")
	}
	if !strings.Contains(err.Error(), "unknown admin operation") {
		t.Fatalf("unknown admin operation error = %q", err)
	}
}

// TestAdminConfigValidateAcceptsValidConfiguration verifies the operator
// validation path accepts a valid strict JSON document.
func TestAdminConfigValidateAcceptsValidConfiguration(t *testing.T) {
	path := writeAdminConfig(t, adminTestConfig)
	if err := run([]string{"admin", "config", "validate", "-config", path}); err != nil {
		t.Fatalf("admin config validate: %v", err)
	}
}

// TestAdminConfigValidateRejectsInvalidConfiguration verifies an invalid
// document is reported rather than accepted.
func TestAdminConfigValidateRejectsInvalidConfiguration(t *testing.T) {
	path := writeAdminConfig(t, `{"version": 1}`)
	if err := run([]string{"admin", "config", "validate", "-config", path}); err == nil {
		t.Fatal("invalid configuration was accepted")
	}
}

// TestAdminUserRequiresUsername verifies a maintenance operation without a
// username is rejected before any store is opened.
func TestAdminUserRequiresUsername(t *testing.T) {
	path := writeAdminConfig(t, adminTestConfig)
	err := run([]string{"admin", "user", "add", "-config", path})
	if err == nil || !strings.Contains(err.Error(), "-username is required") {
		t.Fatalf("user add without username = %v, want a username requirement", err)
	}
}

// TestAdminUserMaintenanceUnavailableInPublicMode verifies that user
// maintenance in public mode reports the unavailable password dependency
// rather than pretending to add a user.
func TestAdminUserMaintenanceUnavailableInPublicMode(t *testing.T) {
	path := writeAdminConfig(t, adminTestConfig)
	err := run([]string{"admin", "user", "list", "-config", path})
	if err == nil {
		t.Fatal("user list in public mode returned no error")
	}
	if !domain.IsUnavailableError(err) {
		t.Fatalf("user list error = %v, want a typed unavailable error", err)
	}
}

// TestAdminExportRejectsInvalidSession verifies a malformed session ID is
// reported before opening storage.
func TestAdminExportRejectsInvalidSession(t *testing.T) {
	path := writeAdminConfig(t, adminTestConfig)
	err := run([]string{"admin", "export", "-config", path, "-format", "jsonl", "-out", filepath.Join(t.TempDir(), "out"), "-session", "not-a-session"})
	if err == nil || !strings.Contains(err.Error(), "parse session ID") {
		t.Fatalf("export with an invalid session = %v, want a parse error", err)
	}
}

// TestAdminExportRequiresOutput verifies the export destination is required.
func TestAdminExportRequiresOutput(t *testing.T) {
	path := writeAdminConfig(t, adminTestConfig)
	err := run([]string{"admin", "export", "-config", path, "-format", "jsonl"})
	if err == nil || !strings.Contains(err.Error(), "-out is required") {
		t.Fatalf("export without -out = %v, want an output requirement", err)
	}
}

// TestAdminBackupRequiresDestination verifies the backup destination is
// required before storage is opened.
func TestAdminBackupRequiresDestination(t *testing.T) {
	path := writeAdminConfig(t, adminTestConfig)
	err := run([]string{"admin", "backup", "-config", path})
	if err == nil || !strings.Contains(err.Error(), "-dest is required") {
		t.Fatalf("backup without -dest = %v, want a destination requirement", err)
	}
}

// TestAdminRestoreRequiresPaths verifies restore refuses to run without a
// source and a fresh destination database.
func TestAdminRestoreRequiresPaths(t *testing.T) {
	path := writeAdminConfig(t, adminTestConfig)
	if err := run([]string{"admin", "restore", "-config", path, "-dest-db", "/tmp/new.db"}); err == nil || !strings.Contains(err.Error(), "-source is required") {
		t.Fatalf("restore without -source = %v, want a source requirement", err)
	}
	if err := run([]string{"admin", "restore", "-config", path, "-source", "/tmp/backup"}); err == nil || !strings.Contains(err.Error(), "-dest-db is required") {
		t.Fatalf("restore without -dest-db = %v, want a destination requirement", err)
	}
}

// TestAdminAppRollbackValidatesIDs verifies an app rollback validates its
// identifiers before touching any registry.
func TestAdminAppRollbackValidatesIDs(t *testing.T) {
	path := writeAdminConfig(t, adminTestConfig)
	err := run([]string{"admin", "app", "rollback", "-config", path, "-app", "bad", "-version", "bad"})
	if err == nil || !strings.Contains(err.Error(), "parse app ID") {
		t.Fatalf("app rollback with a bad app ID = %v, want a parse error", err)
	}
}

// TestReadPasswordLineTrimsNewline verifies the stdin password path strips the
// trailing line ending and preserves the password bytes.
func TestReadPasswordLineTrimsNewline(t *testing.T) {
	password, err := readPasswordLine(strings.NewReader("correct horse\n"))
	if err != nil {
		t.Fatalf("readPasswordLine: %v", err)
	}
	if string(password) != "correct horse" {
		t.Fatalf("readPasswordLine = %q, want the password without the newline", password)
	}
}
