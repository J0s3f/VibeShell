package simulation

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// Result bounds for tool inputs and outputs. Adapters may configure smaller
// limits; these are the package-level maxima so a single tool call can never
// grow without bound.
const (
	// DefaultResultLimit is the page size a tool uses when the model does not specify one.
	DefaultResultLimit = 50
	// MaxResultLimit caps any single tool result page.
	MaxResultLimit = 500
	// MaxContentReadBytes bounds one content.read or fact.lookup response.
	MaxContentReadBytes = 64 << 10
	// MaxStageBytes bounds the content of one staged change.
	MaxStageBytes = 1 << 20
	// DefaultSurrounding is the history.context window when the model does not specify one.
	DefaultSurrounding = 10
	// MaxSurrounding caps the history.context before/after counts.
	MaxSurrounding = 100
	// MaxEnvEntries bounds the environment block in assembled context.
	MaxEnvEntries = 64
	// MaxEnvValueBytes bounds one environment value in assembled context.
	MaxEnvValueBytes = 1024
	// DefaultRecentLimit is the per-bucket raw event count the context assembler fetches.
	DefaultRecentLimit = 50
	// MaxRelevantRevisions bounds the state revisions listed in assembled context.
	MaxRelevantRevisions = 10
)

// Local error codes for failures that have no domain constant yet.
const (
	CodeUnknownTool         = "unknown_tool"
	CodeToolArgsInvalid     = "tool_args_invalid"
	CodeInteractionInvalid  = "interaction_invalid"
	CodeAppCandidateInvalid = "app_candidate_invalid"
	CodeContextInvalid      = "context_invalid"
	CodeRecordUndisclosable = "record_undisclosable"
)

// CallContext is the trusted application context for one tool call. The
// composition root builds it from session state and the effective scope
// policy; it is never derived from model input. Model arguments can name a
// scope, but namespace IDs, user/session identity, and the policy itself are
// always injected here.
type CallContext struct {
	SessionID     domain.SessionID
	UserID        domain.UserID
	CWD           domain.ValidPath
	Policy        domain.ScopePolicy
	Namespaces    CallNamespaces
	UID           uint32
	GID           uint32
	TurnID        domain.TurnID
	AttemptID     domain.AttemptID
	PromptVersion string
	NowUnixMilli  int64
}

// CallNamespaces carries the server-resolved namespace IDs for every scope
// the caller may address. The model selects a scope by name; the ID always
// comes from this trusted mapping.
type CallNamespaces struct {
	Session  domain.NamespaceID
	User     domain.NamespaceID
	Shared   domain.NamespaceID
	Baseline domain.NamespaceID
}

// scopeNamespace resolves a model-supplied scope name to the trusted
// namespace ID injected by the application context.
func (c CallContext) scopeNamespace(name string) (namespaceID domain.NamespaceID, scope domain.Scope, err error) {
	switch name {
	case "session":
		return c.Namespaces.Session, domain.ScopeSession, nil
	case "user":
		return c.Namespaces.User, domain.ScopeUser, nil
	case "shared":
		return c.Namespaces.Shared, domain.ScopeShared, nil
	case "baseline":
		return c.Namespaces.Baseline, domain.ScopeBaseline, nil
	default:
		return domain.NamespaceID{}, domain.ScopeSession, domain.NewValidationError(
			domain.CodeInvalidInput, fmt.Sprintf("unknown scope %q", name), nil)
	}
}

// ToolCall is one model-requested tool invocation: a name plus JSON
// arguments. The name is validated against the server-owned allowlist.
type ToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolResult is the successful outcome of one tool call.
type ToolResult struct {
	Tool   string          `json:"tool"`
	Result json.RawMessage `json:"result"`
}

// ToolHandler executes one allowed tool.
type ToolHandler func(ctx context.Context, call CallContext, args json.RawMessage) (json.RawMessage, error)

// Dependencies are the ports and services the tool registry needs. The
// composition root wires real adapters; tests wire fakes. No field is
// optional at execution time — a missing dependency fails the call that
// needs it rather than panicking.
type Dependencies struct {
	World      ports.WorldStore
	Events     ports.EventStore
	Retrieval  ports.RetrievalStore
	Content    ports.ContentStore
	Apps       ports.AppRegistry
	Sandbox    ports.AppSandbox
	Summarizer Summarizer
	Generator  ContentGenerator
	Redactor   SecretRedactor
	Random     ports.Random
}

// Registry is the server-owned tool allowlist (PLAN 7.1 step 5). The set of
// tools is fixed here; a model request for any other name is a typed
// failure, never a fallback to some other behavior.
//
// Mutating tools stage change sets for the turn rather than applying them,
// so a tool call has no immediate shared effect. The turn coordinator reads
// StagedChanges and commits or discards them; staged changes are never
// derived from model-supplied identity.
type Registry struct {
	deps     Dependencies
	handlers map[string]ToolHandler

	mu     sync.Mutex
	staged map[domain.TurnID][]domain.ChangeSet
}

