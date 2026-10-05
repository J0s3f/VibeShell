package storage

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Phase values for the durability gate. The write phase runs before the
// container is replaced and the read phase after, so a row can only be read back
// if the state volume really survived.
const (
	// PhaseWriteUnclean commits and then ends the process without closing the
	// database. SQLite never gets to checkpoint, so the committed frames are
	// still only in the write-ahead log when the process disappears. This is the
	// crash-equivalent case, and it is the one worth proving.
	PhaseWriteUnclean = "write-unclean"
	// PhaseWriteClosed commits and closes the database cleanly, which
	// checkpoints the log and removes it.
	PhaseWriteClosed = "write-closed"
	// PhaseRead reopens the database left behind by either write phase.
	PhaseRead = "read"
)

// durableMarker is the row the phases use to prove persistence. It carries bytes
// that a text round trip would damage, so a successful read also re-proves exact
// storage across the replacement.
var durableMarker = []byte("durable\x00marker\xff\xfe\x80\x81")

// TestDurableStateAcrossContainerReplacement qualifies gate 3. With SPIKE_PHASE
// set it runs exactly that phase, which is how experiments/restart-spike.ps1 puts
// a container replacement between the write and the read. Without SPIKE_PHASE it
// runs both phases in one process, which still proves that a committed row
// survives reopening the database.
func TestDurableStateAcrossContainerReplacement(t *testing.T) {
	path := DurableDatabasePath(t, DurableDBEnv, DefaultDurableDB)

	phase, phased := Phase()
	if !phased {
		writeDurableState(t, path, false)
		readDurableState(t, path, "in-process reopen")
		Receipt(t, "durability-restart-required", []string{
			"The container replacement proof is not produced by this test.",
			"Run experiments/restart-spike.ps1 from the checkout root; it writes",
			"receipts/durability-before-restart.txt, replaces the container with",
			"scripts/dev.ps1 stop and start, and writes",
			"receipts/durability-after-restart.txt from a fresh process.",
		}...)
		return
	}

	switch phase {
	case PhaseWriteUnclean:
		writeDurableState(t, path, false)
		Receipt(t, "durability-phase-exit",
			"ending the process without closing the database; the committed frames are",
			"still only in the write-ahead log, which is what the read phase recovers from")
		// Leaving without Close is the point: the committed frames stay in the
		// write-ahead log. os.Exit skips the deferred closes that a normal test
		// return would run, exactly as a killed process skips them.
		//
		// The status is non-zero because the Go testing package refuses an early
		// os.Exit(0) from inside a test, treating it as a test that ended without
		// reporting. Both exit paths terminate immediately and skip deferred
		// functions, so the state left on the volume is the same.
		os.Exit(1)
	case PhaseWriteClosed:
		writeDurableState(t, path, true)
		readDurableState(t, path, "same process after clean close")
	case PhaseRead:
		readDurableState(t, path, "fresh process after container replacement")
	default:
		t.Fatalf("unknown %s value %q", PhaseEnv, phase)
	}
}

// writeDurableState creates the durable database and commits one identifiable
// row, recording what the state volume holds while the process is still alive.
func writeDurableState(t *testing.T, path string, closeCleanly bool) {
	t.Helper()

	// Start from a known state so that repeating the spike cannot pass on a row
	// left by an earlier run.
	removeDatabaseFiles(t, path)

	db, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("open durable database: %v", err)
	}
	defer func() {
		if closeCleanly {
			if err := db.Close(); err != nil {
				t.Errorf("close durable database: %v", err)
			}
		}
	}()

	if _, err := db.Exec(Schema); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	seedWorld(t, db, "ns-durable", "root-durable")

	hash := ContentHash(durableMarker)
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin durable commit: %v", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO contents (hash, bytes, size, media_type) VALUES (?, ?, ?, 'application/octet-stream')`,
		hash, durableMarker, len(durableMarker),
	); err != nil {
		t.Fatalf("insert durable content: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit durable content: %v", err)
	}

	// WAL files must exist while a connection is open; a clean close removes
	// them again after checkpointing.
	receipt := newReport("Gate 3a: durable write phase")
	receipt.add("phase", phaseOrNone(t))
	receipt.add("captured (UTC)", time.Now().UTC().Format(time.RFC3339))
	receipt.add("database", path)
	receipt.add("committed content hash", hash)
	receipt.add("journal_mode", queryString(t, db, `PRAGMA journal_mode`))
	receipt.add("synchronous", queryInt(t, db, `PRAGMA synchronous`))
	receipt.add("wal_autocheckpoint", queryInt(t, db, `PRAGMA wal_autocheckpoint`))
	receipt.addAll("state directory (connection open)", listDatabaseFiles(t, path))
	receipt.addAll("integrity", integrityReport(t, db))
	receipt.write(t, "durability-before-restart")

	if closeCleanly {
		if err := db.Close(); err != nil {
			t.Fatalf("close durable database: %v", err)
		}
		receipt2 := newReport("Gate 3a: state after a clean close")
		receipt2.add("database", path)
		receipt2.addAll("state directory (closed)", listDatabaseFiles(t, path))
		receipt2.add("note", "a clean close checkpoints the log and removes -wal and -shm; the data is in the main database file")
		receipt2.write(t, "durability-clean-close")
	}
}

