// Package editor implements line-mode command editing (PLAN 6.1): a local
// buffer with a cursor, grapheme-aware erasure, the common Emacs movement and
// erase keys, history navigation, bracketed multiline paste, selection of
// cached completions, and the prompt-region redraw the local layer needs.
//
// Editing never calls a model and never touches the simulated world. It
// reports what a key asked for through an Action, so the session decides
// whether a submission runs as a command, feeds a continuation prompt, or
// opens a completion request against committed facts.
package editor

// Action reports what a key press asked the session to do.
type Action int

const (
	// ActionNone means the key changed nothing.
	ActionNone Action = iota
	// ActionModified means the draft changed and must be redrawn.
	ActionModified
	// ActionSubmit means the draft was accepted as input; Result.Text holds it.
	ActionSubmit
	// ActionCancel means the draft and any pending completion were discarded.
	ActionCancel
	// ActionEOF means the end of input arrived with an empty draft, which
	// closes a session rather than editing it.
	ActionEOF
	// ActionRequestCompletion means the user asked for completion candidates.
	// The session answers with ApplyCompletion when it has facts to offer.
	ActionRequestCompletion
	// ActionUnhandled means the key has no local meaning. The session may map it
	// to an application action; the editor leaves the draft untouched.
	ActionUnhandled
	// ActionClearScreen means the user asked for a repaint.
	ActionClearScreen
)

// String returns the action name.
func (a Action) String() string {
	switch a {
	case ActionNone:
		return "none"
	case ActionModified:
		return "modified"
	case ActionSubmit:
		return "submit"
	case ActionCancel:
		return "cancel"
	case ActionEOF:
		return "eof"
	case ActionRequestCompletion:
		return "request-completion"
	case ActionUnhandled:
		return "unhandled"
	case ActionClearScreen:
		return "clear-screen"
	default:
		return "unknown"
	}
}

// Result reports what one event changed.
type Result struct {
	Action Action
	// Text holds the submitted draft for ActionSubmit.
	Text string
	// Inserted counts the runes the event added to the draft.
	Inserted int
	// Truncated reports that an insertion reached the draft limit and was cut
	// short, so the caller can tell the user instead of losing input silently.
	Truncated bool
}
