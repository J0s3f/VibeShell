package application

import (
	"context"
	"fmt"
	"strings"

	"j0s.at/vibeshell/internal/domain"
)

// This file is the application-facing session contract: the types and the
// interface an inbound transport adapter (SSH today, a local terminal later)
// uses to drive one shell session. It contains only domain types plus
// primitives, so no adapter type ever leaks into the application and the
// application never learns about SSH channels or terminal codecs.
//
// Contract baseline: PLAN 14 "Session input" and "Session output".

// Bounds on transport-reported and transport-delivered values. The adapter
// validates its own payloads too; these are the application-side limits that
// keep a misbehaving or hostile client from driving unbounded work.
const (
	// MaxCommandBytes bounds one accepted command line.
	MaxCommandBytes = 64 * 1024
	// MaxPasteBytes bounds one paste payload.
	MaxPasteBytes = 256 * 1024
	// MaxTerminalCols and MaxTerminalRows bound PTY dimensions. A zero
	// dimension is replaced by DefaultTerminalSize because a shell without a
	// size still needs a deterministic layout.
	MaxTerminalCols = 1000
	MaxTerminalRows = 1000
)

// DefaultTerminalSize is the size used when the transport reports none.
var DefaultTerminalSize = domain.TermSize{Cols: 80, Rows: 24}

// AuthMode records how the principal authenticated. It is research metadata
// only: it never grants privileges inside the simulation.
type AuthMode string

const (
	AuthModePublic AuthMode = "public"
	AuthModeSecure AuthMode = "secure"
)

// TerminalMetadata is the terminal description the transport reported at
// connect time. Client environment variables are deliberately absent: the
// application never applies client-supplied environment to anything.
type TerminalMetadata struct {
	Term          string          `json:"term"`
	Size          domain.TermSize `json:"size"`
	ClientAddr    string          `json:"client_addr,omitempty"`
	ClientVersion string          `json:"client_version,omitempty"`
}

// OpenSessionRequest is what an adapter passes to accept a connection. One
// accepted shell channel becomes exactly one session.
type OpenSessionRequest struct {
	// Principal is the authenticated durable identity, already resolved by the
	// adapter from the presented username. It is never the displayed name.
	Principal domain.UserID `json:"principal"`
	// AuthMode records how the principal authenticated.
	AuthMode AuthMode `json:"auth_mode"`
	// Terminal is the transport-reported terminal metadata.
	Terminal TerminalMetadata `json:"terminal"`
	// CWD is the session's initial working directory. The adapter resolves the
	// visible home path and its display encoding; the application only records
	// the accepted path as session-local state.
	CWD domain.ValidPath `json:"cwd"`
	// TransportRef is an opaque connection/channel reference for transport
	// research records. It is never the domain identity of the session.
	TransportRef string `json:"transport_ref,omitempty"`
}

// Validate rejects an unusable accept request before any state is created.
func (r OpenSessionRequest) Validate() error {
	if r.Principal.IsZero() {
		return domain.NewValidationError(domain.CodeInvalidIdentity, "session requires an authenticated principal", nil)
	}
	switch r.AuthMode {
	case AuthModePublic, AuthModeSecure:
	default:
		return domain.NewValidationError(domain.CodeInvalidInput, "unknown auth mode", map[string]string{"auth_mode": string(r.AuthMode)})
	}
	if r.CWD == "" {
		return domain.NewValidationError(domain.CodeInvalidPath, "session requires an initial working directory", nil)
	}
	return validateTerminalMetadata(r.Terminal)
}

func validateTerminalMetadata(t TerminalMetadata) error {
	if t.Size.Cols == 0 && t.Size.Rows == 0 {
		return nil // replaced by DefaultTerminalSize at accept time
	}
	if t.Size.Cols > MaxTerminalCols || t.Size.Rows > MaxTerminalRows {
		return domain.NewValidationError(domain.CodeInvalidInput, "terminal size out of bounds", map[string]string{
			"cols": fmt.Sprint(t.Size.Cols),
			"rows": fmt.Sprint(t.Size.Rows),
		})
	}
	return nil
}

// InputKind classifies one accepted semantic input. The transport owns line
// editing; it submits a complete line or a control key, never raw keystrokes
// for the model to interpret.
type InputKind string

const (
	// InputCommand is one complete accepted command line.
	InputCommand InputKind = "command"
	// InputKey is one control key that the shell must act on itself, such as
	// an arrow key inside a foreground interaction.
	InputKey InputKind = "key"
	// InputPaste is one pasted payload delivered as a single input.
	InputPaste InputKind = "paste"
	// InputResize is a new terminal size.
	InputResize InputKind = "resize"
	// InputEOF is end of input on the transport.
	InputEOF InputKind = "eof"
	// InputCancel asks the application to cancel the active turn.
	InputCancel InputKind = "cancel"
)

