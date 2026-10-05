package apps

import (
	"context"
	"sync"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// Compile-time proof that the default store stays behind the port.
var _ ports.AppStateStore = (*InMemoryStateStore)(nil)

// InMemoryStateStore is the ports.AppStateStore the service uses when the
// composition supplies no durable store: tests and unconfigured builds keep
// their behavior, while the SQLite adapter behind the same port makes the
// identical data survive a restart. It doubles as the adapter's contract
// double: missing records report ok=false, saving session state without a
// pin is refused with session_not_resolved, and a re-recorded command name
// moves to the latest app, exactly like the durable store.
//
// Nothing here survives the process.
type InMemoryStateStore struct {
	clock ports.Clock

	// mu guards the pins, durable state partitions, and command index.
	mu           sync.Mutex
	pins         map[domain.SessionID]map[domain.AppID]ports.SessionPin
	userStates   map[domain.UserID]map[domain.AppID]ports.AppStateRecord
	sharedStates map[domain.AppID]ports.AppStateRecord
	commands     map[string]domain.AppID
}

// NewInMemoryStateStore builds an empty in-memory store. The clock stamps
// pin times so the double reports the same fields as the durable adapter.
func NewInMemoryStateStore(clock ports.Clock) *InMemoryStateStore {
	return &InMemoryStateStore{
		clock:        clock,
		pins:         make(map[domain.SessionID]map[domain.AppID]ports.SessionPin),
		userStates:   make(map[domain.UserID]map[domain.AppID]ports.AppStateRecord),
		sharedStates: make(map[domain.AppID]ports.AppStateRecord),
		commands:     make(map[string]domain.AppID),
	}
}

// SessionPin returns a session's pin for an app, or ok=false when the session
// never resolved it.
func (s *InMemoryStateStore) SessionPin(_ context.Context, session domain.SessionID, app domain.AppID) (ports.SessionPin, bool, error) {
	if session.IsZero() || app.IsZero() {
		return ports.SessionPin{}, false, domain.NewValidationError(
			domain.CodeInvalidInput, "session and app are required", nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pin, ok := s.pins[session][app]
	if !ok {
		return ports.SessionPin{}, false, nil
	}
	return pin, true, nil
}

// PinSession pins a session to a version and records the session state it
// starts from. Re-pinning replaces both.
func (s *InMemoryStateStore) PinSession(_ context.Context, session domain.SessionID, app domain.AppID, version domain.AppVersionID, state domain.AppState) error {
	if session.IsZero() || app.IsZero() || version.IsZero() {
		return domain.NewValidationError(
			domain.CodeInvalidIdentity, "session, app, and version are required", nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pins[session] == nil {
		s.pins[session] = make(map[domain.AppID]ports.SessionPin)
	}
	s.pins[session][app] = ports.SessionPin{
		Version:  version,
		State:    state,
		PinnedAt: s.clock.NowUnixMilli(),
	}
	return nil
}

// SaveSessionState replaces the state of an existing pin. State for a
// session that never resolved the app, or resolved a different version, is
// refused instead of becoming an orphan.
func (s *InMemoryStateStore) SaveSessionState(_ context.Context, session domain.SessionID, app domain.AppID, version domain.AppVersionID, state domain.AppState) error {
	if session.IsZero() || app.IsZero() || version.IsZero() {
		return domain.NewValidationError(
			domain.CodeInvalidIdentity, "session, app, and version are required", nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pin, ok := s.pins[session][app]
	if !ok || pin.Version != version {
		return domain.NewValidationError(
			"session_not_resolved", "the session must resolve the app before saving state",
			map[string]string{"session": session.String(), "app": app.String()},
		)
	}
	pin.State = state
	s.pins[session][app] = pin
	return nil
}

// ReleaseSession drops a session's pin and session state. Durable user and
// shared state is retained; releasing an unpinned session is a no-op.
func (s *InMemoryStateStore) ReleaseSession(_ context.Context, session domain.SessionID, app domain.AppID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pins[session], app)
	if len(s.pins[session]) == 0 {
		delete(s.pins, session)
	}
	return nil
}

// UserState returns the durable user-scoped state of an app, or ok=false
// when this user never saved any.
func (s *InMemoryStateStore) UserState(_ context.Context, user domain.UserID, app domain.AppID) (ports.AppStateRecord, bool, error) {
	if user.IsZero() || app.IsZero() {
		return ports.AppStateRecord{}, false, domain.NewValidationError(
			domain.CodeInvalidInput, "user and app are required", nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.userStates[user][app]
	if !ok {
		return ports.AppStateRecord{}, false, nil
	}
	return record, true, nil
}

// SaveUserState records the durable user-scoped portion of a state.
func (s *InMemoryStateStore) SaveUserState(_ context.Context, user domain.UserID, app domain.AppID, record ports.AppStateRecord) error {
	if user.IsZero() || app.IsZero() {
		return domain.NewValidationError(domain.CodeInvalidInput, "user and app are required", nil)
	}
	if err := checkStateRecord(record); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.userStates[user] == nil {
		s.userStates[user] = make(map[domain.AppID]ports.AppStateRecord)
	}
	s.userStates[user][app] = record
	return nil
}

// SharedState returns the durable shared state of an app, or ok=false when
// none was ever saved.
func (s *InMemoryStateStore) SharedState(_ context.Context, app domain.AppID) (ports.AppStateRecord, bool, error) {
	if app.IsZero() {
		return ports.AppStateRecord{}, false, domain.NewValidationError(
			domain.CodeInvalidInput, "app is required", nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.sharedStates[app]
	if !ok {
		return ports.AppStateRecord{}, false, nil
	}
	return record, true, nil
}

// SaveSharedState records the durable shared portion of a state.
func (s *InMemoryStateStore) SaveSharedState(_ context.Context, app domain.AppID, record ports.AppStateRecord) error {
	if app.IsZero() {
		return domain.NewValidationError(domain.CodeInvalidInput, "app is required", nil)
	}
	if err := checkStateRecord(record); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sharedStates[app] = record
	return nil
}

// CommandIndex returns a copy of the command-name to app map so callers
// cannot mutate the stored index.
func (s *InMemoryStateStore) CommandIndex(_ context.Context) (map[string]domain.AppID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]domain.AppID, len(s.commands))
	for name, app := range s.commands {
		out[name] = app
	}
	return out, nil
}

// RecordCommand associates a command name with an app; a re-recorded name
// moves to the latest app.
func (s *InMemoryStateStore) RecordCommand(_ context.Context, name string, app domain.AppID) error {
	if name == "" || app.IsZero() {
		return domain.NewValidationError(
			domain.CodeInvalidInput, "command name and app are required", nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands[name] = app
	return nil
}

func checkStateRecord(record ports.AppStateRecord) error {
	if record.Version.IsZero() {
		return domain.NewValidationError(
			domain.CodeInvalidIdentity, "app state requires the version that produced it", nil)
	}
	return nil
}
