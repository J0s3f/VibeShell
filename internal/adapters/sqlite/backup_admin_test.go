package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"j0s.at/vibeshell/internal/admin"
	"j0s.at/vibeshell/internal/domain"
)

// TestAdminOpSurfaceOverSQLite drives the admin service through the SQLite
// adapter: integrity, port-form backup, restore, session listing, and
// recovery all against a real database, plus the typed error for an
// unconfigured dependency.
func TestAdminOpSurfaceOverSQLite(t *testing.T) {
	ctx := context.Background()
	db, events := testEvents(t)
	_, session := seedBackupWorld(t, ctx, db, events)
	maintenance := NewMaintenance(db)
	recovery := NewRecovery(db, events, fixedTestClock{now: 1_700_000_300_000})
	service := admin.New(admin.Deps{Backups: maintenance, Recovery: recovery, Sessions: recovery})

	integrity, err := service.IntegrityCheck(ctx)
	if err != nil {
		t.Fatalf("IntegrityCheck: %v", err)
	}
	if integrity.IntegrityCheck != "ok" {
		t.Errorf("live integrity_check = %q, want ok", integrity.IntegrityCheck)
	}
	if integrity.LogicalFingerprint == "" {
		t.Error("live fingerprint is empty")
	}

	dest := filepath.Join(t.TempDir(), "backup")
	backup, err := service.Backup(ctx, dest)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if backup.Format != "sqlite-backup" {
		t.Errorf("backup format = %q, want sqlite-backup", backup.Format)
	}
	if backup.Checksum == "" || backup.EventCount < 1 || backup.SizeBytes <= 0 {
		t.Errorf("backup result = %+v, want a checksum, events, and bytes", backup)
	}
	if _, err := maintenance.VerifyBackup(ctx, dest); err != nil {
		t.Fatalf("VerifyBackup on the admin-created backup: %v", err)
	}

	restored, err := service.RestoreBackup(ctx, admin.RestoreRequest{
		SourceDir:    dest,
		DestDatabase: filepath.Join(t.TempDir(), "restore", "vibeshell.db"),
	})
	if err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	if restored.LogicalFingerprint != backup.Checksum {
		t.Errorf("restored fingerprint = %s, want the backup checksum %s", restored.LogicalFingerprint, backup.Checksum)
	}

	page, _, err := service.ListSessions(ctx, 10, "")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(page) != 1 || page[0].SessionID != session {
		t.Fatalf("ListSessions = %+v, want the seeded session", page)
	}
	if page[0].Ended || page[0].Recovered {
		t.Errorf("seeded session flags = %+v, want both false before recovery", page[0])
	}

	report, err := service.RecoverIncomplete(ctx)
	if err != nil {
		t.Fatalf("RecoverIncomplete: %v", err)
	}
	if report.MarkedSessions != 1 {
		t.Errorf("recovered sessions = %d, want 1", report.MarkedSessions)
	}

	// An operation whose dependency is not configured reports a typed
	// unavailable error rather than a nil dereference.
	if err := admin.New(admin.Deps{}).RollbackApp(ctx, domain.AppID{}, domain.AppVersionID{}, "x"); !domain.IsUnavailableError(err) {
		t.Errorf("RollbackApp without dependencies = %v, want an unavailable error", err)
	}
	if _, err := admin.New(admin.Deps{}).RouteStatus(ctx); !domain.IsUnavailableError(err) {
		t.Errorf("RouteStatus without dependencies = %v, want an unavailable error", err)
	}
}
