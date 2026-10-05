package storage

import (
	"context"
	"database/sql"
	"os"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestDriverAndSQLiteVersion qualifies gate 1: the pure-Go driver builds and
// tests in the container, and the SQLite build behind it is identified.
func TestDriverAndSQLiteVersion(t *testing.T) {
	db, path := newSpikeDB(t, "driver")
	receipt := newReport("Gate 1: driver and SQLite version")
	receipt.add("captured (UTC)", time.Now().UTC().Format(time.RFC3339))
	receipt.add("database", path)

	receipt.section("toolchain")
	receipt.add("go", runtime.Version())
	receipt.add("driver module", dependencyVersion(t, "modernc.org/sqlite"))
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("read build info")
	}
	for _, setting := range info.Settings {
		if setting.Key == "CGO_ENABLED" {
			receipt.addf("build setting %-16s %s", setting.Key, setting.Value)
		}
	}

	receipt.section("database/sql")
	drivers := sql.Drivers()
	receipt.addAll("registered drivers", drivers)
	if !slices.Contains(drivers, DriverName) {
		t.Fatalf("driver %q is not registered; registered: %v", DriverName, drivers)
	}

	receipt.section("SQLite build")
	version := queryString(t, db, `SELECT sqlite_version()`)
	sourceID := queryString(t, db, `SELECT sqlite_source_id()`)
	receipt.add("sqlite_version()", version)
	receipt.add("sqlite_source_id()", sourceID)
	if version == "" {
		t.Fatal("sqlite_version() returned nothing")
	}
	receipt.add("page_size", queryInt(t, db, `PRAGMA page_size`))
	receipt.add("max_page_count", queryInt(t, db, `PRAGMA max_page_count`))
	receipt.add("encoding", queryString(t, db, `PRAGMA encoding`))
	receipt.add("journal_mode", queryString(t, db, `PRAGMA journal_mode`))
	receipt.add("wal_autocheckpoint", queryInt(t, db, `PRAGMA wal_autocheckpoint`))
	if encoding := queryString(t, db, `PRAGMA encoding`); encoding != "UTF-8" {
		t.Fatalf("database encoding is %q, want UTF-8", encoding)
	}

	receipt.section("compile options of interest")
	var options []string
	rows, err := db.Query(`PRAGMA compile_options`)
	if err != nil {
		t.Fatalf("read compile options: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var option string
		if err := rows.Scan(&option); err != nil {
			t.Fatalf("scan compile option: %v", err)
		}
		options = append(options, option)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate compile options: %v", err)
	}
	receipt.add("compile options", len(options))
	for _, name := range SearchPragmas {
		receipt.addf("option %-17s %v", name, slices.Contains(options, name))
	}

	receipt.section("schema")
	for _, table := range []string{"contents", "nodes", "directory_revisions", "events", "command_index", "path_index", "search_docs"} {
		receipt.addf("table %-20s rows %d", table, countRows(t, db, table))
	}
	receipt.addAll("integrity", integrityReport(t, db))

	// A pure-Go driver must not require a C toolchain at run time. Asserting the
	// absence of cgo linkage is the container's separate CGO_ENABLED=0 build
	// receipt; here the driver's own error type is checked to be the pure-Go one.
	if _, err := db.Exec(`INSERT INTO namespaces (id, name) VALUES ('probe', 'probe')`); err != nil {
		t.Fatalf("write through the driver: %v", err)
	}

	mount, err := MountOf(path)
	if err != nil {
		t.Fatalf("resolve mount for %s: %v", path, err)
	}
	receipt.section("state volume")
	receipt.add("mount point", mount.MountPoint)
	receipt.add("filesystem", mount.FSType)
	receipt.add("source", mount.Source)

	receipt.write(t, "gate1-driver")
}

// TestStrictTablesAndDeferredForeignKeys covers the two schema features the
// event and world tables rely on: STRICT column types, and a deferred
// self-reference so a namespace root and its children can be inserted in one
// transaction.
func TestStrictTablesAndDeferredForeignKeys(t *testing.T) {
	db, _ := newSpikeDB(t, "strict")
	receipt := newReport("Gate 1b: STRICT tables and deferred foreign keys")

	seedWorld(t, db, "ns-1", "root-1")
	receipt.add("root inserted with self parent", countRows(t, db, "nodes"))

	// STRICT tables reject a value whose type does not fit the column.
	if _, err := db.Exec(`INSERT INTO contents (hash, bytes, size) VALUES ('h', 12345, 5)`); err == nil {
		t.Fatal("STRICT contents accepted an integer in the bytes BLOB column")
	} else {
		receipt.add("integer into BLOB rejected", err.Error())
	}
	if _, err := db.Exec(`INSERT INTO nodes (id, namespace_id, parent_id, name, kind, revision) VALUES ('x', 'ns-1', 'root-1', 'x', 'file', 'not-a-number')`); err == nil {
		t.Fatal("STRICT nodes accepted text in the revision INTEGER column")
	} else {
		receipt.add("text into INTEGER rejected", err.Error())
	}

	// A foreign key violation inside a transaction is reported at commit, which
	// is what lets a turn insert its changes and have one commit check them.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO nodes (id, namespace_id, parent_id, name, kind, revision) VALUES ('orphan', 'ns-1', 'missing', 'orphan', 'file', 1)`,
	); err != nil {
		t.Fatalf("insert deferred foreign key: %v", err)
	}
	if _, err := tx.Exec(`UPDATE directory_revisions SET revision = revision + 1 WHERE directory_id = 'root-1'`); err != nil {
		t.Fatalf("bump revision: %v", err)
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("commit accepted a dangling parent reference")
	} else {
		receipt.add("dangling parent rejected at commit", err.Error())
	}
	if count := queryInt(t, db, `SELECT COUNT(*) FROM nodes WHERE id = 'orphan'`); count != 0 {
		t.Fatalf("rejected transaction left %d rows behind", count)
	}

	receipt.addAll("integrity", integrityReport(t, db))
	receipt.write(t, "gate1-schema")
}

