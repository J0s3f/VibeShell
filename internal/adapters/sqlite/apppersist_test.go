package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"j0s.at/vibeshell/internal/apps"
	"j0s.at/vibeshell/internal/domain"
)

// durableDB opens a file-backed database that the test may close and reopen to
// observe what survives a restart.
func durableDB(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "durable.db")
	db, err := Open(path, DefaultOptions())
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() {
		if db != nil {
			_ = db.Close()
		}
	})
	return db, path
}

// restart closes db and opens the same file again, which is exactly what a
// process restart observes: no in-process state survives.
func restart(t *testing.T, db *DB, path string) *DB {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db = nil
	reopened, err := Open(path, DefaultOptions())
	if err != nil {
		t.Fatalf("reopen %s: %v", path, err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	return reopened
}

type appIDs struct {
	app     domain.AppID
	version domain.AppVersionID
	user    domain.UserID
	session domain.SessionID
}

func newAppIDs(t *testing.T) appIDs {
	t.Helper()
	return appIDs{
		app:     mustParseAppID(t, domain.PrefixApp),
		version: mustParseAppVersionID(t, domain.PrefixAppVer),
		user:    testUserID(t),
		session: mustParseSessionID(t),
	}
}

// candidate builds a validated artifact with a distinct source per version, the
// way the app service hands an artifact to the registry.
func candidate(t *testing.T, ids appIDs, scope domain.Scope, source string, passed bool) domain.AppArtifact {
	t.Helper()
	hash, err := contentIDFor([]byte(source))
	if err != nil {
		t.Fatalf("derive source hash: %v", err)
	}
	issues := []string(nil)
	if !passed {
		issues = []string{"staging smoke test failed: boom"}
	}
	return domain.AppArtifact{
		AppID:     ids.app,
		VersionID: ids.version,
		Manifest: domain.AppManifest{
			ABIVersion:   domain.AppABIVersion,
			CommandNames: []string{"ledger"},
			Description:  "durable generated application",
			StateSchema:  json.RawMessage(`{"type":"object"}`),
			Capabilities: []string{domain.CapabilityTime},
			Entrypoint:   "main",
			Version:      "1.0.0",
		},
		Source:     source,
		SourceHash: hash,
		Owner:      ids.user,
		Scope:      scope,
		CreatedAt:  1_700_000_000_000,
		Provenance: domain.Provenance{Source: "model", Actor: "rte_demo", PromptVersion: "p1"},
		Validation: domain.ValidationResult{
			Passed:           passed,
			Issues:           issues,
			TestResults:      []domain.TestResult{{Name: apps.SmokeTestName, Passed: passed, DurationMs: 3}},
			ValidatedAt:      1_700_000_000_500,
			ValidatorVersion: apps.ValidatorVersion,
		},
	}
}

func TestAppSourceHashMatchesTheServiceDerivation(t *testing.T) {
	// The adapter re-derives the source reference with the content store's
	// derivation; it must agree with the hash the app service records, or every
	// real candidate would be refused.
	source := "function main(event){ return {mode:'text'}; }"
	hash, err := contentIDFor([]byte(source))
	if err != nil {
		t.Fatalf("derive source hash: %v", err)
	}
	if want := apps.HashSource(source); hash != want {
		t.Fatalf("source hash derivation differs: adapter %s, service %s", hash, want)
	}
}

func TestAppRegistrySurvivesRestart(t *testing.T) {
	ctx := context.Background()
	db, path := durableDB(t)
	apps1, err := NewApps(db.SQL(), fixedTestClock{now: 1_700_000_100_000})
	if err != nil {
		t.Fatalf("NewApps: %v", err)
	}

	ids := newAppIDs(t)
	const activatedAt = 1_700_000_100_000
	artifact := candidate(t, ids, domain.ScopeShared, "function main(){ return 1; }", true)
	if _, err := apps1.RegisterCandidate(ctx, artifact); err != nil {
		t.Fatalf("RegisterCandidate: %v", err)
	}
	if err := apps1.Activate(ctx, ids.app, ids.version); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	// Activation stamps the artifact in place; nothing else about it changes.
	artifact.ActivatedAt = activatedAt
	userState := domain.AppState{UserState: json.RawMessage(`{"rows":[1,2]}`)}
	sharedState := domain.AppState{SharedState: json.RawMessage(`{"rows":[1,2,3]}`)}
	if err := apps1.SaveUserState(ctx, ids.user, ids.app, AppStateRecord{Version: ids.version, State: userState}); err != nil {
		t.Fatalf("SaveUserState: %v", err)
	}
	if err := apps1.SaveSharedState(ctx, ids.app, AppStateRecord{Version: ids.version, State: sharedState}); err != nil {
		t.Fatalf("SaveSharedState: %v", err)
	}
	sessionState := domain.AppState{SessionState: json.RawMessage(`{"cursor":3}`)}
	if err := apps1.PinSession(ctx, ids.session, ids.app, ids.version, sessionState); err != nil {
		t.Fatalf("PinSession: %v", err)
	}

	db = restart(t, db, path)
	apps2, err := NewApps(db.SQL(), fixedTestClock{now: 1_700_000_200_000})
	if err != nil {
		t.Fatalf("NewApps after restart: %v", err)
	}

	got, err := apps2.GetArtifact(ctx, ids.version)
	if err != nil {
		t.Fatalf("GetArtifact after restart: %v", err)
	}
	if !reflect.DeepEqual(got, artifact) {
		t.Errorf("artifact after restart:\n got %+v\nwant %+v", got, artifact)
	}
	current, err := apps2.Current(ctx, ids.app)
	if err != nil {
		t.Fatalf("Current after restart: %v", err)
	}
	if current != ids.version {
		t.Errorf("current version after restart = %s, want %s", current, ids.version)
	}
	userRecord, ok, err := apps2.UserState(ctx, ids.user, ids.app)
	if err != nil || !ok {
		t.Fatalf("UserState after restart: ok=%v err=%v", ok, err)
	}
	if userRecord.Version != ids.version || !reflect.DeepEqual(userRecord.State, userState) {
		t.Errorf("user state after restart = %+v, want version %s with %s", userRecord, ids.version, userState.UserState)
	}
	sharedRecord, ok, err := apps2.SharedState(ctx, ids.app)
	if err != nil || !ok {
		t.Fatalf("SharedState after restart: ok=%v err=%v", ok, err)
	}
	if sharedRecord.Version != ids.version || !reflect.DeepEqual(sharedRecord.State, sharedState) {
		t.Errorf("shared state after restart = %+v, want version %s with %s", sharedRecord, ids.version, sharedState.SharedState)
	}
	pin, ok, err := apps2.SessionPin(ctx, ids.session, ids.app)
	if err != nil || !ok {
		t.Fatalf("SessionPin after restart: ok=%v err=%v", ok, err)
	}
	if pin.Version != ids.version || !reflect.DeepEqual(pin.State, sessionState) {
		t.Errorf("session pin after restart = %+v, want version %s with %s", pin, ids.version, sessionState.SessionState)
	}

	// A session that never resolved the app has no pin and no state.
	other := newAppIDs(t)
	if _, ok, err := apps2.SessionPin(ctx, other.session, ids.app); err != nil || ok {
		t.Errorf("unpinned session: ok=%v err=%v, want ok=false", ok, err)
	}
	if _, ok, err := apps2.UserState(ctx, other.user, ids.app); err != nil || ok {
		t.Errorf("user without saved state: ok=%v err=%v, want ok=false", ok, err)
	}
}

func TestAppSessionStateAndReleaseAreDurable(t *testing.T) {
	ctx := context.Background()
	db, path := durableDB(t)
	store, err := NewApps(db.SQL(), fixedTestClock{now: 1})
	if err != nil {
		t.Fatalf("NewApps: %v", err)
	}
	ids := newAppIDs(t)
	artifact := candidate(t, ids, domain.ScopeUser, "function main(){ return 2; }", true)
	if _, err := store.RegisterCandidate(ctx, artifact); err != nil {
		t.Fatalf("RegisterCandidate: %v", err)
	}
	if err := store.Activate(ctx, ids.app, ids.version); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if err := store.PinSession(ctx, ids.session, ids.app, ids.version, domain.AppState{}); err != nil {
		t.Fatalf("PinSession: %v", err)
	}

	// State for an unresolved session is refused instead of becoming an orphan.
	unpinned := newAppIDs(t)
	err = store.SaveSessionState(ctx, unpinned.session, ids.app, ids.version, domain.AppState{})
	if domain.GetErrorCode(err) != "session_not_resolved" {
		t.Errorf("SaveSessionState without a pin = %v, want session_not_resolved", err)
	}

	updated := domain.AppState{SessionState: json.RawMessage(`{"cursor":9}`)}
	if err := store.SaveSessionState(ctx, ids.session, ids.app, ids.version, updated); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}
	db = restart(t, db, path)
	store, err = NewApps(db.SQL(), fixedTestClock{now: 2})
	if err != nil {
		t.Fatalf("NewApps after restart: %v", err)
	}
	pin, ok, err := store.SessionPin(ctx, ids.session, ids.app)
	if err != nil || !ok {
		t.Fatalf("SessionPin after restart: ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(pin.State, updated) {
		t.Errorf("session state after restart = %s, want %s", pin.State.SessionState, updated.SessionState)
	}
	if err := store.ReleaseSession(ctx, ids.session, ids.app); err != nil {
		t.Fatalf("ReleaseSession: %v", err)
	}
	if _, ok, err := store.SessionPin(ctx, ids.session, ids.app); err != nil || ok {
		t.Errorf("pin after release: ok=%v err=%v, want ok=false", ok, err)
	}
}

func TestAppActivationIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db, _ := durableDB(t)
	clock := fixedTestClock{now: 1_700_000_300_000}
	store, err := NewApps(db.SQL(), clock)
	if err != nil {
		t.Fatalf("NewApps: %v", err)
	}

	ids := newAppIDs(t)
	first := candidate(t, ids, domain.ScopeUser, "function main(){ return 'one'; }", true)
	if _, err := store.RegisterCandidate(ctx, first); err != nil {
		t.Fatalf("RegisterCandidate first: %v", err)
	}
	if err := store.Activate(ctx, ids.app, ids.version); err != nil {
		t.Fatalf("Activate first: %v", err)
	}
	second := newAppIDs(t)
	second.app = ids.app
	second.user = ids.user
	extension := candidate(t, second, domain.ScopeUser, "function main(){ return 'two'; }", true)
	extension.ParentVersion = &ids.version
	if _, err := store.RegisterCandidate(ctx, extension); err != nil {
		t.Fatalf("RegisterCandidate extension: %v", err)
	}
	if err := store.Activate(ctx, ids.app, second.version); err != nil {
		t.Fatalf("Activate second: %v", err)
	}
	activatedAt, err := store.Activations(ctx, ids.app)
	if err != nil {
		t.Fatal(err)
	}
	if len(activatedAt) != 2 {
		t.Fatalf("activation log has %d entries, want 2", len(activatedAt))
	}

	// Re-activating the current version changes nothing: same pointer, same
	// artifact stamp, no extra log entry.
	if err := store.Activate(ctx, ids.app, second.version); err != nil {
		t.Fatalf("repeat Activate: %v", err)
	}
	current, err := store.Current(ctx, ids.app)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if current != second.version {
		t.Errorf("current version = %s, want %s", current, second.version)
	}
	got, err := store.GetArtifact(ctx, second.version)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if got.ActivatedAt != clock.now {
		t.Errorf("activated_at = %d, want the first acceptance stamp %d", got.ActivatedAt, clock.now)
	}
	after, err := store.Activations(ctx, ids.app)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Errorf("activation log has %d entries after a repeat activation, want 2", len(after))
	}

	// An unvalidated candidate is never activated, even idempotently.
	third := newAppIDs(t)
	third.app = ids.app
	third.user = ids.user
	rejected := candidate(t, third, domain.ScopeUser, "function main(){ throw 1; }", false)
	if _, err := store.RegisterCandidate(ctx, rejected); err != nil {
		t.Fatalf("RegisterCandidate rejected: %v", err)
	}
	err = store.Activate(ctx, ids.app, third.version)
	if domain.GetErrorCode(err) != "candidate_not_validated" {
		t.Errorf("Activate unvalidated = %v, want candidate_not_validated", err)
	}
}

