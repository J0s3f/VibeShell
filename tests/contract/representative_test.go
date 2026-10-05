package contract_test

import (
	"bytes"
	"encoding/json"
	"errors"
)

// jsonObject models a JSON object whose properties the owning schema leaves
// unconstrained. Using it keeps the representative structures from inventing a
// stricter contract than the shipped schema states.
type jsonObject = map[string]any

// decodeAs unmarshals raw into T as the representative structure of a contract.
// Unknown fields are rejected because every shipped schema sets
// additionalProperties to false where the fields are enumerated.
func decodeAs[T any](raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var value T
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after the JSON document")
	}
	return value, nil
}

// Model request / result, mirroring schemas/model-request-result.schema.json.
type ModelExchange struct {
	Request ModelRequest `json:"request"`
	Result  ModelResult  `json:"result"`
}

type ModelRequest struct {
	RouteID     string         `json:"route_id"`
	AccountID   string         `json:"account_id"`
	Messages    []ModelMessage `json:"messages"`
	Tools       []ModelTool    `json:"tools,omitempty"`
	MaxTokens   int            `json:"max_tokens"`
	Temperature *float64       `json:"temperature,omitempty"`
	DeadlineMS  int64          `json:"deadline_ms"`
	RequestID   string         `json:"request_id"`
}

type ModelMessage struct {
	Role       string          `json:"role"`
	Content    *string         `json:"content,omitempty"`
	ToolCalls  []ModelToolCall `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type ModelToolCall struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Arguments jsonObject `json:"arguments"`
}

type ModelTool struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Parameters  jsonObject `json:"parameters"`
	Strict      *bool      `json:"strict,omitempty"`
}

type ModelResult struct {
	RequestID    string         `json:"request_id"`
	RouteID      string         `json:"route_id"`
	AccountID    string         `json:"account_id"`
	Message      jsonObject     `json:"message,omitempty"`
	FinishReason string         `json:"finish_reason"`
	Usage        ModelUsage     `json:"usage"`
	Error        *ErrorEnvelope `json:"error,omitempty"`
	LatencyMS    int64          `json:"latency_ms"`
	Timestamp    int64          `json:"timestamp"`
}

type ModelUsage struct {
	PromptTokens     int      `json:"prompt_tokens"`
	CompletionTokens int      `json:"completion_tokens"`
	TotalTokens      int      `json:"total_tokens"`
	EstimatedCostUSD *float64 `json:"estimated_cost_usd,omitempty"`
}

type ErrorEnvelope struct {
	Class        string `json:"class"`
	Message      string `json:"message"`
	RouteID      string `json:"route_id,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
	RetryAfterMS *int64 `json:"retry_after_ms,omitempty"`
	StatusCode   *int   `json:"status_code,omitempty"`
	RawError     string `json:"raw_error,omitempty"`
	Timestamp    int64  `json:"timestamp"`
}

// World snapshot / change set, mirroring schemas/world-changeset.schema.json.
type ChangeSet struct {
	Mutations        []Mutation       `json:"mutations"`
	ReadDependencies []ReadDependency `json:"read_dependencies"`
	TurnID           string           `json:"turn_id"`
	AttemptID        string           `json:"attempt_id"`
	Timestamp        int64            `json:"timestamp"`
}

type Mutation struct {
	Type          string        `json:"type"`
	NamespaceID   string        `json:"namespace_id"`
	Path          string        `json:"path"`
	NodeID        string        `json:"node_id,omitempty"`
	ExpectedRev   *int64        `json:"expected_rev,omitempty"`
	Kind          string        `json:"kind,omitempty"`
	Metadata      *FileMetadata `json:"metadata,omitempty"`
	Content       *ContentRef   `json:"content,omitempty"`
	SymlinkTarget string        `json:"symlink_target,omitempty"`
}

type FileMetadata struct {
	Mode          int    `json:"mode"`
	UID           int    `json:"uid"`
	GID           int    `json:"gid"`
	ModTime       int64  `json:"mod_time"`
	AccessTime    int64  `json:"access_time"`
	ChangeTime    int64  `json:"change_time"`
	SymlinkTarget string `json:"symlink_target,omitempty"`
}

