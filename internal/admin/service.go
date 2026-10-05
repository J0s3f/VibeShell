package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// ConfigValidator parses and semantically validates a configuration
// snapshot. The config adapter's strict loader satisfies it; validation never
// makes an inference call (PLAN B08).
type ConfigValidator interface {
	Validate(raw []byte) error
}

// HealthStore is the route/account health view the admin surface reads and
// mutates. It is structurally identical to the router's health port, so the
// router's SQLite-backed store plugs in unchanged.
type HealthStore interface {
	Get(ctx context.Context, key domain.HealthKey) (rec domain.HealthRecord, ok bool, err error)
	Save(ctx context.Context, rec domain.HealthRecord) error
	List(ctx context.Context) ([]domain.HealthRecord, error)
}

// ProbeRunner executes due, bounded health probes. The routing adapter
// satisfies it.
type ProbeRunner interface {
	RunDueProbes(ctx context.Context) (int, error)
}

// PasswordUser is one password-file entry without its hash: the admin surface
// lists identities and enablement, never hash material.
type PasswordUser struct {
	Username string
	Enabled  bool
	Identity domain.UserID
}

// PasswordMaintenance delegates user/hash maintenance to the config adapter's
// password store. Passwords travel as byte slices read from a TTY or stdin,
// never as command-line arguments (PLAN 4.2).
type PasswordMaintenance interface {
	AddUser(username string, password []byte) (domain.UserID, error)
	SetPassword(username string, password []byte) error
	SetEnabled(username string, enabled bool) error
	RemoveUser(username string) error
	Users() []PasswordUser
}

// Exporter streams the research projections. D01's export adapter satisfies
// it; while that package is absent the dependency is simply not configured.
type Exporter interface {
	Export(ctx context.Context, q domain.ExportQuery) (domain.ExportResult, error)
}

// Deps are the admin service's collaborators. Every field except Clock,
// Random, and Backups is optional: an operation whose dependency is nil
// reports a typed unavailable error instead of pretending to run.
type Deps struct {
	Clock     ports.Clock
	Random    ports.Random
	Validator ConfigValidator
	Passwords PasswordMaintenance
	Sessions  SessionLister
	Exporter  Exporter
	Health    HealthStore
	Probes    ProbeRunner
	Backups   BackupOps
	Recovery  RecoveryOps
	Apps      AppRollbacker
	// BackupExtras are the host files every backup carries by default, such
	// as the persistent SSH host key.
	BackupExtras []BackupExtra
}

// Service is the operator-facing application service. It performs no I/O of
// its own: every effect goes through an injected dependency, so policy and
// delegation are testable with doubles.
type Service struct {
	deps Deps
}

// Compile-time proof that the admin surface carries the inbound port the
// container executable drives.
var _ ports.AdminOps = (*Service)(nil)

// New builds the admin service over its dependencies.
func New(deps Deps) *Service { return &Service{deps: deps} }

