// Package ports declares VibeShell's hexagonal boundaries: small inbound and
// outbound contracts that use only domain types plus standard-library
// plumbing (context). No port references SSH sessions, SQL rows, HTTP
// clients, provider SDK structures, or terminal libraries; adapters translate
// those into and out of these interfaces.
//
// Dependency direction: application use cases depend on these ports and on
// the domain; outbound adapters implement them. Adding a provider, a
// transport, or a storage engine must not change shell behavior, only the
// adapter behind the port. The architecture guard test in internal/domain
// enforces that neither domain nor ports import adapter packages.
//
// Conventions shared by every port below:
//   - ctx carries deadlines and cancellation; blocking calls honor it.
//   - Bounds (sizes, counts, durations) are documented per method and
//     enforced by adapters; domain validation rejects the rest.
//   - Failures are typed *domain.DomainError values (usable with
//     errors.Is/As) or domain.ErrorEnvelope for model/routing failures.
//   - Secrets never cross these boundaries as values: accounts travel as
//     domain.AccountID and credentials as domain.KeyRef.
package ports

import (
	"context"

	"j0s.at/vibeshell/internal/domain"
)

// ---------------------------------------------------------------------------
// Outbound: model integration
// ---------------------------------------------------------------------------

// ModelGateway is the single provider-neutral route to OpenCode product
// routes (PLAN 8.1). One implementation serves every wire protocol (chat
// completions, responses, messages, Gemini-style generation) behind this
// canonical shape, so switching models never switches the session memory
// store. Invariants: requests are bounded by req.DeadlineMs; responses are
// normalized into text/tool calls/usage; error details are redacted (no key
// material); unknown errors return a diagnostic FailureUnknown envelope,
// never success.
type ModelGateway interface {
	Request(ctx context.Context, req domain.ModelRequest) (domain.ModelResponse, error)
}

// ---------------------------------------------------------------------------
// Outbound: world, events, retrieval, content
// ---------------------------------------------------------------------------

// WorldStore persists current world state: namespaces, nodes, and exact
// content references. Reads observe an acknowledged commit version or later;
// writes stage a ChangeSet and commit atomically against expected revisions.
// A stale expected revision fails with a conflict DomainError (never a silent
// overwrite); conflicts carry no provider-health penalty.
type WorldStore interface {
	// GetNode returns node metadata at its current revision.
	GetNode(ctx context.Context, ns domain.NamespaceID, id domain.NodeID) (domain.Node, error)
	// LookupPath resolves an absolute path inside one namespace view.
	LookupPath(ctx context.Context, ns domain.NamespaceID, path domain.ValidPath) (domain.Node, error)
	// ListDirectory returns the direct children of a directory node, paged.
	ListDirectory(ctx context.Context, ns domain.NamespaceID, dir domain.NodeID, limit int, cursor string) ([]domain.Node, string, error)
	// Commit applies every mutation in cs atomically after verifying read
	// dependencies and expected revisions in one short transaction.
	Commit(ctx context.Context, cs domain.ChangeSet, policy domain.ScopePolicy) (domain.Revision, error)
}

// EventStore is the append-only research record. Events are immutable once
// appended; ordering within a session is by per-session sequence, never by
// timestamp alone. Large payloads travel as content references, not inline
// duplicates. If durable recording fails, the adapter reports it so the
// service can stop accepting semantic work rather than run without logs.
type EventStore interface {
	// Append persists one event and returns its assigned sequence and ID.
	Append(ctx context.Context, evt domain.EventEnvelope) (domain.EventRecord, error)
	// GetByID fetches a single event by its stable ID.
	GetByID(ctx context.Context, id domain.EventID) (domain.EventRecord, error)
	// List returns up to limit events for a session from a minimum sequence.
	List(ctx context.Context, session domain.SessionID, minSeq uint64, limit int) ([]domain.EventRecord, error)
}

// RetrievalStore is the agent's scoped window into history. The policy
// argument is injected by trusted application context; sharing-off denies
// cross-user and shared reads at this boundary regardless of model requests.
type RetrievalStore interface {
	// Query searches events under the given scope/filter with pagination.
	Query(ctx context.Context, q domain.RetrievalQuery, policy domain.ScopePolicy) (domain.RetrievalResult, error)
	// Surrounding returns up to before events preceding ref and up to after
	// events following it, all under the same scope authorization.
	Surrounding(ctx context.Context, ref domain.EventReference, before, after int, policy domain.ScopePolicy) ([]domain.EventRecord, error)
}