type ReadDependency struct {
	NodeID      string `json:"node_id"`
	Revision    int64  `json:"revision"`
	Kind        string `json:"kind"`
	IsAbsence   bool   `json:"is_absence"`
	DirMemberOf string `json:"dir_member_of"`
}

type ContentRef struct {
	Hash      string `json:"hash"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type"`
}

// Research event envelope, mirroring schemas/event-envelope.schema.json.
type EventEnvelope struct {
	SchemaVersion   int             `json:"schema_version"`
	EventID         string          `json:"event_id"`
	SessionID       string          `json:"session_id"`
	Sequence        int64           `json:"sequence"`
	Timestamp       int64           `json:"timestamp"`
	MonotonicOffset int64           `json:"monotonic_offset"`
	Kind            string          `json:"kind"`
	Payload         EnvelopePayload `json:"payload"`
	Provenance      Provenance      `json:"provenance"`
	TurnID          string          `json:"turn_id,omitempty"`
	AttemptID       string          `json:"attempt_id,omitempty"`
	AppVersionID    string          `json:"app_version_id,omitempty"`
	NodeRevision    *int64          `json:"node_revision,omitempty"`
}

type EnvelopePayload struct {
	ContentID string     `json:"content_id,omitempty"`
	Inline    jsonObject `json:"inline,omitempty"`
	Truncated *bool      `json:"truncated,omitempty"`
}

type Provenance struct {
	Source           string `json:"source"`
	Actor            string `json:"actor,omitempty"`
	PromptVersion    string `json:"prompt_version,omitempty"`
	ConfigVersion    string `json:"config_version,omitempty"`
	CatalogueVersion string `json:"catalogue_version,omitempty"`
}

// App artifact / event / result, mirroring schemas/app-abi.schema.json.
type AppExchange struct {
	Artifact AppArtifact `json:"artifact"`
	Event    AppEvent    `json:"event"`
	Result   AppResult   `json:"result"`
}

type AppArtifact struct {
	AppID         string        `json:"app_id"`
	VersionID     string        `json:"version_id"`
	ParentVersion string        `json:"parent_version,omitempty"`
	Manifest      AppManifest   `json:"manifest"`
	Source        string        `json:"source"`
	SourceHash    string        `json:"source_hash"`
	Owner         string        `json:"owner"`
	Scope         string        `json:"scope"`
	CreatedAt     int64         `json:"created_at"`
	Provenance    jsonObject    `json:"provenance"`
	Validation    AppValidation `json:"validation"`
	ActivatedAt   *int64        `json:"activated_at,omitempty"`
}

type AppManifest struct {
	ABIVersion   int        `json:"abi_version"`
	CommandNames []string   `json:"command_names"`
	Description  string     `json:"description"`
	StateSchema  jsonObject `json:"state_schema"`
	Capabilities []string   `json:"capabilities"`
	Entrypoint   string     `json:"entrypoint"`
	Version      string     `json:"version"`
}

type AppValidation struct {
	Passed bool `json:"passed"`
	// Issues is a pointer so that an explicitly empty list, which states that
	// validation found nothing wrong, survives a round trip.
	Issues           *[]string `json:"issues,omitempty"`
	ValidatedAt      int64     `json:"validated_at"`
	ValidatorVersion string    `json:"validator_version"`
}

type AppEvent struct {
	EventType string     `json:"event_type"`
	Payload   jsonObject `json:"payload"`
	Timestamp int64      `json:"timestamp"`
	SessionID string     `json:"session_id,omitempty"`
	TurnID    string     `json:"turn_id,omitempty"`
}