// InputActionCancel is the action recorded for cancellation input.
const InputActionCancel = "cancel"

// Control keys the application itself interprets rather than forwarding to the
// simulation.
const (
	KeyCtrlC     = "ctrl-c"
	KeyInterrupt = "interrupt"
)

// SessionInput is one inbound semantic event for one session. Session and
// Principal must match the handle they arrive on, so an adapter multiplexing
// several sessions cannot cross-deliver input by mistake.
type SessionInput struct {
	Session   domain.SessionID
	Principal domain.UserID
	// Sequence is the transport's monotonic per-channel input counter. It makes
	// a replayed input recognisable: re-submitting the same sequence returns the
	// turn the first submission established instead of starting a second one.
	// Only input that starts a turn requires one; control keys, resize, EOF,
	// and cancellation have no turn to duplicate.
	Sequence uint64
	Kind     InputKind
	Command  string           // InputCommand
	Key      string           // InputKey, e.g. "ctrl-c", "up"
	Text     string           // InputPaste
	Size     *domain.TermSize // InputResize
}

// Validate enforces the per-kind invariants of one input.
func (in SessionInput) Validate() error {
	if in.Session.IsZero() {
		return domain.NewValidationError(domain.CodeInvalidIdentity, "input requires a session", nil)
	}
	if in.Principal.IsZero() {
		return domain.NewValidationError(domain.CodeInvalidIdentity, "input requires a principal", nil)
	}
	if in.StartsTurn() && in.Sequence == 0 {
		return domain.NewValidationError(domain.CodeInvalidInput, "input requires a monotonic sequence number", nil)
	}
	switch in.Kind {
	case InputCommand:
		if strings.TrimSpace(in.Command) == "" {
			return domain.NewValidationError(domain.CodeInvalidInput, "command input must not be empty", nil)
		}
		if len(in.Command) > MaxCommandBytes {
			return domain.NewValidationError(domain.CodeOutputTooLarge, "command exceeds the accepted length", map[string]string{
				"bytes": fmt.Sprint(len(in.Command)),
				"limit": fmt.Sprint(MaxCommandBytes),
			})
		}
	case InputKey:
		if in.Key == "" {
			return domain.NewValidationError(domain.CodeInvalidInput, "key input requires a key", nil)
		}
	case InputPaste:
		if in.Text == "" {
			return domain.NewValidationError(domain.CodeInvalidInput, "paste input must not be empty", nil)
		}
		if len(in.Text) > MaxPasteBytes {
			return domain.NewValidationError(domain.CodeOutputTooLarge, "paste exceeds the accepted length", map[string]string{
				"bytes": fmt.Sprint(len(in.Text)),
				"limit": fmt.Sprint(MaxPasteBytes),
			})
		}
	case InputResize:
		if in.Size == nil {
			return domain.NewValidationError(domain.CodeInvalidInput, "resize input requires a size", nil)
		}
		if err := validateTerminalMetadata(TerminalMetadata{Size: *in.Size}); err != nil {
			return err
		}
	case InputEOF, InputCancel:
	default:
		return domain.NewValidationError(domain.CodeInvalidInput, "unknown input kind", map[string]string{"kind": string(in.Kind)})
	}
	return nil
}

// StartsTurn reports whether this input establishes a new turn. Only a command
// or a paste produces semantic work; control keys, resize, EOF, and
// cancellation are handled by the session itself.
func (in SessionInput) StartsTurn() bool {
	return in.Kind == InputCommand || in.Kind == InputPaste
}

// normalizeInput maps the transport's control keys onto application intent, so
// Ctrl-C and an explicit signal both become one cancellation request.
func normalizeInput(in SessionInput) (SessionInput, error) {
	if err := in.Validate(); err != nil {
		return SessionInput{}, err
	}
	if in.Kind == InputKey && (in.Key == KeyCtrlC || in.Key == KeyInterrupt) {
		in.Kind = InputCancel
	}
	return in, nil
}

// OutputKind classifies one accepted output item. Every item carries either
// exact text or an immutable content reference; the application never emits a
// raw control sequence that it has not rendered or validated itself.
type OutputKind string

const (
	// OutputText is accepted plain text for the session terminal.
	OutputText OutputKind = "text"
	// OutputContent is a chunk of already accepted immutable content.
	OutputContent OutputKind = "content"
	// OutputFrame is a validated full-screen view rendered to exact bytes.
	OutputFrame OutputKind = "frame"
	// OutputPrompt asks the terminal to (re)draw the shell prompt from the
	// accepted session state.
	OutputPrompt OutputKind = "prompt"
)

// PromptState is the accepted prompt position of a session. The terminal
// renderer owns prompt wording; the application supplies the facts.
type PromptState struct {
	CWD      domain.ValidPath `json:"cwd"`
	ExitCode int              `json:"exit_code"`
	// App carries the foreground application's prompt and is nil while the
	// shell itself is foreground.
	App *AppPrompt `json:"app,omitempty"`
}

