package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"j0s.at/vibeshell/internal/admin"
	"j0s.at/vibeshell/internal/buildinfo"
	"j0s.at/vibeshell/internal/domain"
)

// Backup layout. A backup is a self-contained directory so an operator can
// move it as one unit and a restore needs only that directory:
//
//	<dest>/manifest.json   the format version, schema version, and the
//	                       logical fingerprint a restore must reproduce
//	<dest>/vibeshell.db    the VACUUM INTO snapshot, including all committed
//	                       content and versions
//	<dest>/extras/<name>   sidecar host files (the SSH host key), copied with
//	                       their hashes and permission bits in the manifest
const (
	backupFormatVersion = 1
	backupDatabaseName  = "vibeshell.db"
	backupManifestName  = "manifest.json"
	backupExtrasDirName = "extras"
)

// fingerprintTables are the authoritative tables summarized by the logical
// fingerprint, in a stable order. Derived FTS rows are included because they
// are rebuilt from the events, and a restore must carry the exact same
// searchable projection. Internal SQLite tables (sqlite_sequence) are not
// ported.
var fingerprintTables = []string{
	"schema_migrations",
	"namespaces",
	"contents",
	"nodes",
	"node_versions",
	"commits",
	"events",
	"command_index",
	"path_index",
	"transcript_fts",
}

// backupManifest is the on-disk record a restore trusts. The logical
// fingerprint, not a file checksum, decides whether a restore is accepted:
// VACUUM relayouts pages, so the file bytes legitimately differ while the
// logical content must not.
type backupManifest struct {
	FormatVersion      int                      `json:"format_version"`
	SchemaVersion      int                      `json:"schema_version"`
	CreatedAtUnixMilli int64                    `json:"created_at_unix_milli"`
	AppVersion         string                   `json:"app_version"`
	AppRevision        string                   `json:"app_revision"`
	LogicalFingerprint string                   `json:"logical_fingerprint"`
	EventRows          int64                    `json:"event_rows"`
	ExtraFiles         []admin.BackupFileReport `json:"extra_files,omitempty"`
}

// Maintenance implements the admin backup operations over one migrated
// database handle. It is the SQLite adapter behind admin.BackupOps.
type Maintenance struct {
	db *DB
}

// Compile-time proof that the adapter stays behind the admin port.
var _ admin.BackupOps = (*Maintenance)(nil)

// NewMaintenance builds the backup/maintenance adapter over db.
func NewMaintenance(db *DB) *Maintenance { return &Maintenance{db: db} }

// Fingerprint returns the order-independent logical fingerprint of the live
// database: for every authoritative table, the sorted per-row hashes are
// folded into one digest. Because rows are sorted before folding, the value is
// independent of physical row order (which VACUUM may change) while still
// covering every value, including the actual bytes of content BLOBs.
func (m *Maintenance) Fingerprint(ctx context.Context) (string, error) {
	if m == nil || m.db == nil {
		return "", domain.NewUnavailableError(domain.CodeDatabaseUnavailable, "backup maintenance has no database", nil, nil)
	}
	return fingerprintDB(ctx, m.db.sql)
}

// IntegrityCheck checks the live database.
func (m *Maintenance) IntegrityCheck(ctx context.Context) (admin.IntegrityReport, error) {
	if m == nil || m.db == nil {
		return admin.IntegrityReport{}, domain.NewUnavailableError(domain.CodeDatabaseUnavailable, "backup maintenance has no database", nil, nil)
	}
	return integrityReport(ctx, m.db.sql)
}