// NewRegistry builds the full tool registry over the given dependencies.
func NewRegistry(deps Dependencies) *Registry {
	r := &Registry{deps: deps, handlers: make(map[string]ToolHandler), staged: map[domain.TurnID][]domain.ChangeSet{}}
	r.handlers["world.lookup"] = r.worldLookup
	r.handlers["world.list"] = r.worldList
	r.handlers["content.read"] = r.contentRead
	r.handlers["fact.lookup"] = r.factLookup
	r.handlers["world.stage"] = r.worldStage
	r.handlers["world.materialize"] = r.worldMaterialize
	r.handlers["history.search"] = r.historySearch
	r.handlers["history.context"] = r.historyContext
	r.handlers["interaction.propose"] = r.interactionPropose
	r.handlers["app.lookup"] = r.appLookup
	r.handlers["app.candidate"] = r.appCandidate
	return r
}

// Execute validates the tool name against the server-owned allowlist and
// runs the handler. Unknown tools are a typed failure.
func (r *Registry) Execute(ctx context.Context, call CallContext, tc ToolCall) (*ToolResult, error) {
	handler, ok := r.handlers[tc.Name]
	if !ok {
		return nil, domain.NewValidationError(
			CodeUnknownTool,
			fmt.Sprintf("tool %q is not in the server allowlist", tc.Name), nil)
	}
	result, err := handler(ctx, call, tc.Arguments)
	if err != nil {
		return nil, err
	}
	return &ToolResult{Tool: tc.Name, Result: result}, nil
}

// decodeArgs unmarshals tool arguments into the typed argument struct.
func decodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return domain.NewValidationError(
			CodeToolArgsInvalid, "tool arguments are not valid JSON: "+err.Error(), nil)
	}
	return nil
}

// marshalResult encodes a tool result payload.
func marshalResult(v any) (json.RawMessage, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, domain.NewInternalError(domain.CodeSerializationFailed, "marshal tool result", err)
	}
	return data, nil
}

// clampLimit bounds a model-supplied limit to [1, max], falling back to def
// when the model did not specify one.
func clampLimit(n, def, max int) int {
	if n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// ---------------------------------------------------------------------------
// Turn staging
// ---------------------------------------------------------------------------

// stageChangeSet records a change set for the turn's commit step. It has no
// immediate effect on the world.
func (r *Registry) stageChangeSet(turn domain.TurnID, cs domain.ChangeSet) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.staged == nil {
		r.staged = map[domain.TurnID][]domain.ChangeSet{}
	}
	r.staged[turn] = append(r.staged[turn], cs)
}

// StagedChanges returns the change sets staged for a turn, oldest first. The
// turn coordinator commits them through the world store; this is the only
// path by which a tool's proposed mutation reaches durable state.
func (r *Registry) StagedChanges(turn domain.TurnID) []domain.ChangeSet {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.ChangeSet(nil), r.staged[turn]...)
}

// ClearStaged forgets a turn's staged changes after commit or discard.
func (r *Registry) ClearStaged(turn domain.TurnID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.staged, turn)
}

// stagedChange is the model-facing summary of a staged change set. The
// authoritative domain.ChangeSet (with its read dependencies) stays behind
// the staging boundary; this summary omits internal zero-valued identities
// that have no place in a tool result.
type stagedChange struct {
	TurnID      domain.TurnID    `json:"turn_id"`
	AttemptID   domain.AttemptID `json:"attempt_id"`
	Timestamp   int64            `json:"timestamp"`
	Mutations   []stagedMutation `json:"mutations"`
	StagedCount int              `json:"staged_count"`
}

// stagedMutation summarizes one proposed mutation.
type stagedMutation struct {
	Type          domain.MutationType `json:"type"`
	Path          domain.ValidPath    `json:"path"`
	Kind          domain.NodeKind     `json:"kind"`
	ExpectedRev   domain.Revision     `json:"expected_rev,omitempty"`
	ContentSize   int64               `json:"content_size,omitempty"`
	SymlinkTarget string              `json:"symlink_target,omitempty"`
}

// summarizeChangeSet renders a staged change set for a tool result.
func summarizeChangeSet(cs domain.ChangeSet, stagedCount int) stagedChange {
	out := stagedChange{
		TurnID:      cs.TurnID,
		AttemptID:   cs.AttemptID,
		Timestamp:   cs.Timestamp,
		StagedCount: stagedCount,
	}
	for _, m := range cs.Mutations {
		out.Mutations = append(out.Mutations, stagedMutation{
			Type:          m.Type,
			Path:          m.Path,
			Kind:          m.Kind,
			ExpectedRev:   m.ExpectedRev,
			ContentSize:   m.Content.Size,
			SymlinkTarget: m.SymlinkTarget,
		})
	}
	return out
}
