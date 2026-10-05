package load

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
	"j0s.at/vibeshell/internal/routing"
)

// DelayedProvider is the deterministic provider double the model scenarios run
// against. It performs no network I/O and holds no credential: it sleeps for a
// fixed delay, honours the request deadline and context cancellation, and
// returns a response derived from a hash of the request so two identical
// requests are byte-identical across runs.
//
// Its purpose is bounded: prove admission control and queueing without a paid
// load test. Its delay is therefore an injected constant, and every receipt
// labels it as such.
type DelayedProvider struct {
	// Delay is the injected service time of one request.
	Delay time.Duration

	mu                    sync.Mutex
	requests              int64
	inFlight              int
	maxConcurrentObserved int
	latency               Latencies
	cancelled             int64
	deadlineExceeded      int64
}

var _ ports.ModelGateway = (*DelayedProvider)(nil)

// ProviderStats reports what the double observed.
type ProviderStats struct {
	Requests              int64          `json:"requests"`
	Cancelled             int64          `json:"cancelled"`
	DeadlineExceeded      int64          `json:"deadline_exceeded"`
	MaxConcurrentInFlight int            `json:"max_concurrent_in_flight"`
	Latency               LatencySummary `json:"latency"`
}

// Stats snapshots the double's counters.
func (p *DelayedProvider) Stats() ProviderStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return ProviderStats{
		Requests:              p.requests,
		Cancelled:             p.cancelled,
		DeadlineExceeded:      p.deadlineExceeded,
		MaxConcurrentInFlight: p.maxConcurrentObserved,
		Latency:               p.latency.Summary(),
	}
}

// Request implements ports.ModelGateway with an injected delay instead of a
// provider call. Cancellation and deadline expiry surface as the context error
// so routing health behaviour stays observable.
func (p *DelayedProvider) Request(ctx context.Context, req domain.ModelRequest) (domain.ModelResponse, error) {
	p.mu.Lock()
	p.requests++
	p.inFlight++
	if p.inFlight > p.maxConcurrentObserved {
		p.maxConcurrentObserved = p.inFlight
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.inFlight--
		p.mu.Unlock()
	}()

	started := time.Now()
	if err := p.sleep(ctx); err != nil {
		return domain.ModelResponse{}, err
	}
	p.latency.Record(time.Since(started))

	response := delayedResponse(req)
	response.LatencyMs = time.Since(started).Milliseconds()
	return response, nil
}

// sleep waits out the injected delay, honouring both the caller's context and
// the request deadline, and records which one ended the wait.
func (p *DelayedProvider) sleep(ctx context.Context) error {
	if p.Delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(p.Delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		p.mu.Lock()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			p.deadlineExceeded++
		} else {
			p.cancelled++
		}
		p.mu.Unlock()
		return ctx.Err()
	}
}

// delayedResponse builds the deterministic reply for one request. Content is
// hashed from the request so the answer varies with the input without ever being
// random, and usage is derived from the same hash so a research record replays
// identically.
func delayedResponse(req domain.ModelRequest) domain.ModelResponse {
	prompt := ""
	for _, m := range req.Messages {
		if m.Role == domain.RoleUser {
			prompt = m.Content
			break
		}
	}
	digest := fnv.New64a()
	_, _ = digest.Write([]byte(prompt))
	_, _ = digest.Write([]byte(req.RouteID.Value()))
	_, _ = digest.Write([]byte(req.AccountID.Value()))
	sum := digest.Sum64()

	tokensIn := len(prompt)/4 + 1
	tokensOut := int(sum%64 + 8)
	return domain.ModelResponse{
		RequestID: req.RequestID,
		RouteID:   req.RouteID,
		AccountID: req.AccountID,
		Message: domain.Message{
			Role:    domain.RoleAssistant,
			Content: fmt.Sprintf("loadharness: deterministic reply %016x for %d prompt bytes", sum, len(prompt)),
		},
		FinishReason: domain.FinishReasonStop,
		Usage: domain.Usage{
			PromptTokens:     tokensIn,
			CompletionTokens: tokensOut,
			TotalTokens:      tokensIn + tokensOut,
		},
	}
}

