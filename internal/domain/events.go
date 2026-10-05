package domain

import (
	"encoding/json"
	"errors"
	"fmt"
)

// EventSchemaVersion is the current event envelope schema version.
const EventSchemaVersion = 1

// EventKind identifies the category of an event (PLAN 10.1 categories).
type EventKind string

const (
	// Session events
	EventKindSessionStart     EventKind = "session.start"
	EventKindSessionEnd       EventKind = "session.end"
	EventKindSessionRecovered EventKind = "session.recovered"

	// Turn events
	EventKindTurnTransition EventKind = "turn.transition"

	// Input events
	EventKindInputRaw      EventKind = "input.raw"
	EventKindInputDecoded  EventKind = "input.decoded"
	EventKindInputAccepted EventKind = "input.accepted"

	// Terminal events
	EventKindTerminalFrame  EventKind = "terminal.frame"
	EventKindTerminalPrompt EventKind = "terminal.prompt"
	EventKindTerminalMode   EventKind = "terminal.mode"
	EventKindTerminalWrite  EventKind = "terminal.write"

	// Model events
	EventKindModelRequest  EventKind = "model.request"
	EventKindModelResponse EventKind = "model.response"
	EventKindModelError    EventKind = "model.error"
	EventKindModelUsage    EventKind = "model.usage"

	// Tool events
	EventKindToolRequest EventKind = "tool.request"
	EventKindToolResult  EventKind = "tool.result"
	EventKindToolError   EventKind = "tool.error"

	// Routing events
	EventKindRouteCandidate EventKind = "route.candidate"
	EventKindRouteSelected  EventKind = "route.selected"
	EventKindRouteFailure   EventKind = "route.failure"
	EventKindRouteCooldown  EventKind = "route.cooldown"
	EventKindRouteProbe     EventKind = "route.probe"
	EventKindRouteRecovery  EventKind = "route.recovery"

	// World events
	EventKindWorldRead        EventKind = "world.read"
	EventKindWorldStage       EventKind = "world.stage"
	EventKindWorldCommit      EventKind = "world.commit"
	EventKindWorldConflict    EventKind = "world.conflict"
	EventKindWorldMaterialize EventKind = "world.materialize"

	// App events
	EventKindAppCreated    EventKind = "app.created"
	EventKindAppExtended   EventKind = "app.extended"
	EventKindAppValidated  EventKind = "app.validated"
	EventKindAppActivated  EventKind = "app.activated"
	EventKindAppRolledBack EventKind = "app.rolled_back"
	EventKindAppSandboxRun EventKind = "app.sandbox_run"

	// Context events
	EventKindContextSummary   EventKind = "context.summary"
	EventKindContextRetrieval EventKind = "context.retrieval"

	// Operations events
	EventKindConfigChange EventKind = "config.change"
	EventKindShutdown     EventKind = "shutdown"
	EventKindExport       EventKind = "export"
	EventKindBackup       EventKind = "backup"
)

// AllEventKinds returns all known event kinds for validation.
func AllEventKinds() []EventKind {
	return []EventKind{
		EventKindSessionStart, EventKindSessionEnd, EventKindSessionRecovered,
		EventKindTurnTransition,
		EventKindInputRaw, EventKindInputDecoded, EventKindInputAccepted,
		EventKindTerminalFrame, EventKindTerminalPrompt, EventKindTerminalMode, EventKindTerminalWrite,
		EventKindModelRequest, EventKindModelResponse, EventKindModelError, EventKindModelUsage,
		EventKindToolRequest, EventKindToolResult, EventKindToolError,
		EventKindRouteCandidate, EventKindRouteSelected, EventKindRouteFailure, EventKindRouteCooldown, EventKindRouteProbe, EventKindRouteRecovery,
		EventKindWorldRead, EventKindWorldStage, EventKindWorldCommit, EventKindWorldConflict, EventKindWorldMaterialize,
		EventKindAppCreated, EventKindAppExtended, EventKindAppValidated, EventKindAppActivated, EventKindAppRolledBack, EventKindAppSandboxRun,
		EventKindContextSummary, EventKindContextRetrieval,
		EventKindConfigChange, EventKindShutdown, EventKindExport, EventKindBackup,
	}
}