func TestAppRollbackRestoresAcceptedVersion(t *testing.T) {
	ctx := context.Background()
	db, path := durableDB(t)
	clock := fixedTestClock{now: 1_700_000_400_000}
	store, err := NewApps(db.SQL(), clock)
	if err != nil {
		t.Fatalf("NewApps: %v", err)
	}
	ids := newAppIDs(t)
	first := candidate(t, ids, domain.ScopeUser, "function main(){ return 'one'; }", true)
	if _, err := store.RegisterCandidate(ctx, first); err != nil {
		t.Fatalf("RegisterCandidate first: %v", err)
	}
	if err := store.Activate(ctx, ids.app, ids.version); err != nil {
		t.Fatalf("Activate first: %v", err)
	}
	second := newAppIDs(t)
	second.app, second.user = ids.app, ids.user
	extension := candidate(t, second, domain.ScopeUser, "function main(){ return 'two'; }", true)
	if _, err := store.RegisterCandidate(ctx, extension); err != nil {
		t.Fatalf("RegisterCandidate extension: %v", err)
	}
	if err := store.Activate(ctx, ids.app, second.version); err != nil {
		t.Fatalf("Activate second: %v", err)
	}

	const reason = "extension broke the counter"
	if err := store.Rollback(ctx, ids.app, ids.version, reason); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	db = restart(t, db, path)
	store, err = NewApps(db.SQL(), clock)
	if err != nil {
		t.Fatalf("NewApps after restart: %v", err)
	}
	current, err := store.Current(ctx, ids.app)
	if err != nil {
		t.Fatalf("Current after restart: %v", err)
	}
	if current != ids.version {
		t.Errorf("current version after rollback = %s, want %s", current, ids.version)
	}
	log, err := store.Activations(ctx, ids.app)
	if err != nil {
		t.Fatalf("Activations: %v", err)
	}
	if len(log) != 3 {
		t.Fatalf("activation log has %d entries, want 3", len(log))
	}
	rollback := log[2]
	if rollback.Kind != ActivationRollback || rollback.Reason != reason {
		t.Errorf("last log entry = %+v, want a rollback with the operator's reason", rollback)
	}
	if rollback.From == nil || *rollback.From != second.version || rollback.To != ids.version {
		t.Errorf("rollback entry = %+v, want from %s to %s", rollback, second.version, ids.version)
	}
	// The rolled-away version stays available for research and re-activation.
	if _, err := store.GetArtifact(ctx, second.version); err != nil {
		t.Errorf("rolled-away version must stay retrievable: %v", err)
	}

	// Rolling back to the current version is a no-op, not a new log entry.
	if err := store.Rollback(ctx, ids.app, ids.version, "already there"); err != nil {
		t.Fatalf("repeat Rollback: %v", err)
	}
	log, err = store.Activations(ctx, ids.app)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 3 {
		t.Errorf("activation log has %d entries after a no-op rollback, want 3", len(log))
	}
}

