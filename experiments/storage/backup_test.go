package storage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Phase values for the backup gate. The three phases run in three processes, with
// a container replacement between each pair, so a restore cannot pass on state that
// only ever lived in memory or in a bind-mounted checkout.
const (
	// PhaseBackupWrite builds a representative world, checkpoints it, and writes a
	// self-contained copy with VACUUM INTO.
	PhaseBackupWrite = "backup-write"
	// PhaseRestore reads the copy in a fresh container, rebuilds a live database
	// from it, and replaces the live database with that rebuild.
	PhaseRestore = "restore"
	// PhaseRestoreVerify opens the replaced live database in yet another container
	// and requires the same content the backup carried.
	PhaseRestoreVerify = "restore-verify"
)

// BackupDBEnv names the live database of the backup gate. The backup and the
// rebuild are written beside it on the same state volume.
const (
	BackupDBEnv     = "SPIKE_BACKUP_DB"
	DefaultBackupDB = "/state/spike/backup/live.db"
)

// backupPaths are the three files the backup gate moves between phases. They all
// live in one directory on the state volume, so a restore reads the copy from
// local block storage exactly as an operator would.
type backupPaths struct {
	live     string
	backup   string
	restored string
	expected string
}

func resolveBackupPaths(t *testing.T) backupPaths {
	t.Helper()
	live := DurableDatabasePath(t, BackupDBEnv, DefaultBackupDB)
	dir := filepath.Dir(live)
	return backupPaths{
		live:     live,
		backup:   filepath.Join(dir, "backup.db"),
		restored: filepath.Join(dir, "restored.db"),
		expected: filepath.Join(dir, "expected-fingerprint.txt"),
	}
}

// TestBackupAndRestore qualifies gate 8: a snapshot taken with VACUUM INTO can be
// restored into a fresh container and becomes the live database again.
//
// The proof spans processes, so experiments/restart-spike.ps1 drives the phases. Run
// without SPIKE_PHASE the test only records that the proof still needs the script.
func TestBackupAndRestore(t *testing.T) {
	paths := resolveBackupPaths(t)

	phase, phased := Phase()
	if !phased {
		Receipt(t, "backup-restart-required", []string{
			"The cross-container backup and restore proof is not produced by this test.",
			"Run experiments/restart-spike.ps1 from the checkout root; it runs",
			"SPIKE_PHASE=" + PhaseBackupWrite + ", replaces the container, runs",
			"SPIKE_PHASE=" + PhaseRestore + ", replaces it again, and runs",
			"SPIKE_PHASE=" + PhaseRestoreVerify + ".",
		}...)
		return
	}

	switch phase {
	case PhaseBackupWrite:
		writeBackup(t, paths)
	case PhaseRestore:
		restoreBackup(t, paths)
	case PhaseRestoreVerify:
		verifyRestoredDatabase(t, paths)
	default:
		t.Fatalf("unknown %s value %q", PhaseEnv, phase)
	}
}

// writeBackup builds a world worth restoring, then copies it with VACUUM INTO.
func writeBackup(t *testing.T, paths backupPaths) {
	t.Helper()
	for _, path := range []string{paths.live, paths.backup, paths.restored, paths.expected} {
		removeDatabaseFiles(t, path)
	}

	source, err := Open(paths.live, Options{})
	if err != nil {
		t.Fatalf("open live database: %v", err)
	}
	if _, err := source.Exec(Schema); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	applySearchSchema(t, source)
	buildBackupWorld(t, source)
	sourceFingerprint := fingerprint(t, source)
	integrity := integrityReport(t, source)

	// A checkpoint before the copy means the snapshot is a complete file rather
	// than a database plus a log, which is what makes it movable.
	var busy, moved, remaining int64
	if err := source.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &moved, &remaining); err != nil {
		t.Fatalf("checkpoint before the backup: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("close live database: %v", err)
	}

	// VACUUM INTO writes a compacted, self-contained copy. The target must not
	// exist, which is why the file list above is cleared first.
	backup, err := Open(paths.live, Options{})
	if err != nil {
		t.Fatalf("reopen live database: %v", err)
	}
	defer backup.Close()
	if _, err := backup.Exec(`VACUUM INTO ?`, paths.backup); err != nil {
		t.Fatalf("VACUUM INTO %s: %v", paths.backup, err)
	}

	copied, err := Open(paths.backup, Options{})
	if err != nil {
		t.Fatalf("open the backup copy: %v", err)
	}
	defer copied.Close()
	copyFingerprint := fingerprint(t, copied)
	copyIntegrity := integrityReport(t, copied)

	receipt := newReport("Gate 8a: a backup taken with VACUUM INTO")
	receipt.add("phase", phaseOrNone(t))
	receipt.add("live database", paths.live)
	receipt.add("backup copy", paths.backup)
	receipt.add("checkpoint pages", moved)
	receipt.add("checkpoint remaining log pages", remaining)
	receipt.add("live bytes", databaseFileSize(t, paths.live))
	receipt.add("backup bytes", databaseFileSize(t, paths.backup))
	receipt.add("fingerprints equal", sourceFingerprint == copyFingerprint)
	receipt.addAll("live integrity", integrity)
	receipt.addAll("copy integrity", copyIntegrity)

	if sourceFingerprint != copyFingerprint {
		t.Errorf("the backup holds different content from the live database")
		for _, line := range fingerprintDiff(sourceFingerprint, copyFingerprint) {
			t.Logf("%s", line)
		}
	}
	receipt.add("event rows", countRows(t, copied, "events"))
	receipt.add("content rows", countRows(t, copied, "contents"))
	receipt.add("full-text hits", len(ftsHits(t, copied, "cat")))
	receipt.add("subtree hits", len(subtreePaths(t, copied, "/project")))
	receipt.write(t, "backup-before-restore")
}