// CreateBackup writes a consistent snapshot and the named extras into
// DestDir. The snapshot uses VACUUM INTO, so it includes the WAL state (never
// a bare copy of the live .db file) and every referenced content BLOB.
func (m *Maintenance) CreateBackup(ctx context.Context, req admin.BackupRequest) (admin.BackupReport, error) {
	if m == nil || m.db == nil {
		return admin.BackupReport{}, domain.NewUnavailableError(domain.CodeDatabaseUnavailable, "backup maintenance has no database", nil, nil)
	}
	if err := ctx.Err(); err != nil {
		return admin.BackupReport{}, err
	}
	if strings.TrimSpace(req.DestDir) == "" {
		return admin.BackupReport{}, domain.NewValidationError(domain.CodeInvalidInput, "backup destination directory is empty", nil)
	}
	if err := os.MkdirAll(req.DestDir, 0o700); err != nil {
		return admin.BackupReport{}, fmt.Errorf("sqlite: create backup directory %q: %w", req.DestDir, err)
	}
	dbPath := filepath.Join(req.DestDir, backupDatabaseName)
	// VACUUM INTO refuses to overwrite an existing file, and a rerun must not
	// silently keep a stale snapshot beside a fresh manifest.
	if err := os.Remove(dbPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return admin.BackupReport{}, fmt.Errorf("sqlite: clear stale backup %q: %w", dbPath, err)
	}
	if err := m.db.Backup(ctx, dbPath); err != nil {
		return admin.BackupReport{}, err
	}

	report, err := integrityOfFile(ctx, dbPath)
	if err != nil {
		return admin.BackupReport{}, err
	}
	if report.IntegrityCheck != "ok" {
		return admin.BackupReport{}, domain.NewInternalError(
			domain.CodeInvariantViolation,
			"newly written backup failed its integrity check",
			fmt.Errorf("integrity_check: %s", report.IntegrityCheck),
		)
	}

	extraFiles, err := copyBackupExtras(req.Extras, filepath.Join(req.DestDir, backupExtrasDirName))
	if err != nil {
		return admin.BackupReport{}, err
	}

	manifest := backupManifest{
		FormatVersion:      backupFormatVersion,
		SchemaVersion:      report.SchemaVersion,
		CreatedAtUnixMilli: time.Now().UnixMilli(),
		AppVersion:         buildinfo.Current().Version,
		AppRevision:        buildinfo.Current().Revision,
		LogicalFingerprint: report.LogicalFingerprint,
		EventRows:          report.RowCounts["events"],
		ExtraFiles:         extraFiles,
	}
	if err := writeManifest(req.DestDir, manifest); err != nil {
		return admin.BackupReport{}, err
	}
	size, err := fileSize(dbPath)
	if err != nil {
		return admin.BackupReport{}, err
	}

	return admin.BackupReport{
		DestDir:            req.DestDir,
		DatabaseFile:       dbPath,
		LogicalFingerprint: report.LogicalFingerprint,
		SchemaVersion:      report.SchemaVersion,
		DatabaseBytes:      size,
		EventRows:          report.RowCounts["events"],
		CreatedAtUnixMilli: manifest.CreatedAtUnixMilli,
		ExtraFiles:         extraFiles,
	}, nil
}

// VerifyBackup reopens a backup directory, checks SQLite integrity, and
// requires the logical fingerprint to match the manifest. A tampered backup
// (a changed value in any table) fails here even when SQLite still reports a
// structurally intact file, because the fingerprint covers row content.
func (m *Maintenance) VerifyBackup(ctx context.Context, dir string) (admin.IntegrityReport, error) {
	if strings.TrimSpace(dir) == "" {
		return admin.IntegrityReport{}, domain.NewValidationError(domain.CodeInvalidInput, "backup directory is empty", nil)
	}
	manifest, err := readManifest(dir)
	if err != nil {
		return admin.IntegrityReport{}, err
	}
	if manifest.FormatVersion != backupFormatVersion {
		return admin.IntegrityReport{}, domain.NewValidationError(
			"backup_format_unsupported",
			fmt.Sprintf("backup format version %d is not supported (this build reads %d)", manifest.FormatVersion, backupFormatVersion),
			nil,
		)
	}
	extraFiles, err := verifyBackupExtras(filepath.Join(dir, backupExtrasDirName), manifest.ExtraFiles)
	if err != nil {
		return admin.IntegrityReport{}, err
	}
	report, err := integrityOfFile(ctx, filepath.Join(dir, backupDatabaseName))
	if err != nil {
		return admin.IntegrityReport{}, err
	}
	report.ExtraFiles = extraFiles
	if report.IntegrityCheck != "ok" {
		return report, domain.NewInternalError(
			domain.CodeInvariantViolation,
			"backup failed its integrity check",
			fmt.Errorf("integrity_check: %s", report.IntegrityCheck),
		)
	}
	if report.SchemaVersion != manifest.SchemaVersion {
		return report, domain.NewValidationError(
			"backup_schema_mismatch",
			"backup schema version does not match its manifest",
			map[string]string{
				"manifest": fmt.Sprintf("%d", manifest.SchemaVersion),
				"actual":   fmt.Sprintf("%d", report.SchemaVersion),
			},
		)
	}
	if report.LogicalFingerprint != manifest.LogicalFingerprint {
		return report, fingerprintMismatchError(manifest.LogicalFingerprint, report.LogicalFingerprint)
	}
	return report, nil
}

