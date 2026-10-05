// Package storage is the A05 storage qualification spike for VibeShell.
//
// It is an isolated experiment, not application code. Every declaration here
// answers one question from PLAN 3.2, 5.1-5.3, 10.2, or 10.3 and leaves a
// reproducible receipt under receipts/. Findings and the resulting decision
// live in docs/research/2026-10-03-storage-spike.md.
package storage

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver
)

// DriverName is the database/sql driver name registered by modernc.org/sqlite.
const DriverName = "sqlite"

// Environment overrides. The container always keeps spike databases on the
// local block volume mounted at /state; the overrides exist so one gate can
// address a second path on the same volume.
const (
	// StateDirEnv overrides the directory holding spike databases.
	StateDirEnv = "SPIKE_STATE_DIR"
	// DefaultStateDir is the /state subdirectory used inside the container.
	DefaultStateDir = "/state/spike"

	// PhaseEnv selects a phase of a gate whose proof spans processes, so that a
	// container replacement can sit between two steps. The phase names are
	// declared next to the gates that use them.
	PhaseEnv = "SPIKE_PHASE"

	// DurableDBEnv names the database shared by the two phases of the
	// durability gate.
	DurableDBEnv = "SPIKE_DURABLE_DB"
	// DefaultDurableDB is the durability gate database on the state volume.
	DefaultDurableDB = "/state/spike/durability/vibeshell.db"

	// RunLabelEnv labels measurement receipts so a race-detector run and a
	// plain run do not overwrite each other's numbers.
	RunLabelEnv = "SPIKE_RUN_LABEL"

	// DefaultRunLabel is used when RunLabelEnv is unset.
	DefaultRunLabel = "plain"
)

// Pragma is one connection setting. It is a named type so that a caller
// overrides a default instead of appending a second value for the same pragma,
// which a driver would resolve silently and differently.
type Pragma struct{ Name, Value string }

// Pragmas are the connection settings the spike qualifies. They are applied per
// connection through the modernc.org/sqlite DSN: journal_mode is persistent in
// the database file, but synchronous, busy_timeout, and case_sensitive_like
// belong to the connection, and database/sql hands out several connections per
// pool.
//
// modernc.org/sqlite does not return a row for PRAGMA threadsafe or PRAGMA
// case_sensitive_like; it consumes both itself. case_sensitive_like is therefore
// verified by behaviour in the path lookup gate rather than by reading it back.
var Pragmas = []Pragma{
	{"journal_mode", "WAL"},
	{"synchronous", "FULL"},
	{"busy_timeout", "5000"},
	{"foreign_keys", "ON"},
	{"case_sensitive_like", "ON"},
}

// ReadablePragmas are the settings above that the driver reports back on query,
// so a test can assert them per connection. case_sensitive_like is excluded
// because modernc.org/sqlite returns no row for it.
var ReadablePragmas = map[string]any{
	"journal_mode": "wal",
	"synchronous":  int64(2),
	"busy_timeout": int64(5000),
	"foreign_keys": int64(1),
}

// Options modify how a spike database is opened.
type Options struct {
	// Pragmas replace the qualified defaults by name.
	Pragmas []Pragma
	// Immediate makes transactions reserve the write lock when they begin
	// instead of partway through, which is what turns a late SQLITE_BUSY into a
	// refusal before any work has been done.
	Immediate bool
}

// Effective returns the pragmas to apply for these options.
func (o Options) Effective() []Pragma {
	if len(o.Pragmas) == 0 {
		return Pragmas
	}
	effective := append([]Pragma(nil), Pragmas...)
	for _, override := range o.Pragmas {
		replaced := false
		for i := range effective {
			if effective[i].Name == override.Name {
				effective[i] = override
				replaced = true
				break
			}
		}
		if !replaced {
			effective = append(effective, override)
		}
	}
	return effective
}

// NoWait asks for SQLITE_BUSY to be reported immediately instead of retried
// until busy_timeout expires, which is how a test observes the driver's own
// answer rather than the wait policy.
func NoWait() Options {
	return Options{Pragmas: []Pragma{{"busy_timeout", "0"}}}
}

