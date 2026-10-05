package application

import (
	"j0s.at/vibeshell/internal/domain"
)

// This file holds the session-local shell state as pure values: working
// directory, foreground interaction, last exit status, and terminal size.
// Nothing here is shared between sessions. Durable world state is reached only
// through the world store, which is why two sessions of one user see the same
// files while keeping separate cwd and foreground state (PLAN 5.2, 4.3).

// ForegroundKind is what the session is currently attached to.
type ForegroundKind string

const (
	// ForegroundShell means the session is attached to the shell itself.
	ForegroundShell ForegroundKind = "shell"
	// ForegroundApp means the session is attached to a generated application
	// interaction.
	ForegroundApp ForegroundKind = "app"
)

// ForegroundState identifies the current interactive attachment of a session.
// AppID and AppVersionID are set for ForegroundApp and pin the accepted
// artifact version for as long as the interaction lasts.
type ForegroundState struct {
	Kind         ForegroundKind      `json:"kind"`
	AppID        domain.AppID        `json:"app_id,omitempty"`
	AppVersionID domain.AppVersionID `json:"app_version_id,omitempty"`
}

// shellForeground is the state of a session that is not attached to an app.
func shellForeground() ForegroundState {
	return ForegroundState{Kind: ForegroundShell}
}

// Validate rejects a foreground state that cannot be an accepted attachment.
func (f ForegroundState) Validate() error {
	switch f.Kind {
	case ForegroundShell:
		return nil
	case ForegroundApp:
		if f.AppID.IsZero() || f.AppVersionID.IsZero() {
			return domain.NewValidationError(domain.CodeInvalidInput, "foreground app requires an app and version", nil)
		}
		return nil
	default:
		return domain.NewValidationError(domain.CodeInvalidInput, "unknown foreground kind", map[string]string{
			"kind": string(f.Kind),
		})
	}
}

// sessionState is the mutable session-local state. The session goroutine is its
// only writer; readers take the session lock.
type sessionState struct {
	id             domain.SessionID
	principal      domain.UserID
	authMode       AuthMode
	terminal       TerminalMetadata
	cwd            domain.ValidPath
	home           domain.ValidPath
	foreground     ForegroundState
	appPrompt      *AppPrompt
	exitStatus     int
	turnsCompleted int
	startedAtMs    int64
}

// newSessionState builds the state of a freshly accepted session. The home
// directory of the principal is the starting working directory; the adapter
// resolves the visible path, so the application starts from an explicit value.
func newSessionState(id domain.SessionID, principal domain.UserID, authMode AuthMode, terminal TerminalMetadata, cwd domain.ValidPath, nowUnixMilli int64) sessionState {
	return sessionState{
		id:          id,
		principal:   principal,
		authMode:    authMode,
		terminal:    terminal,
		cwd:         cwd,
		home:        cwd,
		foreground:  shellForeground(),
		startedAtMs: nowUnixMilli,
	}
}

// context returns the value handed to one turn.
func (s *sessionState) context() SessionContext {
	return SessionContext{
		CWD:          s.cwd,
		Home:         s.home,
		Foreground:   s.foreground,
		ExitStatus:   s.exitStatus,
		TerminalSize: s.terminal.Size,
	}
}

// applyPatch applies an accepted session-state change. It runs at the commit
// point, before emission, so the prompt shows accepted state.
func (s *sessionState) applyPatch(patch SessionPatch) {
	if patch.CWD != nil {
		s.cwd = *patch.CWD
	}
	if patch.Foreground != nil {
		s.foreground = *patch.Foreground
	}
	if patch.ExitStatus != nil {
		s.exitStatus = *patch.ExitStatus
	}
	if patch.AppPrompt != nil {
		s.appPrompt = patch.AppPrompt
	}
	if s.foreground.Kind != ForegroundApp {
		// An app prompt exists only while an application is foreground, so a
		// patch that leaves the session at the shell drops it whatever else the
		// patch carries.
		s.appPrompt = nil
	}
}

// resize records a new terminal size for this session only.
func (s *sessionState) resize(size domain.TermSize) {
	s.terminal.Size = size
}

// prompt returns the accepted prompt position, including the foreground
// application's prompt when one is foreground.
func (s *sessionState) prompt() PromptState {
	return PromptState{CWD: s.cwd, ExitCode: s.exitStatus, App: s.appPrompt}
}