// errUnavailable names a missing dependency so an operator sees which
// subsystem is not configured rather than a nil dereference.
func errUnavailable(dependency string) error {
	return domain.NewUnavailableError(
		"admin_dependency_unavailable",
		fmt.Sprintf("admin operation requires the %s dependency", dependency),
		map[string]string{"dependency": dependency},
		nil,
	)
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// ValidateConfig parses and semantically checks a configuration snapshot
// without resolving secrets.
func (s *Service) ValidateConfig(ctx context.Context, raw []byte) error {
	if s.deps.Validator == nil {
		return errUnavailable("config validator")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.deps.Validator.Validate(raw)
}

// ---------------------------------------------------------------------------
// User and hash maintenance
// ---------------------------------------------------------------------------

// AddUser creates a secure-mode user with a stable identity.
func (s *Service) AddUser(ctx context.Context, username string, password []byte) (domain.UserID, error) {
	if s.deps.Passwords == nil {
		return domain.UserID{}, errUnavailable("password maintenance")
	}
	if err := ctx.Err(); err != nil {
		return domain.UserID{}, err
	}
	return s.deps.Passwords.AddUser(username, password)
}

// SetPassword replaces an existing user's stored hash.
func (s *Service) SetPassword(ctx context.Context, username string, password []byte) error {
	if s.deps.Passwords == nil {
		return errUnavailable("password maintenance")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.deps.Passwords.SetPassword(username, password)
}

// SetEnabled enables or disables login for a user.
func (s *Service) SetEnabled(ctx context.Context, username string, enabled bool) error {
	if s.deps.Passwords == nil {
		return errUnavailable("password maintenance")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.deps.Passwords.SetEnabled(username, enabled)
}

// RemoveUser deletes a user.
func (s *Service) RemoveUser(ctx context.Context, username string) error {
	if s.deps.Passwords == nil {
		return errUnavailable("password maintenance")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.deps.Passwords.RemoveUser(username)
}

// ListUsers returns the password-file entries without hash material.
func (s *Service) ListUsers(ctx context.Context) ([]PasswordUser, error) {
	if s.deps.Passwords == nil {
		return nil, errUnavailable("password maintenance")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.deps.Passwords.Users(), nil
}

// ---------------------------------------------------------------------------
// Sessions and export
// ---------------------------------------------------------------------------

// ListSessions lists sessions derived from the event log.
func (s *Service) ListSessions(ctx context.Context, limit int, cursor string) ([]SessionSummary, string, error) {
	if s.deps.Sessions == nil {
		return nil, "", errUnavailable("session lister")
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	return s.deps.Sessions.ListSessions(ctx, limit, cursor)
}

// Export delegates a research export to the configured exporter.
func (s *Service) Export(ctx context.Context, q domain.ExportQuery) (domain.ExportResult, error) {
	if s.deps.Exporter == nil {
		return domain.ExportResult{}, errUnavailable("research exporter")
	}
	if err := ctx.Err(); err != nil {
		return domain.ExportResult{}, err
	}
	return s.deps.Exporter.Export(ctx, q)
}

// ---------------------------------------------------------------------------
// Route and account health
// ---------------------------------------------------------------------------

// RouteStatus returns every persisted health record for operator display.
func (s *Service) RouteStatus(ctx context.Context) ([]domain.HealthRecord, error) {
	if s.deps.Health == nil {
		return nil, errUnavailable("health store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.deps.Health.List(ctx)
}

// TriggerProbe admits exactly one bounded probe for a due health key. It
// applies the same half-open admission the router uses, so a key can never be
// probed twice concurrently; the router's scheduler executes the probe. A key
// that is not due fails with a validation error rather than being forced.
func (s *Service) TriggerProbe(ctx context.Context, key domain.HealthKey) error {
	if s.deps.Health == nil {
		return errUnavailable("health store")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	rec, ok, err := s.deps.Health.Get(ctx, key)
	if err != nil {
		return err
	}
	if !ok {
		return domain.NewNotFoundError(
			domain.CodeRouteNotFound,
			"no health record for the requested key",
			map[string]string{"health_key": key.String()},
		)
	}
	admitted, ok := domain.AdmitProbe(rec, s.now())
	if !ok {
		return domain.NewValidationError(
			"probe_not_due",
			"the health key is not due for a probe",
			map[string]string{"health_key": key.String(), "state": string(rec.State)},
		)
	}
	return s.deps.Health.Save(ctx, admitted)
}

// RunDueProbes delegates a bounded batch of due probes to the routing
// adapter and reports how many ran.
func (s *Service) RunDueProbes(ctx context.Context) (int, error) {
	if s.deps.Probes == nil {
		return 0, errUnavailable("probe runner")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return s.deps.Probes.RunDueProbes(ctx)
}

// ---------------------------------------------------------------------------
// Backup, restore, integrity
// ---------------------------------------------------------------------------

// IntegrityCheck checks the live database.
func (s *Service) IntegrityCheck(ctx context.Context) (IntegrityReport, error) {
	if s.deps.Backups == nil {
		return IntegrityReport{}, errUnavailable("backup operations")
	}
	return s.deps.Backups.IntegrityCheck(ctx)
}

// CreateBackup captures a consistent snapshot. Explicit extras replace the
// service's configured default set; otherwise the defaults (for example, the
// SSH host key) are included.
func (s *Service) CreateBackup(ctx context.Context, req BackupRequest) (BackupReport, error) {
	if s.deps.Backups == nil {
		return BackupReport{}, errUnavailable("backup operations")
	}
	if req.Extras == nil {
		req.Extras = s.deps.BackupExtras
	}
	return s.deps.Backups.CreateBackup(ctx, req)
}

// RestoreBackup rebuilds a database from a verified backup into fresh paths.
func (s *Service) RestoreBackup(ctx context.Context, req RestoreRequest) (RestoreReport, error) {
	if s.deps.Backups == nil {
		return RestoreReport{}, errUnavailable("backup operations")
	}
	return s.deps.Backups.RestoreBackup(ctx, req)
}

// Backup is the inbound-port form of CreateBackup: it captures a snapshot
// with the configured extras and reports it as an export result, so the
// operations event audit can reference one identifier and checksum.
func (s *Service) Backup(ctx context.Context, dest string) (domain.ExportResult, error) {
	started := s.now()
	report, err := s.CreateBackup(ctx, BackupRequest{DestDir: dest})
	if err != nil {
		return domain.ExportResult{}, err
	}
	return domain.ExportResult{
		ExportID:    newBackupID(),
		EventCount:  report.EventRows,
		SizeBytes:   report.DatabaseBytes,
		Checksum:    report.LogicalFingerprint,
		StartedAt:   started,
		CompletedAt: report.CreatedAtUnixMilli,
		Format:      "sqlite-backup",
	}, nil
}

// ---------------------------------------------------------------------------
// Recovery and applications
// ---------------------------------------------------------------------------

// RecoverIncomplete marks interrupted sessions and turns at startup.
func (s *Service) RecoverIncomplete(ctx context.Context) (RecoveryReport, error) {
	if s.deps.Recovery == nil {
		return RecoveryReport{}, errUnavailable("recovery operations")
	}
	return s.deps.Recovery.RecoverIncomplete(ctx)
}

// RollbackApp restores an application's active pointer to a prior accepted
// version.
func (s *Service) RollbackApp(ctx context.Context, app domain.AppID, target domain.AppVersionID, reason string) error {
	if s.deps.Apps == nil {
		return errUnavailable("app rollback")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.deps.Apps.RollbackApp(ctx, app, target, reason)
}

func (s *Service) now() int64 {
	if s.deps.Clock != nil {
		return s.deps.Clock.NowUnixMilli()
	}
	return time.Now().UnixMilli()
}

// newBackupID mints an opaque identifier for one backup. It carries no secret
// and is recorded in research events rather than a path.
func newBackupID() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("bkp_%d", time.Now().UnixNano())
	}
	return "bkp_" + hex.EncodeToString(raw[:])
}