// PatientWait asks for a writer to wait longer than the qualified default, to
// measure what lock holding costs a caller that does wait.
func PatientWait(milliseconds int) Options {
	return Options{Pragmas: []Pragma{{"busy_timeout", fmt.Sprint(milliseconds)}}}
}

// DefaultPoolSize is the reader pool size. WAL allows one writer beside many
// readers, so a reader pool larger than one is the normal shape.
const DefaultPoolSize = 4

// dsn renders a modernc.org/sqlite connection string for path.
func dsn(path string, opts Options) string {
	query := make([]string, 0, len(opts.Effective())+1)
	for _, pragma := range opts.Effective() {
		query = append(query, "_pragma="+pragma.Name+"("+pragma.Value+")")
	}
	if opts.Immediate {
		query = append(query, "_txlock=immediate")
	}
	return "file:" + path + "?" + strings.Join(query, "&")
}

// Open returns a pool for path with the qualified settings applied to every new
// connection.
func Open(path string, opts Options) (*sql.DB, error) {
	db, err := sql.Open(DriverName, dsn(path, opts))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(DefaultPoolSize)
	db.SetMaxIdleConns(DefaultPoolSize)
	return db, nil
}

// OpenWriter returns a single-connection pool that reserves the write lock when
// a transaction begins. SQLite permits one writer at a time, so a writer pool of
// one connection turns lock contention into queueing that the application
// controls and can observe.
func OpenWriter(path string, opts Options) (*sql.DB, error) {
	opts.Immediate = true
	db, err := sql.Open(DriverName, dsn(path, opts))
	if err != nil {
		return nil, fmt.Errorf("open writer %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

// Schema is the subset of the PLAN 10.2 structure that the spike exercises:
// immutable content addressed by hash, versioned world nodes with per-directory
// membership revisions, append-only events, and the two lookup indexes.
//
// Nodes carry a deferred self-reference so a namespace root can be inserted in
// the same transaction as its children and still pass the foreign key check at
// commit.
const Schema = `
CREATE TABLE contents (
	hash       TEXT PRIMARY KEY,
	bytes      BLOB NOT NULL,
	size       INTEGER NOT NULL,
	media_type TEXT NOT NULL DEFAULT 'application/octet-stream'
) STRICT;

CREATE TABLE namespaces (
	id   TEXT PRIMARY KEY,
	name TEXT NOT NULL
) STRICT;

CREATE TABLE nodes (
	id           TEXT PRIMARY KEY,
	namespace_id TEXT NOT NULL REFERENCES namespaces(id),
	parent_id    TEXT NOT NULL REFERENCES nodes(id) DEFERRABLE INITIALLY DEFERRED,
	name         TEXT NOT NULL,
	kind         TEXT NOT NULL,
	revision     INTEGER NOT NULL,
	content_hash TEXT REFERENCES contents(hash),
	tombstone    INTEGER NOT NULL DEFAULT 0,
	UNIQUE (namespace_id, parent_id, name)
) STRICT;

CREATE INDEX nodes_membership ON nodes (namespace_id, parent_id, name);

CREATE TABLE directory_revisions (
	directory_id TEXT PRIMARY KEY REFERENCES nodes(id) DEFERRABLE INITIALLY DEFERRED,
	revision     INTEGER NOT NULL
) STRICT;

CREATE TABLE events (
	id          TEXT PRIMARY KEY,
	session_id  TEXT NOT NULL,
	sequence    INTEGER NOT NULL,
	occurred_at TEXT NOT NULL,
	kind        TEXT NOT NULL,
	payload     BLOB,
	UNIQUE (session_id, sequence)
) STRICT;

CREATE INDEX events_user_time ON events (session_id, occurred_at);

-- The same command legitimately appears in many events, so neither command
-- column can be unique: the indexes exist to find events by command, not to
-- constrain it. Only the (event, command) pair is unique, which keeps one
-- accepted command from being indexed twice.
CREATE TABLE command_index (
	event_id           TEXT NOT NULL REFERENCES events(id),
	command            TEXT NOT NULL,
	normalized_command TEXT NOT NULL,
	cwd                TEXT NOT NULL,
	exit_status        INTEGER,
	UNIQUE (event_id, command)
) STRICT;

CREATE INDEX command_index_exact ON command_index (command);
CREATE INDEX command_index_normalized ON command_index (normalized_command, cwd);

-- path_key is path with a trailing separator. The key of every descendant of p
-- starts with p + "/", so a subtree is a contiguous key range and a prefix query
-- needs no LIKE, no case folding, and no wildcard escaping.
--
-- Many events touch the same path, so the path itself is not unique; only the
-- (event, path) pair is, which keeps one event from listing a path twice.
CREATE TABLE path_index (
	id         INTEGER PRIMARY KEY,
	event_id   TEXT NOT NULL REFERENCES events(id),
	path       TEXT NOT NULL,
	path_key   TEXT NOT NULL
) STRICT;

CREATE UNIQUE INDEX path_index_event_path ON path_index (event_id, path);
CREATE INDEX path_index_path ON path_index (path);
CREATE INDEX path_index_key ON path_index (path_key);

CREATE TABLE search_docs (
	event_id TEXT PRIMARY KEY REFERENCES events(id),
	command  TEXT NOT NULL,
	path     TEXT NOT NULL
) STRICT;
`

// SchemaSearch is the full-text projection of PLAN 10.2. It is separate from
// Schema because FTS5 is the one part of the plan that may be unavailable: a
// gate that has to report its absence must not stop the other gates from
// running.
//
// transcript_fts is an external-content table over search_docs, which keeps the
// events authoritative and the index derived. The triggers maintain it, and
// 'rebuild' remains available to repair it.
const SchemaSearch = `
CREATE VIRTUAL TABLE transcript_fts USING fts5(
	command,
	path,
	content = 'search_docs',
	content_rowid = 'rowid',
	tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TRIGGER search_docs_ai AFTER INSERT ON search_docs BEGIN
	INSERT INTO transcript_fts (rowid, command, path)
	VALUES (new.rowid, new.command, new.path);
END;

CREATE TRIGGER search_docs_ad AFTER DELETE ON search_docs BEGIN
	INSERT INTO transcript_fts (transcript_fts, rowid, command, path)
	VALUES ('delete', old.rowid, old.command, old.path);
END;

CREATE TRIGGER search_docs_au AFTER UPDATE ON search_docs BEGIN
	INSERT INTO transcript_fts (transcript_fts, rowid, command, path)
	VALUES ('delete', old.rowid, old.command, old.path);
	INSERT INTO transcript_fts (rowid, command, path)
	VALUES (new.rowid, new.command, new.path);
END;
`

// SearchPragmas lists the FTS5 extension state recorded by the search gate.
var SearchPragmas = []string{
	"ENABLE_FTS5", "ENABLE_FTS4", "ENABLE_FTS3", "ENABLE_JSON1", "ENABLE_MATH_FUNCTIONS",
}

// ContentHash is the key content is addressed by, matching the content hash
// column of PLAN 10.2.
func ContentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// StateDirectory returns the directory holding spike databases. Every spike
// database lives here so that the durability and backup claims describe local
// block storage rather than the bind-mounted checkout.
func StateDirectory() (string, error) {
	dir := os.Getenv(StateDirEnv)
	if dir == "" {
		dir = DefaultStateDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create spike state directory %s: %w", dir, err)
	}
	return dir, nil
}

// NewDatabasePath returns a fresh database path under the state volume for the
// running test and removes its directory when the test ends.
func NewDatabasePath(t *testing.T, name string) string {
	t.Helper()
	dir := stateSubdir(t, sanitize(t.Name()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Logf("remove %s: %v", dir, err)
		}
	})
	return filepath.Join(dir, name+".db")
}

func stateSubdir(t *testing.T, name string) string {
	t.Helper()
	root, err := StateDirectory()
	if err != nil {
		t.Fatalf("state directory: %v", err)
	}
	return filepath.Join(root, name)
}

// DurableDatabasePath returns the database shared by the two process phases of
// the durability and backup gates. It is never cleaned up by the test, because
// the point is that it outlives the process.
func DurableDatabasePath(t *testing.T, envName, fallback string) string {
	t.Helper()
	path := os.Getenv(envName)
	if path == "" {
		path = fallback
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	return path
}

// Phase returns the requested phase of a gate and whether one was requested.
func Phase() (string, bool) {
	phase := os.Getenv(PhaseEnv)
	return phase, phase != ""
}

// RunLabel returns the label distinguishing measurement receipts from different
// runs of the same test. A race-detector run instruments every memory access and
// reports timings that are not representative, so its numbers are kept beside the
// plain run's instead of overwriting them.
func RunLabel() string {
	label := os.Getenv(RunLabelEnv)
	if label == "" {
		return DefaultRunLabel
	}
	return sanitize(label)
}

// Receipt writes lines to receipts/<name>.txt. The path is derived from this
// source file so a run from any working directory lands in the same place.
func Receipt(t *testing.T, name string, lines ...string) {
	t.Helper()
	dir, err := ModuleDir()
	if err != nil {
		t.Fatalf("locate spike module: %v", err)
	}
	receipts := filepath.Join(dir, ReceiptsDir)
	if err := os.MkdirAll(receipts, 0o755); err != nil {
		t.Fatalf("create %s: %v", receipts, err)
	}
	path := filepath.Join(receipts, name+".txt")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Logf("receipt %s", path)
}

// ReceiptsDir is the module-relative directory holding verification output.
const ReceiptsDir = "receipts"

// ModuleDir returns the directory holding this source file.
func ModuleDir() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot determine the spike module directory")
	}
	return filepath.Dir(file), nil
}

// MountInfo describes one entry of /proc/self/mountinfo.
type MountInfo struct {
	MountPoint string
	FSType     string
	Source     string
}

// MountOf returns the mount entry whose mount point is the longest prefix of
// path. PLAN 10.2 requires the database to sit on local block storage, so a
// receipt names the filesystem type instead of asserting it in prose.
func MountOf(path string) (MountInfo, error) {
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return MountInfo{}, fmt.Errorf("read mountinfo: %w", err)
	}
	target := filepath.Clean(path)
	best := MountInfo{}
	bestLen := -1
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		// The optional fields end at the " - " separator, which is followed by
		// the filesystem type and then its source and mount options.
		fields := strings.Fields(line)
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		if separator < 0 || len(fields) < separator+3 || len(fields) < 10 {
			continue
		}
		mountPoint := unescapeMount(fields[4])
		if mountPoint != target && !strings.HasPrefix(target, mountPoint+"/") {
			continue
		}
		if len(mountPoint) <= bestLen {
			continue
		}
		best = MountInfo{MountPoint: mountPoint, FSType: fields[separator+1], Source: fields[separator+2]}
		bestLen = len(mountPoint)
	}
	if bestLen < 0 {
		return MountInfo{}, fmt.Errorf("no mount entry covers %s", target)
	}
	return best, nil
}

// unescapeMount undoes the octal escaping mountinfo applies to path names.
func unescapeMount(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	var out strings.Builder
	for i := 0; i < len(field); i++ {
		if field[i] == '\\' && i+3 < len(field) {
			var value int
			if _, err := fmt.Sscanf(field[i+1:i+4], "%03o", &value); err == nil {
				out.WriteByte(byte(value))
				i += 3
				continue
			}
		}
		out.WriteByte(field[i])
	}
	return out.String()
}

func sanitize(name string) string {
	return strings.NewReplacer("/", "_", " ", "_", ":", "_").Replace(name)
}

// fileSHA256 is the checksum of a whole file, used to show that the bytes on the
// state volume are the same bytes before and after a container replacement.
func fileSHA256(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return ContentHash(raw), nil
}
