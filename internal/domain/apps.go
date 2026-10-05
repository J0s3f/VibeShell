package domain

import (
	"encoding/json"
	"errors"
	"fmt"
)

// AppABIVersion is the current application ABI version.
const AppABIVersion = 1

// MaxAppPromptBytes bounds the continuation prompt a generated application may
// ask the shell to show while it is foreground (schema maxLength 64). The prompt
// is data, not a control sequence: the terminal layer sanitizes it like any
// other artifact-supplied text, and the bound keeps one line of output from
// turning into a full screen of prompt.
const MaxAppPromptBytes = 64

// AppManifest describes a generated application's static properties.
type AppManifest struct {
	ABIVersion   int             `json:"abi_version"`
	CommandNames []string        `json:"command_names"` // primary + aliases
	Description  string          `json:"description"`
	StateSchema  json.RawMessage `json:"state_schema"` // JSON Schema for state
	Capabilities []string        `json:"capabilities"` // simulated capabilities requested
	Entrypoint   string          `json:"entrypoint"`   // JS function name
	Version      string          `json:"version"`      // semantic version
}

// AppArtifact is an immutable generated application artifact.
type AppArtifact struct {
	AppID         AppID            `json:"app_id"`
	VersionID     AppVersionID     `json:"version_id"`
	ParentVersion *AppVersionID    `json:"parent_version,omitempty"`
	Manifest      AppManifest      `json:"manifest"`
	Source        string           `json:"source"`                 // JavaScript source code
	SourceHash    ContentID        `json:"source_hash"`            // hash of source
	Owner         UserID           `json:"owner"`                  // creating user
	Scope         Scope            `json:"scope"`                  // user/shared
	CreatedAt     int64            `json:"created_at"`             // unix milliseconds
	Provenance    Provenance       `json:"provenance"`             // model route, prompt version, etc.
	Validation    ValidationResult `json:"validation"`             // validation results
	ActivatedAt   int64            `json:"activated_at,omitempty"` // unix milliseconds, 0 if not activated
}

// ValidationResult records the outcome of artifact validation.
type ValidationResult struct {
	Passed           bool         `json:"passed"`
	Issues           []string     `json:"issues,omitempty"`
	TestResults      []TestResult `json:"test_results,omitempty"`
	ValidatedAt      int64        `json:"validated_at"`
	ValidatorVersion string       `json:"validator_version"`
}