// RestoreBackup rebuilds a database from a verified backup into fresh paths.
// The rebuilt file is produced by VACUUM INTO (fresh page layout), then its
// logical fingerprint is recomputed and must match the manifest before the
// restore is reported as successful. A mismatch deletes the rebuilt database
// so a partial restore is never left behind.
func (m *Maintenance) RestoreBackup(ctx context.Context, req admin.RestoreRequest) (admin.RestoreReport, error) {
	if err := ctx.Err(); err != nil {
		return admin.RestoreReport{}, err
	}
	if strings.TrimSpace(req.SourceDir) == "" || strings.TrimSpace(req.DestDatabase) == "" {
		return admin.RestoreReport{}, domain.NewValidationError(domain.CodeInvalidInput, "restore source and destination database are required", nil)
	}
	manifest, err := readManifest(req.SourceDir)
	if err != nil {
		return admin.RestoreReport{}, err
	}
	// Verify before touching the destination: an unverified backup must not
	// produce a database an operator might mistake for good.
	if _, err := m.VerifyBackup(ctx, req.SourceDir); err != nil {
		return admin.RestoreReport{}, err
	}
	source := filepath.Join(req.SourceDir, backupDatabaseName)
	if err := os.Remove(req.DestDatabase); err != nil && !errors.Is(err, os.ErrNotExist) {
		return admin.RestoreReport{}, fmt.Errorf("sqlite: clear restore destination %q: %w", req.DestDatabase, err)
	}
	if dir := filepath.Dir(req.DestDatabase); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return admin.RestoreReport{}, fmt.Errorf("sqlite: create restore directory %q: %w", dir, err)
		}
	}
	sourceDB, err := openSQLite(source)
	if err != nil {
		return admin.RestoreReport{}, err
	}
	_, vacuumErr := sourceDB.ExecContext(ctx, `VACUUM INTO ?`, req.DestDatabase)
	closeErr := sourceDB.Close()
	if vacuumErr != nil {
		return admin.RestoreReport{}, fmt.Errorf("sqlite: rebuild %q from %q: %w", req.DestDatabase, source, vacuumErr)
	}
	if closeErr != nil {
		return admin.RestoreReport{}, fmt.Errorf("sqlite: close backup source %q: %w", source, closeErr)
	}

	report, err := integrityOfFile(ctx, req.DestDatabase)
	if err != nil {
		_ = os.Remove(req.DestDatabase)
		return admin.RestoreReport{}, err
	}
	if report.LogicalFingerprint != manifest.LogicalFingerprint {
		_ = os.Remove(req.DestDatabase)
		return admin.RestoreReport{}, fingerprintMismatchError(manifest.LogicalFingerprint, report.LogicalFingerprint)
	}

	var restoredExtras []admin.BackupFileReport
	if len(manifest.ExtraFiles) > 0 && strings.TrimSpace(req.DestExtrasDir) != "" {
		if err := os.MkdirAll(req.DestExtrasDir, 0o700); err != nil {
			return admin.RestoreReport{}, fmt.Errorf("sqlite: create extras directory %q: %w", req.DestExtrasDir, err)
		}
		restoredExtras, err = restoreBackupExtras(filepath.Join(req.SourceDir, backupExtrasDirName), req.DestExtrasDir, manifest.ExtraFiles)
		if err != nil {
			_ = os.Remove(req.DestDatabase)
			return admin.RestoreReport{}, err
		}
	}
	size, err := fileSize(req.DestDatabase)
	if err != nil {
		return admin.RestoreReport{}, err
	}
	return admin.RestoreReport{
		LogicalFingerprint: report.LogicalFingerprint,
		SchemaVersion:      report.SchemaVersion,
		DatabaseFile:       req.DestDatabase,
		DatabaseBytes:      size,
		ExtraFiles:         restoredExtras,
	}, nil
}