type AppResult struct {
	NewState AppState `json:"new_state"`
	View     AppView  `json:"view"`
	// Effects and WorldReads are pointers so that an explicitly empty list
	// survives a round trip; an empty list is a statement about the run.
	Effects    *[]AppEffect `json:"effects,omitempty"`
	WorldReads *[]WorldRead `json:"world_reads,omitempty"`
	AIRequest  *AIRequest   `json:"ai_request,omitempty"`
	Exited     bool         `json:"exited"`
	ExitCode   *int         `json:"exit_code,omitempty"`
	// AwaitingInput and Prompt are pointers so that "the app did not ask to
	// stay foreground" stays distinguishable from "the app asked and the value
	// was false or empty", which is what a round trip of the optional
	// line-based interactive fields has to preserve.
	AwaitingInput *bool   `json:"awaiting_input,omitempty"`
	Prompt        *string `json:"prompt,omitempty"`
}

type AppState struct {
	SessionState *jsonObject `json:"session_state,omitempty"`
	UserState    *jsonObject `json:"user_state,omitempty"`
	SharedState  *jsonObject `json:"shared_state,omitempty"`
}

type AppView struct {
	Mode        string       `json:"mode"`
	BufferRef   *ContentRef  `json:"buffer_ref,omitempty"`
	Cursor      *ViewCursor  `json:"cursor,omitempty"`
	StatusLine  string       `json:"status_line,omitempty"`
	KeyBindings []KeyBinding `json:"key_bindings,omitempty"`
}

type ViewCursor struct {
	Row int `json:"row"`
	Col int `json:"col"`
}

type KeyBinding struct {
	Key    string `json:"key"`
	Action string `json:"action"`
}

type AppEffect struct {
	Mutation jsonObject `json:"mutation"`
	Sync     bool       `json:"sync"`
}

type WorldRead struct {
	RequestID string `json:"request_id"`
	Scope     string `json:"scope"`
	Path      string `json:"path,omitempty"`
	NodeID    string `json:"node_id,omitempty"`
	MaxBytes  *int   `json:"max_bytes,omitempty"`
}

type AIRequest struct {
	RequestID string     `json:"request_id"`
	Prompt    string     `json:"prompt"`
	Context   jsonObject `json:"context,omitempty"`
	MaxTokens int        `json:"max_tokens"`
	TimeoutMS int64      `json:"timeout_ms"`
}

// Route policy and health record, mirroring schemas/route-policy.schema.json.
type RoutePolicy struct {
	Policy RoutePolicyBody `json:"policy"`
	Health RouteHealth     `json:"health"`
}

type RoutePolicyBody struct {
	Tiers          []RouteTier         `json:"tiers"`
	AccountPools   map[string][]string `json:"account_pools,omitempty"`
	MaxAttempts    int                 `json:"max_attempts"`
	TurnDeadlineMS int64               `json:"turn_deadline_ms"`
	ProbeBudget    ProbeBudget         `json:"probe_budget"`
}

type RouteTier struct {
	Name        string   `json:"name"`
	RouteIDs    []string `json:"route_ids,omitempty"`
	AutoFree    *bool    `json:"auto_free,omitempty"`
	AccountPool string   `json:"account_pool,omitempty"`
	Enabled     bool     `json:"enabled"`
	MinHealthy  *int     `json:"min_healthy,omitempty"`
}

type ProbeBudget struct {
	MaxConcurrentProbes int   `json:"max_concurrent_probes"`
	ProbeIntervalMS     int64 `json:"probe_interval_ms"`
	MaxProbesPerHour    int   `json:"max_probes_per_hour"`
}

type RouteHealth struct {
	RouteID             string `json:"route_id"`
	AccountID           string `json:"account_id,omitempty"`
	Scope               string `json:"scope"`
	State               string `json:"state"`
	FailureClass        string `json:"failure_class,omitempty"`
	FailureMessage      string `json:"failure_message,omitempty"`
	ConsecutiveFailures int64  `json:"consecutive_failures"`
	LastFailure         int64  `json:"last_failure"`
	LastSuccess         *int64 `json:"last_success,omitempty"`
	NextProbeAt         *int64 `json:"next_probe_at,omitempty"`
	CooldownUntil       *int64 `json:"cooldown_until,omitempty"`
	RetryAfter          *int64 `json:"retry_after,omitempty"`
	ProbeCount          *int64 `json:"probe_count,omitempty"`
	UpdatedAt           int64  `json:"updated_at"`
}

// Candidate structures for the contracts that have no shipped schema yet. They
// are proposals, not frozen contracts; see fixtures/README.md.

