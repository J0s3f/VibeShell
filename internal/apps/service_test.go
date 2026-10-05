package apps

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// newTestService builds a service over the contract
// doubles with one activated v1 application owned by
// the given user.
func newTestService(t *testing.T) (*Service, *InMemoryRegistry, *FakeSandbox, *FixedClock, domain.UserID, domain.AppID, domain.AppVersionID) {
	t.Helper()
	ctx := context.Background()
	clock := NewFixedClock(1000)
	registry := NewInMemoryRegistry(clock)
	sandbox := &FakeSandbox{}
	service := NewService(registry, sandbox, clock, &SequenceRandom{}, nil)
	owner := mustUserID(t, "usr_01ARZ3NDEKTSV4RRFFQ69G5FAV")

	version, err := service.RegisterCandidate(ctx, CandidateRequest{
		Manifest: testManifest("app", 1),
		Source:   testSource(),
		Owner:    owner,
		Scope:    domain.ScopeUser,
		Policy:   domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate v1: %v", err)
	}
	artifact, err := registry.GetArtifact(ctx, version)
	if err != nil {
		t.Fatalf("GetArtifact v1: %v", err)
	}
	if _, err := service.Activate(ctx, ActivateRequest{
		AppID: artifact.AppID, Version: version, Actor: owner, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("Activate v1: %v", err)
	}
	return service, registry, sandbox, clock, owner, artifact.AppID, version
}

func TestRegisterCandidateDoesNotActivate(t *testing.T) {
	ctx := context.Background()
	service, registry, _, _, owner, appID, current := newTestService(t)

	next, err := service.RegisterCandidate(ctx, CandidateRequest{
		AppID:         appID,
		ParentVersion: &current,
		Manifest:      testManifest("app", 1),
		Source:        testSource() + "// v2\n",
		Owner:         owner,
		Scope:         domain.ScopeUser,
		Policy:        domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate: %v", err)
	}
	if next == current {
		t.Fatalf("candidate reused the current version ID")
	}
	registered, err := registry.GetArtifact(ctx, next)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if registered.ActivatedAt != 0 {
		t.Errorf("candidate ActivatedAt = %d, want 0 (registered without activation)", registered.ActivatedAt)
	}
	if registered.ParentVersion == nil || *registered.ParentVersion != current {
		t.Errorf("candidate parent = %v, want %s", registered.ParentVersion, current)
	}
	still, err := registry.Current(ctx, appID)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if still != current {
		t.Errorf("current moved to %s on registration, want %s", still, current)
	}
}

func TestActivationAtomicity(t *testing.T) {
	ctx := context.Background()
	service, registry, _, _, owner, appID, current := newTestService(t)

	// A failed activation leaves the old version intact:
	// an unvalidated candidate cannot move the pointer.
	invalid, err := service.RegisterCandidate(ctx, CandidateRequest{
		AppID:         appID,
		ParentVersion: &current,
		Manifest:      testManifest("app", 1),
		Source:        testSource() + MarkerCompileError,
		Owner:         owner,
		Scope:         domain.ScopeUser,
		Policy:        domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate (invalid): %v", err)
	}
	if _, err := service.Activate(ctx, ActivateRequest{
		AppID: appID, Version: invalid, Actor: owner, Policy: domain.DefaultScopePolicy(),
	}); !domain.IsValidationError(err) {
		t.Fatalf("activating an unvalidated candidate = %v, want validation error", err)
	}
	still, err := registry.Current(ctx, appID)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if still != current {
		t.Errorf("current = %s after failed activation, want %s", still, current)
	}

	// Concurrent identical activations converge on the
	// single version with one activation record.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := service.Activate(ctx, ActivateRequest{
				AppID: appID, Version: current, Actor: owner, Policy: domain.DefaultScopePolicy(),
			}); err != nil {
				t.Errorf("concurrent idempotent activation: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := registry.History(); len(got) != 1 {
		t.Errorf("activation history has %d records, want 1", len(got))
	}

	// Concurrent activations of two valid versions end
	// with exactly one registered current version.
	validA, err := service.RegisterCandidate(ctx, CandidateRequest{
		AppID:         appID,
		ParentVersion: &current,
		Manifest:      testManifest("app", 1),
		Source:        testSource() + "// a\n",
		Owner:         owner,
		Scope:         domain.ScopeUser,
		Policy:        domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate a: %v", err)
	}
	validB, err := service.RegisterCandidate(ctx, CandidateRequest{
		AppID:         appID,
		ParentVersion: &current,
		Manifest:      testManifest("app", 1),
		Source:        testSource() + "// b\n",
		Owner:         owner,
		Scope:         domain.ScopeUser,
		Policy:        domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate b: %v", err)
	}
	wg = sync.WaitGroup{}
	for _, version := range []domain.AppVersionID{validA, validB} {
		wg.Add(1)
		go func(version domain.AppVersionID) {
			defer wg.Done()
			if _, err := service.Activate(ctx, ActivateRequest{
				AppID: appID, Version: version, Actor: owner, Policy: domain.DefaultScopePolicy(),
			}); err != nil {
				t.Errorf("concurrent activation: %v", err)
			}
		}(version)
	}
	wg.Wait()
	final, err := registry.Current(ctx, appID)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if final != validA && final != validB {
		t.Errorf("current = %s, want one of the two activated versions", final)
	}
	// The pointer never leaves the set of registered versions.
	if _, err := registry.GetArtifact(ctx, final); err != nil {
		t.Errorf("current version %s is not retrievable: %v", final, err)
	}
}

func TestRollbackRestoresPreviousVersion(t *testing.T) {
	ctx := context.Background()
	service, registry, _, _, owner, appID, v1 := newTestService(t)

	v2, err := service.RegisterCandidate(ctx, CandidateRequest{
		AppID:         appID,
		ParentVersion: &v1,
		Manifest:      testManifest("app", 2),
		Source:        testSource() + "// v2\n",
		Owner:         owner,
		Scope:         domain.ScopeUser,
		Policy:        domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate v2: %v", err)
	}
	if _, err := service.Activate(ctx, ActivateRequest{
		AppID: appID, Version: v2, Actor: owner, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("Activate v2: %v", err)
	}

	// Rollback restores the previous version and keeps
	// the rolled-away version available for research.
	rolledBack, err := service.Rollback(ctx, RollbackRequest{
		AppID: appID, Target: v1, Actor: owner,
		Policy: domain.DefaultScopePolicy(), Reason: "regression in v2",
	})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rolledBack != v1 {
		t.Errorf("rollback returned %s, want %s", rolledBack, v1)
	}
	current, err := registry.Current(ctx, appID)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if current != v1 {
		t.Errorf("current = %s after rollback, want %s", current, v1)
	}
	retained, err := registry.GetArtifact(ctx, v2)
	if err != nil {
		t.Fatalf("rolled-away version is not retrievable: %v", err)
	}
	if retained.Validation.Passed != true {
		t.Errorf("rolled-away version lost its validation record")
	}

	history := registry.History()
	if len(history) != 3 {
		t.Fatalf("history has %d records, want 3", len(history))
	}
	rollbackRecord := history[2]
	if rollbackRecord.Kind != ActivationRollback ||
		rollbackRecord.From == nil || *rollbackRecord.From != v2 ||
		rollbackRecord.To != v1 ||
		rollbackRecord.Reason != "regression in v2" {
		t.Errorf("rollback record = %+v, want rollback %s -> %s with reason", rollbackRecord, v2, v1)
	}

	// Rollback requires a reason and a previously
	// accepted version.
	if _, err := service.Rollback(ctx, RollbackRequest{
		AppID: appID, Target: v1, Actor: owner, Policy: domain.DefaultScopePolicy(),
	}); !domain.IsValidationError(err) {
		t.Errorf("rollback without a reason = %v, want validation error", err)
	}
}

func TestInvalidCandidateLeavesOldVersionIntact(t *testing.T) {
	ctx := context.Background()
	service, registry, _, _, owner, appID, current := newTestService(t)

	// A candidate that fails its staging smoke test is
	// retained for research with its validation issues,
	// but never activates and never touches the
	// current pointer.
	candidate, err := service.RegisterCandidate(ctx, CandidateRequest{
		AppID:         appID,
		ParentVersion: &current,
		Manifest:      testManifest("app", 1),
		Source:        testSource() + MarkerCompileError,
		Owner:         owner,
		Scope:         domain.ScopeUser,
		Policy:        domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate: %v", err)
	}
	artifact, err := registry.GetArtifact(ctx, candidate)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if artifact.Validation.Passed {
		t.Error("candidate with a compile error passed validation")
	}
	if len(artifact.Validation.Issues) == 0 {
		t.Error("failed candidate carries no validation issues")
	}
	if len(artifact.Validation.TestResults) != 1 || artifact.Validation.TestResults[0].Passed {
		t.Errorf("failed candidate smoke test = %+v, want one failing test", artifact.Validation.TestResults)
	}
	still, err := registry.Current(ctx, appID)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if still != current {
		t.Errorf("current = %s, want the old version %s", still, current)
	}

	// A structurally invalid candidate is rejected
	// outright and leaves no artifact behind.
	if _, err := service.RegisterCandidate(ctx, CandidateRequest{
		Manifest: domain.AppManifest{ABIVersion: 99},
		Source:   testSource(),
		Owner:    owner,
		Scope:    domain.ScopeUser,
		Policy:   domain.DefaultScopePolicy(),
	}); !domain.IsValidationError(err) {
		t.Errorf("registering a manifest-invalid candidate = %v, want validation error", err)
	}
}

func TestSessionVersionPinning(t *testing.T) {
	ctx := context.Background()
	service, registry, _, _, owner, appID, v1 := newTestService(t)
	existing := mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FAV")

	// An existing session pins the current version.
	resolution, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: existing, UserID: owner, AppID: appID, Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	if resolution.Version != v1 || resolution.Stale {
		t.Errorf("first resolution = %+v, want version %s", resolution, v1)
	}

	// A new version activates; the existing session
	// keeps its pinned version, new sessions get the
	// current one.
	v2, err := service.RegisterCandidate(ctx, CandidateRequest{
		AppID:         appID,
		ParentVersion: &v1,
		Manifest:      testManifest("app", 2),
		Source:        testSource() + "// v2\n",
		Owner:         owner,
		Scope:         domain.ScopeUser,
		Policy:        domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate v2: %v", err)
	}
	if _, err := service.Activate(ctx, ActivateRequest{
		AppID: appID, Version: v2, Actor: owner, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("Activate v2: %v", err)
	}

	pinned, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: existing, UserID: owner, AppID: appID, Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("ResolveSession after activation: %v", err)
	}
	if pinned.Version != v1 {
		t.Errorf("existing session resolved %s, want its pinned version %s", pinned.Version, v1)
	}
	if pinned.Current != v2 {
		t.Errorf("existing session current = %s, want %s", pinned.Current, v2)
	}

	fresh := mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FBW")
	newSession, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: fresh, UserID: owner, AppID: appID, Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("ResolveSession for a new session: %v", err)
	}
	if newSession.Version != v2 {
		t.Errorf("new session resolved %s, want the current version %s", newSession.Version, v2)
	}

	// After the old session is released, a later
	// session gets the current version too.
	if err := service.ReleaseSession(ctx, existing, appID); err != nil {
		t.Fatalf("ReleaseSession: %v", err)
	}
	if _, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: existing, UserID: owner, AppID: appID, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("ResolveSession after release: %v", err)
	}
	current, err := registry.Current(ctx, appID)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if current != v2 {
		t.Errorf("current = %s, want %s", current, v2)
	}
}

func TestAdditiveStateMigration(t *testing.T) {
	ctx := context.Background()
	service, _, _, _, owner, appID, v1 := newTestService(t)
	session := mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FAV")

	// The session runs v1 and saves state at schema 1.
	if _, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: session, UserID: owner, AppID: appID, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	saved := domain.AppState{UserState: userState(1, "important")}
	if err := service.SaveSessionState(ctx, SaveStateRequest{
		SessionID: session, UserID: owner, AppID: appID, State: saved,
	}); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	// v2 extends v1 with an additive migration that
	// preserves the saved data.
	v2, err := service.RegisterCandidate(ctx, CandidateRequest{
		AppID:         appID,
		ParentVersion: &v1,
		Manifest:      testManifest("app", 2),
		Source:        testSource() + "// v2\n",
		Owner:         owner,
		Scope:         domain.ScopeUser,
		Policy:        domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate v2: %v", err)
	}
	if err := service.RegisterMigration(ctx, StateMigration{
		FromVersion: v1,
		ToVersion:   v2,
		Description: "bump state schema to 2",
		Migrate: func(state domain.AppState) (domain.AppState, error) {
			var payload struct {
				Schema int64  `json:"schema"`
				Data   string `json:"data"`
			}
			if err := json.Unmarshal(state.UserState, &payload); err != nil {
				return domain.AppState{}, err
			}
			payload.Schema = 2
			encoded, err := json.Marshal(payload)
			if err != nil {
				return domain.AppState{}, err
			}
			return domain.AppState{UserState: encoded}, nil
		},
	}); err != nil {
		t.Fatalf("RegisterMigration: %v", err)
	}
	if _, err := service.Activate(ctx, ActivateRequest{
		AppID: appID, Version: v2, Actor: owner, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("Activate v2: %v", err)
	}

	// The existing session migrates to v2 and keeps
	// its data.
	migrated, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: session, UserID: owner, AppID: appID, Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("ResolveSession after migration: %v", err)
	}
	if migrated.Version != v2 || migrated.Stale {
		t.Fatalf("session resolved %+v, want a clean migration to %s", migrated, v2)
	}
	var payload struct {
		Schema int64  `json:"schema"`
		Data   string `json:"data"`
	}
	if err := json.Unmarshal(migrated.State.UserState, &payload); err != nil {
		t.Fatalf("migrated state: %v", err)
	}
	if payload.Schema != 2 || payload.Data != "important" {
		t.Errorf("migrated state = {schema:%d data:%q}, want {schema:2 data:%q}", payload.Schema, payload.Data, "important")
	}

	// A new session inherits the durable user state,
	// migrated to the current version.
	fresh := mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FBW")
	inherited, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: fresh, UserID: owner, AppID: appID, Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("ResolveSession for a new session: %v", err)
	}
	if inherited.Version != v2 {
		t.Errorf("new session resolved %s, want %s", inherited.Version, v2)
	}
	if err := json.Unmarshal(inherited.State.UserState, &payload); err != nil {
		t.Fatalf("inherited state: %v", err)
	}
	if payload.Schema != 2 || payload.Data != "important" {
		t.Errorf("inherited state = {schema:%d data:%q}, want migrated durable state", payload.Schema, payload.Data)
	}
}

func TestStateFallbackRetainsOldVersion(t *testing.T) {
	ctx := context.Background()
	service, _, _, _, owner, appID, v1 := newTestService(t)
	session := mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FAV")

	if _, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: session, UserID: owner, AppID: appID, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	saved := domain.AppState{UserState: userState(1, "important")}
	if err := service.SaveSessionState(ctx, SaveStateRequest{
		SessionID: session, UserID: owner, AppID: appID, State: saved,
	}); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	// v2 is a breaking rewrite: it only understands
	// its own schema and no migration exists.
	v2, err := service.RegisterCandidate(ctx, CandidateRequest{
		AppID:         appID,
		ParentVersion: &v1,
		Manifest:      testManifest("app", 2),
		Source:        testSource() + MarkerIncompatibleState,
		Owner:         owner,
		Scope:         domain.ScopeUser,
		Policy:        domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate v2: %v", err)
	}
	if _, err := service.Activate(ctx, ActivateRequest{
		AppID: appID, Version: v2, Actor: owner, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("Activate v2: %v", err)
	}

	// The existing session retains its pinned version
	// and its exact state; nothing is discarded.
	retained, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: session, UserID: owner, AppID: appID, Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("ResolveSession after breaking activation: %v", err)
	}
	if !retained.Stale || retained.Version != v1 {
		t.Fatalf("session resolved %+v, want a stale retention of %s", retained, v1)
	}
	var payload struct {
		Schema int64  `json:"schema"`
		Data   string `json:"data"`
	}
	if err := json.Unmarshal(retained.State.UserState, &payload); err != nil {
		t.Fatalf("retained state: %v", err)
	}
	if payload.Schema != 1 || payload.Data != "important" {
		t.Errorf("retained state = {schema:%d data:%q}, want the untouched {schema:1 data:%q}", payload.Schema, payload.Data, "important")
	}

	// A new session gets the current version with
	// fresh session state rather than state it cannot
	// interpret.
	fresh := mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FBW")
	freshState, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: fresh, UserID: owner, AppID: appID, Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("ResolveSession for a new session: %v", err)
	}
	if freshState.Version != v2 {
		t.Errorf("new session resolved %s, want the current version %s", freshState.Version, v2)
	}
	if len(freshState.State.UserState) != 0 {
		t.Errorf("new session inherited uninterpretable state %s, want fresh state", freshState.State.UserState)
	}

	// The durable record is retained: the old session
	// still resolves to its pinned version with its
	// state intact.
	again, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: session, UserID: owner, AppID: appID, Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("ResolveSession repeat: %v", err)
	}
	if !again.Stale || again.Version != v1 {
		t.Errorf("repeat resolution = %+v, want the retained version %s", again, v1)
	}
}

func TestStateFallbackWhenVerificationFails(t *testing.T) {
	ctx := context.Background()
	service, _, _, _, owner, appID, v1 := newTestService(t)
	session := mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FAV")

	if _, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: session, UserID: owner, AppID: appID, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	saved := domain.AppState{UserState: userState(1, "important")}
	if err := service.SaveSessionState(ctx, SaveStateRequest{
		SessionID: session, UserID: owner, AppID: appID, State: saved,
	}); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	// v3 is a breaking rewrite (schema 3) and the
	// only migration produces schema 2, which v3
	// cannot interpret: the verified-migration gate
	// must catch it and fall back safely.
	v3, err := service.RegisterCandidate(ctx, CandidateRequest{
		AppID:         appID,
		ParentVersion: &v1,
		Manifest:      testManifest("app", 3),
		Source:        testSource() + MarkerIncompatibleState,
		Owner:         owner,
		Scope:         domain.ScopeUser,
		Policy:        domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate v3: %v", err)
	}
	if err := service.RegisterMigration(ctx, StateMigration{
		FromVersion: v1,
		ToVersion:   v3,
		Description: "partial migration (buggy: stops at schema 2)",
		Migrate: func(state domain.AppState) (domain.AppState, error) {
			var payload struct {
				Schema int64  `json:"schema"`
				Data   string `json:"data"`
			}
			if err := json.Unmarshal(state.UserState, &payload); err != nil {
				return domain.AppState{}, err
			}
			payload.Schema = 2
			encoded, err := json.Marshal(payload)
			if err != nil {
				return domain.AppState{}, err
			}
			return domain.AppState{UserState: encoded}, nil
		},
	}); err != nil {
		t.Fatalf("RegisterMigration: %v", err)
	}
	if _, err := service.Activate(ctx, ActivateRequest{
		AppID: appID, Version: v3, Actor: owner, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("Activate v3: %v", err)
	}

	resolved, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: session, UserID: owner, AppID: appID, Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	if !resolved.Stale || resolved.Version != v1 {
		t.Fatalf("session resolved %+v, want a safe fallback to %s", resolved, v1)
	}
	var payload struct {
		Schema int64  `json:"schema"`
		Data   string `json:"data"`
	}
	if err := json.Unmarshal(resolved.State.UserState, &payload); err != nil {
		t.Fatalf("retained state: %v", err)
	}
	if payload.Schema != 1 || payload.Data != "important" {
		t.Errorf("retained state = {schema:%d data:%q}, want the untouched v1 state", payload.Schema, payload.Data)
	}
}

func TestConcurrentGenerationHasSingleWinner(t *testing.T) {
	ctx := context.Background()
	service, registry, _, _, owner, appID, base := newTestService(t)

	const generators = 8
	type outcome struct {
		version domain.AppVersionID
		err     error
	}
	outcomes := make([]outcome, generators)
	var wg sync.WaitGroup
	for i := 0; i < generators; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each generation registers its own
			// candidate and tries to activate it
			// from the version it observed.
			version, err := service.RegisterCandidate(ctx, CandidateRequest{
				AppID:         appID,
				ParentVersion: &base,
				Manifest:      testManifest("app", 1),
				Source:        fmt.Sprintf("%s// generation %d\n", testSource(), i),
				Owner:         owner,
				Scope:         domain.ScopeUser,
				Policy:        domain.DefaultScopePolicy(),
			})
			if err != nil {
				outcomes[i] = outcome{err: err}
				return
			}
			baseVersion := base
			activated, err := service.Activate(ctx, ActivateRequest{
				AppID: appID, Version: version, Actor: owner,
				Policy: domain.DefaultScopePolicy(), Base: &baseVersion,
			})
			outcomes[i] = outcome{version: activated, err: err}
		}(i)
	}
	wg.Wait()

	winners := 0
	var winnerVersion domain.AppVersionID
	for _, result := range outcomes {
		if result.err == nil {
			winners++
			winnerVersion = result.version
			continue
		}
		if !IsGenerationLost(result.err) {
			t.Errorf("concurrent generation error = %v, want a generation-lost conflict", result.err)
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent generation produced %d winners, want exactly 1", winners)
	}
	current, err := registry.Current(ctx, appID)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if current != winnerVersion {
		t.Errorf("current = %s, want the winning generation %s", current, winnerVersion)
	}
	// Every candidate is retained, winner included.
	for _, result := range outcomes {
		if result.err != nil {
			continue
		}
		if _, err := registry.GetArtifact(ctx, result.version); err != nil {
			t.Errorf("winning candidate %s is not retrievable: %v", result.version, err)
		}
	}
	// The losing candidates stay registered for
	// research even though they did not win.
	registered := 0
	for _, result := range outcomes {
		if result.err != nil && !IsGenerationLost(result.err) {
			continue
		}
		registered++
	}
	if registered != generators {
		t.Errorf("only %d of %d candidates registered", registered, generators)
	}
}

func TestOwnershipAndSharing(t *testing.T) {
	ctx := context.Background()
	service, registry, _, _, owner, appID, v1 := newTestService(t)
	stranger := mustUserID(t, "usr_01ARZ3NDEKTSV4RRFFQ69G5FBW")

	// Only the owning user may activate a user-scoped
	// app.
	if _, err := service.Activate(ctx, ActivateRequest{
		AppID: appID, Version: v1, Actor: stranger, Policy: domain.DefaultScopePolicy(),
	}); !domain.IsDeniedError(err) {
		t.Errorf("stranger activating a user-scoped app = %v, want denied", err)
	}

	// A restricted policy hides another user's
	// user-scoped app.
	restricted := domain.RestrictedScopePolicy()
	if _, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FBW"),
		UserID:    stranger, AppID: appID, Policy: restricted,
	}); !domain.IsDeniedError(err) {
		t.Errorf("cross-user resolution with sharing off = %v, want denied", err)
	}
	// With cross-user reads enabled the same session
	// resolves.
	crossUser := domain.DefaultScopePolicy()
	if _, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FBW"),
		UserID:    stranger, AppID: appID, Policy: crossUser,
	}); err != nil {
		t.Errorf("cross-user resolution with sharing on: %v", err)
	}

	// Registering a shared app requires an enabled
	// sharing policy.
	if _, err := service.RegisterCandidate(ctx, CandidateRequest{
		Manifest: testManifest("shared", 1),
		Source:   testSource(),
		Owner:    owner,
		Scope:    domain.ScopeShared,
		Policy:   restricted,
	}); !domain.IsDeniedError(err) {
		t.Errorf("shared registration with sharing off = %v, want denied", err)
	}
	sharedVersion, err := service.RegisterCandidate(ctx, CandidateRequest{
		Manifest: testManifest("shared", 1),
		Source:   testSource(),
		Owner:    owner,
		Scope:    domain.ScopeShared,
		Policy:   domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate (shared): %v", err)
	}
	sharedArtifact, err := registry.GetArtifact(ctx, sharedVersion)
	if err != nil {
		t.Fatalf("GetArtifact (shared): %v", err)
	}
	// Activating a shared app also follows the
	// sharing policy.
	if _, err := service.Activate(ctx, ActivateRequest{
		AppID: sharedArtifact.AppID, Version: sharedVersion, Actor: stranger, Policy: restricted,
	}); !domain.IsDeniedError(err) {
		t.Errorf("shared activation with sharing off = %v, want denied", err)
	}
	if _, err := service.Activate(ctx, ActivateRequest{
		AppID: sharedArtifact.AppID, Version: sharedVersion, Actor: stranger, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("shared activation with sharing on: %v", err)
	}
	// The owner of the user-scoped app can still
	// activate it (the earlier denial was the
	// stranger, not the owner).
	if _, err := service.Activate(ctx, ActivateRequest{
		AppID: appID, Version: v1, Actor: owner, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Errorf("owner activating their own app: %v", err)
	}
}

func TestRegisterMigrationValidatesTheVersionChain(t *testing.T) {
	ctx := context.Background()
	service, _, _, _, owner, appID, v1 := newTestService(t)

	v2, err := service.RegisterCandidate(ctx, CandidateRequest{
		AppID:         appID,
		ParentVersion: &v1,
		Manifest:      testManifest("app", 2),
		Source:        testSource() + "// v2\n",
		Owner:         owner,
		Scope:         domain.ScopeUser,
		Policy:        domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate v2: %v", err)
	}
	v3, err := service.RegisterCandidate(ctx, CandidateRequest{
		AppID:         appID,
		ParentVersion: &v2,
		Manifest:      testManifest("app", 3),
		Source:        testSource() + "// v3\n",
		Owner:         owner,
		Scope:         domain.ScopeUser,
		Policy:        domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate v3: %v", err)
	}

	// A migration must follow the parent chain: v1
	// -> v3 skips v2.
	if err := service.RegisterMigration(ctx, StateMigration{
		FromVersion: v1,
		ToVersion:   v3,
		Migrate: func(domain.AppState) (domain.AppState, error) {
			return domain.AppState{}, nil
		},
	}); !domain.IsValidationError(err) {
		t.Errorf("chain-skipping migration = %v, want validation error", err)
	}
	// A migration needs a function.
	if err := service.RegisterMigration(ctx, StateMigration{
		FromVersion: v1,
		ToVersion:   v2,
	}); !domain.IsValidationError(err) {
		t.Errorf("migration without a function = %v, want validation error", err)
	}
	// A well-formed chain migration registers.
	if err := service.RegisterMigration(ctx, StateMigration{
		FromVersion: v2,
		ToVersion:   v3,
		Migrate: func(state domain.AppState) (domain.AppState, error) {
			return state, nil
		},
	}); err != nil {
		t.Errorf("valid migration: %v", err)
	}
	// Unknown endpoints are rejected.
	if err := service.RegisterMigration(ctx, StateMigration{
		FromVersion: mustVersionID(t, "av_01ARZ3NDEKTSV4RRFFQ69G5FCH"),
		ToVersion:   v3,
		Migrate: func(domain.AppState) (domain.AppState, error) {
			return domain.AppState{}, nil
		},
	}); !domain.IsNotFoundError(err) {
		t.Errorf("migration from an unknown version = %v, want not-found", err)
	}
}

func TestSaveSessionStateRequiresResolve(t *testing.T) {
	ctx := context.Background()
	service, _, _, _, owner, appID, _ := newTestService(t)
	err := service.SaveSessionState(ctx, SaveStateRequest{
		SessionID: mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FAV"),
		UserID:    owner,
		AppID:     appID,
		State:     domain.AppState{UserState: userState(1, "x")},
	})
	if !domain.IsValidationError(err) {
		t.Errorf("saving state for an unresolved session = %v, want validation error", err)
	}
}

// registerAndActivateSharedApp registers and activates a
// shared-scoped app under a permissive policy, returning
// its app and version IDs.
func registerAndActivateSharedApp(t *testing.T, service *Service, registry *InMemoryRegistry, owner domain.UserID) (domain.AppID, domain.AppVersionID) {
	t.Helper()
	ctx := context.Background()
	version, err := service.RegisterCandidate(ctx, CandidateRequest{
		Manifest: testManifest("shared", 1),
		Source:   testSource(),
		Owner:    owner,
		Scope:    domain.ScopeShared,
		Policy:   domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate (shared): %v", err)
	}
	artifact, err := registry.GetArtifact(ctx, version)
	if err != nil {
		t.Fatalf("GetArtifact (shared): %v", err)
	}
	if _, err := service.Activate(ctx, ActivateRequest{
		AppID: artifact.AppID, Version: version, Actor: owner, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("Activate (shared): %v", err)
	}
	return artifact.AppID, version
}

func TestSharedAppVisibilityFollowsSharingPolicy(t *testing.T) {
	ctx := context.Background()
	service, registry, _, _, owner, _, _ := newTestService(t)
	stranger := mustUserID(t, "usr_01ARZ3NDEKTSV4RRFFQ69G5FBW")
	sharedApp, _ := registerAndActivateSharedApp(t, service, registry, owner)

	// With sharing disabled a shared app is not visible,
	// matching the canonical domain scope policy.
	if _, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FAV"),
		UserID:    stranger, AppID: sharedApp, Policy: domain.RestrictedScopePolicy(),
	}); !domain.IsDeniedError(err) {
		t.Errorf("resolving a shared app with sharing off = %v, want denied", err)
	}

	// With sharing enabled any user resolves it.
	resolution, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FAV"),
		UserID:    stranger, AppID: sharedApp, Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("resolving a shared app with sharing on: %v", err)
	}
	if resolution.Version.IsZero() {
		t.Error("shared app resolved to a zero version")
	}
}

func TestUserScopedAppDoesNotPersistSharedState(t *testing.T) {
	ctx := context.Background()
	service, _, _, _, owner, appID, _ := newTestService(t)
	session := mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if _, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: session, UserID: owner, AppID: appID, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	// A user-scoped app reports a shared portion; it must
	// not become durable shared state that another
	// resolution could inherit.
	if err := service.SaveSessionState(ctx, SaveStateRequest{
		SessionID: session, UserID: owner, AppID: appID,
		State: domain.AppState{UserState: userState(1, "mine"), SharedState: userState(1, "leak")},
	}); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}
	fresh := mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FBW")
	resolved, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: fresh, UserID: owner, AppID: appID, Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("ResolveSession for a new session: %v", err)
	}
	if len(resolved.State.SharedState) != 0 {
		t.Errorf("new session inherited shared state %s from a user-scoped app, want none", resolved.State.SharedState)
	}
}

func TestSharedAppStateIsInheritedAcrossUsers(t *testing.T) {
	ctx := context.Background()
	service, registry, _, _, owner, _, _ := newTestService(t)
	stranger := mustUserID(t, "usr_01ARZ3NDEKTSV4RRFFQ69G5FBW")
	sharedApp, version := registerAndActivateSharedApp(t, service, registry, owner)

	writer := mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if _, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: writer, UserID: owner, AppID: sharedApp, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("ResolveSession (writer): %v", err)
	}
	if err := service.SaveSessionState(ctx, SaveStateRequest{
		SessionID: writer, UserID: owner, AppID: sharedApp,
		State: domain.AppState{SharedState: userState(1, "common")},
	}); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	reader := mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FBW")
	inherited, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: reader, UserID: stranger, AppID: sharedApp, Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("ResolveSession (reader): %v", err)
	}
	if inherited.Version != version {
		t.Errorf("reader resolved %s, want %s", inherited.Version, version)
	}
	var payload struct {
		Schema int64  `json:"schema"`
		Data   string `json:"data"`
	}
	if err := json.Unmarshal(inherited.State.SharedState, &payload); err != nil {
		t.Fatalf("inherited shared state: %v", err)
	}
	if payload.Data != "common" {
		t.Errorf("inherited shared state = %+v, want data %q", payload, "common")
	}
}

// activateNextVersion registers and activates a
// successor of parent owned by owner, so a test can move the
// current pointer without repeating the lifecycle calls.
func activateNextVersion(
	t *testing.T,
	service *Service,
	owner domain.UserID,
	app domain.AppID,
	parent domain.AppVersionID,
	schemaVersion int64,
) domain.AppVersionID {
	t.Helper()
	ctx := context.Background()
	version, err := service.RegisterCandidate(ctx, CandidateRequest{
		AppID:         app,
		ParentVersion: &parent,
		Manifest:      testManifest("app", schemaVersion),
		Source:        testSource() + "// successor\n",
		Owner:         owner,
		Scope:         domain.ScopeUser,
		Policy:        domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate: %v", err)
	}
	if _, err := service.Activate(ctx, ActivateRequest{
		AppID: app, Version: version, Actor: owner, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	return version
}

func TestResolvePinnedReturnsTheStateOfTheNamedVersion(t *testing.T) {
	ctx := context.Background()
	service, _, _, _, owner, appID, v1 := newTestService(t)
	session := mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FAV")

	if _, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: session, UserID: owner, AppID: appID, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	saved := domain.AppState{SessionState: userState(1, "after the first line")}
	if err := service.SaveSessionState(ctx, SaveStateRequest{
		SessionID: session, UserID: owner, AppID: appID, State: saved,
	}); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	resolution, err := service.ResolvePinned(ctx, ResolvePinnedRequest{
		SessionID: session, UserID: owner, AppID: appID, Version: v1, Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("ResolvePinned: %v", err)
	}
	if resolution.Version != v1 {
		t.Errorf("pinned resolution = %+v, want version %s", resolution, v1)
	}
	if string(resolution.State.SessionState) != string(saved.SessionState) {
		t.Errorf("pinned state = %s, want %s", resolution.State.SessionState, saved.SessionState)
	}
	if resolution.Stale {
		t.Errorf("pinned resolution reported stale (%s) for the version the session pinned", resolution.Reason)
	}
}

func TestPinnedResolutionSurvivesANewerActivation(t *testing.T) {
	ctx := context.Background()
	service, _, _, _, owner, appID, v1 := newTestService(t)
	session := mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FAV")

	// The session starts the app, which pins v1, and runs
	// one line of it.
	if _, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: session, UserID: owner, AppID: appID, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	kept := userState(1, "state of the running app")
	if err := service.SaveSessionState(ctx, SaveStateRequest{
		SessionID: session, UserID: owner, AppID: appID, State: domain.AppState{UserState: kept},
	}); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	// A newer version is activated while the app is
	// foreground: the running app must not change.
	v2 := activateNextVersion(t, service, owner, appID, v1, 2)

	resolution, err := service.ResolvePinned(ctx, ResolvePinnedRequest{
		SessionID: session, UserID: owner, AppID: appID, Version: v1, Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("ResolvePinned after activation: %v", err)
	}
	if resolution.Version != v1 {
		t.Errorf("pinned resolution = %+v, want the running version %s", resolution, v1)
	}
	if resolution.Current != v2 {
		t.Errorf("pinned resolution current = %s, want %s", resolution.Current, v2)
	}
	if string(resolution.State.UserState) != string(kept) {
		t.Errorf("pinned state = %s, want the running app's own state %s", resolution.State.UserState, kept)
	}
	if resolution.Stale {
		t.Errorf("pinned resolution reported stale (%s)", resolution.Reason)
	}
}

func TestStateSavedAfterAPinnedResolutionRoundTrips(t *testing.T) {
	ctx := context.Background()
	service, _, _, _, owner, appID, v1 := newTestService(t)
	session := mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FAV")

	// A resumed foreground session resolves its version
	// without ever resolving the current one; the pin it
	// creates is what a later save targets.
	resolution, err := service.ResolvePinned(ctx, ResolvePinnedRequest{
		SessionID: session, UserID: owner, AppID: appID, Version: v1, Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("ResolvePinned: %v", err)
	}
	if resolution.Version != v1 {
		t.Fatalf("pinned resolution = %+v, want version %s", resolution, v1)
	}

	saved := domain.AppState{UserState: userState(1, "state after the line")}
	if err := service.SaveSessionState(ctx, SaveStateRequest{
		SessionID: session, UserID: owner, AppID: appID, State: saved,
	}); err != nil {
		t.Fatalf("SaveSessionState after a pinned resolution: %v", err)
	}

	again, err := service.ResolvePinned(ctx, ResolvePinnedRequest{
		SessionID: session, UserID: owner, AppID: appID, Version: v1, Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("ResolvePinned again: %v", err)
	}
	if string(again.State.UserState) != string(saved.UserState) {
		t.Errorf("resolved state = %s, want the saved state %s", again.State.UserState, saved.UserState)
	}
}

func TestResolvePinnedRefusesAReadThePolicyForbids(t *testing.T) {
	ctx := context.Background()
	service, _, _, _, _, appID, v1 := newTestService(t)
	stranger := mustUserID(t, "usr_01ARZ3NDEKTSV4RRFFQ69G5FBW")
	session := mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FBW")

	// Another user's app is not readable while sharing is
	// off, exactly as at first resolution.
	if _, err := service.ResolvePinned(ctx, ResolvePinnedRequest{
		SessionID: session, UserID: stranger, AppID: appID, Version: v1,
		Policy: domain.RestrictedScopePolicy(),
	}); !domain.IsDeniedError(err) {
		t.Fatalf("ResolvePinned with sharing off = %v, want denied", err)
	}
	// The refused read pinned nothing, so there is no state
	// to save against.
	if err := service.SaveSessionState(ctx, SaveStateRequest{
		SessionID: session, UserID: stranger, AppID: appID,
		State: domain.AppState{UserState: userState(1, "x")},
	}); !domain.IsValidationError(err) {
		t.Errorf("SaveSessionState after a refused pinned read = %v, want validation error", err)
	}

	// With cross-user reads allowed the same call is
	// permitted, so the refusal came from the policy.
	permitted, err := service.ResolvePinned(ctx, ResolvePinnedRequest{
		SessionID: mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FCW"),
		UserID:    stranger, AppID: appID, Version: v1,
		Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("ResolvePinned with sharing on: %v", err)
	}
	if permitted.Version != v1 {
		t.Errorf("permitted pinned resolution = %+v, want version %s", permitted, v1)
	}
}

func TestResolvePinnedRefusesAVersionThatCannotRun(t *testing.T) {
	ctx := context.Background()
	service, registry, _, _, owner, appID, v1 := newTestService(t)
	session := mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FAV")

	// A registered candidate that was never activated is
	// still being validated and may not run.
	candidate, err := service.RegisterCandidate(ctx, CandidateRequest{
		AppID:         appID,
		ParentVersion: &v1,
		Manifest:      testManifest("app", 2),
		Source:        testSource() + "// candidate\n",
		Owner:         owner,
		Scope:         domain.ScopeUser,
		Policy:        domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate: %v", err)
	}
	if _, err := service.ResolvePinned(ctx, ResolvePinnedRequest{
		SessionID: session, UserID: owner, AppID: appID, Version: candidate,
		Policy: domain.DefaultScopePolicy(),
	}); !domain.IsValidationError(err) || domain.GetErrorCode(err) != codeNotAnAcceptedVersion {
		t.Errorf("resolving an unactivated candidate = %v, want %s", err, codeNotAnAcceptedVersion)
	}

	// A version of another app is refused too.
	_, otherVersion := registerAndActivateSharedApp(t, service, registry, owner)
	if _, err := service.ResolvePinned(ctx, ResolvePinnedRequest{
		SessionID: session, UserID: owner, AppID: appID, Version: otherVersion,
		Policy: domain.DefaultScopePolicy(),
	}); !domain.IsValidationError(err) {
		t.Errorf("resolving a version of another app = %v, want validation error", err)
	}
	// An unregistered version never reaches the store.
	if _, err := service.ResolvePinned(ctx, ResolvePinnedRequest{
		SessionID: session, UserID: owner, AppID: appID,
		Version: mustVersionID(t, "av_01ARZ3NDEKTSV4RRFFQ69G5FAW"),
		Policy:  domain.DefaultScopePolicy(),
	}); !domain.IsNotFoundError(err) {
		t.Errorf("resolving an unregistered version = %v, want not-found", err)
	}
}

func TestResolvePinnedRefusesAVersionTheSessionDoesNotRun(t *testing.T) {
	ctx := context.Background()
	service, _, _, _, owner, appID, v1 := newTestService(t)
	session := mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FAV")

	if _, err := service.ResolveSession(ctx, ResolveRequest{
		SessionID: session, UserID: owner, AppID: appID, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	v2 := activateNextVersion(t, service, owner, appID, v1, 2)

	// The session's stored state belongs to v1, so running v2
	// against it would either lose that state or record it
	// under the wrong version.
	if _, err := service.ResolvePinned(ctx, ResolvePinnedRequest{
		SessionID: session, UserID: owner, AppID: appID, Version: v2,
		Policy: domain.DefaultScopePolicy(),
	}); !domain.IsValidationError(err) || domain.GetErrorCode(err) != "session_pinned_another_version" {
		t.Errorf("resolving %s for a session pinned to %s = %v, want session_pinned_another_version", v2, v1, err)
	}
}

func TestResolvePinnedRequiresItsRequestFields(t *testing.T) {
	ctx := context.Background()
	service, _, _, _, owner, appID, v1 := newTestService(t)
	complete := ResolvePinnedRequest{
		SessionID: mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FAV"),
		UserID:    owner,
		AppID:     appID,
		Version:   v1,
		Policy:    domain.DefaultScopePolicy(),
	}
	for name, mutate := range map[string]func(*ResolvePinnedRequest){
		"without a session": func(r *ResolvePinnedRequest) { r.SessionID = domain.SessionID{} },
		"without a user":    func(r *ResolvePinnedRequest) { r.UserID = domain.UserID{} },
		"without an app":    func(r *ResolvePinnedRequest) { r.AppID = domain.AppID{} },
		"without a version": func(r *ResolvePinnedRequest) { r.Version = domain.AppVersionID{} },
	} {
		t.Run(name, func(t *testing.T) {
			req := complete
			mutate(&req)
			if _, err := service.ResolvePinned(ctx, req); !domain.IsValidationError(err) {
				t.Errorf("ResolvePinned %s = %v, want validation error", name, err)
			}
		})
	}
}