// restoreBackup rebuilds a live database from the backup copy and puts it in place,
// which is the operation an operator performs after losing a container.
func restoreBackup(t *testing.T, paths backupPaths) {
	t.Helper()
	if _, err := os.Stat(paths.backup); err != nil {
		t.Fatalf("the backup copy is missing at %s: %v", paths.backup, err)
	}

	backup, err := Open(paths.backup, Options{})
	if err != nil {
		t.Fatalf("open the backup copy: %v", err)
	}
	backupFingerprint := fingerprint(t, backup)
	backupIntegrity := integrityReport(t, backup)
	backup.Close()

	removeDatabaseFiles(t, paths.restored)

	// Rebuilding with VACUUM INTO rather than copying the file leaves a database
	// whose pages are freshly laid out, which is what a restore should produce.
	rebuild, err := Open(paths.backup, Options{})
	if err != nil {
		t.Fatalf("open the backup copy for the rebuild: %v", err)
	}
	if _, err := rebuild.Exec(`VACUUM INTO ?`, paths.restored); err != nil {
		t.Fatalf("VACUUM INTO %s: %v", paths.restored, err)
	}
	rebuild.Close()

	restored, err := Open(paths.restored, Options{})
	if err != nil {
		t.Fatalf("open the rebuilt database: %v", err)
	}
	restoredFingerprint := fingerprint(t, restored)
	restoredIntegrity := integrityReport(t, restored)
	restored.Close()

	receipt := newReport("Gate 8b: restore from the backup copy in a fresh container")
	receipt.add("phase", phaseOrNone(t))
	receipt.add("captured (UTC)", time.Now().UTC().Format(time.RFC3339))
	receipt.add("backup copy", paths.backup)
	receipt.add("rebuilt database", paths.restored)
	receipt.add("fingerprints equal", backupFingerprint == restoredFingerprint)
	receipt.addAll("backup integrity", backupIntegrity)
	receipt.addAll("rebuilt integrity", restoredIntegrity)
	receipt.add("rebuilt bytes", databaseFileSize(t, paths.restored))
	if backupFingerprint != restoredFingerprint {
		t.Errorf("the rebuilt database holds different content from the backup copy")
	}

	// Replace the live database with the rebuild. Rename is atomic within the
	// volume, so the replacement is never observed half done.
	liveChecksumBefore, _ := fileSHA256(paths.live)
	removeDatabaseFiles(t, paths.live)
	if err := os.Rename(paths.restored, paths.live); err != nil {
		t.Fatalf("replace the live database: %v", err)
	}
	liveChecksumAfter, err := fileSHA256(paths.live)
	if err != nil {
		t.Fatalf("checksum the replaced live database: %v", err)
	}
	receipt.add("live checksum before", liveChecksumBefore)
	receipt.add("live checksum after", liveChecksumAfter)

	// The fingerprint a later container has to reproduce is written beside the
	// database, on the same volume, so the verify phase compares against it rather
	// than against anything from this process.
	if err := os.WriteFile(paths.expected, []byte(restoredFingerprint), 0o644); err != nil {
		t.Fatalf("write the expected fingerprint: %v", err)
	}
	receipt.add("expected fingerprint written", paths.expected)
	receipt.write(t, "backup-after-restore")
}

