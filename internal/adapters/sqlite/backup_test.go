package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"j0s.at/vibeshell/internal/admin"
	"j0s.at/vibeshell/internal/domain"
)

// appendInlineEvent appends one inline event through the real writer and
// returns the error, for use from goroutines where t.Fatalf is not allowed.
func appendInlineEvent(ctx context.Context, events *Events, session domain.SessionID, kind domain.EventKind, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	envelope := makeEnvelope(session, kind, raw, 1_700_000_000_000)
	_, err = events.Append(ctx, envelope)
	return err
}

// seedBackupWorld creates exact content, a directory tree, and events worth
// restoring.
func seedBackupWorld(t *testing.T, ctx context.Context, db *DB, events *Events) (domain.Namespace, domain.SessionID) {
	t.Helper()
	user := testUserID(t)
	_, _, personal := mustNamespaces(t, ctx, db, user)
	ref := putFile(t, ctx, db, []byte("exact bytes \x00\xff backup"))
	commitCreates(t, ctx, db, personal, []string{"/work"}, "/work/notes.txt", ref)
	session := testSessionID(t)
	appendTestEvent(t, events, session, domain.EventKindSessionStart, map[string]string{"kind": "start"}, 1_700_000_000_000)
	appendTestEvent(t, events, session, domain.EventKindInputAccepted, map[string]string{"command": "ls /work"}, 1_700_000_000_001)
	return personal, session
}

