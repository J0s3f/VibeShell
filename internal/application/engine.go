package application

import (
	"context"
	"encoding/json"

	"j0s.at/vibeshell/internal/domain"
)

// TurnEngine performs the simulation work of a turn. It is owned here because
// it is the seam the coordinator drives; internal/simulation (context and
// tools, generation and extension) implements it against the same contract, so
// the coordinator stays testable with doubles.
//
// The engine never commits, never emits, and never decides turn outcomes: it
// produces validated candidates and tool batches, and the coordinator owns
// journaling, ordering, cancellation, and the commit point.
type TurnEngine interface {
	// Prepare assembles the bounded turn context from the pinned snapshot and
	// the session context. It runs once per turn, before generation.
	Prepare(ctx context.Context, req TurnRequest) (PreparedTurn, error)
	// Generate runs one generation attempt. It returns StepAwaitingTool while
	// more tool work is required, or StepCandidate once the turn has a
	// complete structured outcome to validate.
	Generate(ctx context.Context, in GenerationInput) (StepOutcome, error)
}

// ToolExecutor runs one bounded batch of tool calls requested by the engine.
// It executes only tools on the server-owned allowlist and only within the
// snapshot's scope policy.
type ToolExecutor interface {
	Execute(ctx context.Context, batch ToolBatch) (ToolBatchResult, error)
}

// TurnRequest is the stable identity and pinned context of one turn. Generation
// and Attempt change per generation; everything else stays constant so a retry
// re-runs the same work against the same configuration.
type TurnRequest struct {
	Session   domain.SessionID
	Principal domain.UserID
	Turn      domain.TurnID
	// Generation increases for every attempt of the turn. A response carrying
	// an older generation is late and must be discarded.
	Generation uint64
	Attempt    domain.AttemptID
	Input      SessionInput
	Context    SessionContext
	Snapshot   ConfigSnapshot
	// DeadlineMs is the absolute wall-clock deadline for the whole turn.
	DeadlineMs int64
	// AttemptNumber is 1 for the first generation of the turn.
	AttemptNumber int
	// RebaseNumber counts conflict rebases already performed.
	RebaseNumber int
}

// SessionContext is the session-local state captured for one turn: working
// directory, home directory, foreground interaction, and last exit status. It is
// a value, so a turn never observes a concurrent cwd change half-applied.
type SessionContext struct {
	CWD          domain.ValidPath
	Home         domain.ValidPath
	Foreground   ForegroundState
	ExitStatus   int
	TerminalSize domain.TermSize
}

// PreparedTurn is the assembled context for one turn.
type PreparedTurn struct {
	ContextBytes int
	// Revisions are the world revisions the context was built from. They are
	// the read dependencies the commit verifies.
	Revisions []domain.Revision
	// AppVersionID is the app source/version handed to the model, if any.
	AppVersionID *domain.AppVersionID
}

// GenerationInput is one generation attempt's input.
type GenerationInput struct {
	Request  TurnRequest
	Prepared PreparedTurn
	// ToolResults carries the results of the previous tool batch. It is empty
	// on the first generation of an attempt.
	ToolResults []ToolCallResult
}

// StepPhase distinguishes a turn that still needs tool work from one that has a
// complete candidate outcome.
type StepPhase string

const (
	StepAwaitingTool StepPhase = "awaiting_tool"
	StepCandidate    StepPhase = "candidate"
)

// StepOutcome is the result of one generation attempt.
type StepOutcome struct {
	Phase StepPhase
	// ToolCalls are requested when Phase is StepAwaitingTool.
	ToolCalls []ToolCallRequest
	// Candidate is set when Phase is StepCandidate.
	Candidate *TurnCandidate
	Usage     domain.Usage
	Route     domain.RouteID
	// Repair marks an attempt that re-asks the model after a rejected
	// response; it consumes the turn's repair budget instead of its attempts.
	Repair bool
}

// ToolCallRequest is one requested tool invocation with its proposed scope.
type ToolCallRequest struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Scope     domain.Scope    `json:"scope"`
	TimeoutMs int64           `json:"timeout_ms"`
}

// ToolBatch is one bounded batch of tool calls.
type ToolBatch struct {
	Request TurnRequest
	Calls   []ToolCallRequest
}

// ToolBatchResult is the bounded result of one tool batch.
type ToolBatchResult struct {
	Results []ToolCallResult
}

// ToolCallResult is one tool result, possibly referenced rather than inlined.
type ToolCallResult struct {
	Name      string              `json:"name"`
	Result    json.RawMessage     `json:"result,omitempty"`
	Truncated bool                `json:"truncated,omitempty"`
	Content   *domain.ContentRef  `json:"content,omitempty"`
	Error     *domain.DomainError `json:"error,omitempty"`
}

// CommitKey identifies one logical commit inside a turn.
//
// The engine must reuse a key when a retry repeats the same logical work, and
// must use a different key when the mutation set legitimately differs. The
// coordinator keys its commit ledger by this value, so a replayed commit skips
// mutations it already applied instead of applying them twice (PLAN 9.4).
type CommitKey struct {
	Turn domain.TurnID
	// Logical names the unit of work, e.g. "stage-1" or "cd-home".
	Logical string
}

// TurnCandidate is the structured final outcome the engine proposes for one
// turn. The coordinator validates it, commits it, and emits it; nothing here
// reaches the world or the terminal without passing through that order.
type TurnCandidate struct {
	// CommitKey is required whenever Changes contains mutations.
	CommitKey CommitKey
	Changes   domain.ChangeSet
	Output    CandidateOutput
	// SessionPatch is the accepted session-state change, applied at the commit
	// point so the emitted prompt reflects accepted state.
	SessionPatch SessionPatch
	Usage        domain.Usage
	AppVersionID *domain.AppVersionID
	// ExitStatus is the simulated command exit status for the prompt.
	ExitStatus int
}

// CandidateOutput is the accepted output of one turn. Large content travels by
// immutable content reference so exact bytes are never re-generated.
type CandidateOutput struct {
	Text string
	// View is a validated full-screen interaction; the coordinator renders it
	// through the terminal renderer rather than emitting model text directly.
	View *domain.AppView
	// Content are already committed immutable references to stream in chunks.
	Content []domain.ContentRef
}

// SessionPatch is the accepted change to session-local state.
type SessionPatch struct {
	CWD        *domain.ValidPath
	Foreground *ForegroundState
	ExitStatus *int
	// AppPrompt is the foreground application's continuation prompt. An engine
	// sets it alongside a foreground app and omits it when the patch returns
	// the session to the shell, which clears it.
	AppPrompt *AppPrompt
}