// AppPrompt is what a terminal renderer needs to draw the prompt of a
// foreground application: which application is running and what continuation
// prompt it declared. Prompt is data produced by the generated application; the
// terminal layer sanitizes it before display.
type AppPrompt struct {
	AppID  domain.AppID `json:"app_id"`
	Name   string       `json:"name"`
	Prompt string       `json:"prompt"`
}

// SessionOutput is one accepted output item. Sequence is session-local and
// matches the Sequence an adapter echoes in WriteResult.
type SessionOutput struct {
	Session   domain.SessionID
	Turn      domain.TurnID
	Sequence  uint64
	Kind      OutputKind
	Text      string
	Content   domain.ContentRef
	View      *domain.AppView
	Prompt    *PromptState
	ByteCount int64
}

// OutcomeKind classifies the completion of one turn.
type OutcomeKind string

const (
	OutcomeCompleted   OutcomeKind = "completed"
	OutcomeInterrupted OutcomeKind = "interrupted"
	OutcomeFailed      OutcomeKind = "failed"
)

// SessionOutcome is the terminal result of one turn. Emitted output always
// precedes it; Undelivered counts output items that could not be handed to the
// transport before the turn deadline.
type SessionOutcome struct {
	Session     domain.SessionID
	Turn        domain.TurnID
	Kind        OutcomeKind
	State       TurnState
	ExitStatus  int
	Revision    domain.Revision
	Undelivered int
	Failure     *TurnFailure
}

// WriteStatus is the transport's report for one output item.
type WriteStatus string

const (
	WriteOK      WriteStatus = "ok"
	WriteFailed  WriteStatus = "failed"
	WriteDropped WriteStatus = "dropped"
)

// WriteResult is what the adapter reports after handing one output item to its
// transport. It is research metadata: a reported success means the bytes were
// handed to the transport, never that a person read them.
type WriteResult struct {
	Session   domain.SessionID
	Turn      domain.TurnID
	Sequence  uint64
	Status    WriteStatus
	ByteCount int64
	Error     string
}

// EndReason is why a session ended.
type EndReason string

const (
	EndExit       EndReason = "exit"
	EndDisconnect EndReason = "disconnect"
	EndTimeout    EndReason = "timeout"
	EndError      EndReason = "error"
	EndKilled     EndReason = "killed"
	EndShutdown   EndReason = "shutdown"
)

// ActiveTurn is the read-only view of the turn currently running.
type ActiveTurn struct {
	ID         domain.TurnID
	State      TurnState
	Generation uint64
	Attempts   int
	Rebases    int
}

// SessionSnapshot is the read-only view of a session. It is safe to read from
// any goroutine at any time.
type SessionSnapshot struct {
	ID             domain.SessionID
	Principal      domain.UserID
	AuthMode       AuthMode
	Terminal       TerminalMetadata
	CWD            domain.ValidPath
	Foreground     ForegroundState
	ExitStatus     int
	TurnsCompleted int
	ActiveTurn     *ActiveTurn
	QueuedTurns    int
	// RecordingHealthy is false once durable research recording failed. The
	// session then refuses new semantic work instead of running unrecorded.
	RecordingHealthy bool
	// PolicyRevision is the scope-policy revision of the last turn this
	// session pinned. It changes when an administrator changes sharing.
	PolicyRevision int64
	Ended          bool
	EndReason      EndReason
}

// Shell is the application-facing session handle. One handle belongs to one
// accepted shell channel. Every method is safe for concurrent use by the
// adapter's reader and writer goroutines.
//
// An adapter must drain both Outputs and Outcomes continuously. Accept blocks
// until the input is durably recorded and classified, so it must run off the
// transport's write path.
type Shell interface {
	// ID returns the session identity. Each SSH shell channel gets its own.
	ID() domain.SessionID
	// Principal returns the authenticated identity of this session.
	Principal() domain.UserID
	// Snapshot returns the current read-only session state.
	Snapshot() SessionSnapshot
	// Accept records one semantic input. For a command or paste it returns the
	// established turn; other input kinds return the zero turn ID.
	Accept(ctx context.Context, in SessionInput) (domain.TurnID, error)
	// CancelActive interrupts the running turn, if any, and returns whether a
	// turn was interrupted.
	CancelActive(ctx context.Context, reason string) (bool, error)
	// End ends the session with the given reason.
	End(ctx context.Context, reason EndReason) error
	// Outputs yields accepted output items in session order.
	Outputs() <-chan SessionOutput
	// Outcomes yields one terminal result per turn, in completion order.
	Outcomes() <-chan SessionOutcome
	// ReportWrite records the transport outcome for one emitted output item.
	ReportWrite(ctx context.Context, result WriteResult) error
	// Done is closed once the session has ended and no further output or
	// outcome will be produced.
	Done() <-chan struct{}
}