// IsValidEventKind returns true if the kind is known.
func IsValidEventKind(k EventKind) bool {
	for _, known := range AllEventKinds() {
		if k == known {
			return true
		}
	}
	return false
}

// Provenance records the origin of an event for audit and debugging.
type Provenance struct {
	Source           string `json:"source"`          // "ssh", "model", "tool", "sandbox", "admin", "system"
	Actor            string `json:"actor,omitempty"` // user ID, model route, tool name, etc.
	PromptVersion    string `json:"prompt_version,omitempty"`
	ConfigVersion    string `json:"config_version,omitempty"`
	CatalogueVersion string `json:"catalogue_version,omitempty"`
}

// PayloadRef references an event payload (stored separately if large).
type PayloadRef struct {
	ContentID *ContentID      `json:"content_id,omitempty"` // reference to full payload in content store
	Inline    json.RawMessage `json:"inline,omitempty"`     // small payloads inlined (<=1KB)
	Truncated bool            `json:"truncated,omitempty"`  // true if inline was truncated
}

// EventEnvelope is the immutable envelope for every research event.
type EventEnvelope struct {
	SchemaVersion   int        `json:"schema_version"`   // EventSchemaVersion
	EventID         EventID    `json:"event_id"`         // immutable, globally unique
	SessionID       SessionID  `json:"session_id"`       // originating session
	Sequence        uint64     `json:"sequence"`         // per-session monotonic sequence
	Timestamp       int64      `json:"timestamp"`        // UTC unix milliseconds
	MonotonicOffset int64      `json:"monotonic_offset"` // nanoseconds since process start (for ordering)
	Kind            EventKind  `json:"kind"`
	Payload         PayloadRef `json:"payload"`
	Provenance      Provenance `json:"provenance"`
	// Optional references for correlation. Pointers keep absent references
	// out of the encoding instead of emitting placeholder IDs.
	TurnID       *TurnID       `json:"turn_id,omitempty"`
	AttemptID    *AttemptID    `json:"attempt_id,omitempty"`
	AppVersionID *AppVersionID `json:"app_version_id,omitempty"`
	NodeRevision Revision      `json:"node_revision,omitempty"` // world state revision after this event
}

// NewEventEnvelope creates a new event envelope. Timestamps come from the
// Clock port (wall-clock milliseconds plus the process monotonic offset) so
// event construction stays deterministic under test. EventID is assigned on
// persist; adapters must never reuse an assigned ID.
func NewEventEnvelope(session SessionID, sequence uint64, kind EventKind, provenance Provenance, timestampUnixMilli, monotonicOffsetNanos int64) EventEnvelope {
	return EventEnvelope{
		SchemaVersion:   EventSchemaVersion,
		EventID:         EventID{}, // assigned on persist
		SessionID:       session,
		Sequence:        sequence,
		Timestamp:       timestampUnixMilli,
		MonotonicOffset: monotonicOffsetNanos,
		Kind:            kind,
		Provenance:      provenance,
	}
}

// EventPayload is the interface for typed event payloads.
// Each event kind has a corresponding payload type.
type EventPayload interface {
	EventKind() EventKind
}

// SessionStartPayload records session initialization.
type SessionStartPayload struct {
	UserID         UserID   `json:"user_id"`
	AuthMode       string   `json:"auth_mode"`     // "public" or "secure"
	TerminalType   string   `json:"terminal_type"` // e.g., "xterm-256color"
	TerminalSize   TermSize `json:"terminal_size"`
	ClientAddr     string   `json:"client_addr"`
	SharingEnabled bool     `json:"sharing_enabled"`
}

func (SessionStartPayload) EventKind() EventKind { return EventKindSessionStart }

// SessionEndPayload records session termination.
type SessionEndPayload struct {
	Reason     string `json:"reason"` // "exit", "disconnect", "timeout", "error", "killed"
	TurnsCount int    `json:"turns_count"`
	DurationMs int64  `json:"duration_ms"`
}