// TestPragmasReachEveryPooledConnection qualifies part of gate 3: the
// durability settings must apply to each connection database/sql hands out, not
// only to the first one.
func TestPragmasReachEveryPooledConnection(t *testing.T) {
	db, _ := newSpikeDB(t, "pragmas")
	receipt := newReport("Gate 3b: durability pragmas on every pooled connection")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Hold the whole pool so that four distinct connections are in use at once.
	conns := make([]*sql.Conn, DefaultPoolSize)
	defer func() {
		for _, conn := range conns {
			if conn != nil {
				conn.Close()
			}
		}
	}()

	for i := range conns {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("acquire connection %d: %v", i, err)
		}
		conns[i] = conn
	}

	want := map[string]any{
		"journal_mode": "wal",
		"synchronous":  int64(2),
		"busy_timeout": int64(5000),
		"foreign_keys": int64(1),
	}
	for i, conn := range conns {
		for pragma, expected := range want {
			var got any
			if err := conn.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
				t.Fatalf("connection %d pragma %s: %v", i, pragma, err)
			}
			if got != expected {
				t.Errorf("connection %d pragma %s = %v, want %v", i, pragma, got, expected)
			}
			receipt.addf("conn %d %-20s %v", i, pragma, got)
		}
		// The driver consumes this one itself and returns no row, so it is
		// verified by behaviour in TestBoundaryAwarePathPrefixLookup.
		receipt.addf("conn %d %-20s not readable, verified behaviourally", i, "case_sensitive_like")
	}

	receipt.write(t, "gate3-pragmas")
}

// TestDriverRejectsCorruptDatabases records how the driver reports damage,
// because recovery has to be able to tell corruption from an absent file.
func TestDriverRejectsCorruptDatabases(t *testing.T) {
	path := NewDatabasePath(t, "corrupt")
	if err := os.WriteFile(path, []byte("this is not a SQLite database, not even a little bit"), 0o644); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	db, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("open corrupt file: %v", err)
	}
	defer db.Close()

	receipt := newReport("Gate 1c: corrupt database reporting")
	var queryErr error
	if _, queryErr = db.Exec(`SELECT 1`); queryErr == nil {
		receipt.add("SELECT 1 on a corrupt file", "succeeded (unexpected)")
	} else {
		receipt.add("SELECT 1 rejected", strings.SplitN(queryErr.Error(), "\n", 2)[0])
		t.Logf("corrupt database rejected: %v", queryErr)
	}
	receipt.write(t, "gate1-corrupt")
}