// ContentStore holds immutable exact bytes addressed by content hash.
// Metadata commits and content commits are atomic; readers get exact bytes
// for output while models receive validated references instead of being
// asked to reproduce bytes from memory.
type ContentStore interface {
	// Put stores bytes (bounded by the configured content limit) and returns
	// their content reference.
	Put(ctx context.Context, data []byte, mediaType string) (domain.ContentRef, error)
	// Get returns the exact stored bytes for a reference, or a bounded range.
	Get(ctx context.Context, ref domain.ContentRef, offset, length int64) ([]byte, error)
}

// ---------------------------------------------------------------------------
// Outbound: generated applications and sandbox
// ---------------------------------------------------------------------------

// AppRegistry tracks immutable application artifacts. Existing sessions pin
// their version; new sessions use the current pointer. Extension creates a
// candidate that must validate and smoke-test before atomic activation; the
// previous version stays available for rollback and research.
type AppRegistry interface {
	// GetArtifact returns the immutable artifact for a version ID.
	GetArtifact(ctx context.Context, version domain.AppVersionID) (domain.AppArtifact, error)
	// Current returns the active version ID for an app.
	Current(ctx context.Context, app domain.AppID) (domain.AppVersionID, error)
	// RegisterCandidate stores a candidate version without activating it.
	RegisterCandidate(ctx context.Context, artifact domain.AppArtifact) (domain.AppVersionID, error)
	// Activate atomically moves the current pointer after validation.
	Activate(ctx context.Context, app domain.AppID, version domain.AppVersionID) error
	// Rollback restores the current pointer to a prior accepted version.
	Rollback(ctx context.Context, app domain.AppID, to domain.AppVersionID, reason string) error
}

// AppStateRecord is a stored application-state snapshot together with the
// artifact version that produced it. State written by an older version needs
// a verified migration before the current version may interpret it (PLAN 5.6).
type AppStateRecord struct {
	Version domain.AppVersionID
	State   domain.AppState
}

// SessionPin is a session's pinned artifact version together with the
// session-scoped state stored for that pin.
type SessionPin struct {
	Version domain.AppVersionID
	State   domain.AppState
	// PinnedAt is the store's pin time in Unix milliseconds; an
	// implementation that does not track pin time reports 0.
	PinnedAt int64
}

// AppStateStore persists generated-app session state, pins, and the
// command-name index durably. The application service reads and writes every
// pin, session state, and durable user/shared partition through this port, so
// a restart loses nothing when the composition supplies the SQLite adapter;
// the in-memory default in internal/apps implements the same contract for
// tests and unconfigured builds. Implementations are safe for concurrent use,
// and missing records report ok=false rather than an error.
type AppStateStore interface {
	// SessionPin returns a session's pinned version and stored session
	// state for an app, or ok=false when the session never resolved it.
	SessionPin(ctx context.Context, session domain.SessionID, app domain.AppID) (SessionPin, bool, error)
	// PinSession pins a session to a version and records the session state
	// it starts from; re-pinning replaces both.
	PinSession(ctx context.Context, session domain.SessionID, app domain.AppID, version domain.AppVersionID, state domain.AppState) error
	// SaveSessionState replaces the session-state portion of an existing
	// pin. State without a pin is refused (session_not_resolved).
	SaveSessionState(ctx context.Context, session domain.SessionID, app domain.AppID, version domain.AppVersionID, state domain.AppState) error
	// ReleaseSession drops a session's pin and session state; durable
	// user and shared state is retained.
	ReleaseSession(ctx context.Context, session domain.SessionID, app domain.AppID) error
	// UserState returns the durable user-scoped state of an app, or
	// ok=false when this user never saved any.
	UserState(ctx context.Context, user domain.UserID, app domain.AppID) (AppStateRecord, bool, error)
	// SaveUserState records the durable user-scoped portion of a state.
	SaveUserState(ctx context.Context, user domain.UserID, app domain.AppID, record AppStateRecord) error
	// SharedState returns the durable shared state of a shared-scoped app,
	// or ok=false when none was ever saved.
	SharedState(ctx context.Context, app domain.AppID) (AppStateRecord, bool, error)
	// SaveSharedState records the durable shared portion of a state. A
	// user-scoped app has no shared portion and must never receive one.
	SaveSharedState(ctx context.Context, app domain.AppID, record AppStateRecord) error
	// CommandIndex returns every visible command name mapped to the app
	// that implements it, so the shell resolves a command to its stored
	// app after a restart.
	CommandIndex(ctx context.Context) (map[string]domain.AppID, error)
	// RecordCommand associates a visible command name with an app; a
	// re-recorded name moves to the latest app.
	RecordCommand(ctx context.Context, name string, app domain.AppID) error
}