// TestResult records a single test outcome.
type TestResult struct {
	Name       string `json:"name"`
	Passed     bool   `json:"passed"`
	Output     string `json:"output,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

// AppState is the explicit serializable state for a generated application.
type AppState struct {
	SessionState json.RawMessage `json:"session_state,omitempty"` // per-session ephemeral state
	UserState    json.RawMessage `json:"user_state,omitempty"`    // durable user-scoped state
	SharedState  json.RawMessage `json:"shared_state,omitempty"`  // durable shared-scoped state
}

// AppEvent is an input event to a generated application.
type AppEvent struct {
	EventType string          `json:"event_type"` // "input", "timer", "resize", "ai_request", "world_change"
	Payload   json.RawMessage `json:"payload"`
	Timestamp int64           `json:"timestamp"` // unix milliseconds
	SessionID *SessionID      `json:"session_id,omitempty"`
	TurnID    *TurnID         `json:"turn_id,omitempty"`
}

// AppResult is the output from a generated application execution.
//
// An app that sets neither AwaitingInput nor Exited is one-shot: it answers the
// line it was invoked with and the shell prompt returns immediately. A
// line-based interactive app instead sets AwaitingInput on every line but the
// last; the shell keeps the app foreground and delivers each subsequent line as
// another AppEvent with EventType AppEventInput and the payload
// {"line": <string>, "args": [<string>, ...]}, where "line" is the whole
// submitted line and "args" is that line split into arguments. The app ends the
// session by returning Exited.
type AppResult struct {
	NewState   AppState           `json:"new_state"`
	View       AppView            `json:"view"`                 // declarative view for renderer
	Effects    []AppEffect        `json:"effects"`              // proposed world mutations
	WorldReads []WorldReadRequest `json:"world_reads"`          // requested world reads
	AIRequest  *AIRequest         `json:"ai_request,omitempty"` // request for AI extension
	Exited     bool               `json:"exited"`               // app requests termination
	ExitCode   int                `json:"exit_code,omitempty"`
	// AwaitingInput asks the shell to keep this app foreground and deliver the
	// next line as another input event. It cannot be combined with Exited: an
	// app is either waiting for a line or finished.
	AwaitingInput bool `json:"awaiting_input,omitempty"`
	// Prompt is the continuation prompt shown while the app is foreground. It
	// is data, sanitized by the terminal layer; bounded to MaxAppPromptBytes.
	// An empty prompt leaves the shell's own prompt in place.
	Prompt string `json:"prompt,omitempty"`
}

// AppView is a validated declarative view consumed by the trusted renderer.
type AppView struct {
	Mode        string          `json:"mode"`                 // "text", "form", "table", "editor", "pager", "status"
	BufferRef   *ContentRef     `json:"buffer_ref,omitempty"` // reference to buffer content
	Cursor      CursorPosition  `json:"cursor,omitempty"`
	Viewport    Viewport        `json:"viewport,omitempty"`
	StatusLine  string          `json:"status_line,omitempty"`
	KeyBindings []KeyBinding    `json:"key_bindings,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"` // mode-specific data
}

// CursorPosition represents a cursor location.
type CursorPosition struct {
	Row int `json:"row"`
	Col int `json:"col"`
}

// Viewport represents a visible region.
type Viewport struct {
	Top    int `json:"top"`
	Left   int `json:"left"`
	Height int `json:"height"`
	Width  int `json:"width"`
}

// KeyBinding maps a key to a primitive action.
type KeyBinding struct {
	Key    string          `json:"key"`              // e.g., "ctrl-c", "enter", "escape", "up"
	Action string          `json:"action"`           // primitive: "move_cursor", "edit_buffer", "scroll", "search", "mode_switch", "emit_event"
	Params json.RawMessage `json:"params,omitempty"` // action-specific parameters
}

// AppEffect is a proposed simulated-world effect from an app.
type AppEffect struct {
	Mutation Mutation `json:"mutation"`
	Sync     bool     `json:"sync"` // if true, wait for commit before continuing
}

// WorldReadRequest requests a world read from the application.
type WorldReadRequest struct {
	RequestID string    `json:"request_id"`
	Scope     Scope     `json:"scope"`
	Path      ValidPath `json:"path,omitempty"`
	NodeID    *NodeID   `json:"node_id,omitempty"`
	MaxBytes  int64     `json:"max_bytes,omitempty"`
}

// AIRequest is a request from an app for AI assistance/extension.
type AIRequest struct {
	RequestID string          `json:"request_id"`
	Prompt    string          `json:"prompt"`
	Context   json.RawMessage `json:"context"` // app-specific context
	MaxTokens int             `json:"max_tokens"`
	TimeoutMs int64           `json:"timeout_ms"`
}

// ErrInvalidAppEvent is returned for malformed app events.
var ErrInvalidAppEvent = errors.New("invalid app event")

// AppEventType constants for validation.
const (
	AppEventInput       = "input"
	AppEventTimer       = "timer"
	AppEventResize      = "resize"
	AppEventAIRequest   = "ai_request"
	AppEventWorldChange = "world_change"
)

func IsValidAppEventType(t string) bool {
	switch t {
	case AppEventInput, AppEventTimer, AppEventResize, AppEventAIRequest, AppEventWorldChange:
		return true
	default:
		return false
	}
}

// AppViewMode constants for validation.
const (
	AppViewModeText   = "text"
	AppViewModeForm   = "form"
	AppViewModeTable  = "table"
	AppViewModeEditor = "editor"
	AppViewModePager  = "pager"
	AppViewModeStatus = "status"
)

func IsValidAppViewMode(m string) bool {
	switch m {
	case AppViewModeText, AppViewModeForm, AppViewModeTable, AppViewModeEditor, AppViewModePager, AppViewModeStatus:
		return true
	default:
		return false
	}
}

// PrimitiveAction constants for validation.
const (
	ActionMoveCursor   = "move_cursor"
	ActionEditBuffer   = "edit_buffer"
	ActionSelectRange  = "select_range"
	ActionSearchBuffer = "search_buffer"
	ActionScroll       = "scroll"
	ActionUpdateStatus = "update_status"
	ActionModeSwitch   = "mode_switch"
	ActionEmitEvent    = "emit_event"
)

func IsValidPrimitiveAction(a string) bool {
	switch a {
	case ActionMoveCursor, ActionEditBuffer, ActionSelectRange, ActionSearchBuffer,
		ActionScroll, ActionUpdateStatus, ActionModeSwitch, ActionEmitEvent:
		return true
	default:
		return false
	}
}

// AppCapability constants for requested simulated capabilities.
const (
	CapabilityFileRead     = "file.read"
	CapabilityFileWrite    = "file.write"
	CapabilityFileList     = "file.list"
	CapabilityFactRead     = "fact.read"
	CapabilityFactWrite    = "fact.write"
	CapabilityProcessSpawn = "process.spawn" // simulated sub-process
	CapabilityNetwork      = "network"       // simulated network (future)
	CapabilityTime         = "time"          // simulated time access
	CapabilityRandom       = "random"        // simulated randomness
)

func IsValidCapability(c string) bool {
	switch c {
	case CapabilityFileRead, CapabilityFileWrite, CapabilityFileList,
		CapabilityFactRead, CapabilityFactWrite, CapabilityProcessSpawn,
		CapabilityNetwork, CapabilityTime, CapabilityRandom:
		return true
	default:
		return false
	}
}

// ValidateManifest validates an app manifest.
func ValidateManifest(m AppManifest) error {
	if m.ABIVersion != AppABIVersion {
		return fmt.Errorf("unsupported ABI version: %d (current: %d)", m.ABIVersion, AppABIVersion)
	}
	if len(m.CommandNames) == 0 {
		return errors.New("at least one command name required")
	}
	for _, name := range m.CommandNames {
		if name == "" {
			return errors.New("command name cannot be empty")
		}
	}
	if m.Entrypoint == "" {
		return errors.New("entrypoint required")
	}
	for _, cap := range m.Capabilities {
		if !IsValidCapability(cap) {
			return fmt.Errorf("invalid capability: %q", cap)
		}
	}
	return nil
}

// ValidateView validates an app view.
func ValidateView(v AppView) error {
	if !IsValidAppViewMode(v.Mode) {
		return fmt.Errorf("invalid view mode: %q", v.Mode)
	}
	for _, kb := range v.KeyBindings {
		if !IsValidPrimitiveAction(kb.Action) {
			return fmt.Errorf("invalid primitive action: %q", kb.Action)
		}
	}
	return nil
}

// ValidateResult validates an app result.
func ValidateResult(r AppResult) error {
	if err := ValidateView(r.View); err != nil {
		return fmt.Errorf("invalid view: %w", err)
	}
	for _, eff := range r.Effects {
		if err := validateMutation(eff.Mutation); err != nil {
			return fmt.Errorf("invalid effect mutation: %w", err)
		}
	}
	for _, wr := range r.WorldReads {
		if wr.RequestID == "" {
			return errors.New("world read request_id required")
		}
		if wr.MaxBytes < 0 {
			return errors.New("max_bytes cannot be negative")
		}
	}
	if r.AIRequest != nil {
		if r.AIRequest.RequestID == "" {
			return errors.New("ai_request request_id required")
		}
		if r.AIRequest.MaxTokens <= 0 {
			return errors.New("ai_request max_tokens must be positive")
		}
	}
	if r.AwaitingInput && r.Exited {
		return WrapError(
			errors.New("awaiting_input and exited cannot both be set"),
			CategoryValidation, CodeInvalidAppResult,
			"app result asks for another line and for termination",
		)
	}
	if len(r.Prompt) > MaxAppPromptBytes {
		return WrapError(
			fmt.Errorf("prompt is %d bytes, the bound is %d", len(r.Prompt), MaxAppPromptBytes),
			CategoryValidation, CodeInvalidAppResult,
			"app prompt exceeds the prompt bound",
		)
	}
	return nil
}

func validateMutation(m Mutation) error {
	if m.NamespaceID.IsZero() {
		return errors.New("mutation requires namespace_id")
	}
	switch m.Type {
	case MutationCreate:
		if m.Path == "" {
			return errors.New("create mutation requires path")
		}
		if !m.Kind.IsValid() {
			return errors.New("create mutation requires a valid kind")
		}
	case MutationUpdate, MutationDelete, MutationTombstone:
		if m.NodeID == nil || m.NodeID.IsZero() {
			return errors.New("update/delete/tombstone requires node_id")
		}
		if m.ExpectedRev == 0 {
			return errors.New("update/delete/tombstone requires expected_rev")
		}
	default:
		return errors.New("unknown mutation type")
	}
	return nil
}