// verifyRestoredDatabase opens the replaced live database in a fresh container and
// requires the content the backup carried, which is the end of the restore.
func verifyRestoredDatabase(t *testing.T, paths backupPaths) {
	t.Helper()
	expected, err := os.ReadFile(paths.expected)
	if err != nil {
		t.Fatalf("read the expected fingerprint: %v", err)
	}

	db, err := Open(paths.live, Options{})
	if err != nil {
		t.Fatalf("open the restored live database: %v", err)
	}
	defer db.Close()

	got := fingerprint(t, db)
	receipt := newReport("Gate 8c: the restored database in a fresh container")
	receipt.add("phase", phaseOrNone(t))
	receipt.add("captured (UTC)", time.Now().UTC().Format(time.RFC3339))
	receipt.add("live database", paths.live)
	receipt.add("matches the restore fingerprint", got == string(expected))
	receipt.addAll("integrity", integrityReport(t, db))
	receipt.add("event rows", countRows(t, db, "events"))
	receipt.add("content rows", countRows(t, db, "contents"))
	receipt.add("full-text hits", len(ftsHits(t, db, "cat")))
	receipt.add("subtree hits", len(subtreePaths(t, db, "/project")))
	receipt.addAll("state directory", listDatabaseFiles(t, paths.live))
	receipt.add("live bytes", databaseFileSize(t, paths.live))
	if got != string(expected) {
		t.Errorf("the restored database differs from the restore fingerprint")
		for _, line := range fingerprintDiff(string(expected), got) {
			t.Logf("%s", line)
		}
	}
	receipt.write(t, "backup-verify-after-restore")
}

// buildBackupWorld fills a namespace with the kinds of row a restore has to
// reproduce: binary content, nested directories, an accepted event with its lookup
// and search projections, and enough rows for a search to have more than one hit.
func buildBackupWorld(t *testing.T, db *sql.DB) {
	t.Helper()
	seedWorld(t, db, "ns-backup", "root-backup")
	insertDirectory(t, db, "project", "root-backup", "project")
	insertDirectory(t, db, "src", "project", "src")

	files := []struct {
		id, parent, name string
		body             []byte
	}{
		{"node-readme", "project", "README.md", []byte("# project\n\ncat /project/README.md\n")},
		{"node-main", "src", "main.go", append([]byte("package main\n"), damagePattern...)},
		{"node-empty", "project", "empty.bin", []byte{}},
	}
	for index, file := range files {
		hash := ContentHash(file.body)
		revision := directoryRevision(t, db, file.parent)
		if err := runCommit(t, db, ChangeSet{
			NamespaceID: "ns-backup",
			Contents:    []Content{{Hash: hash, Bytes: file.body, MediaType: guessMediaType(file.name)}},
			Changes: []StagedChange{{
				NodeID: file.id, ParentID: file.parent, Name: file.name,
				Kind: "file", ContentHash: hash,
				ExpectAbsent: true, ExpectedParentRevision: revision,
			}},
			Events: []AcceptedEvent{{
				ID: "ev-" + file.id, SessionID: "session-backup", Sequence: index + 1, Kind: "accepted",
				OccurredAt: fixedTimestamp(index + 1),
				Command:    "cat /project/" + file.name,
				Path:       "/project/" + file.name,
				Payload:    []byte{0x00, 0xff},
			}},
		}); err != nil {
			t.Fatalf("commit %s: %v", file.name, err)
		}
	}

	// The search projection is derived data, so a restore has to be able to
	// rebuild or carry it; here the triggers maintain it in the same transaction.
	applySearchProjection(t, db)
}

// applySearchProjection fills the search projection for every recorded event, as a
// separate step from the commit above so the restore also proves the projection can
// be rebuilt independently of the events.
func applySearchProjection(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM events ORDER BY id`)
	if err != nil {
		t.Fatalf("read events for the search projection: %v", err)
	}
	var eventIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan event id: %v", err)
		}
		eventIDs = append(eventIDs, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate event ids: %v", err)
	}
	rows.Close()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin the search projection: %v", err)
	}
	defer tx.Rollback()
	for _, id := range eventIDs {
		if err := IndexEventForSearch(context.Background(), tx, id,
			queryString(t, db, `SELECT command FROM command_index WHERE event_id = ?`, id),
			queryString(t, db, `SELECT path FROM path_index WHERE event_id = ?`, id),
		); err != nil {
			t.Fatalf("index %s for search: %v", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit the search projection: %v", err)
	}
}

// subtreePaths returns the paths under a prefix using the qualified key range.
func subtreePaths(t *testing.T, db *sql.DB, prefix string) []string {
	t.Helper()
	low, high := SubtreeRange(prefix)
	return queryStrings(t, db,
		`SELECT DISTINCT path FROM path_index WHERE path_key >= ? AND path_key < ? ORDER BY path`, low, high)
}

func guessMediaType(name string) string {
	if filepath.Ext(name) == ".md" {
		return "text/markdown"
	}
	return "application/octet-stream"
}

// fingerprintDiff reports lines that appear in only one of two fingerprints, so a
// mismatch names the rows that moved instead of only that something did.
func fingerprintDiff(want, got string) []string {
	counts := map[string]int{}
	for _, line := range nonEmptyLines(want) {
		counts[line]++
	}

	var diff []string
	for _, line := range nonEmptyLines(got) {
		if counts[line] > 0 {
			counts[line]--
			continue
		}
		diff = append(diff, "only in this container: "+line)
	}
	for line, count := range counts {
		for i := 0; i < count; i++ {
			diff = append(diff, "only in the expected fingerprint: "+line)
		}
	}
	sort.Strings(diff)
	return diff
}

func nonEmptyLines(body string) []string {
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