func (SessionEndPayload) EventKind() EventKind { return EventKindSessionEnd }

// TurnTransitionPayload records one turn lifecycle transition (PLAN 7.1). Every
// transition carries a session-local sequence and a durable event; a lifecycle
// note that does not move the turn repeats the same From and To and explains
// itself in Reason.
type TurnTransitionPayload struct {
	From TurnState `json:"from"`
	To   TurnState `json:"to"`
	// Generation distinguishes a retry, which starts a new generation of the
	// same state, from the first attempt at it.
	Generation uint64 `json:"generation"`
	// Attempt is a pointer because the transitions before the first generation
	// have no attempt yet: a turn is received and queued before any provider
	// attempt exists. Emitting a zero identity instead would produce a payload
	// the domain identity format cannot decode.
	Attempt *AttemptID `json:"attempt_id,omitempty"`
	// ConfigVersion, PromptVersion, and CatalogueVersion repeat the pinned
	// snapshot so a turn can be audited against the configuration it used even
	// after later reloads.
	ConfigVersion    int64  `json:"config_version,omitempty"`
	PromptVersion    string `json:"prompt_version,omitempty"`
	CatalogueVersion string `json:"catalogue_version,omitempty"`
	// Reason explains retries, discards, and interruptions.
	Reason string `json:"reason,omitempty"`
}

func (TurnTransitionPayload) EventKind() EventKind { return EventKindTurnTransition }

// InputRawPayload records raw bytes from the transport.
type InputRawPayload struct {
	Bytes     []byte `json:"bytes"` // base64 encoded in JSON
	ByteCount int    `json:"byte_count"`
	IsPaste   bool   `json:"is_paste"`
}

func (InputRawPayload) EventKind() EventKind { return EventKindInputRaw }

// InputDecodedPayload records decoded terminal events.
type InputDecodedPayload struct {
	Events []TerminalEvent `json:"events"`
}

func (InputDecodedPayload) EventKind() EventKind { return EventKindInputDecoded }

// InputAcceptedPayload records an accepted semantic action.
type InputAcceptedPayload struct {
	Action  string    `json:"action"` // "command", "key", "resize", "paste", "eof", "cancel"
	Command string    `json:"command,omitempty"`
	Key     string    `json:"key,omitempty"`
	Resize  *TermSize `json:"resize,omitempty"`
}

func (InputAcceptedPayload) EventKind() EventKind { return EventKindInputAccepted }

