package apps

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// fakeStateStore wraps the in-memory store and records the port calls the
// service makes, so a test can prove that state and pins travel through the
// AppStateStore port instead of service-private maps.
type fakeStateStore struct {
	*InMemoryStateStore

	mu    sync.Mutex
	calls []string
}

func newFakeStateStore(clock *FixedClock) *fakeStateStore {
	return &fakeStateStore{InMemoryStateStore: NewInMemoryStateStore(clock)}
}

func (f *fakeStateStore) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeStateStore) count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, recorded := range f.calls {
		if recorded == call {
			count++
		}
	}
	return count
}

func (f *fakeStateStore) SessionPin(ctx context.Context, session domain.SessionID, app domain.AppID) (ports.SessionPin, bool, error) {
	f.record("SessionPin")
	return f.InMemoryStateStore.SessionPin(ctx, session, app)
}

func (f *fakeStateStore) PinSession(ctx context.Context, session domain.SessionID, app domain.AppID, version domain.AppVersionID, state domain.AppState) error {
	f.record("PinSession")
	return f.InMemoryStateStore.PinSession(ctx, session, app, version, state)
}

func (f *fakeStateStore) SaveSessionState(ctx context.Context, session domain.SessionID, app domain.AppID, version domain.AppVersionID, state domain.AppState) error {
	f.record("SaveSessionState")
	return f.InMemoryStateStore.SaveSessionState(ctx, session, app, version, state)
}

func (f *fakeStateStore) SaveUserState(ctx context.Context, user domain.UserID, app domain.AppID, record ports.AppStateRecord) error {
	f.record("SaveUserState")
	return f.InMemoryStateStore.SaveUserState(ctx, user, app, record)
}

func (f *fakeStateStore) ReleaseSession(ctx context.Context, session domain.SessionID, app domain.AppID) error {
	f.record("ReleaseSession")
	return f.InMemoryStateStore.ReleaseSession(ctx, session, app)
}

func (f *fakeStateStore) RecordCommand(ctx context.Context, name string, app domain.AppID) error {
	f.record("RecordCommand")
	return f.InMemoryStateStore.RecordCommand(ctx, name, app)
}