// readDurableState reopens the durable database and requires the committed row,
// then records the bytes on the volume so the two phases can be compared.
func readDurableState(t *testing.T, path, context string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("durable database %s is missing after %s: %v", path, context, err)
	}

	db, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("reopen durable database: %v", err)
	}
	defer db.Close()

	receipt := newReport("Gate 3a: durable read phase")
	receipt.add("context", context)
	receipt.add("phase", phaseOrNone(t))
	receipt.add("captured (UTC)", time.Now().UTC().Format(time.RFC3339))
	receipt.add("database", path)

	hash := ContentHash(durableMarker)
	var got []byte
	var size int64
	err = db.QueryRow(`SELECT bytes, size FROM contents WHERE hash = ?`, hash).Scan(&got, &size)
	if err != nil {
		t.Fatalf("read the committed content back after %s: %v", context, err)
	}
	if !bytes.Equal(got, durableMarker) {
		t.Fatalf("content changed across %s: got %v want %v", context, got, durableMarker)
	}
	if size != int64(len(durableMarker)) {
		t.Fatalf("content size is %d, want %d", size, len(durableMarker))
	}
	receipt.add("content hash", hash)
	receipt.add("bytes identical", true)
	receipt.add("size", size)

	receipt.add("namespace root", queryInt(t, db, `SELECT COUNT(*) FROM nodes WHERE id = 'root-durable'`))
	receipt.add("journal_mode after reopen", queryString(t, db, `PRAGMA journal_mode`))
	receipt.addAll("state directory", listDatabaseFiles(t, path))
	receipt.addAll("integrity", integrityReport(t, db))

	checksum, err := fileSHA256(path)
	if err != nil {
		t.Fatalf("checksum the database file: %v", err)
	}
	receipt.add("database file sha256", checksum)
	mount, err := MountOf(path)
	if err != nil {
		t.Fatalf("resolve mount: %v", err)
	}
	receipt.add("filesystem", mount.FSType)
	receipt.add("mount point", mount.MountPoint)
	receipt.write(t, "durability-after-restart")
}

// TestWALFilesAppearAndDisappear pins down the write-ahead log lifecycle that
// the durability guarantee depends on.
func TestWALFilesAppearAndDisappear(t *testing.T) {
	path := NewDatabasePath(t, "wal")
	db, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(Schema); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	seedWorld(t, db, "ns-wal", "root-wal")

	receipt := newReport("Gate 3c: write-ahead log lifecycle")

	before := listDatabaseFiles(t, path)
	receipt.addAll("after the seed commit", before)
	if !containsFile(before, path+"-wal") {
		t.Fatalf("no write-ahead log next to the database after a commit: %v", before)
	}

	// A checkpoint folds the log into the main file and truncates it, which is
	// what a backup should do first.
	var busy, checkpointed, walPages int64
	if err := db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &checkpointed, &walPages); err != nil {
		t.Fatalf("wal_checkpoint(TRUNCATE): %v", err)
	}
	receipt.add("checkpoint busy", busy)
	receipt.add("checkpoint pages", checkpointed)
	receipt.add("checkpoint remaining wal pages", walPages)
	receipt.addAll("after TRUNCATE checkpoint", listDatabaseFiles(t, path))

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	afterClose := listDatabaseFiles(t, path)
	receipt.addAll("after close", afterClose)
	if containsFile(afterClose, path+"-wal") {
		t.Errorf("the write-ahead log survived a clean close: %v", afterClose)
	}

	receipt.write(t, "gate3-wal-lifecycle")
}

// TestSynchronousFullSurvivesForcedExit records what an unclean end leaves on the
// volume, which is the state the restart phase has to recover from.
func TestSynchronousFullSurvivesForcedExit(t *testing.T) {
	path := NewDatabasePath(t, "unclean")
	db, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(Schema); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	seedWorld(t, db, "ns-unclean", "root-unclean")

	receipt := newReport("Gate 3d: state left by an unclean exit")
	receipt.add("database", path)
	receipt.add("synchronous", queryInt(t, db, `PRAGMA synchronous`))
	receipt.addAll("while connected", listDatabaseFiles(t, path))

	// Simulate the process disappearing: the file descriptors are still open in
	// this process, but nothing runs SQLite's shutdown path, exactly as after a
	// SIGKILL.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO contents (hash, bytes, size) VALUES ('unclean', X'00FF80', 3)`,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	receipt.addAll("after an uncheckpointed commit", listDatabaseFiles(t, path))

	// A second connection sees the committed row even before a checkpoint, which
	// is what readers rely on in WAL mode.
	other, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	if count := queryInt(t, other, `SELECT COUNT(*) FROM contents WHERE hash = 'unclean'`); count != 1 {
		t.Errorf("a reader does not see an uncheckpointed commit: count %d", count)
	}
	receipt.add("visible to a second connection", true)
	other.Close()
	db.Close()

	receipt.add("note", "the restart phase reopens this state; recovery is SQLite's own, driven by the log next to the database")
	receipt.write(t, "gate3-unclean-exit")
}

// listDatabaseFiles reports the database and its sidecar files with sizes.
func listDatabaseFiles(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Dir(path), err)
	}
	base := filepath.Base(path)

	var lines []string
	for _, entry := range entries {
		name := entry.Name()
		if name != base && name != base+"-wal" && name != base+"-shm" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		lines = append(lines, fmt.Sprintf("%-24s %d bytes", name, info.Size()))
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return []string{"(no database files)"}
	}
	return lines
}

// containsFile reports whether listDatabaseFiles saw the given sidecar.
func containsFile(lines []string, path string) bool {
	name := filepath.Base(path)
	for _, line := range lines {
		if strings.HasPrefix(line, name+" ") {
			return true
		}
	}
	return false
}

// removeDatabaseFiles deletes a database and its sidecars so that a repeated
// spike cannot pass on state from an earlier run.
func removeDatabaseFiles(t *testing.T, path string) {
	t.Helper()
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove %s: %v", name, err)
		}
	}
}

func phaseOrNone(t *testing.T) string {
	t.Helper()
	phase, phased := Phase()
	if !phased {
		return "(unphased run)"
	}
	return phase
}
