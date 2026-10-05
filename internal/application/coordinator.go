package application

import (
	"context"
	"fmt"
	"sync"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// identityBytes is the entropy behind every identity this package mints. It
// matches the 128-bit value the domain identity encoding expects.
const identityBytes = 16

// crockford is the base32 alphabet used by domain identity values.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Coordinator owns sessions: it accepts them, mints their identities, and
// starts and stops their goroutines. It holds no session state itself, so a
// slow or stuck session can never block another one.
type Coordinator struct {
	engine    TurnEngine
	tools     ToolExecutor
	events    ports.EventStore
	world     ports.WorldStore
	content   ports.ContentStore
	renderer  ports.TerminalRenderer
	clock     ports.Clock
	random    ports.Random
	snapshots SnapshotSource
	limits    Limits

	mu       sync.Mutex
	sessions map[domain.SessionID]*session
	closing  bool
}

// NewCoordinator validates the composition boundary and returns a coordinator.
// Every dependency is an interface, so the same coordinator runs against
// adapters and against test doubles.
func NewCoordinator(options CoordinatorOptions) (*Coordinator, error) {
	if err := options.validate(); err != nil {
		return nil, err
	}
	return &Coordinator{
		engine:    options.Engine,
		tools:     options.Tools,
		events:    options.Events,
		world:     options.World,
		content:   options.Content,
		renderer:  options.Renderer,
		clock:     options.Clock,
		random:    options.Random,
		snapshots: options.Snapshots,
		limits:    options.Limits,
		sessions:  make(map[domain.SessionID]*session),
	}, nil
}

// Limits returns the bounds this coordinator enforces.
func (c *Coordinator) Limits() Limits { return c.limits }

// Open accepts one shell session and returns its handle. One accepted shell
// channel becomes one session with its own cwd and foreground state, while the
// durable world stays shared with the principal's other sessions.
func (c *Coordinator) Open(ctx context.Context, request OpenSessionRequest) (Shell, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}

	sessionID, err := c.mintSessionID()
	if err != nil {
		return nil, err
	}
	terminal := request.Terminal
	if terminal.Size == (domain.TermSize{}) {
		terminal.Size = DefaultTerminalSize
	}

	// The connect-time snapshot only describes the session itself; each turn
	// pins its own snapshot when it starts.
	snapshot, err := c.snapshots.CurrentSnapshot(ctx)
	if err != nil {
		return nil, c.wrapSnapshotError(err)
	}

	s := c.newSession(sessionID, request, terminal, snapshot)

	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return nil, domain.NewUnavailableError(domain.CodeShutdown, "coordinator is shutting down", nil, nil)
	}
	c.sessions[sessionID] = s
	c.mu.Unlock()

	if err := s.recordStart(ctx, snapshot); err != nil {
		c.forget(sessionID)
		return nil, err
	}
	s.start()
	return s, nil
}

// Shutdown ends every live session with the given reason. The composition root
// calls it on service shutdown; sessions already ended are left untouched.
func (c *Coordinator) Shutdown(ctx context.Context, reason EndReason) error {
	c.mu.Lock()
	c.closing = true
	live := make([]*session, 0, len(c.sessions))
	for _, s := range c.sessions {
		live = append(live, s)
	}
	c.mu.Unlock()

	var firstErr error
	for _, s := range live {
		if err := s.End(ctx, reason); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ActiveSessions reports how many sessions are currently running.
func (c *Coordinator) ActiveSessions() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sessions)
}

// newSession builds the session runtime and its journal.
func (c *Coordinator) newSession(id domain.SessionID, request OpenSessionRequest, terminal TerminalMetadata, snapshot ConfigSnapshot) *session {
	journal := newEventJournal(
		c.events,
		c.content,
		c.clock,
		c.limits.JournalTimeout,
		c.limits.MaxInlinePayloadBytes,
		id,
		domain.Provenance{
			Source:           "application",
			Actor:            request.Principal.String(),
			PromptVersion:    snapshot.PromptVersion,
			ConfigVersion:    fmt.Sprint(snapshot.ConfigVersion),
			CatalogueVersion: snapshot.CatalogueVersion,
		},
	)
	return &session{
		coord:    c,
		journal:  journal,
		id:       id,
		commands: make(chan sessionCommand, 8),
		finished: make(chan *turn, c.limits.MaxQueuedTurns+1),
		outputs:  make(chan SessionOutput, c.limits.OutputQueue),
		outcomes: make(chan SessionOutcome, c.limits.OutputQueue),
		done:     make(chan struct{}),
		state: newSessionState(
			id,
			request.Principal,
			request.AuthMode,
			terminal,
			request.CWD,
			c.clock.NowUnixMilli(),
		),
		recordingHealthy: true,
	}
}

// forget removes an ended session from the registry.
func (c *Coordinator) forget(id domain.SessionID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sessions, id)
}

// mintSessionID creates one session identity.
func (c *Coordinator) mintSessionID() (domain.SessionID, error) {
	raw, err := c.mintID(domain.PrefixSession)
	if err != nil {
		return domain.SessionID{}, err
	}
	return domain.ParseSessionID(raw)
}

// mintTurnID creates one turn identity.
func (c *Coordinator) mintTurnID() (domain.TurnID, error) {
	raw, err := c.mintID(domain.PrefixTurn)
	if err != nil {
		return domain.TurnID{}, err
	}
	return domain.ParseTurnID(raw)
}

// mintAttemptID creates one attempt identity.
func (c *Coordinator) mintAttemptID() (domain.AttemptID, error) {
	raw, err := c.mintID(domain.PrefixAttempt)
	if err != nil {
		return domain.AttemptID{}, err
	}
	return domain.ParseAttemptID(raw)
}

// mintID creates one domain identity string from the injected randomness
// source. The value is parseable by the domain identity format.
func (c *Coordinator) mintID(prefix string) (string, error) {
	raw, err := c.random.Bytes(identityBytes)
	if err != nil {
		return "", domain.NewInternalError(domain.CodeInvariantViolation, "randomness source failed", err)
	}
	if len(raw) != identityBytes {
		return "", domain.NewInternalError(domain.CodeInvariantViolation, "randomness source returned the wrong length", nil)
	}
	return prefix + "_" + encodeBase32(raw), nil
}

// wrapSnapshotError reports a configuration snapshot failure without pretending
// the session can continue without a pinned scope policy.
func (c *Coordinator) wrapSnapshotError(err error) error {
	if failure := classifyError(err, failureFatal); failure.Class != domain.FailureUnknown {
		return failure.AsDomainError()
	}
	return domain.NewUnavailableError(domain.CodeInvalidConfig, "configuration snapshot is unavailable", nil, err)
}

// encodeBase32 encodes bytes as unpadded Crockford base32, MSB first. The
// encoding matches the domain identity format: 16 bytes become 26 characters.
func encodeBase32(raw []byte) string {
	encoded := make([]byte, 0, (len(raw)*8+4)/5)
	var acc uint16
	var bits uint
	for _, b := range raw {
		acc = acc<<8 | uint16(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			encoded = append(encoded, crockford[(acc>>bits)&0x1f])
		}
	}
	if bits > 0 {
		encoded = append(encoded, crockford[(acc<<(5-bits))&0x1f])
	}
	return string(encoded)
}
