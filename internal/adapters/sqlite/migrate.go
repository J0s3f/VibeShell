package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Migration is one numbered schema step. Versions are dense and ascending;
// B01 owns the numbering. B02 registers its steps through RegisterMigration
// instead of applying its own numbering.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

var (
	registryMu sync.Mutex
	registry   []Migration
)

// RegisterMigration adds a schema step. It panics on a duplicate or
// non-positive version so mis-numbered merges fail fast at startup.
func RegisterMigration(m Migration) {
	if m.Version <= 0 || m.Name == "" || m.SQL == "" {
		panic(fmt.Sprintf("sqlite: invalid migration %+v", m))
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	for _, existing := range registry {
		if existing.Version == m.Version {
			panic(fmt.Sprintf("sqlite: duplicate migration version %d (%q vs %q)", m.Version, existing.Name, existing.Name))
		}
	}
	registry = append(registry, m)
	sort.Slice(registry, func(i, j int) bool { return registry[i].Version < registry[j].Version })
}

// orderedMigrations returns a copy of the registry in version order.
func orderedMigrations() []Migration {
	registryMu.Lock()
	defer registryMu.Unlock()
	out := make([]Migration, len(registry))
	copy(out, registry)
	return out
}

// NeedsMigration reports whether unapplied migrations exist and the highest
// registered version. Production startup uses this to take a recoverable
// backup (see DB.Backup) before running Migrate.
func NeedsMigration(ctx context.Context, db *sql.DB) (pending bool, latest int, err error) {
	migrations := orderedMigrations()
	if len(migrations) == 0 {
		return false, 0, nil
	}
	if err := ctx.Err(); err != nil {
		return false, 0, err
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return false, 0, err
	}
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return false, 0, err
	}
	defer rows.Close()
	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return false, 0, err
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return false, 0, err
	}
	latest = migrations[len(migrations)-1].Version
	for _, m := range migrations {
		if !applied[m.Version] {
			return true, latest, nil
		}
	}
	return false, latest, nil
}

// Migrate applies every pending migration in one exclusive transaction per
// step and returns the current schema version. It is run once at startup
// while holding the process storage lock; writer serialization comes from
// the single-connection pool (see Open).
func Migrate(ctx context.Context, db *sql.DB) (int, error) {
	migrations := orderedMigrations()
	pending, latest, err := NeedsMigration(ctx, db)
	if err != nil {
		return 0, err
	}
	if !pending {
		return latest, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return 0, err
	}
	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return 0, err
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, m := range migrations {
		if applied[m.Version] {
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("sqlite: migration %d (%s): %w", m.Version, m.Name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, name, applied_at) VALUES(?,?,?)`,
			m.Version, m.Name, time.Now().UnixMilli()); err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("sqlite: record migration %d: %w", m.Version, err)
		}
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("sqlite: commit migration %d: %w", m.Version, err)
		}
	}
	return latest, nil
}

// SchemaVersion returns the highest applied migration version, or 0 for a
// fresh database handle that has not been migrated.
func SchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var max sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&max); err != nil {
		// Table may not exist yet on a fresh handle.
		return 0, nil
	}
	if !max.Valid {
		return 0, nil
	}
	return int(max.Int64), nil
}

// init registers the B01 world-storage schema. Later migrations (B02 events
// and beyond) take the next free version in their own files.
func init() {
	RegisterMigration(Migration{Version: 1, Name: "world-namespaces-nodes-contents", SQL: migrationV1})
}

const migrationV1 = `
CREATE TABLE namespaces (
	id TEXT PRIMARY KEY,
	owner TEXT NOT NULL DEFAULT '',
	scope TEXT NOT NULL,
	label TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_namespaces_scope_owner_label ON namespaces(scope, owner, label);

CREATE TABLE contents (
	hash TEXT PRIMARY KEY,
	size INTEGER NOT NULL,
	media_type TEXT NOT NULL,
	bytes BLOB NOT NULL
);

CREATE TABLE nodes (
	id TEXT PRIMARY KEY,
	namespace_id TEXT NOT NULL REFERENCES namespaces(id) ON DELETE RESTRICT,
	parent_id TEXT NOT NULL DEFAULT '',
	name TEXT NOT NULL,
	kind TEXT NOT NULL,
	revision INTEGER NOT NULL,
	mode INTEGER NOT NULL,
	uid INTEGER NOT NULL,
	gid INTEGER NOT NULL,
	mod_time INTEGER NOT NULL,
	access_time INTEGER NOT NULL,
	change_time INTEGER NOT NULL,
	symlink_target TEXT NOT NULL DEFAULT '',
	content_hash TEXT NOT NULL DEFAULT '',
	content_size INTEGER NOT NULL DEFAULT 0,
	content_media TEXT NOT NULL DEFAULT '',
	tombstone INTEGER NOT NULL DEFAULT 0,
	shadow_of TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL,
	UNIQUE(namespace_id, parent_id, name)
);
CREATE INDEX idx_nodes_ns_parent ON nodes(namespace_id, parent_id);

CREATE TABLE node_versions (
	node_id TEXT NOT NULL,
	revision INTEGER NOT NULL,
	namespace_id TEXT NOT NULL,
	parent_id TEXT NOT NULL DEFAULT '',
	name TEXT NOT NULL,
	kind TEXT NOT NULL,
	mode INTEGER NOT NULL,
	uid INTEGER NOT NULL,
	gid INTEGER NOT NULL,
	mod_time INTEGER NOT NULL,
	access_time INTEGER NOT NULL,
	change_time INTEGER NOT NULL,
	symlink_target TEXT NOT NULL DEFAULT '',
	content_hash TEXT NOT NULL DEFAULT '',
	content_size INTEGER NOT NULL DEFAULT 0,
	content_media TEXT NOT NULL DEFAULT '',
	tombstone INTEGER NOT NULL DEFAULT 0,
	shadow_of TEXT NOT NULL DEFAULT '',
	commit_seq INTEGER NOT NULL,
	PRIMARY KEY(node_id, revision)
);
CREATE INDEX idx_node_versions_ns_seq ON node_versions(namespace_id, commit_seq);

CREATE TABLE commits (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	turn_id TEXT NOT NULL,
	attempt_id TEXT NOT NULL,
	timestamp INTEGER NOT NULL,
	mutation_count INTEGER NOT NULL
);
`

// CurrentSchemaVersion is the highest migration version registered by the
// compiled-in adapters. Tests assert against it instead of a literal.
func CurrentSchemaVersion() int {
	migrations := orderedMigrations()
	if len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].Version
}
