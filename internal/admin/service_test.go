package admin

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"j0s.at/vibeshell/internal/adapters/config"
	"j0s.at/vibeshell/internal/apps"
	"j0s.at/vibeshell/internal/domain"
)

// ---------------------------------------------------------------------------
// Doubles
// ---------------------------------------------------------------------------

type fakeValidator struct {
	raw []byte
	err error
}

func (f *fakeValidator) Validate(raw []byte) error {
	f.raw = raw
	return f.err
}

type fakePasswords struct {
	users []PasswordUser
	err   error
}

func (f *fakePasswords) AddUser(username string, _ []byte) (domain.UserID, error) {
	if f.err != nil {
		return domain.UserID{}, f.err
	}
	f.users = append(f.users, PasswordUser{Username: username, Enabled: true})
	return domain.UserID{}, nil
}

func (f *fakePasswords) SetPassword(string, []byte) error { return f.err }
func (f *fakePasswords) SetEnabled(string, bool) error    { return f.err }
func (f *fakePasswords) RemoveUser(string) error          { return f.err }
func (f *fakePasswords) Users() []PasswordUser            { return f.users }

type fakeHealth struct {
	records map[string]domain.HealthRecord
	lists   int
}

func (f *fakeHealth) Get(_ context.Context, key domain.HealthKey) (domain.HealthRecord, bool, error) {
	if f.records == nil {
		return domain.HealthRecord{}, false, nil
	}
	rec, ok := f.records[key.String()]
	return rec, ok, nil
}

func (f *fakeHealth) Save(_ context.Context, rec domain.HealthRecord) error {
	if f.records == nil {
		f.records = map[string]domain.HealthRecord{}
	}
	f.records[domain.HealthKey{RouteID: rec.RouteID, Scope: rec.Scope}.String()] = rec
	return nil
}

func (f *fakeHealth) List(context.Context) ([]domain.HealthRecord, error) {
	f.lists++
	out := make([]domain.HealthRecord, 0, len(f.records))
	for _, rec := range f.records {
		out = append(out, rec)
	}
	return out, nil
}

type fakeProbes struct {
	ran int
	err error
}

func (f *fakeProbes) RunDueProbes(context.Context) (int, error) { return f.ran, f.err }

type fakeExporter struct{ query domain.ExportQuery }

func (f *fakeExporter) Export(_ context.Context, q domain.ExportQuery) (domain.ExportResult, error) {
	f.query = q
	return domain.ExportResult{Format: q.Format, EventCount: 7}, nil
}

type fakeSessions struct {
	page   []SessionSummary
	cursor string
}

func (f *fakeSessions) ListSessions(context.Context, int, string) ([]SessionSummary, string, error) {
	return f.page, f.cursor, nil
}

type fakeRecovery struct {
	report RecoveryReport
	err    error
}

func (f *fakeRecovery) IncompleteSessions(context.Context) ([]SessionSummary, error) {
	return nil, f.err
}

func (f *fakeRecovery) RecoverIncomplete(context.Context) (RecoveryReport, error) {
	return f.report, f.err
}

type fakeBackups struct {
	created   BackupRequest
	report    BackupReport
	integrity IntegrityReport
	err       error
}

func (f *fakeBackups) CreateBackup(_ context.Context, req BackupRequest) (BackupReport, error) {
	f.created = req
	return f.report, f.err
}

func (f *fakeBackups) VerifyBackup(context.Context, string) (IntegrityReport, error) {
	return f.integrity, f.err
}

func (f *fakeBackups) RestoreBackup(context.Context, RestoreRequest) (RestoreReport, error) {
	return RestoreReport{}, f.err
}

func (f *fakeBackups) IntegrityCheck(context.Context) (IntegrityReport, error) {
	return f.integrity, f.err
}

type fakeApps struct {
	called bool
	reason string
	err    error
}

func (f *fakeApps) RollbackApp(_ context.Context, _ domain.AppID, _ domain.AppVersionID, reason string) error {
	f.called = true
	f.reason = reason
	return f.err
}

// ---------------------------------------------------------------------------
// Delegation tests
// ---------------------------------------------------------------------------