// AdmittedGateway is the harness's bounded admission gate in front of the
// provider double.
//
// Ownership matters here: the service configuration validates
// inference.global_concurrency, inference.max_account_concurrency, and
// inference.wait_queue_depth, but no runtime package enforces them yet. The gate
// therefore lives in the harness, is named as harness-owned, and the receipt
// records the gap as a finding. Writing the gate here rather than editing the
// routing or coordinator packages keeps this task inside its owned paths while
// still measuring whether backpressure works when it exists.
//
// Ordering inside the gate is deliberate. A caller enters the bounded wait queue
// first, then takes a global slot, then a per-account slot, and releases them in
// reverse. Taking a slot waits, bounded by the caller's context, so a bound
// delays work instead of failing it; only a saturated wait queue rejects. A
// caller holding a global slot while waiting for an account slot cannot starve
// the others: every global slot holder either runs or is itself waiting on a
// slot that only a running holder releases, so progress is guaranteed.
type AdmittedGateway struct {
	inner ports.ModelGateway
	// slots bounds concurrent in-flight requests.
	slots chan struct{}
	// queueDepth bounds how many callers may wait for a slot before the gate
	// rejects instead of queueing.
	queueDepth int
	// accountSlots bounds one account, created on first use.
	accountSlots map[domain.AccountID]chan struct{}

	mu             sync.Mutex
	waiting        int
	maxWaiting     int
	maxInFlight    int
	admitted       int64
	rejected       int64
	maxAccountSeen int
	queueWait      Latencies

	accountMu       sync.Mutex
	perAccountLimit int
}

// AdmittedGatewayOptions configures the gate.
type AdmittedGatewayOptions struct {
	// Concurrency is the global admitted pool size.
	Concurrency int
	// QueueDepth bounds waiters before rejection. Zero means unbounded waiting.
	QueueDepth int
	// MaxAccountConcurrency bounds one account inside the global pool. Zero
	// disables the per-account bound.
	MaxAccountConcurrency int
}

// NewAdmittedGateway wraps inner with a bounded admission gate.
func NewAdmittedGateway(inner ports.ModelGateway, opts AdmittedGatewayOptions) *AdmittedGateway {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 1
	}
	if opts.QueueDepth < 0 {
		opts.QueueDepth = 0
	}
	return &AdmittedGateway{
		inner:           inner,
		slots:           make(chan struct{}, opts.Concurrency),
		queueDepth:      opts.QueueDepth,
		perAccountLimit: opts.MaxAccountConcurrency,
		accountSlots:    map[domain.AccountID]chan struct{}{},
	}
}

// AdmissionReport is the observable backpressure evidence.
type AdmissionReport struct {
	Concurrency        int            `json:"admitted_concurrency"`
	QueueDepth         int            `json:"queue_depth"`
	MaxAccountLimit    int            `json:"max_account_concurrency,omitempty"`
	Admitted           int64          `json:"admitted"`
	Rejected           int64          `json:"rejected"`
	MaxInFlight        int            `json:"max_in_flight"`
	MaxWaiting         int            `json:"max_waiting"`
	MaxAccountInFlight int            `json:"max_account_in_flight"`
	QueueWait          LatencySummary `json:"queue_wait"`
}

// Report snapshots the gate's counters.
func (g *AdmittedGateway) Report() AdmissionReport {
	g.mu.Lock()
	defer g.mu.Unlock()
	return AdmissionReport{
		Concurrency:        cap(g.slots),
		QueueDepth:         g.queueDepth,
		MaxAccountLimit:    g.perAccountLimit,
		Admitted:           g.admitted,
		Rejected:           g.rejected,
		MaxInFlight:        g.maxInFlight,
		MaxWaiting:         g.maxWaiting,
		MaxAccountInFlight: g.maxAccountSeen,
		QueueWait:          g.queueWait.Summary(),
	}
}

// ErrAdmissionRejected reports that the gate refused a request instead of
// queueing it. It is a simulated temporary service error: the caller retries or
// reports unavailability, and no provider time was ever consumed.
var ErrAdmissionRejected = errors.New("loadharness: admission gate rejected the request")