func testFileSHA256(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// mutateBackupDB runs one statement against a backup database and checkpoints
// so the change lands in the main file a read-only verification opens.
func mutateBackupDB(t *testing.T, path, statement string) {
	t.Helper()
	db, err := openSQLite(path)
	if err != nil {
		t.Fatalf("open backup for mutation: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(statement); err != nil {
		t.Fatalf("mutate backup (%s): %v", statement, err)
	}
	// Best effort: a rollback-journal database has no WAL to checkpoint.
	_, _ = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
}

// TestBackupDuringActivityRestoresIdenticalContent captures a snapshot while
// events are being appended, then restores it and requires the restored
// content hashes and row counts to match the snapshot exactly.
func TestBackupDuringActivityRestoresIdenticalContent(t *testing.T) {
	ctx := context.Background()
	db, events := testEvents(t)
	_, session := seedBackupWorld(t, ctx, db, events)
	maintenance := NewMaintenance(db)

	before, err := maintenance.IntegrityCheck(ctx)
	if err != nil {
		t.Fatalf("IntegrityCheck before backup: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var appendErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := appendInlineEvent(ctx, events, session, domain.EventKindInputRaw, map[string]int{"n": i}); err != nil {
				appendErr = err
				return
			}
		}
	}()

	backupDir := filepath.Join(t.TempDir(), "backup")
	report, err := maintenance.CreateBackup(ctx, admin.BackupRequest{DestDir: backupDir})
	close(stop)
	wg.Wait()
	if appendErr != nil {
		t.Fatalf("concurrent append during backup: %v", appendErr)
	}
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}

	verified, err := maintenance.VerifyBackup(ctx, backupDir)
	if err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	if verified.IntegrityCheck != "ok" {
		t.Errorf("backup integrity_check = %q, want ok", verified.IntegrityCheck)
	}
	if verified.ForeignKeyViolations != 0 {
		t.Errorf("backup foreign-key violations = %d, want 0", verified.ForeignKeyViolations)
	}
	if verified.LogicalFingerprint != report.LogicalFingerprint {
		t.Errorf("verified fingerprint = %s, want %s", verified.LogicalFingerprint, report.LogicalFingerprint)
	}

	// The restored database must reproduce the snapshot's logical content.
	restoreDir := t.TempDir()
	destDB := filepath.Join(restoreDir, "restored", "vibeshell.db")
	restored, err := maintenance.RestoreBackup(ctx, admin.RestoreRequest{SourceDir: backupDir, DestDatabase: destDB})
	if err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	if restored.LogicalFingerprint != report.LogicalFingerprint {
		t.Errorf("restored fingerprint = %s, want %s", restored.LogicalFingerprint, report.LogicalFingerprint)
	}

	restoredReport, err := integrityOfFile(ctx, destDB)
	if err != nil {
		t.Fatalf("inspect restored database: %v", err)
	}
	for _, table := range []string{"contents", "namespaces", "nodes", "node_versions", "commits"} {
		if got, want := restoredReport.RowCounts[table], verified.RowCounts[table]; got != want {
			t.Errorf("restored %s rows = %d, want %d", table, got, want)
		}
	}
	// Every event that predated the backup survives.
	if got, want := restoredReport.RowCounts["events"], before.RowCounts["events"]; got < want {
		t.Errorf("restored events = %d, want at least the %d recorded before the backup", got, want)
	}
	// The exact content bytes are present under the same hash.
	var hashCount int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM contents`).Scan(&hashCount); err != nil {
		t.Fatalf("count contents: %v", err)
	}
	if restoredReport.RowCounts["contents"] != int64(hashCount) {
		t.Errorf("restored content rows = %d, want %d", restoredReport.RowCounts["contents"], hashCount)
	}
}

// TestBackupTamperDetected requires the logical fingerprint to catch a value
// changed in the snapshot, even when SQLite still reports a structurally
// intact file.
func TestBackupTamperDetected(t *testing.T) {
	ctx := context.Background()
	db, events := testEvents(t)
	seedBackupWorld(t, ctx, db, events)
	maintenance := NewMaintenance(db)

	backupDir := filepath.Join(t.TempDir(), "backup")
	if _, err := maintenance.CreateBackup(ctx, admin.BackupRequest{DestDir: backupDir}); err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if _, err := maintenance.VerifyBackup(ctx, backupDir); err != nil {
		t.Fatalf("VerifyBackup before tamper: %v", err)
	}

	// Change a stored node name without touching the file structure.
	mutateBackupDB(t, filepath.Join(backupDir, backupDatabaseName),
		`UPDATE nodes SET name = name || '-tampered' WHERE id = (SELECT id FROM nodes ORDER BY id LIMIT 1)`)

	_, err := maintenance.VerifyBackup(ctx, backupDir)
	if err == nil {
		t.Fatal("VerifyBackup accepted a tampered backup")
	}
	if code := domain.GetErrorCode(err); code != "backup_fingerprint_mismatch" {
		t.Fatalf("VerifyBackup error code = %q, want backup_fingerprint_mismatch (%v)", code, err)
	}

	// A restore must refuse the same backup and leave no database behind.
	destDB := filepath.Join(t.TempDir(), "restored", "vibeshell.db")
	if _, err := maintenance.RestoreBackup(ctx, admin.RestoreRequest{SourceDir: backupDir, DestDatabase: destDB}); err == nil {
		t.Fatal("RestoreBackup accepted a tampered backup")
	}
	if _, statErr := os.Stat(destDB); !os.IsNotExist(statErr) {
		t.Errorf("restore left a database behind after a fingerprint mismatch: %v", statErr)
	}
}

// TestRestoreValidatedByLogicalFingerprintNotFileChecksum changes bytes that
// are not part of the logical content (the internal AUTOINCREMENT counter) so
// the snapshot's file checksum changes while its logical fingerprint does
// not. Verification and restore must still succeed, proving acceptance is
// logical rather than a file-checksum comparison.
func TestRestoreValidatedByLogicalFingerprintNotFileChecksum(t *testing.T) {
	ctx := context.Background()
	db, events := testEvents(t)
	seedBackupWorld(t, ctx, db, events)
	maintenance := NewMaintenance(db)

	backupDir := filepath.Join(t.TempDir(), "backup")
	report, err := maintenance.CreateBackup(ctx, admin.BackupRequest{DestDir: backupDir})
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	dbPath := filepath.Join(backupDir, backupDatabaseName)
	before := testFileSHA256(t, dbPath)

	// sqlite_sequence is internal bookkeeping and is deliberately not part of
	// the logical fingerprint, so bumping it changes the file's bytes only.
	mutateBackupDB(t, dbPath, `UPDATE sqlite_sequence SET seq = seq + 1000 WHERE name = 'events'`)
	after := testFileSHA256(t, dbPath)
	if before == after {
		t.Fatal("mutating sqlite_sequence did not change the snapshot file checksum")
	}

	if _, err := maintenance.VerifyBackup(ctx, backupDir); err != nil {
		t.Fatalf("VerifyBackup after a non-logical file change: %v", err)
	}
	destDB := filepath.Join(t.TempDir(), "restored", "vibeshell.db")
	restored, err := maintenance.RestoreBackup(ctx, admin.RestoreRequest{SourceDir: backupDir, DestDatabase: destDB})
	if err != nil {
		t.Fatalf("RestoreBackup after a non-logical file change: %v", err)
	}
	if restored.LogicalFingerprint != report.LogicalFingerprint {
		t.Errorf("restored fingerprint = %s, want the recorded %s", restored.LogicalFingerprint, report.LogicalFingerprint)
	}
}

// TestBackupCarriesAndRestoresHostKey proves the retained-host-key path: an
// extra file is copied, verified, and restored with its bytes and mode.
func TestBackupCarriesAndRestoresHostKey(t *testing.T) {
	ctx := context.Background()
	db, events := testEvents(t)
	seedBackupWorld(t, ctx, db, events)
	maintenance := NewMaintenance(db)

	liveDir := t.TempDir()
	hostKey := filepath.Join(liveDir, "ssh_host_ed25519_key")
	keyBytes := []byte("FAKE-HOST-KEY-MATERIAL\x00\x01")
	if err := os.WriteFile(hostKey, keyBytes, 0o600); err != nil {
		t.Fatalf("write host key: %v", err)
	}

	backupDir := filepath.Join(t.TempDir(), "backup")
	report, err := maintenance.CreateBackup(ctx, admin.BackupRequest{
		DestDir: backupDir,
		Extras:  []admin.BackupExtra{{Name: "ssh_host_key", Path: hostKey}},
	})
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if len(report.ExtraFiles) != 1 || report.ExtraFiles[0].Name != "ssh_host_key" {
		t.Fatalf("backup extras = %+v, want one ssh_host_key", report.ExtraFiles)
	}

	extrasDir := filepath.Join(t.TempDir(), "restored-extras")
	destDB := filepath.Join(t.TempDir(), "restored", "vibeshell.db")
	if _, err := maintenance.RestoreBackup(ctx, admin.RestoreRequest{
		SourceDir:     backupDir,
		DestDatabase:  destDB,
		DestExtrasDir: extrasDir,
	}); err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	restoredKey := filepath.Join(extrasDir, "ssh_host_key")
	got, err := os.ReadFile(restoredKey)
	if err != nil {
		t.Fatalf("read restored host key: %v", err)
	}
	if string(got) != string(keyBytes) {
		t.Errorf("restored host key bytes differ from the original")
	}
	info, err := os.Stat(restoredKey)
	if err != nil {
		t.Fatalf("stat restored host key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("restored host key mode = %04o, want 0600", perm)
	}
}