func TestValidateConfigDelegates(t *testing.T) {
	validator := &fakeValidator{err: errors.New("bad config")}
	service := New(Deps{Validator: validator})
	raw := []byte(`{"x":1}`)
	err := service.ValidateConfig(context.Background(), raw)
	if err == nil || err.Error() != "bad config" {
		t.Fatalf("ValidateConfig error = %v, want the validator error", err)
	}
	if string(validator.raw) != string(raw) {
		t.Errorf("validator saw %q, want %q", validator.raw, raw)
	}
}

func TestPasswordMaintenanceDelegates(t *testing.T) {
	passwords := &fakePasswords{}
	service := New(Deps{Passwords: passwords})
	ctx := context.Background()
	if _, err := service.AddUser(ctx, "alice", []byte("pw")); err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	if err := service.SetPassword(ctx, "alice", []byte("pw2")); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if err := service.SetEnabled(ctx, "alice", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	users, err := service.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 || users[0].Username != "alice" {
		t.Fatalf("ListUsers = %+v, want alice", users)
	}
	if err := service.RemoveUser(ctx, "alice"); err != nil {
		t.Fatalf("RemoveUser: %v", err)
	}
}

func TestTriggerProbeAdmitsOnlyDueProbe(t *testing.T) {
	key := domain.HealthKey{Scope: "route"}
	health := &fakeHealth{records: map[string]domain.HealthRecord{
		key.String(): {Scope: "route", State: domain.HealthCoolingDown},
	}}
	service := New(Deps{Health: health})
	ctx := context.Background()

	if err := service.TriggerProbe(ctx, key); err != nil {
		t.Fatalf("TriggerProbe on a due key: %v", err)
	}
	saved, ok, _ := health.Get(ctx, key)
	if !ok || saved.State != domain.HealthProbing {
		t.Fatalf("saved record = %+v, want state probing", saved)
	}
	// A key already probing is not due again: only one probe at a time.
	if err := service.TriggerProbe(ctx, key); !domain.IsValidationError(err) {
		t.Fatalf("second TriggerProbe = %v, want a validation error", err)
	}
	// A healthy key is not due either.
	healthyKey := domain.HealthKey{Scope: "provider"}
	health.records[healthyKey.String()] = domain.HealthRecord{Scope: "provider", State: domain.HealthHealthy}
	if err := service.TriggerProbe(ctx, healthyKey); !domain.IsValidationError(err) {
		t.Fatalf("TriggerProbe on a healthy key = %v, want a validation error", err)
	}
	// An unknown key is a not-found condition, not a silent success.
	if err := service.TriggerProbe(ctx, domain.HealthKey{Scope: "missing"}); !domain.IsNotFoundError(err) {
		t.Fatalf("TriggerProbe on an unknown key = %v, want a not-found error", err)
	}
}

func TestRouteStatusAndProbesDelegate(t *testing.T) {
	health := &fakeHealth{records: map[string]domain.HealthRecord{
		"route:r1": {Scope: "route", State: domain.HealthCoolingDown},
	}}
	probes := &fakeProbes{ran: 2}
	service := New(Deps{Health: health, Probes: probes})

	records, err := service.RouteStatus(context.Background())
	if err != nil {
		t.Fatalf("RouteStatus: %v", err)
	}
	if len(records) != 1 || health.lists != 1 {
		t.Fatalf("RouteStatus = %+v (lists=%d), want one record and one list", records, health.lists)
	}
	ran, err := service.RunDueProbes(context.Background())
	if err != nil || ran != 2 {
		t.Fatalf("RunDueProbes = %d, %v; want 2, nil", ran, err)
	}
}

func TestSessionAndExportDelegation(t *testing.T) {
	session := domain.SessionID{}
	sessions := &fakeSessions{page: []SessionSummary{{SessionID: session}}, cursor: "next"}
	exporter := &fakeExporter{}
	service := New(Deps{Sessions: sessions, Exporter: exporter})

	page, cursor, err := service.ListSessions(context.Background(), 10, "")
	if err != nil || len(page) != 1 || cursor != "next" {
		t.Fatalf("ListSessions = %+v, %q, %v", page, cursor, err)
	}
	result, err := service.Export(context.Background(), domain.ExportQuery{Format: "jsonl"})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if result.Format != "jsonl" || exporter.query.Format != "jsonl" {
		t.Fatalf("Export result = %+v, exporter query = %+v", result, exporter.query)
	}
}

func TestBackupAndRecoveryDelegation(t *testing.T) {
	backups := &fakeBackups{
		report:    BackupReport{LogicalFingerprint: "abc", EventRows: 5, DatabaseBytes: 100, CreatedAtUnixMilli: 42},
		integrity: IntegrityReport{IntegrityCheck: "ok"},
	}
	recovery := &fakeRecovery{report: RecoveryReport{MarkedSessions: 3}}
	service := New(Deps{
		Backups:      backups,
		Recovery:     recovery,
		BackupExtras: []BackupExtra{{Name: "ssh_host_key", Path: "/state/host_key"}},
	})
	ctx := context.Background()

	created, err := service.CreateBackup(ctx, BackupRequest{DestDir: "/state/backups/1"})
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if len(backups.created.Extras) != 1 || backups.created.Extras[0].Name != "ssh_host_key" {
		t.Errorf("default extras were not applied: %+v", backups.created.Extras)
	}
	if created.LogicalFingerprint != "abc" {
		t.Errorf("backup report = %+v", created)
	}

	// The port-form Backup reports an export result carrying the fingerprint.
	export, err := service.Backup(ctx, "/state/backups/2")
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if export.Format != "sqlite-backup" || export.Checksum != "abc" || export.EventCount != 5 {
		t.Errorf("port Backup result = %+v", export)
	}

	integrity, err := service.IntegrityCheck(ctx)
	if err != nil || integrity.IntegrityCheck != "ok" {
		t.Fatalf("IntegrityCheck = %+v, %v", integrity, err)
	}
	report, err := service.RecoverIncomplete(ctx)
	if err != nil || report.MarkedSessions != 3 {
		t.Fatalf("RecoverIncomplete = %+v, %v", report, err)
	}
}

func TestUnavailableDependencies(t *testing.T) {
	service := New(Deps{})
	ctx := context.Background()
	checks := []struct {
		name string
		err  error
	}{
		{"ValidateConfig", service.ValidateConfig(ctx, nil)},
		{"SetPassword", service.SetPassword(ctx, "x", nil)},
		{"RemoveUser", service.RemoveUser(ctx, "x")},
		{"TriggerProbe", service.TriggerProbe(ctx, domain.HealthKey{})},
		{"RollbackApp", service.RollbackApp(ctx, domain.AppID{}, domain.AppVersionID{}, "r")},
	}
	for _, check := range checks {
		if !domain.IsUnavailableError(check.err) {
			t.Errorf("%s = %v, want an unavailable error", check.name, check.err)
		}
	}
	if _, _, err := service.ListSessions(ctx, 1, ""); !domain.IsUnavailableError(err) {
		t.Errorf("ListSessions = %v, want unavailable", err)
	}
	if _, err := service.Export(ctx, domain.ExportQuery{}); !domain.IsUnavailableError(err) {
		t.Errorf("Export = %v, want unavailable", err)
	}
	if _, err := service.RouteStatus(ctx); !domain.IsUnavailableError(err) {
		t.Errorf("RouteStatus = %v, want unavailable", err)
	}
	if _, err := service.RunDueProbes(ctx); !domain.IsUnavailableError(err) {
		t.Errorf("RunDueProbes = %v, want unavailable", err)
	}
	if _, err := service.IntegrityCheck(ctx); !domain.IsUnavailableError(err) {
		t.Errorf("IntegrityCheck = %v, want unavailable", err)
	}
	if _, err := service.CreateBackup(ctx, BackupRequest{DestDir: "/tmp/x"}); !domain.IsUnavailableError(err) {
		t.Errorf("CreateBackup = %v, want unavailable", err)
	}
	if _, err := service.RecoverIncomplete(ctx); !domain.IsUnavailableError(err) {
		t.Errorf("RecoverIncomplete = %v, want unavailable", err)
	}
}

// ---------------------------------------------------------------------------
// Bindings against the real config and apps adapters
// ---------------------------------------------------------------------------

func TestRollbackRestoresPriorAppVersion(t *testing.T) {
	ctx := context.Background()
	clock := apps.NewFixedClock(1000)
	registry := apps.NewInMemoryRegistry(clock)
	appService := apps.NewService(registry, &apps.FakeSandbox{}, clock, &apps.SequenceRandom{}, nil)

	owner, err := domain.ParseUserID("usr_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err != nil {
		t.Fatalf("parse owner: %v", err)
	}
	manifest := domain.AppManifest{
		ABIVersion:   domain.AppABIVersion,
		CommandNames: []string{"moon-orchard"},
		Description:  "a generated application",
		StateSchema:  json.RawMessage(`{"type":"object"}`),
		Entrypoint:   "main",
		Version:      "1.0.0",
	}
	v1, err := appService.RegisterCandidate(ctx, apps.CandidateRequest{
		Manifest: manifest, Source: "function main(){}", Owner: owner,
		Scope: domain.ScopeUser, Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate v1: %v", err)
	}
	artifact, err := registry.GetArtifact(ctx, v1)
	if err != nil {
		t.Fatalf("GetArtifact v1: %v", err)
	}
	if _, err := appService.Activate(ctx, apps.ActivateRequest{
		AppID: artifact.AppID, Version: v1, Actor: owner, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("Activate v1: %v", err)
	}
	v2, err := appService.RegisterCandidate(ctx, apps.CandidateRequest{
		AppID: artifact.AppID, ParentVersion: &v1, Manifest: manifest,
		Source: "function main(){ return 2 }", Owner: owner,
		Scope: domain.ScopeUser, Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate v2: %v", err)
	}
	if _, err := appService.Activate(ctx, apps.ActivateRequest{
		AppID: artifact.AppID, Version: v2, Actor: owner, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("Activate v2: %v", err)
	}
	if current, _ := registry.Current(ctx, artifact.AppID); current != v2 {
		t.Fatalf("current = %s, want v2 %s", current, v2)
	}

	service := New(Deps{Apps: NewAppServiceBinding(registry, appService)})
	if err := service.RollbackApp(ctx, artifact.AppID, v1, "operator rollback"); err != nil {
		t.Fatalf("RollbackApp: %v", err)
	}
	current, err := registry.Current(ctx, artifact.AppID)
	if err != nil {
		t.Fatalf("Current after rollback: %v", err)
	}
	if current != v1 {
		t.Fatalf("current = %s after rollback, want the prior version %s", current, v1)
	}
	history := registry.History()
	if len(history) != 3 {
		t.Fatalf("activation history has %d records, want 3 (activate v1, activate v2, rollback)", len(history))
	}
	if last := history[len(history)-1]; last.Kind != apps.ActivationRollback || last.Reason != "operator rollback" {
		t.Errorf("last activation record = %+v, want a rollback with the operator reason", last)
	}
}

func TestPasswordStoreBindingDelegates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "passwords.json")
	store, err := config.CreatePasswordStore(path)
	if err != nil {
		t.Fatalf("CreatePasswordStore: %v", err)
	}
	// A cheap-but-valid cost profile keeps password hashing out of the
	// test's critical path; production uses config.DefaultParams.
	params := config.Params{Time: 1, MemoryKiB: 8 * 1024, Threads: 1, SaltLen: 8, KeyLen: 16}
	binding := NewPasswordStoreBinding(store, &apps.SequenceRandom{}, params)
	service := New(Deps{Passwords: binding})
	ctx := context.Background()

	if _, err := service.AddUser(ctx, "alice", []byte("correct horse")); err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	users, err := service.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 || users[0].Username != "alice" || !users[0].Enabled {
		t.Fatalf("ListUsers = %+v, want enabled alice", users)
	}
	if users[0].Identity.IsZero() {
		t.Error("new user has no stable identity")
	}
	if err := service.SetEnabled(ctx, "alice", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if err := service.SetPassword(ctx, "alice", []byte("new password")); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	users, _ = service.ListUsers(ctx)
	if len(users) != 1 || users[0].Enabled {
		t.Fatalf("ListUsers after disable = %+v, want disabled alice", users)
	}
	if err := service.RemoveUser(ctx, "alice"); err != nil {
		t.Fatalf("RemoveUser: %v", err)
	}
	users, _ = service.ListUsers(ctx)
	if len(users) != 0 {
		t.Fatalf("ListUsers after remove = %+v, want none", users)
	}
}
