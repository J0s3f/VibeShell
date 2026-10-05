package admin

import (
	"context"

	"j0s.at/vibeshell/internal/domain"
)

// BackupExtra names one host file that a backup must carry beside the
// database. The persistent SSH host key is the motivating example: losing it
// changes the server identity every client sees even when the database
// survived (PLAN 10.3). Name is the stable identifier recorded in the
// manifest and used as the file name under the backup's extras directory;
// Path is the live file to copy.
type BackupExtra struct {
	Name string
	Path string
}

// BackupRequest describes one consistent snapshot. Extras supplement the
// service's configured default set when a caller needs to name them
// explicitly (a one-off migration capture, for example).
type BackupRequest struct {
	DestDir string
	Extras  []BackupExtra
}

// BackupFileReport records one sidecar file carried by a backup: its stable
// name, the SHA-256 of its bytes, its size, and its Unix permission bits so a
// restore can put a host key back with the mode it had.
type BackupFileReport struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
}

// BackupReport is the outcome of capturing a backup.
type BackupReport struct {
	DestDir            string
	DatabaseFile       string
	LogicalFingerprint string
	SchemaVersion      int
	DatabaseBytes      int64
	EventRows          int64
	CreatedAtUnixMilli int64
	ExtraFiles         []BackupFileReport
}

// RestoreRequest rebuilds a database from a backup directory into fresh
// paths. DestDatabase must not be the live database: recovery replaces the
// live file after the service has stopped, so a restore never races a running
// writer.
type RestoreRequest struct {
	SourceDir     string
	DestDatabase  string
	DestExtrasDir string
}

// RestoreReport is the outcome of a restore. LogicalFingerprint is the value
// recomputed from the rebuilt database and matched against the backup's
// manifest.
type RestoreReport struct {
	LogicalFingerprint string
	SchemaVersion      int
	DatabaseFile       string
	DatabaseBytes      int64
	ExtraFiles         []BackupFileReport
}

// IntegrityReport is the operator's trusted view of a database or backup: the
// SQLite integrity answer, foreign-key violations, schema version, the
// logical fingerprint, and per-table row counts. For a backup directory it
// also lists the verified sidecar files.
type IntegrityReport struct {
	IntegrityCheck       string
	ForeignKeyViolations int64
	SchemaVersion        int
	LogicalFingerprint   string
	RowCounts            map[string]int64
	ExtraFiles           []BackupFileReport
}

// BackupOps captures, verifies, restores, and checks the SQLite-backed
// persistent state. The SQLite adapter implements it; a restore is accepted
// only when the rebuilt database's logical fingerprint matches the recorded
// manifest, never on a file checksum alone.
type BackupOps interface {
	// CreateBackup writes a consistent snapshot plus the named sidecar files.
	CreateBackup(ctx context.Context, req BackupRequest) (BackupReport, error)
	// VerifyBackup reopens a backup and checks its integrity and fingerprint.
	VerifyBackup(ctx context.Context, dir string) (IntegrityReport, error)
	// RestoreBackup rebuilds a database from a verified backup into fresh paths.
	RestoreBackup(ctx context.Context, req RestoreRequest) (RestoreReport, error)
	// IntegrityCheck checks the live database.
	IntegrityCheck(ctx context.Context) (IntegrityReport, error)
}

// AppRollbacker restores a generated application's active pointer to a prior
// accepted version (PLAN 10.5). The implementation supplies the actor
// authorization the app registry requires.
type AppRollbacker interface {
	RollbackApp(ctx context.Context, app domain.AppID, target domain.AppVersionID, reason string) error
}