// SandboxLimits bounds one generated-application execution: wall-clock
// deadline, guest/heap memory, serialized output, and event queue depth.
// Cancellation of the context must stop both guest and host-bridge work.
type SandboxLimits struct {
	DeadlineMs  int64
	MaxMemoryB  int64
	MaxOutputB  int64
	MaxEvents   int
	SeededRand  int64 // explicit seed; no ambient randomness inside the guest
	SimNowMilli int64 // simulated time visible to the guest
}

// AppSandbox executes generated JavaScript with capabilities over the
// simulated world only: no host filesystem, network, environment, process,
// or native module access. Inputs and outputs cross a bounded JSON bridge.
type AppSandbox interface {
	// Run evaluates one event against explicit state and returns the
	// validated result (new state, declarative view, effect proposals,
	// world-read or AI-extension requests).
	Run(ctx context.Context, artifact domain.AppArtifact, state domain.AppState, event domain.AppEvent, limits SandboxLimits) (domain.AppResult, error)
}

// ---------------------------------------------------------------------------
// Outbound: terminal rendering, time, randomness, export
// ---------------------------------------------------------------------------

// TerminalRenderer consumes validated declarative views and emits terminal
// control sequences. Models and generated apps never emit raw control
// sequences; text shown as data must not activate clipboard, hyperlink,
// title-change, or other unapproved controls.
type TerminalRenderer interface {
	// RenderView validates view (domain.ValidateView) and returns the frame
	// bytes as an immutable content reference plus the byte count.
	RenderView(ctx context.Context, session domain.SessionID, view domain.AppView) (domain.ContentRef, int64, error)
	// WriteText emits plain text output with a trailing prompt state.
	WriteText(ctx context.Context, session domain.SessionID, text string) (int64, error)
}

// Clock is the injectable time source. Policies take explicit int64
// timestamps so tests never depend on real delays; only adapters read the
// real clock.
type Clock interface {
	// NowUnixMilli returns wall-clock UTC milliseconds.
	NowUnixMilli() int64
	// MonotonicNanos returns nanoseconds since process start for ordering.
	MonotonicNanos() int64
}

// Random is the injectable randomness source. IDs and jitter derive from
// here so tests inject a deterministic source.
type Random interface {
	// Bytes returns n random bytes (n > 0, bounded by the caller).
	Bytes(n int) ([]byte, error)
	// Intn returns a uniform value in [0, n).
	Intn(n int) int
}

// Exporter streams the three research projections (versioned JSONL bundle,
// readable UTF-8 transcript, asciinema-compatible replay) from a consistent
// read snapshot with bounded buffers. Exports include incomplete sessions
// with explicit status and never fabricate a successful ending; secrets are
// redacted per the query policy.
type Exporter interface {
	Export(ctx context.Context, q domain.ExportQuery) (domain.ExportResult, error)
}

// ---------------------------------------------------------------------------
// Inbound: administration (implemented by the application core, driven by the
// container executable / podman exec; never exposed as simulated commands)
// ---------------------------------------------------------------------------

// AdminOps covers operator commands: config validation, user/hash
// maintenance, session listing, research export, route/account status,
// bounded probe triggers, app rollback, integrity checks, backup/restore.
type AdminOps interface {
	// ValidateConfig parses and semantically checks a configuration snapshot.
	ValidateConfig(ctx context.Context, raw []byte) error
	// RouteStatus summarizes health records for operator display.
	RouteStatus(ctx context.Context) ([]domain.HealthRecord, error)
	// TriggerProbe admits a single bounded probe for a due health key.
	TriggerProbe(ctx context.Context, key domain.HealthKey) error
	// Backup captures a consistent snapshot including content and versions.
	Backup(ctx context.Context, dest string) (domain.ExportResult, error)
}
