package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
	_ "modernc.org/sqlite"
)

// Compile-time proof that this adapter stays behind its ports: adding a
// provider, transport, or storage engine must not change shell behavior.
var (
	_ ports.WorldStore   = (*DB)(nil)
	_ ports.ContentStore = (*DB)(nil)
)

// Options configures the SQLite adapter. Zero values are replaced by
// DefaultOptions.
type Options struct {
	// MaxContentBytes bounds one content BLOB accepted by Put.
	MaxContentBytes int64
	// BusyTimeoutMs bounds how long a writer waits on a locked page.
	BusyTimeoutMs int
}

// DefaultOptions returns the v1 operational defaults.
func DefaultOptions() Options {
	return Options{
		MaxContentBytes: 8 << 20, // 8 MiB per file content
		BusyTimeoutMs:   5000,
	}
}

// DB is the SQLite-backed world and content store. It holds exactly one
// open connection so concurrent Commit calls serialize in a bounded pool
// queue instead of colliding inside SQLite; transactions stay short and
// never wait on model I/O.
type DB struct {
	sql   *sql.DB
	path  string
	opts  Options
	guard *RecordingGuard
}

// Open creates the database at path (a filesystem path for durable state on
// the /state volume, or ":memory:" for throwaway tests), enforces WAL with
// synchronous=FULL on file-backed databases, runs pending migrations, and
// returns the ready handle.
func Open(path string, opts Options) (*DB, error) {
	if opts.MaxContentBytes <= 0 {
		opts.MaxContentBytes = DefaultOptions().MaxContentBytes
	}
	if opts.BusyTimeoutMs <= 0 {
		opts.BusyTimeoutMs = DefaultOptions().BusyTimeoutMs
	}
	dsn := dsnFor(path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q: %w", path, err)
	}
	// Bounded writer behavior: one connection serializes writers; readers
	// share it through short statements and transactions.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	db := &DB{sql: sqlDB, path: path, opts: opts, guard: NewRecordingGuard(nil, nil)}
	if err := db.applyPragmas(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	ctx := context.Background()
	if _, err := Migrate(ctx, sqlDB); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// SetRecordingGuard installs the durable-store health gate shared with the
// event writer (PLAN 10.3). The composition root supplies the logger and
// readiness reporter; without it the store keeps a private guard.
func (db *DB) SetRecordingGuard(guard *RecordingGuard) {
	if guard != nil {
		db.guard = guard
	}
}

// RecordingGuard returns the guard this store consults before semantic
// writes, so callers can inspect or share the recording-health condition.
func (db *DB) RecordingGuard() *RecordingGuard { return db.guard }

// SQL exposes the underlying handle for migration-aware tooling and tests.
func (db *DB) SQL() *sql.DB { return db.sql }

// Close checkpoints the WAL and closes the handle so a reopened database
// observes every acknowledged commit.
func (db *DB) Close() error {
	ctx := context.Background()
	// Best effort: a failed checkpoint must not hide the close result, and a
	// clean checkpoint keeps restart tests free of WAL replay surprises.
	_, _ = db.sql.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return db.sql.Close()
}

// Backup writes a consistent snapshot including contents and versions to
// destPath. Callers run it before Migrate when NeedsMigration reports
// pending work; never copy the live .db file while ignoring the WAL.
func (db *DB) Backup(ctx context.Context, destPath string) error {
	if destPath == "" {
		return domain.NewValidationError(domain.CodeInvalidInput, "backup destination is empty", nil)
	}
	escaped := strings.ReplaceAll(destPath, `'`, `''`)
	if _, err := db.sql.ExecContext(ctx, `VACUUM INTO '`+escaped+`'`); err != nil {
		return fmt.Errorf("sqlite: backup to %q: %w", destPath, err)
	}
	return nil
}

func dsnFor(path string) string {
	if path == ":memory:" || strings.HasPrefix(path, "file:") {
		return path
	}
	return "file:" + path
}

func (db *DB) applyPragmas() error {
	ctx := context.Background()
	if err := db.sql.PingContext(ctx); err != nil {
		return fmt.Errorf("sqlite: ping: %w", err)
	}
	exec := func(pragma string) error {
		if _, err := db.sql.ExecContext(ctx, pragma); err != nil {
			return fmt.Errorf("sqlite: %s: %w", pragma, err)
		}
		return nil
	}
	// Durability contract (PLAN 10.3): WAL keeps readers unblocked while one
	// writer commits; synchronous=FULL makes the commit durable before the
	// turn is acknowledged. :memory: handles keep their own journal mode.
	if db.path != ":memory:" && !strings.HasPrefix(db.path, "file::memory:") {
		var mode string
		if err := db.sql.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&mode); err != nil {
			return fmt.Errorf("sqlite: journal_mode=WAL: %w", err)
		}
		if !strings.EqualFold(mode, "wal") {
			return fmt.Errorf("sqlite: journal_mode is %q, want wal", mode)
		}
	}
	if err := exec(`PRAGMA synchronous=FULL`); err != nil {
		return err
	}
	var syncMode int
	if err := db.sql.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&syncMode); err != nil {
		return fmt.Errorf("sqlite: read synchronous: %w", err)
	}
	if syncMode != 2 {
		return fmt.Errorf("sqlite: synchronous is %d, want 2 (FULL)", syncMode)
	}
	if err := exec(fmt.Sprintf(`PRAGMA busy_timeout=%d`, db.opts.BusyTimeoutMs)); err != nil {
		return err
	}
	if err := exec(`PRAGMA foreign_keys=ON`); err != nil {
		return err
	}
	return nil
}
