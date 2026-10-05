package sqlite

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// TestAppStateStoreRoundTripsAcrossRestart proves everything the application
// service now reads through ports.AppStateStore is durable: the pin, the
// session state saved under it, and the command-name index all survive the
// database being closed and reopened — exactly what a process restart
// observes.
func TestAppStateStoreRoundTripsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	db, path := durableDB(t)
	store, err := NewApps(db.SQL(), fixedTestClock{now: 1_700_000_600_000})
	if err != nil {
		t.Fatalf("NewApps: %v", err)
	}

	ids := newAppIDs(t)
	artifact := candidate(t, ids, domain.ScopeUser, "function main(){ return 'durable'; }", true)
	if _, err := store.RegisterCandidate(ctx, artifact); err != nil {
		t.Fatalf("RegisterCandidate: %v", err)
	}
	if err := store.Activate(ctx, ids.app, ids.version); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	// A session pins the current version, then saves state under that pin.
	if err := store.PinSession(ctx, ids.session, ids.app, ids.version, domain.AppState{}); err != nil {
		t.Fatalf("PinSession: %v", err)
	}
	saved := domain.AppState{
		SessionState: json.RawMessage(`{"cursor":7}`),
		UserState:    json.RawMessage(`{"rows":[1,2]}`),
	}
	if err := store.SaveSessionState(ctx, ids.session, ids.app, ids.version, saved); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	// The shell records the app's visible command name.
	if err := store.RecordCommand(ctx, "ledger", ids.app); err != nil {
		t.Fatalf("RecordCommand: %v", err)
	}

	db = restart(t, db, path)
	reopened, err := NewApps(db.SQL(), fixedTestClock{now: 1_700_000_700_000})
	if err != nil {
		t.Fatalf("NewApps after restart: %v", err)
	}

	pin, ok, err := reopened.SessionPin(ctx, ids.session, ids.app)
	if err != nil || !ok {
		t.Fatalf("SessionPin after restart: ok=%v err=%v", ok, err)
	}
	if pin.Version != ids.version {
		t.Errorf("pin version after restart = %s, want %s", pin.Version, ids.version)
	}
	if !reflect.DeepEqual(pin.State, saved) {
		t.Errorf("session state after restart = %+v, want %+v", pin.State, saved)
	}
	if pin.PinnedAt != 1_700_000_600_000 {
		t.Errorf("pinned_at after restart = %d, want %d", pin.PinnedAt, 1_700_000_600_000)
	}

	index, err := reopened.CommandIndex(ctx)
	if err != nil {
		t.Fatalf("CommandIndex after restart: %v", err)
	}
	if got, ok := index["ledger"]; !ok || got != ids.app {
		t.Errorf("command index after restart = %v, want ledger -> %s", index, ids.app)
	}
}

// TestRecordCommandGuards checks the boundary the port documents: only a
// complete request for a registered app is recorded, and a re-recorded name
// moves to the latest app exactly like the shell's in-memory index did.
func TestRecordCommandGuards(t *testing.T) {
	ctx := context.Background()
	db, _ := durableDB(t)
	store, err := NewApps(db.SQL(), fixedTestClock{now: 5})
	if err != nil {
		t.Fatalf("NewApps: %v", err)
	}
	ids := newAppIDs(t)

	if code := domain.GetErrorCode(store.RecordCommand(ctx, "", ids.app)); code != domain.CodeInvalidInput {
		t.Errorf("empty command name = %v, want %s", code, domain.CodeInvalidInput)
	}
	if code := domain.GetErrorCode(store.RecordCommand(ctx, "ledger", domain.AppID{})); code != domain.CodeInvalidInput {
		t.Errorf("zero app = %v, want %s", code, domain.CodeInvalidInput)
	}
	if code := domain.GetErrorCode(store.RecordCommand(ctx, "ledger", ids.app)); code != domain.CodeAppNotFound {
		t.Errorf("unregistered app = %v, want %s", code, domain.CodeAppNotFound)
	}

	artifact := candidate(t, ids, domain.ScopeUser, "function main(){ return 'first'; }", true)
	if _, err := store.RegisterCandidate(ctx, artifact); err != nil {
		t.Fatalf("RegisterCandidate: %v", err)
	}
	if err := store.RecordCommand(ctx, "ledger", ids.app); err != nil {
		t.Fatalf("RecordCommand: %v", err)
	}

	// A later app takes the name over: last write wins.
	later := newAppIDs(t)
	laterArtifact := candidate(t, later, domain.ScopeUser, "function main(){ return 'second'; }", true)
	if _, err := store.RegisterCandidate(ctx, laterArtifact); err != nil {
		t.Fatalf("RegisterCandidate (later): %v", err)
	}
	if err := store.RecordCommand(ctx, "ledger", later.app); err != nil {
		t.Fatalf("RecordCommand (later): %v", err)
	}
	index, err := store.CommandIndex(ctx)
	if err != nil {
		t.Fatalf("CommandIndex: %v", err)
	}
	if len(index) != 1 {
		t.Fatalf("command index = %v, want exactly one entry", index)
	}
	if got := index["ledger"]; got != later.app {
		t.Errorf("ledger -> %s, want the later app %s", got, later.app)
	}
}

// TestMigrationV4RegistersTheCommandIndex guards the shared contract the way
// the v3 guard does: migration numbering has a single owner, so version 4
// must be claimed exactly once and must create the table the command index
// reads.
func TestMigrationV4RegistersTheCommandIndex(t *testing.T) {
	registered := 0
	for _, m := range orderedMigrations() {
		if m.Version != 4 {
			continue
		}
		registered++
		if m.Name != "app-command-index" {
			t.Errorf("migration 4 name = %q, want app-command-index", m.Name)
		}
	}
	if registered != 1 {
		t.Fatalf("version 4 is registered %d times, want exactly 1", registered)
	}

	db, _ := durableDB(t)
	var name string
	if err := db.SQL().QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'app_commands'`).Scan(&name); err != nil {
		t.Errorf("table %q is missing after migration: %v", "app_commands", err)
	}
}