// Session input and output, proposed shape (PLAN 14, PLAN 4.3).
type SessionInputOutput struct {
	Input  SessionInput  `json:"input"`
	Output SessionOutput `json:"output"`
}

type SessionInput struct {
	SessionID string           `json:"session_id"`
	UserID    string           `json:"user_id"`
	Event     string           `json:"event"`
	Sequence  int64            `json:"sequence"`
	Terminal  TerminalMetadata `json:"terminal"`
	Text      string           `json:"text"`
}

type TerminalMetadata struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

type SessionOutput struct {
	Kind       string        `json:"kind"`
	Frames     []OutputFrame `json:"frames"`
	Prompt     string        `json:"prompt"`
	Outcome    string        `json:"outcome"`
	ExitStatus *int          `json:"exit_status,omitempty"`
}

type OutputFrame struct {
	Text       *string     `json:"text,omitempty"`
	ContentRef *ContentRef `json:"content_ref,omitempty"`
}

// Agent tool call, proposed shape (PLAN 14, PLAN 7.2).
type AgentToolCall struct {
	Name         string     `json:"name"`
	Version      int        `json:"version"`
	InputSchema  jsonObject `json:"input_schema"`
	ScopeContext jsonObject `json:"scope_context"`
	Input        jsonObject `json:"input"`
	Result       ToolResult `json:"result"`
}

type ToolResult struct {
	Status      string `json:"status"`
	Mutations   int    `json:"mutations"`
	ChangeSetID string `json:"change_set_id,omitempty"`
	// Conflicts is a pointer so that an explicitly empty list, which states
	// that no conflict was found, survives a round trip.
	Conflicts *[]string `json:"conflicts,omitempty"`
	Truncated bool      `json:"truncated"`
}

// Retrieval page, proposed shape (PLAN 14, PLAN 10.4).
type RetrievalPage struct {
	Scope      jsonObject      `json:"scope"`
	Filter     jsonObject      `json:"filter"`
	Events     []EventEnvelope `json:"events"`
	NextCursor string          `json:"next_cursor"`
	Truncated  bool            `json:"truncated"`
	QueriedAt  int64           `json:"queried_at"`
}

// Service configuration, proposed shape (PLAN 14, PLAN 11). Prompts is the
// configuration group PLAN 11 requires and configuration.schema.json version 1
// does not yet carry.
type ServiceConfiguration struct {
	Version     int             `json:"version"`
	Identity    jsonObject      `json:"identity"`
	SSH         jsonObject      `json:"ssh"`
	Auth        jsonObject      `json:"auth"`
	Sharing     jsonObject      `json:"sharing"`
	Tiers       []jsonObject    `json:"tiers"`
	Persistence jsonObject      `json:"persistence"`
	Prompts     PromptSelection `json:"prompts"`
}

type PromptSelection struct {
	MOTD                 string `json:"motd"`
	ShellBehavior        string `json:"shell_behavior"`
	AppGeneration        string `json:"app_generation"`
	AppExtension         string `json:"app_extension"`
	WorldMaterialization string `json:"world_materialization"`
	SummaryRepair        string `json:"summary_repair"`
	MOTDLineBudget       int    `json:"motd_line_budget"`
}

// representativeTypes maps the representative_type named in the fixture
// manifest to the structure a fixture of that contract must decode into. A
// manifest entry that names an unregistered type fails the test instead of
// silently skipping the fixture.
var representativeTypes = map[string]func([]byte) (any, error){
	"ModelExchange":        decodeAs[ModelExchange],
	"ChangeSet":            decodeAs[ChangeSet],
	"EventEnvelope":        decodeAs[EventEnvelope],
	"AppExchange":          decodeAs[AppExchange],
	"RoutePolicy":          decodeAs[RoutePolicy],
	"SessionInputOutput":   decodeAs[SessionInputOutput],
	"AgentToolCall":        decodeAs[AgentToolCall],
	"RetrievalPage":        decodeAs[RetrievalPage],
	"ServiceConfiguration": decodeAs[ServiceConfiguration],
}