func TestAppRollbackRefusesUnacceptedAndUnknownVersions(t *testing.T) {
	ctx := context.Background()
	db, _ := durableDB(t)
	store, err := NewApps(db.SQL(), fixedTestClock{now: 5})
	if err != nil {
		t.Fatalf("NewApps: %v", err)
	}
	ids := newAppIDs(t)
	first := candidate(t, ids, domain.ScopeUser, "function main(){ return 'one'; }", true)
	if _, err := store.RegisterCandidate(ctx, first); err != nil {
		t.Fatalf("RegisterCandidate first: %v", err)
	}
	if err := store.Activate(ctx, ids.app, ids.version); err != nil {
		t.Fatalf("Activate first: %v", err)
	}
	never := newAppIDs(t)
	never.app, never.user = ids.app, ids.user
	candidateArtifact := candidate(t, never, domain.ScopeUser, "function main(){ return 'never'; }", true)
	if _, err := store.RegisterCandidate(ctx, candidateArtifact); err != nil {
		t.Fatalf("RegisterCandidate never: %v", err)
	}
	if code := domain.GetErrorCode(mustReject(t, store.Rollback(ctx, ids.app, never.version, "too soon"))); code != "not_an_accepted_version" {
		t.Errorf("rollback to an unactivated version = %v, want not_an_accepted_version", code)
	}
	unknown := mustParseAppVersionID(t, domain.PrefixAppVer)
	if code := domain.GetErrorCode(mustReject(t, store.Rollback(ctx, ids.app, unknown, "who"))); code != domain.CodeAppVersionNotFound {
		t.Errorf("rollback to an unknown version = %v, want %s", code, domain.CodeAppVersionNotFound)
	}
}