// TestServicePersistsStateAndPinsThroughTheStore proves the service keeps no
// state of its own: every write goes through the store, and a fresh service
// over the same store reloads the pin, the session state, and the durable
// user state the first service recorded.
func TestServicePersistsStateAndPinsThroughTheStore(t *testing.T) {
	ctx := context.Background()
	clock := NewFixedClock(1000)
	registry := NewInMemoryRegistry(clock)
	sandbox := &FakeSandbox{}
	store := newFakeStateStore(clock)
	owner := mustUserID(t, "usr_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	session := mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FAV")

	// One activated v1 application, as newTestService builds it.
	first := NewService(registry, sandbox, clock, &SequenceRandom{}, store)
	version, err := first.RegisterCandidate(ctx, CandidateRequest{
		Manifest: testManifest("app", 1),
		Source:   testSource(),
		Owner:    owner,
		Scope:    domain.ScopeUser,
		Policy:   domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("RegisterCandidate: %v", err)
	}
	artifact, err := registry.GetArtifact(ctx, version)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if _, err := first.Activate(ctx, ActivateRequest{
		AppID: artifact.AppID, Version: version, Actor: owner, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	// Resolve pins the session through the store; saving writes session,
	// user, and pin state through it as well.
	if _, err := first.ResolveSession(ctx, ResolveRequest{
		SessionID: session, UserID: owner, AppID: artifact.AppID, Policy: domain.DefaultScopePolicy(),
	}); err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	if store.count("PinSession") != 1 {
		t.Errorf("PinSession calls = %d, want 1 (the new-session pin)", store.count("PinSession"))
	}
	saved := domain.AppState{
		UserState:    userState(1, "durable"),
		SessionState: userState(1, "cursor"),
	}
	if err := first.SaveSessionState(ctx, SaveStateRequest{
		SessionID: session, UserID: owner, AppID: artifact.AppID, State: saved,
	}); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}
	if store.count("SaveSessionState") != 1 || store.count("SaveUserState") != 1 {
		t.Errorf("store writes = session %d, user %d, want 1 and 1",
			store.count("SaveSessionState"), store.count("SaveUserState"))
	}

	// A fresh service with no maps of its own, over the same store, reads
	// back exactly what the first one wrote.
	second := NewService(registry, sandbox, clock, &SequenceRandom{}, store)
	reloaded, err := second.ResolveSession(ctx, ResolveRequest{
		SessionID: session, UserID: owner, AppID: artifact.AppID, Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("ResolveSession in the fresh service: %v", err)
	}
	if reloaded.Version != version {
		t.Errorf("reloaded pin = %s, want %s", reloaded.Version, version)
	}
	if !reflect.DeepEqual(reloaded.State, saved) {
		t.Errorf("reloaded session state = %+v, want %+v", reloaded.State, saved)
	}

	// The durable user portion reaches a brand-new session too.
	fresh := mustSessionID(t, "ses_01ARZ3NDEKTSV4RRFFQ69G5FBW")
	inherited, err := second.ResolveSession(ctx, ResolveRequest{
		SessionID: fresh, UserID: owner, AppID: artifact.AppID, Policy: domain.DefaultScopePolicy(),
	})
	if err != nil {
		t.Fatalf("ResolveSession for a new session: %v", err)
	}
	if !reflect.DeepEqual(inherited.State.UserState, saved.UserState) {
		t.Errorf("inherited user state = %s, want %s", inherited.State.UserState, saved.UserState)
	}

	// Release goes through the store as well, and the pin is gone: the
	// released session resolves as a new pin again.
	if err := first.ReleaseSession(ctx, session, artifact.AppID); err != nil {
		t.Fatalf("ReleaseSession: %v", err)
	}
	if store.count("ReleaseSession") != 1 {
		t.Errorf("ReleaseSession calls = %d, want 1", store.count("ReleaseSession"))
	}
	if _, ok, err := store.SessionPin(ctx, session, artifact.AppID); err != nil || ok {
		t.Errorf("pin after release: ok=%v err=%v, want ok=false", ok, err)
	}
}

// TestServiceExposesTheCommandIndexThroughTheStore proves RecordCommand and
// CommandIndex ride the same store, so a fresh service (and, after wiring, a
// restarted process) resolves command names to the stored app.
func TestServiceExposesTheCommandIndexThroughTheStore(t *testing.T) {
	ctx := context.Background()
	clock := NewFixedClock(1000)
	registry := NewInMemoryRegistry(clock)
	store := newFakeStateStore(clock)
	service := NewService(registry, &FakeSandbox{}, clock, &SequenceRandom{}, store)
	app, err := NewAppID(&SequenceRandom{})
	if err != nil {
		t.Fatalf("NewAppID: %v", err)
	}

	if err := service.RecordCommand(ctx, "ledger", app); err != nil {
		t.Fatalf("RecordCommand: %v", err)
	}
	if store.count("RecordCommand") != 1 {
		t.Errorf("RecordCommand calls = %d, want 1", store.count("RecordCommand"))
	}

	fresh := NewService(registry, &FakeSandbox{}, clock, &SequenceRandom{}, store)
	index, err := fresh.CommandIndex(ctx)
	if err != nil {
		t.Fatalf("CommandIndex: %v", err)
	}
	if got, ok := index["ledger"]; !ok || got != app {
		t.Errorf("command index = %v, want ledger -> %s", index, app)
	}

	// The default store behind a nil argument behaves the same within the
	// process; only durability differs.
	unconfigured := NewService(registry, &FakeSandbox{}, clock, &SequenceRandom{}, nil)
	if err := unconfigured.RecordCommand(ctx, "notes", app); err != nil {
		t.Fatalf("RecordCommand (default store): %v", err)
	}
	index, err = unconfigured.CommandIndex(ctx)
	if err != nil {
		t.Fatalf("CommandIndex (default store): %v", err)
	}
	if got, ok := index["notes"]; !ok || got != app {
		t.Errorf("default-store command index = %v, want notes -> %s", index, app)
	}

	if err := service.RecordCommand(ctx, "", app); !domain.IsValidationError(err) {
		t.Errorf("recording an empty command name = %v, want validation error", err)
	}
	if err := service.RecordCommand(ctx, "ledger", domain.AppID{}); !domain.IsValidationError(err) {
		t.Errorf("recording a zero app = %v, want validation error", err)
	}
}