// TerminalEvent represents a decoded terminal input event.
type TerminalEvent struct {
	Type      string `json:"type"` // "key", "resize", "paste"
	Key       string `json:"key,omitempty"`
	Chars     string `json:"chars,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	PasteText string `json:"paste_text,omitempty"`
}

// TermSize represents terminal dimensions.
type TermSize struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// TerminalFramePayload records an emitted terminal frame/output.
type TerminalFramePayload struct {
	FrameID    string     `json:"frame_id"`    // sequential per session
	ContentRef ContentRef `json:"content_ref"` // reference to frame bytes
	IsPrompt   bool       `json:"is_prompt"`
	PromptText string     `json:"prompt_text,omitempty"`
	Mode       string     `json:"mode,omitempty"` // "line", "app", "alt"
}

func (TerminalFramePayload) EventKind() EventKind { return EventKindTerminalFrame }

// TerminalPromptPayload records a prompt emission.
type TerminalPromptPayload struct {
	Prompt   string `json:"prompt"`
	CWD      string `json:"cwd"`
	ExitCode int    `json:"exit_code"`
}

func (TerminalPromptPayload) EventKind() EventKind { return EventKindTerminalPrompt }

// TerminalWriteOutcomePayload records what the transport did with one accepted
// output item. A recorded success means the bytes were handed to the transport,
// never that a person read them (PLAN 10.1), so the record stays transport
// research metadata rather than delivery proof.
type TerminalWriteOutcomePayload struct {
	OutputSequence uint64 `json:"output_sequence"`
	Status         string `json:"status"`
	ByteCount      int64  `json:"byte_count"`
	Error          string `json:"error,omitempty"`
}

func (TerminalWriteOutcomePayload) EventKind() EventKind { return EventKindTerminalWrite }

// ModelRequestPayload records a sanitized model request.
type ModelRequestPayload struct {
	RouteID      RouteID         `json:"route_id"`
	AccountID    AccountID       `json:"account_id"`
	Messages     json.RawMessage `json:"messages"` // canonical message format
	Tools        json.RawMessage `json:"tools"`    // canonical tool definitions
	MaxTokens    int             `json:"max_tokens"`
	Temperature  *float64        `json:"temperature,omitempty"`
	DeadlineMs   int64           `json:"deadline_ms"`
	ContextBytes int             `json:"context_bytes"` // estimated
}

func (ModelRequestPayload) EventKind() EventKind { return EventKindModelRequest }

// ModelResponsePayload records a model response.
type ModelResponsePayload struct {
	RouteID      RouteID         `json:"route_id"`
	AccountID    AccountID       `json:"account_id"`
	FinishReason string          `json:"finish_reason"`
	Text         string          `json:"text,omitempty"`
	ToolCalls    json.RawMessage `json:"tool_calls,omitempty"`
	Usage        Usage           `json:"usage"`
	LatencyMs    int64           `json:"latency_ms"`
}

func (ModelResponsePayload) EventKind() EventKind { return EventKindModelResponse }

// ModelErrorPayload records a model error.
type ModelErrorPayload struct {
	RouteID      RouteID      `json:"route_id"`
	AccountID    AccountID    `json:"account_id"`
	ErrorClass   FailureClass `json:"error_class"`
	ErrorMessage string       `json:"error_message"`
	Retryable    bool         `json:"retryable"`
	StatusCode   int          `json:"status_code,omitempty"`
}

func (ModelErrorPayload) EventKind() EventKind { return EventKindModelError }

// Usage records token usage.
type Usage struct {
	PromptTokens     int      `json:"prompt_tokens"`
	CompletionTokens int      `json:"completion_tokens"`
	TotalTokens      int      `json:"total_tokens"`
	EstimatedCostUSD *float64 `json:"estimated_cost_usd,omitempty"` // if available
}

// ToolRequestPayload records a tool invocation request.
type ToolRequestPayload struct {
	ToolName  string          `json:"tool_name"`
	Arguments json.RawMessage `json:"arguments"`
	Scope     Scope           `json:"scope"`
	TimeoutMs int64           `json:"timeout_ms"`
}

func (ToolRequestPayload) EventKind() EventKind { return EventKindToolRequest }

// ToolResultPayload records a tool result.
type ToolResultPayload struct {
	ToolName      string          `json:"tool_name"`
	Result        json.RawMessage `json:"result"`
	Truncated     bool            `json:"truncated"`
	ReferencedRef ContentRef      `json:"referenced_ref,omitzero"` // full result reference
}

func (ToolResultPayload) EventKind() EventKind { return EventKindToolResult }

// ToolErrorPayload records a tool error.
type ToolErrorPayload struct {
	ToolName     string `json:"tool_name"`
	ErrorClass   string `json:"error_class"` // "validation", "not_found", "denied", "limit", "unavailable"
	ErrorMessage string `json:"error_message"`
}

func (ToolErrorPayload) EventKind() EventKind { return EventKindToolError }

// RouteCandidatePayload records a considered route candidate.
type RouteCandidatePayload struct {
	Candidates []RouteCandidate `json:"candidates"`
	Selected   RouteID          `json:"selected"`
	Tier       int              `json:"tier"`
}

func (RouteCandidatePayload) EventKind() EventKind { return EventKindRouteCandidate }

// RouteSelectedPayload records the final route selection.
type RouteSelectedPayload struct {
	RouteID   RouteID   `json:"route_id"`
	AccountID AccountID `json:"account_id"`
	Tier      int       `json:"tier"`
	Reason    string    `json:"reason"`
}

func (RouteSelectedPayload) EventKind() EventKind { return EventKindRouteSelected }

// RouteFailurePayload records a route failure.
type RouteFailurePayload struct {
	RouteID      RouteID      `json:"route_id"`
	AccountID    AccountID    `json:"account_id"`
	FailureClass FailureClass `json:"failure_class"`
	ErrorMessage string       `json:"error_message"`
	Attempt      int          `json:"attempt"`
	WillRetry    bool         `json:"will_retry"`
	NextRoute    *RouteID     `json:"next_route,omitempty"`
}

func (RouteFailurePayload) EventKind() EventKind { return EventKindRouteFailure }

// RouteCooldownPayload records a route entering cooldown.
type RouteCooldownPayload struct {
	RouteID     RouteID    `json:"route_id"`
	AccountID   *AccountID `json:"account_id,omitempty"`
	Scope       string     `json:"scope"` // "credential", "account", "route", "product", "provider"
	Reason      string     `json:"reason"`
	CooldownMs  int64      `json:"cooldown_ms"`
	NextProbeMs int64      `json:"next_probe_ms,omitempty"`
}

func (RouteCooldownPayload) EventKind() EventKind { return EventKindRouteCooldown }

// RouteProbePayload records a health probe.
type RouteProbePayload struct {
	RouteID   RouteID    `json:"route_id"`
	AccountID *AccountID `json:"account_id,omitempty"`
	Result    string     `json:"result"` // "healthy", "failed", "timeout"
	LatencyMs int64      `json:"latency_ms"`
}

func (RouteProbePayload) EventKind() EventKind { return EventKindRouteProbe }

// RouteRecoveryPayload records a route recovery.
type RouteRecoveryPayload struct {
	RouteID       RouteID    `json:"route_id"`
	AccountID     *AccountID `json:"account_id,omitempty"`
	PreviousState string     `json:"previous_state"`
}

func (RouteRecoveryPayload) EventKind() EventKind { return EventKindRouteRecovery }

// WorldReadPayload records a world read operation.
type WorldReadPayload struct {
	Scope       Scope       `json:"scope"`
	NodeIDs     []NodeID    `json:"node_ids,omitempty"`
	Paths       []ValidPath `json:"paths,omitempty"`
	ResultCount int         `json:"result_count"`
	DurationMs  int64       `json:"duration_ms"`
}

func (WorldReadPayload) EventKind() EventKind { return EventKindWorldRead }

// WorldStagePayload records staged mutations.
type WorldStagePayload struct {
	ChangeSet ChangeSet `json:"change_set"`
}

func (WorldStagePayload) EventKind() EventKind { return EventKindWorldStage }

// WorldCommitPayload records a committed world change.
type WorldCommitPayload struct {
	ChangeSet     ChangeSet   `json:"change_set"`
	CommittedRev  Revision    `json:"committed_rev"`
	ContentHashes []ContentID `json:"content_hashes"`
}

func (WorldCommitPayload) EventKind() EventKind { return EventKindWorldCommit }

// WorldConflictPayload records a conflict on commit.
type WorldConflictPayload struct {
	ConflictingNode NodeID   `json:"conflicting_node"`
	ExpectedRev     Revision `json:"expected_rev"`
	ActualRev       Revision `json:"actual_rev"`
	RebaseAttempt   int      `json:"rebase_attempt"`
}

func (WorldConflictPayload) EventKind() EventKind { return EventKindWorldConflict }

// WorldMaterializePayload records a materialization of a new path.
type WorldMaterializePayload struct {
	Path      ValidPath   `json:"path"`
	Namespace NamespaceID `json:"namespace"`
	NodeID    NodeID      `json:"node_id"`
	Source    string      `json:"source"` // "generation", "extension", "baseline"
}

func (WorldMaterializePayload) EventKind() EventKind { return EventKindWorldMaterialize }

// AppCreatedPayload records a new app artifact creation.
type AppCreatedPayload struct {
	AppID        AppID           `json:"app_id"`
	VersionID    AppVersionID    `json:"version_id"`
	CommandNames []string        `json:"command_names"`
	Owner        UserID          `json:"owner"`
	Scope        Scope           `json:"scope"`
	Manifest     json.RawMessage `json:"manifest"`
}

func (AppCreatedPayload) EventKind() EventKind { return EventKindAppCreated }

// AppExtendedPayload records an app extension.
type AppExtendedPayload struct {
	AppID       AppID        `json:"app_id"`
	PrevVersion AppVersionID `json:"prev_version"`
	NewVersion  AppVersionID `json:"new_version"`
	Reason      string       `json:"reason"`
	Changes     string       `json:"changes"`
}

func (AppExtendedPayload) EventKind() EventKind { return EventKindAppExtended }

// AppValidatedPayload records a validation result.
type AppValidatedPayload struct {
	VersionID AppVersionID `json:"version_id"`
	Passed    bool         `json:"passed"`
	Issues    []string     `json:"issues,omitempty"`
}

func (AppValidatedPayload) EventKind() EventKind { return EventKindAppValidated }

// AppActivatedPayload records an app version activation.
type AppActivatedPayload struct {
	AppID      AppID         `json:"app_id"`
	VersionID  AppVersionID  `json:"version_id"`
	PreviousID *AppVersionID `json:"previous_id,omitempty"`
}

func (AppActivatedPayload) EventKind() EventKind { return EventKindAppActivated }

// AppRolledBackPayload records a rollback.
type AppRolledBackPayload struct {
	AppID       AppID        `json:"app_id"`
	FromVersion AppVersionID `json:"from_version"`
	ToVersion   AppVersionID `json:"to_version"`
	Reason      string       `json:"reason"`
}

func (AppRolledBackPayload) EventKind() EventKind { return EventKindAppRolledBack }

// AppSandboxRunPayload records a sandbox execution.
type AppSandboxRunPayload struct {
	VersionID   AppVersionID    `json:"version_id"`
	EventType   string          `json:"event_type"` // "input", "timer", "resize", "ai_request"
	Input       json.RawMessage `json:"input"`
	Result      json.RawMessage `json:"result"`
	DurationMs  int64           `json:"duration_ms"`
	MemoryBytes int64           `json:"memory_bytes"`
	Exited      bool            `json:"exited"`
}

func (AppSandboxRunPayload) EventKind() EventKind { return EventKindAppSandboxRun }

// ContextSummaryPayload records a summary creation.
type ContextSummaryPayload struct {
	SummaryID    string    `json:"summary_id"`
	SourceEvents []EventID `json:"source_events"`
	Scope        Scope     `json:"scope"`
	TokenCount   int       `json:"token_count"`
}

func (ContextSummaryPayload) EventKind() EventKind { return EventKindContextSummary }

// ContextRetrievalPayload records a retrieval operation.
type ContextRetrievalPayload struct {
	Query       string `json:"query"`
	Scope       Scope  `json:"scope"`
	ResultCount int    `json:"result_count"`
	Truncated   bool   `json:"truncated"`
	DurationMs  int64  `json:"duration_ms"`
}

func (ContextRetrievalPayload) EventKind() EventKind { return EventKindContextRetrieval }

// ConfigChangePayload records a configuration change.
type ConfigChangePayload struct {
	ConfigVersion int64    `json:"config_version"`
	ChangedKeys   []string `json:"changed_keys"`
	Actor         string   `json:"actor"`
}

func (ConfigChangePayload) EventKind() EventKind { return EventKindConfigChange }

// ShutdownPayload records a service shutdown.
type ShutdownPayload struct {
	Reason         string `json:"reason"` // "signal", "admin", "error", "restart"
	Graceful       bool   `json:"graceful"`
	ActiveSessions int    `json:"active_sessions"`
}

func (ShutdownPayload) EventKind() EventKind { return EventKindShutdown }

// ExportPayload records an export operation.
type ExportPayload struct {
	ExportID   string `json:"export_id"`
	Format     string `json:"format"` // "jsonl", "transcript", "asciicast"
	Scope      string `json:"scope"`  // "session", "user", "timerange", "experiment"
	EventCount int64  `json:"event_count"`
	SizeBytes  int64  `json:"size_bytes"`
	Checksum   string `json:"checksum"`
}

func (ExportPayload) EventKind() EventKind { return EventKindExport }

// BackupPayload records a backup operation.
type BackupPayload struct {
	BackupID   string `json:"backup_id"`
	SizeBytes  int64  `json:"size_bytes"`
	Checksum   string `json:"checksum"`
	DurationMs int64  `json:"duration_ms"`
	Success    bool   `json:"success"`
}

func (BackupPayload) EventKind() EventKind { return EventKindBackup }

// ErrInvalidEventKind is returned for unknown event kinds.
var ErrInvalidEventKind = errors.New("invalid event kind")

// UnmarshalEventPayload unmarshals a payload based on its event kind.
func UnmarshalEventPayload(kind EventKind, data []byte) (EventPayload, error) {
	var payload EventPayload
	switch kind {
	case EventKindSessionStart:
		payload = &SessionStartPayload{}
	case EventKindSessionEnd:
		payload = &SessionEndPayload{}
	case EventKindTurnTransition:
		payload = &TurnTransitionPayload{}
	case EventKindInputRaw:
		payload = &InputRawPayload{}
	case EventKindInputDecoded:
		payload = &InputDecodedPayload{}
	case EventKindInputAccepted:
		payload = &InputAcceptedPayload{}
	case EventKindTerminalFrame:
		payload = &TerminalFramePayload{}
	case EventKindTerminalPrompt:
		payload = &TerminalPromptPayload{}
	case EventKindTerminalWrite:
		payload = &TerminalWriteOutcomePayload{}
	case EventKindModelRequest:
		payload = &ModelRequestPayload{}
	case EventKindModelResponse:
		payload = &ModelResponsePayload{}
	case EventKindModelError:
		payload = &ModelErrorPayload{}
	case EventKindToolRequest:
		payload = &ToolRequestPayload{}
	case EventKindToolResult:
		payload = &ToolResultPayload{}
	case EventKindToolError:
		payload = &ToolErrorPayload{}
	case EventKindRouteCandidate:
		payload = &RouteCandidatePayload{}
	case EventKindRouteSelected:
		payload = &RouteSelectedPayload{}
	case EventKindRouteFailure:
		payload = &RouteFailurePayload{}
	case EventKindRouteCooldown:
		payload = &RouteCooldownPayload{}
	case EventKindRouteProbe:
		payload = &RouteProbePayload{}
	case EventKindRouteRecovery:
		payload = &RouteRecoveryPayload{}
	case EventKindWorldRead:
		payload = &WorldReadPayload{}
	case EventKindWorldStage:
		payload = &WorldStagePayload{}
	case EventKindWorldCommit:
		payload = &WorldCommitPayload{}
	case EventKindWorldConflict:
		payload = &WorldConflictPayload{}
	case EventKindWorldMaterialize:
		payload = &WorldMaterializePayload{}
	case EventKindAppCreated:
		payload = &AppCreatedPayload{}
	case EventKindAppExtended:
		payload = &AppExtendedPayload{}
	case EventKindAppValidated:
		payload = &AppValidatedPayload{}
	case EventKindAppActivated:
		payload = &AppActivatedPayload{}
	case EventKindAppRolledBack:
		payload = &AppRolledBackPayload{}
	case EventKindAppSandboxRun:
		payload = &AppSandboxRunPayload{}
	case EventKindContextSummary:
		payload = &ContextSummaryPayload{}
	case EventKindContextRetrieval:
		payload = &ContextRetrievalPayload{}
	case EventKindConfigChange:
		payload = &ConfigChangePayload{}
	case EventKindShutdown:
		payload = &ShutdownPayload{}
	case EventKindExport:
		payload = &ExportPayload{}
	case EventKindBackup:
		payload = &BackupPayload{}
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidEventKind, kind)
	}
	if err := json.Unmarshal(data, payload); err != nil {
		return nil, err
	}
	return payload, nil
}