func TestAppRegistryRefusesUnstorableArtifacts(t *testing.T) {
	ctx := context.Background()
	db, _ := durableDB(t)
	store, err := NewApps(db.SQL(), fixedTestClock{now: 5})
	if err != nil {
		t.Fatalf("NewApps: %v", err)
	}
	ids := newAppIDs(t)
	register := func(a domain.AppArtifact) error {
		_, err := store.RegisterCandidate(ctx, a)
		return err
	}

	tampered := candidate(t, ids, domain.ScopeUser, "function main(){ return 1; }", true)
	tampered.Source = "function main(){ /* altered after validation */ }"
	if code := domain.GetErrorCode(mustReject(t, register(tampered))); code != domain.CodeInvalidAppManifest {
		t.Errorf("tampered source = %v, want %s", code, domain.CodeInvalidAppManifest)
	}
	if _, err := store.GetArtifact(ctx, ids.version); !domain.IsNotFoundError(err) {
		t.Errorf("a refused candidate must not be stored, got %v", err)
	}

	valid := candidate(t, ids, domain.ScopeUser, "function main(){ return 1; }", true)
	if err := register(valid); err != nil {
		t.Fatalf("RegisterCandidate: %v", err)
	}
	if code := domain.GetErrorCode(mustReject(t, register(valid))); code != domain.CodeDuplicateKey {
		t.Errorf("duplicate version ID = %v, want %s", code, domain.CodeDuplicateKey)
	}

	foreign := newAppIDs(t)
	foreignVersion := candidate(t, foreign, domain.ScopeUser, "function main(){ return 9; }", true)
	foreignVersion.ParentVersion = &ids.version
	if code := domain.GetErrorCode(mustReject(t, register(foreignVersion))); code != domain.CodeInvalidInput {
		t.Errorf("parent of a different app = %v, want %s", code, domain.CodeInvalidInput)
	}

	orphan := newAppIDs(t)
	unregisteredParent := candidate(t, orphan, domain.ScopeUser, "function main(){ return 8; }", true)
	unregisteredParent.AppID = ids.app
	unregisteredParent.ParentVersion = &foreign.version
	if code := domain.GetErrorCode(mustReject(t, register(unregisteredParent))); code != domain.CodeAppVersionNotFound {
		t.Errorf("unregistered parent = %v, want %s", code, domain.CodeAppVersionNotFound)
	}
}

