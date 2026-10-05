package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"testing"
)

// newSpikeDB opens a fresh database on the state volume and applies the schema.
func newSpikeDB(t *testing.T, name string) (*sql.DB, string) {
	t.Helper()
	path := NewDatabasePath(t, name)
	db, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("open spike database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(Schema); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return db, path
}

// applySearchSchema adds the optional search projection, so a test can exercise
// the gates that assume it without repeating the statement.
func applySearchSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(SchemaSearch); err != nil {
		t.Fatalf("apply search schema: %v", err)
	}
}

// seedWorld inserts a namespace with its root directory and the directory
// revision row that membership checks read. The root references itself as its
// own parent so that the deferred foreign key is satisfied inside one
// transaction instead of by a bootstrap special case.
func seedWorld(t *testing.T, db *sql.DB, namespaceID, rootID string) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`INSERT INTO namespaces (id, name) VALUES (?, ?)`, namespaceID, namespaceID); err != nil {
		t.Fatalf("insert namespace: %v", err)
	}
	rootRevision := int64(1)
	if _, err := tx.Exec(
		`INSERT INTO nodes (id, namespace_id, parent_id, name, kind, revision) VALUES (?, ?, ?, '/', 'directory', ?)`,
		rootID, namespaceID, rootID, rootRevision,
	); err != nil {
		t.Fatalf("insert root node: %v", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO directory_revisions (directory_id, revision) VALUES (?, ?)`,
		rootID, rootRevision,
	); err != nil {
		t.Fatalf("insert root directory revision: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
}

// insertDirectory adds a child directory and bumps its parent's membership
// revision, which is the dependency a staged create must have read.
func insertDirectory(t *testing.T, db *sql.DB, id, parentID, name string) int64 {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin insert directory: %v", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO nodes (id, namespace_id, parent_id, name, kind, revision)
		 SELECT ?, namespace_id, ?, ?, 'directory', 1 FROM nodes WHERE id = ?`,
		id, parentID, name, parentID,
	); err != nil {
		t.Fatalf("insert directory %s: %v", name, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO directory_revisions (directory_id, revision) VALUES (?, 1)`, id,
	); err != nil {
		t.Fatalf("insert directory revision for %s: %v", name, err)
	}
	revision := bumpDirectoryRevision(t, tx, parentID)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit directory %s: %v", name, err)
	}
	return revision
}

// bumpDirectoryRevision advances one directory's membership revision and returns
// the new value.
func bumpDirectoryRevision(t *testing.T, tx *sql.Tx, directoryID string) int64 {
	t.Helper()
	if _, err := tx.Exec(
		`UPDATE directory_revisions SET revision = revision + 1 WHERE directory_id = ?`, directoryID,
	); err != nil {
		t.Fatalf("bump directory revision: %v", err)
	}
	var revision int64
	if err := tx.QueryRow(
		`SELECT revision FROM directory_revisions WHERE directory_id = ?`, directoryID,
	).Scan(&revision); err != nil {
		t.Fatalf("read directory revision: %v", err)
	}
	return revision
}

// queryString reads a single string result.
func queryString(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var value string
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value
}

// queryInt reads a single integer result.
func queryInt(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var value int64
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value
}

// explain returns the query plan of query as reportable lines.
func explain(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain %q: %v", query, err)
	}
	defer rows.Close()

	var plan []string
	for rows.Next() {
		var id, parent, notUsed int64
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan query plan: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate query plan: %v", err)
	}
	if len(plan) == 0 {
		return []string{"(no plan rows)"}
	}
	return plan
}

// countRows returns the number of rows in table.
func countRows(t *testing.T, db *sql.DB, table string) int64 {
	t.Helper()
	// The table names come from this package's own constants, never from input.
	return queryInt(t, db, "SELECT COUNT(*) FROM "+table)
}

// dependencyVersion reports the version of module in the test binary's build
// graph, so a receipt records the pinned dependency rather than a copy that can
// drift from go.mod.
func dependencyVersion(t *testing.T, module string) string {
	t.Helper()
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatalf("read build info")
	}
	for _, dep := range info.Deps {
		if dep.Path == module {
			return dep.Version
		}
	}
	// A dependency in the main module is reported by the main module version.
	if info.Main.Path == module {
		return info.Main.Version
	}
	t.Fatalf("module %s is not in the build graph", module)
	return ""
}

// fingerprint summarises a database by its row counts and content hashes, which
// is what a restore has to reproduce exactly.
func fingerprint(t *testing.T, db *sql.DB) string {
	t.Helper()
	var out strings.Builder

	tables := []string{
		"contents", "namespaces", "nodes", "directory_revisions",
		"events", "command_index", "path_index", "search_docs",
	}
	for _, table := range tables {
		fmt.Fprintf(&out, "%-20s %d\n", table, countRows(t, db, table))
	}

	rows, err := db.Query(`SELECT hash, size FROM contents ORDER BY hash`)
	if err != nil {
		t.Fatalf("read contents for fingerprint: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var hash string
		var size int64
		if err := rows.Scan(&hash, &size); err != nil {
			t.Fatalf("scan content fingerprint: %v", err)
		}
		fmt.Fprintf(&out, "content %s %d\n", hash, size)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate content fingerprint: %v", err)
	}

	rows, err = db.Query(`SELECT id, parent_id, name, kind, revision, content_hash, tombstone
	                      FROM nodes ORDER BY id`)
	if err != nil {
		t.Fatalf("read nodes for fingerprint: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, name, kind string
		var revision, tombstone int64
		var contentHash sql.NullString
		if err := rows.Scan(&id, &parent, &name, &kind, &revision, &contentHash, &tombstone); err != nil {
			t.Fatalf("scan node fingerprint: %v", err)
		}
		fmt.Fprintf(&out, "node %s parent=%s name=%q kind=%s revision=%d tombstone=%d content=%s\n",
			id, parent, name, kind, revision, tombstone, contentHash.String)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate node fingerprint: %v", err)
	}

	rows, err = db.Query(`SELECT id, session_id, sequence, occurred_at, kind FROM events ORDER BY id`)
	if err != nil {
		t.Fatalf("read events for fingerprint: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, session, occurredAt, kind string
		var sequence int64
		if err := rows.Scan(&id, &session, &sequence, &occurredAt, &kind); err != nil {
			t.Fatalf("scan event fingerprint: %v", err)
		}
		fmt.Fprintf(&out, "event %s session=%s sequence=%d at=%s kind=%s\n", id, session, sequence, occurredAt, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate event fingerprint: %v", err)
	}

	rows, err = db.Query(`SELECT event_id, command, path FROM search_docs ORDER BY event_id`)
	if err != nil {
		t.Fatalf("read search docs for fingerprint: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, command, path string
		if err := rows.Scan(&id, &command, &path); err != nil {
			t.Fatalf("scan search doc fingerprint: %v", err)
		}
		fmt.Fprintf(&out, "doc %s %q %q\n", id, command, path)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate search doc fingerprint: %v", err)
	}
	return out.String()
}

// databaseFileSize reports the space a database occupies on the state volume,
// including the write-ahead log and shared-memory files, because that is what a
// storage claim has to account for.
func databaseFileSize(t *testing.T, path string) int64 {
	t.Helper()
	var total int64
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		total += info.Size()
	}
	return total
}

// integrityReport runs the checks an operator would run before trusting a
// snapshot.
func integrityReport(t *testing.T, db *sql.DB) []string {
	t.Helper()
	lines := []string{
		"integrity_check: " + queryString(t, db, `PRAGMA integrity_check`),
		"foreign_key_check rows: " + fmt.Sprint(queryInt(t, db, `SELECT COUNT(*) FROM pragma_foreign_key_check`)),
	}
	return lines
}

// recordEvent appends one accepted event row inside tx with its lookup
// projections, which is the shape of a committed turn.
func recordEvent(t *testing.T, tx *sql.Tx, eventID, sessionID string, sequence int, kind, command, path string, payload []byte) {
	t.Helper()
	err := appendAcceptedEvent(context.Background(), tx, AcceptedEvent{
		ID:         eventID,
		SessionID:  sessionID,
		Sequence:   sequence,
		Kind:       kind,
		OccurredAt: fixedTimestamp(sequence),
		Payload:    payload,
		Command:    command,
		Path:       path,
	})
	if err != nil {
		t.Fatalf("record event %s: %v", eventID, err)
	}
}

// fixedTimestamp gives the fixtures a stable clock. The tests inject time rather
// than reading it, so a receipt does not change on every run for that reason.
func fixedTimestamp(sequence int) string {
	return fmt.Sprintf("2026-10-03T00:%02d:%02dZ", sequence/60, sequence%60)
}

// sortedCopy returns a sorted copy of values, so receipt comparisons do not
// depend on row order.
func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

// mustExecContext runs a statement with a context, for the few places that need
// cancellation rather than the test's own deadline.
func mustExecContext(ctx context.Context, db *sql.DB, query string, args ...any) (sql.Result, error) {
	return db.ExecContext(ctx, query, args...)
}