func fingerprintMismatchError(want, got string) error {
	return domain.NewValidationError(
		"backup_fingerprint_mismatch",
		"restored content does not match the backup's recorded logical fingerprint",
		map[string]string{"expected": want, "actual": got},
	)
}

// ---------------------------------------------------------------------------
// Fingerprint
// ---------------------------------------------------------------------------

// fingerprintDB hashes every authoritative table row-by-row and folds the
// sorted per-row hashes into one digest. Sorting makes the result independent
// of physical row order.
func fingerprintDB(ctx context.Context, db *sql.DB) (string, error) {
	total := sha256.New()
	for _, table := range fingerprintTables {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		// The table names come from this package's constant list, never input.
		rows, err := db.QueryContext(ctx, "SELECT * FROM "+table)
		if err != nil {
			return "", fmt.Errorf("sqlite: fingerprint %s: %w", table, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			return "", fmt.Errorf("sqlite: fingerprint %s columns: %w", table, err)
		}
		var rowHashes []string
		for rows.Next() {
			values := make([]any, len(columns))
			targets := make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			if err := rows.Scan(targets...); err != nil {
				rows.Close()
				return "", fmt.Errorf("sqlite: fingerprint %s scan: %w", table, err)
			}
			rowHash := sha256.New()
			for i, column := range columns {
				writeCanonicalValue(rowHash, column, values[i])
			}
			rowHashes = append(rowHashes, hex.EncodeToString(rowHash.Sum(nil)))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return "", fmt.Errorf("sqlite: fingerprint %s rows: %w", table, err)
		}
		rows.Close()
		sort.Strings(rowHashes)
		fmt.Fprintf(total, "table %s rows %d\n", table, len(rowHashes))
		for _, hash := range rowHashes {
			total.Write([]byte(hash))
			total.Write([]byte{'\n'})
		}
	}
	return hex.EncodeToString(total.Sum(nil)), nil
}

// writeCanonicalValue encodes one scanned value unambiguously, including the
// column name and the value's type and length, so adjacent values cannot
// shift boundaries.
func writeCanonicalValue(w io.Writer, column string, value any) {
	fmt.Fprintf(w, "%s=", column)
	switch v := value.(type) {
	case nil:
		_, _ = io.WriteString(w, "null;")
	case int64:
		fmt.Fprintf(w, "i%d;", v)
	case float64:
		fmt.Fprintf(w, "f%v;", v)
	case bool:
		fmt.Fprintf(w, "b%t;", v)
	case string:
		fmt.Fprintf(w, "s%d:", len(v))
		_, _ = io.WriteString(w, v)
		_, _ = io.WriteString(w, ";")
	case []byte:
		fmt.Fprintf(w, "x%d:", len(v))
		_, _ = w.Write(v)
		_, _ = io.WriteString(w, ";")
	case time.Time:
		fmt.Fprintf(w, "t%s;", v.UTC().Format(time.RFC3339Nano))
	default:
		fmt.Fprintf(w, "?%T:%v;", value, value)
	}
}

// ---------------------------------------------------------------------------
// Integrity
// ---------------------------------------------------------------------------

// integrityOfFile opens path read-only and reports its integrity.
func integrityOfFile(ctx context.Context, path string) (admin.IntegrityReport, error) {
	db, err := openReadOnly(path)
	if err != nil {
		return admin.IntegrityReport{}, err
	}
	defer db.Close()
	return integrityReport(ctx, db)
}

// integrityReport runs the checks an operator runs before trusting a
// database: SQLite integrity, foreign-key violations, schema version, the
// logical fingerprint, and per-table row counts.
func integrityReport(ctx context.Context, db *sql.DB) (admin.IntegrityReport, error) {
	var check string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&check); err != nil {
		return admin.IntegrityReport{}, fmt.Errorf("sqlite: integrity_check: %w", err)
	}
	var violations int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		return admin.IntegrityReport{}, fmt.Errorf("sqlite: foreign_key_check: %w", err)
	}
	schemaVersion, err := readSchemaVersion(ctx, db)
	if err != nil {
		return admin.IntegrityReport{}, err
	}
	counts, err := tableRowCounts(ctx, db)
	if err != nil {
		return admin.IntegrityReport{}, err
	}
	fingerprint, err := fingerprintDB(ctx, db)
	if err != nil {
		return admin.IntegrityReport{}, err
	}
	return admin.IntegrityReport{
		IntegrityCheck:       check,
		ForeignKeyViolations: violations,
		SchemaVersion:        schemaVersion,
		LogicalFingerprint:   fingerprint,
		RowCounts:            counts,
	}, nil
}

func readSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var max sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&max); err != nil {
		// A fresh handle may not have the table yet.
		return 0, nil
	}
	if !max.Valid {
		return 0, nil
	}
	return int(max.Int64), nil
}

func tableRowCounts(ctx context.Context, db *sql.DB) (map[string]int64, error) {
	counts := make(map[string]int64, len(fingerprintTables))
	for _, table := range fingerprintTables {
		var n int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			return nil, fmt.Errorf("sqlite: count %s: %w", table, err)
		}
		counts[table] = n
	}
	return counts, nil
}

// ---------------------------------------------------------------------------
// Sidecar files (retained host keys)
// ---------------------------------------------------------------------------

// copyBackupExtras copies each named host file into extrasDir, returning the
// manifest records. An empty extras list is valid: a database-only backup.
func copyBackupExtras(extras []admin.BackupExtra, extrasDir string) ([]admin.BackupFileReport, error) {
	if len(extras) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(extrasDir, 0o700); err != nil {
		return nil, fmt.Errorf("sqlite: create extras directory %q: %w", extrasDir, err)
	}
	reports := make([]admin.BackupFileReport, 0, len(extras))
	for _, extra := range extras {
		if strings.TrimSpace(extra.Name) == "" || strings.TrimSpace(extra.Path) == "" {
			return nil, domain.NewValidationError(domain.CodeInvalidInput, "backup extra requires a name and a path", nil)
		}
		if strings.ContainsAny(extra.Name, `/\`) || extra.Name == "." || extra.Name == ".." {
			return nil, domain.NewValidationError(domain.CodeInvalidInput, "backup extra name must be a plain file name", map[string]string{"name": extra.Name})
		}
		info, err := os.Stat(extra.Path)
		if err != nil {
			return nil, fmt.Errorf("sqlite: stat backup extra %q: %w", extra.Path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, domain.NewValidationError(domain.CodeInvalidInput, "backup extra is not a regular file", map[string]string{"path": extra.Path})
		}
		source, err := os.Open(extra.Path)
		if err != nil {
			return nil, fmt.Errorf("sqlite: open backup extra %q: %w", extra.Path, err)
		}
		report, err := copyToExtras(source, filepath.Join(extrasDir, extra.Name), extra.Name, uint32(info.Mode().Perm()))
		source.Close()
		if err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	return reports, nil
}

func copyToExtras(source *os.File, destPath, name string, mode uint32) (admin.BackupFileReport, error) {
	dest, err := os.OpenFile(destPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return admin.BackupFileReport{}, fmt.Errorf("sqlite: create backup extra %q: %w", destPath, err)
	}
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(dest, hash), source)
	closeErr := dest.Close()
	if err != nil {
		return admin.BackupFileReport{}, fmt.Errorf("sqlite: copy backup extra %q: %w", name, err)
	}
	if closeErr != nil {
		return admin.BackupFileReport{}, fmt.Errorf("sqlite: close backup extra %q: %w", name, closeErr)
	}
	return admin.BackupFileReport{Name: name, SHA256: hex.EncodeToString(hash.Sum(nil)), Size: size, Mode: mode}, nil
}

// verifyBackupExtras checks every recorded extra against its hash and size.
func verifyBackupExtras(extrasDir string, expected []admin.BackupFileReport) ([]admin.BackupFileReport, error) {
	verified := make([]admin.BackupFileReport, 0, len(expected))
	for _, want := range expected {
		path := filepath.Join(extrasDir, want.Name)
		hash, size, err := hashFile(path)
		if err != nil {
			return nil, err
		}
		if hash != want.SHA256 || size != want.Size {
			return nil, domain.NewValidationError(
				"backup_extra_mismatch",
				"a backup sidecar file does not match its recorded hash",
				map[string]string{"name": want.Name, "expected": want.SHA256, "actual": hash},
			)
		}
		verified = append(verified, want)
	}
	return verified, nil
}

// restoreBackupExtras copies verified sidecars into destDir with their
// recorded modes and re-checks their hashes after writing.
func restoreBackupExtras(extrasDir, destDir string, records []admin.BackupFileReport) ([]admin.BackupFileReport, error) {
	restored := make([]admin.BackupFileReport, 0, len(records))
	for _, record := range records {
		source, err := os.Open(filepath.Join(extrasDir, record.Name))
		if err != nil {
			return nil, fmt.Errorf("sqlite: open sidecar %q for restore: %w", record.Name, err)
		}
		destPath := filepath.Join(destDir, record.Name)
		mode := os.FileMode(record.Mode)
		if mode.Perm() == 0 {
			mode = 0o600
		}
		dest, err := os.OpenFile(destPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
		if err != nil {
			source.Close()
			return nil, fmt.Errorf("sqlite: create restored sidecar %q: %w", destPath, err)
		}
		hash := sha256.New()
		size, err := io.Copy(io.MultiWriter(dest, hash), source)
		source.Close()
		closeErr := dest.Close()
		if err != nil {
			return nil, fmt.Errorf("sqlite: restore sidecar %q: %w", record.Name, err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("sqlite: close restored sidecar %q: %w", record.Name, closeErr)
		}
		got := hex.EncodeToString(hash.Sum(nil))
		if got != record.SHA256 || size != record.Size {
			return nil, domain.NewValidationError(
				"backup_extra_mismatch",
				"a restored sidecar file does not match its recorded hash",
				map[string]string{"name": record.Name},
			)
		}
		restored = append(restored, admin.BackupFileReport{Name: record.Name, SHA256: got, Size: size, Mode: record.Mode})
	}
	return restored, nil
}

func hashFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("sqlite: open %q: %w", path, err)
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, fmt.Errorf("sqlite: hash %q: %w", path, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("sqlite: stat %q: %w", path, err)
	}
	return info.Size(), nil
}

// ---------------------------------------------------------------------------
// Manifest and connection helpers
// ---------------------------------------------------------------------------

func readManifest(dir string) (backupManifest, error) {
	path := filepath.Join(dir, backupManifestName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return backupManifest{}, fmt.Errorf("sqlite: read backup manifest %q: %w", path, err)
	}
	var manifest backupManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return backupManifest{}, domain.NewValidationError("backup_manifest_invalid", "backup manifest is not valid JSON", map[string]string{"error": err.Error()})
	}
	return manifest, nil
}

func writeManifest(dir string, manifest backupManifest) error {
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("sqlite: encode backup manifest: %w", err)
	}
	path := filepath.Join(dir, backupManifestName)
	tmp, err := os.CreateTemp(dir, ".manifest-*.tmp")
	if err != nil {
		return fmt.Errorf("sqlite: create backup manifest: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("sqlite: write backup manifest: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sqlite: sync backup manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("sqlite: close backup manifest: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("sqlite: publish backup manifest: %w", err)
	}
	tmpName = ""
	return nil
}

// openReadOnly opens path for reading so verification never mutates the
// snapshot it inspects.
func openReadOnly(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q read-only: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: ping %q read-only: %w", path, err)
	}
	return db, nil
}

// openSQLite opens path read-write without running migrations, for reading a
// snapshot back out with VACUUM INTO.
func openSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsnFor(path))
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: ping %q: %w", path, err)
	}
	return db, nil
}