func TestAppRegistryRequiresClockAndSchema(t *testing.T) {
	db, _ := durableDB(t)
	if _, err := NewApps(db.SQL(), nil); err == nil {
		t.Error("NewApps without a clock must fail at the composition boundary")
	}
	if _, err := NewApps(nil, fixedTestClock{now: 1}); err == nil {
		t.Error("NewApps without a database handle must fail")
	}
}

// TestMigrationV3IsRegisteredOnceWithItsTables guards the shared contract: B01
// owns numbering, so version 3 must be claimed exactly once, under one unique
// name, and it must create every table the adapters in this task write to.
func TestMigrationV3IsRegisteredOnceWithItsTables(t *testing.T) {
	registered := 0
	for _, m := range orderedMigrations() {
		if m.Version != 3 {
			continue
		}
		registered++
		if m.Name != "apps-routing-durable-state" {
			t.Errorf("migration 3 name = %q, want apps-routing-durable-state", m.Name)
		}
	}
	if registered != 1 {
		t.Fatalf("version 3 is registered %d times, want exactly 1", registered)
	}

	db, _ := durableDB(t)
	for _, table := range []string{
		"apps", "app_versions", "app_state", "app_session_pins", "app_activations",
		"route_health", "quota_groups", "account_usage", "spending_reservations",
	} {
		var name string
		if err := db.SQL().QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name); err != nil {
			t.Errorf("table %q is missing after migration: %v", table, err)
		}
	}
}

// mustReject asserts that an operation failed and returns the error so the test
// can assert on its domain code.
func mustReject(t *testing.T, err error) error {
	t.Helper()
	if err == nil {
		t.Fatal("expected the operation to fail, got nil")
	}
	return err
}

func mustParseAppID(t *testing.T, prefix string) domain.AppID {
	t.Helper()
	raw, err := newTestPrefixID(prefix)
	if err != nil {
		t.Fatal(err)
	}
	id, err := domain.ParseAppID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustParseAppVersionID(t *testing.T, prefix string) domain.AppVersionID {
	t.Helper()
	raw, err := newTestPrefixID(prefix)
	if err != nil {
		t.Fatal(err)
	}
	id, err := domain.ParseAppVersionID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustParseSessionID(t *testing.T) domain.SessionID {
	t.Helper()
	raw, err := newTestPrefixID(domain.PrefixSession)
	if err != nil {
		t.Fatal(err)
	}
	id, err := domain.ParseSessionID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
