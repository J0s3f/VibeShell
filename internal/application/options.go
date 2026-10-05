package application

import (
	"context"
	"fmt"
	"time"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// ConfigSnapshot is the configuration, prompt, catalogue, and scope policy one
// turn runs under. A turn captures it once and keeps it for every generation,
// so a live configuration or prompt reload changes the next turn, never a turn
// already in flight (PLAN 7.4).
type ConfigSnapshot struct {
	ConfigVersion    int64              `json:"config_version"`
	PromptVersion    string             `json:"prompt_version"`
	CatalogueVersion string             `json:"catalogue_version"`
	ScopePolicy      domain.ScopePolicy `json:"scope_policy"`
	// TurnDeadlineMs bounds the whole turn, measured from when the turn starts.
	TurnDeadlineMs int64 `json:"turn_deadline_ms"`
	// MaxAttempts and MaxRebases bound this turn's own retry budget. The
	// effective budget is the smaller of these values and the coordinator
	// limits, so an operator can lower it per configuration revision.
	MaxAttempts int `json:"max_attempts"`
	MaxRebases  int `json:"max_rebases"`
}

// Validate rejects a snapshot that cannot bound a turn.
func (s ConfigSnapshot) Validate() error {
	if s.TurnDeadlineMs <= 0 {
		return domain.NewValidationError(domain.CodeInvalidConfig, "snapshot needs a positive turn deadline", nil)
	}
	if s.MaxAttempts <= 0 {
		return domain.NewValidationError(domain.CodeInvalidConfig, "snapshot needs a positive attempt budget", nil)
	}
	if s.MaxRebases < 0 {
		return domain.NewValidationError(domain.CodeInvalidConfig, "snapshot rebase budget must not be negative", nil)
	}
	// The scope policy is injected by trusted application context and pinned
	// per turn. A revision identifies which policy a turn ran under, so the
	// session can detect a live sharing change and invalidate cross-scope
	// context. A policy without a revision could not be pinned or compared.
	if s.ScopePolicy.PolicyRevision <= 0 {
		return domain.NewValidationError(domain.CodeInvalidConfig, "snapshot scope policy needs a positive revision", nil)
	}
	return nil
}

// SnapshotSource supplies the snapshot a new turn pins. The composition root
// wires the configuration adapter; tests inject a deterministic double.
type SnapshotSource interface {
	// CurrentSnapshot returns the snapshot currently in force. It is called
	// once per turn, never per generation.
	CurrentSnapshot(ctx context.Context) (ConfigSnapshot, error)
}

// Limits are the coordinator's resource bounds. They exist so a hostile client
// or an unbounded model cannot make one session consume the process.
type Limits struct {
	// OutputQueue is the per-session queue depth for accepted output.
	OutputQueue int
	// MaxQueuedTurns is how many accepted turns may wait behind the running
	// one before the session refuses further semantic input.
	MaxQueuedTurns int
	// MaxAttempts bounds provider attempts per turn.
	MaxAttempts int
	// MaxRebases bounds conflict rebases per turn.
	MaxRebases int
	// MaxRepairs bounds the repair opportunity for a malformed response.
	MaxRepairs int
	// MaxOutputBytes bounds one emitted text or frame item.
	MaxOutputBytes int
	// MaxInlinePayloadBytes is the largest event payload stored inline; larger
	// payloads travel as a content reference.
	MaxInlinePayloadBytes int
	// TurnDeadline bounds one turn end to end.
	TurnDeadline time.Duration
	// JournalTimeout bounds one durable event append so a stalled store cannot
	// wedge a session goroutine. Journal writes survive turn cancellation.
	JournalTimeout time.Duration
}

// DefaultLimits returns the operational defaults for interactive use. They
// bound work, not quality: a long answer is cut short rather than a session
// hanging forever.
func DefaultLimits() Limits {
	return Limits{
		OutputQueue:           64,
		MaxQueuedTurns:        4,
		MaxAttempts:           3,
		MaxRebases:            2,
		MaxRepairs:            1,
		MaxOutputBytes:        256 * 1024,
		MaxInlinePayloadBytes: 1024,
		TurnDeadline:          2 * time.Minute,
		JournalTimeout:        10 * time.Second,
	}
}

// Validate rejects a Limits value that would leave the coordinator unbounded.
func (l Limits) Validate() error {
	for _, bound := range []struct {
		name  string
		value int
	}{
		{"OutputQueue", l.OutputQueue},
		{"MaxQueuedTurns", l.MaxQueuedTurns},
		{"MaxAttempts", l.MaxAttempts},
		{"MaxRepairs", l.MaxRepairs},
		{"MaxOutputBytes", l.MaxOutputBytes},
		{"MaxInlinePayloadBytes", l.MaxInlinePayloadBytes},
	} {
		if bound.value <= 0 {
			return domain.NewValidationError(domain.CodeInvalidConfig, "limit must be positive", map[string]string{
				"limit": bound.name,
			})
		}
	}
	if l.MaxRebases < 0 {
		return domain.NewValidationError(domain.CodeInvalidConfig, "limit must not be negative", map[string]string{"limit": "MaxRebases"})
	}
	if l.TurnDeadline <= 0 {
		return domain.NewValidationError(domain.CodeInvalidConfig, "limit must be positive", map[string]string{"limit": "TurnDeadline"})
	}
	if l.JournalTimeout <= 0 {
		return domain.NewValidationError(domain.CodeInvalidConfig, "limit must be positive", map[string]string{"limit": "JournalTimeout"})
	}
	if l.OutputQueue > 4096 {
		return domain.NewValidationError(domain.CodeInvalidConfig, "output queue is implausibly large", map[string]string{
			"limit": "OutputQueue",
		})
	}
	return nil
}

// CoordinatorOptions are the coordinator's explicit dependencies. Every
// boundary is an interface from internal/ports or this package, so the
// coordinator is usable with test doubles and never with adapter types.
type CoordinatorOptions struct {
	// Engine performs one generation attempt of a turn (internal/simulation).
	Engine TurnEngine
	// Tools executes bounded tool batches requested by the engine.
	Tools ToolExecutor
	// Events is the append-only research record.
	Events ports.EventStore
	// World persists staged world changes.
	World ports.WorldStore
	// Content holds immutable exact bytes for frames and large event payloads.
	Content ports.ContentStore
	// Renderer turns validated views and text into exact terminal bytes.
	Renderer ports.TerminalRenderer
	// Clock and Random are the injected time and randomness seams.
	Clock  ports.Clock
	Random ports.Random
	// Snapshots supplies the per-turn pinned configuration snapshot.
	Snapshots SnapshotSource
	// Limits bounds one session and one turn. Use DefaultLimits for the
	// documented operational defaults.
	Limits Limits
}

// validate rejects an incomplete composition at the boundary instead of
// failing later inside a session goroutine.
func (o CoordinatorOptions) validate() error {
	missing := make([]string, 0, 8)
	if o.Engine == nil {
		missing = append(missing, "Engine")
	}
	if o.Tools == nil {
		missing = append(missing, "Tools")
	}
	if o.Events == nil {
		missing = append(missing, "Events")
	}
	if o.World == nil {
		missing = append(missing, "World")
	}
	if o.Content == nil {
		missing = append(missing, "Content")
	}
	if o.Renderer == nil {
		missing = append(missing, "Renderer")
	}
	if o.Clock == nil {
		missing = append(missing, "Clock")
	}
	if o.Random == nil {
		missing = append(missing, "Random")
	}
	if o.Snapshots == nil {
		missing = append(missing, "Snapshots")
	}
	if len(missing) > 0 {
		return domain.NewValidationError(domain.CodeInvalidConfig, "coordinator dependencies missing", map[string]string{
			"missing": fmt.Sprint(missing),
		})
	}
	return o.Limits.Validate()
}