// Request admits the caller into the bounded pool, runs inner, and releases the
// slots. It records how long admission itself took, which is the queueing cost
// backpressure is supposed to create.
func (g *AdmittedGateway) Request(ctx context.Context, req domain.ModelRequest) (domain.ModelResponse, error) {
	queuedAt := time.Now()

	if !g.enterQueue() {
		g.countRejected()
		return domain.ModelResponse{}, fmt.Errorf("%w: the wait queue of %d is full", ErrAdmissionRejected, g.queueDepth)
	}
	defer g.leaveQueue()

	if err := g.acquire(ctx, g.slots); err != nil {
		g.countRejected()
		return domain.ModelResponse{}, fmt.Errorf("%w: %w", ErrAdmissionRejected, err)
	}
	defer func() { <-g.slots }()

	if g.perAccountLimit > 0 {
		accountSlots := g.accountSemaphore(req.AccountID)
		if err := g.acquire(ctx, accountSlots); err != nil {
			g.countRejected()
			return domain.ModelResponse{}, fmt.Errorf("%w: account pool: %w", ErrAdmissionRejected, err)
		}
		g.noteAccountInFlight(cap(accountSlots))
		defer func() { <-accountSlots }()
	}

	g.mu.Lock()
	g.queueWait.Record(time.Since(queuedAt))
	if inFlight := len(g.slots); inFlight > g.maxInFlight {
		g.maxInFlight = inFlight
	}
	g.admitted++
	g.mu.Unlock()

	return g.inner.Request(ctx, req)
}

// acquire takes one slot, waiting until it is free or ctx ends.
func (g *AdmittedGateway) acquire(ctx context.Context, slots chan struct{}) error {
	select {
	case slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// enterQueue registers a waiter and reports whether the wait queue had room.
func (g *AdmittedGateway) enterQueue() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.queueDepth > 0 && g.waiting >= g.queueDepth {
		return false
	}
	g.waiting++
	if g.waiting > g.maxWaiting {
		g.maxWaiting = g.waiting
	}
	return true
}

// leaveQueue removes one waiter.
func (g *AdmittedGateway) leaveQueue() {
	g.mu.Lock()
	g.waiting--
	g.mu.Unlock()
}

// accountSemaphore returns the bounded semaphore for one account, creating it on
// first use so an unseen account is bounded exactly like a known one.
func (g *AdmittedGateway) accountSemaphore(account domain.AccountID) chan struct{} {
	g.accountMu.Lock()
	defer g.accountMu.Unlock()
	if slots, ok := g.accountSlots[account]; ok {
		return slots
	}
	slots := make(chan struct{}, g.perAccountLimit)
	g.accountSlots[account] = slots
	return slots
}

// noteAccountInFlight records the highest simultaneous use of one account's
// semaphore, which is what proves the per-account bound held.
func (g *AdmittedGateway) noteAccountInFlight(inUse int) {
	g.mu.Lock()
	if inUse > g.maxAccountSeen {
		g.maxAccountSeen = inUse
	}
	g.mu.Unlock()
}

// countRejected records one refused request.
func (g *AdmittedGateway) countRejected() {
	g.mu.Lock()
	g.rejected++
	g.mu.Unlock()
}

// MemoryHealthStore is the in-memory routing.HealthStore the harness uses. It
// keeps route health accounting inside the measurement instead of in a file, so
// a scenario's health transitions stay in its own receipt.
type MemoryHealthStore struct {
	mu      sync.Mutex
	records map[string]domain.HealthRecord
	puts    int64
	gets    int64
}

var _ routing.HealthStore = (*MemoryHealthStore)(nil)

// NewMemoryHealthStore returns an empty health store.
func NewMemoryHealthStore() *MemoryHealthStore {
	return &MemoryHealthStore{records: map[string]domain.HealthRecord{}}
}

// Get returns the stored record for key.
func (s *MemoryHealthStore) Get(_ context.Context, key domain.HealthKey) (domain.HealthRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	record, ok := s.records[key.String()]
	return record, ok, nil
}

// Save upserts one record.
func (s *MemoryHealthStore) Save(_ context.Context, record domain.HealthRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	s.records[healthKeyOf(record)] = record
	return nil
}

// List returns every stored record.
func (s *MemoryHealthStore) List(context.Context) ([]domain.HealthRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.HealthRecord, 0, len(s.records))
	for _, record := range s.records {
		out = append(out, record)
	}
	return out, nil
}

// healthKeyOf builds a stable map key for a record. The account pointer is
// optional in the domain type (route-level records carry none), so it is
// rendered defensively rather than dereferenced.
func healthKeyOf(record domain.HealthRecord) string {
	account := ""
	if record.AccountID != nil {
		account = record.AccountID.Value()
	}
	return record.RouteID.Value() + "|" + account + "|" + record.Scope
}
