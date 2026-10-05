package inference

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// Config carries the operator's inference concurrency settings (PLAN 11,
// inference group) into the gate. Zero keeps each bound's documented "not
// configured" meaning rather than meaning zero capacity.
type Config struct {
	// GlobalConcurrency is the instance-wide cap on concurrent model requests.
	// It is required: a gate without a global bound protects nothing.
	GlobalConcurrency int
	// MaxAccountConcurrency caps one account inside that global pool. Zero
	// disables the per-account bound.
	MaxAccountConcurrency int
	// WaitQueueDepth bounds how many callers may wait for a slot. Zero refuses a
	// caller as soon as the gate is saturated instead of queueing it.
	WaitQueueDepth int
	// MaxWait bounds how long one caller waits for a slot, independently of its
	// own context. Zero leaves the caller's context as the only bound.
	MaxWait time.Duration
}

// ErrQueueFull reports that the bounded wait queue had no room, so the request
// was refused before any provider time was spent.
var ErrQueueFull = errors.New("inference: the wait queue is full")

// ErrWaitExceeded reports that the gate's bounded admission wait elapsed while
// the caller's context was still live.
var ErrWaitExceeded = errors.New("inference: the admission wait limit elapsed")

// Gate is a bounded admission gate in front of a ports.ModelGateway. It is a
// decorator: the composition root wraps the provider adapter with it, so
// routing, failover, and shell behavior are unchanged while every request that
// reaches a provider has passed the same limits.
//
// The account is the unit of admission. The gate reads only
// domain.ModelRequest.AccountID, so it is blind to keys, routes, sessions, and
// attempts: revoking or adding a key inside one account can neither change
// another account's budget nor multiply that account's own (PLAN 8.2 groups keys
// into an account that shares its quota).
//
// Concurrency model: one mutex guards the queue, the in-flight count, and the
// per-account counts, so every decision about capacity is made in one place.
// Provider I/O happens outside the lock, and each admitted caller releases its
// own slots when the request ends, however it ends. A queued caller holds no
// capacity: it waits before taking either slot, so an account that is saturated
// cannot hold the instance-wide pool hostage.
type Gate struct {
	inner ports.ModelGateway
	cfg   Config

	mu         sync.Mutex
	queue      waiters
	waiting    int
	inFlight   int
	perAccount map[domain.AccountID]int
	counters   counters
}

// Compile-time proof that the gate is a drop-in gateway.
var _ ports.ModelGateway = (*Gate)(nil)

// counters is the gate's observable history. It is plain state under the gate's
// mutex: these are observations rather than policy, so they measure with the
// runtime clock instead of an injected Clock port.
type counters struct {
	admitted    int64
	queueFull   int64
	waitExpired int64
	cancelled   int64
	maxInFlight int
	maxWaiting  int
	waitedMs    int64
}

// NewGate wraps inner with the configured bounds. The configuration is validated
// here, at the composition boundary, so an unusable limit fails startup instead
// of quietly meaning "no limit" once requests are already running.
func NewGate(inner ports.ModelGateway, cfg Config) (*Gate, error) {
	switch {
	case inner == nil:
		return nil, invalidConfig("a model gateway to admit is required")
	case cfg.GlobalConcurrency <= 0:
		return nil, invalidConfig(fmt.Sprintf("inference.global_concurrency must be at least 1, got %d", cfg.GlobalConcurrency))
	case cfg.MaxAccountConcurrency < 0:
		return nil, invalidConfig(fmt.Sprintf("inference.max_account_concurrency must not be negative, got %d", cfg.MaxAccountConcurrency))
	case cfg.MaxAccountConcurrency > cfg.GlobalConcurrency:
		return nil, invalidConfig(fmt.Sprintf(
			"inference.max_account_concurrency (%d) must not exceed inference.global_concurrency (%d)",
			cfg.MaxAccountConcurrency, cfg.GlobalConcurrency))
	case cfg.WaitQueueDepth < 0:
		return nil, invalidConfig(fmt.Sprintf("inference.wait_queue_depth must not be negative, got %d", cfg.WaitQueueDepth))
	case cfg.MaxWait < 0:
		return nil, invalidConfig(fmt.Sprintf("the inference admission wait must not be negative, got %s", cfg.MaxWait))
	}
	return &Gate{
		inner:      inner,
		cfg:        cfg,
		perAccount: map[domain.AccountID]int{},
	}, nil
}

func invalidConfig(detail string) error {
	return domain.NewValidationError(
		domain.CodeInvalidConfig,
		"the inference admission gate cannot be built",
		map[string]string{"detail": detail},
	)
}

// Request admits the caller, runs the inner gateway, and releases both slots
// however the request ends: success, provider failure, cancellation, or panic.
func (g *Gate) Request(ctx context.Context, req domain.ModelRequest) (domain.ModelResponse, error) {
	release, err := g.admit(ctx, req.AccountID)
	if err != nil {
		return domain.ModelResponse{}, err
	}
	defer release()
	return g.inner.Request(ctx, req)
}

// admit takes one global slot and one per-account slot, waiting in the bounded
// queue when both are saturated. The returned function frees the slots and is
// safe to call more than once, so a caller cannot inflate the budget.
func (g *Gate) admit(ctx context.Context, account domain.AccountID) (func(), error) {
	w := &waiter{account: account, started: time.Now(), ready: make(chan struct{})}

	g.mu.Lock()
	if err := ctx.Err(); err != nil {
		g.mu.Unlock()
		return nil, err
	}
	if g.canServeLocked(account) {
		g.takeLocked(account)
		g.mu.Unlock()
		return g.releaseFunc(account), nil
	}
	if g.queueIsFullLocked() {
		g.counters.queueFull++
		g.mu.Unlock()
		return nil, queueFullError(g.cfg.WaitQueueDepth)
	}
	g.queue.push(w)
	g.waiting++
	if g.waiting > g.counters.maxWaiting {
		g.counters.maxWaiting = g.waiting
	}
	g.mu.Unlock()

	// A nil channel blocks forever, so an unbounded gate-level wait leaves the
	// caller's context as the only bound.
	var maxWait <-chan time.Time
	if g.cfg.MaxWait > 0 {
		timer := time.NewTimer(g.cfg.MaxWait)
		defer timer.Stop()
		maxWait = timer.C
	}

	select {
	case <-w.ready:
		return g.releaseFunc(account), nil
	case <-maxWait:
		g.mu.Lock()
		g.abandonLocked(w)
		g.counters.waitExpired++
		g.mu.Unlock()
		return nil, waitExceededError(g.cfg.MaxWait)
	case <-ctx.Done():
		g.mu.Lock()
		g.abandonLocked(w)
		g.counters.cancelled++
		g.mu.Unlock()
		return nil, fmt.Errorf("inference: the admission wait ended: %w", ctx.Err())
	}
}

// canServeLocked reports whether both bounds leave room for the account.
func (g *Gate) canServeLocked(account domain.AccountID) bool {
	return g.inFlight < g.cfg.GlobalConcurrency && g.accountHasRoomLocked(account)
}

// accountHasRoomLocked reports whether the per-account bound leaves room. The
// bound is counted per account and never multiplied by an account's keys, routes,
// or sessions: an account holding several keys or several routes still spends
// one budget, so one account's credentials can never widen another's.
func (g *Gate) accountHasRoomLocked(account domain.AccountID) bool {
	if g.cfg.MaxAccountConcurrency <= 0 {
		return true // the per-account bound is not configured
	}
	return g.perAccount[account] < g.cfg.MaxAccountConcurrency
}

// queueIsFullLocked reports whether the bounded wait queue has no room. A depth
// of zero means the gate never waits: it refuses rather than hold the caller.
func (g *Gate) queueIsFullLocked() bool {
	return g.cfg.WaitQueueDepth <= 0 || g.waiting >= g.cfg.WaitQueueDepth
}

func (g *Gate) takeLocked(account domain.AccountID) {
	g.inFlight++
	g.perAccount[account]++
	g.counters.admitted++
	if g.inFlight > g.counters.maxInFlight {
		g.counters.maxInFlight = g.inFlight
	}
}

// releaseFunc returns the idempotent handoff that frees one global and one
// per-account slot.
func (g *Gate) releaseFunc(account domain.AccountID) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.releaseLocked(account)
		})
	}
}

// releaseLocked frees one slot pair and hands the freed capacity on.
func (g *Gate) releaseLocked(account domain.AccountID) {
	g.inFlight--
	if g.perAccount[account] <= 1 {
		delete(g.perAccount, account)
	} else {
		g.perAccount[account]--
	}
	g.pumpLocked()
}

// pumpLocked grants freed capacity to queued waiters in arrival order. A waiter
// whose account is still saturated is skipped instead of blocking the queue, so
// one busy account cannot stall everyone behind it; the skipped waiter keeps its
// place and is granted by a later handoff. Progress is guaranteed because only a
// holder that is running, or a holder already waiting on a running account,
// releases the capacity this loop hands out.
func (g *Gate) pumpLocked() {
	for g.inFlight < g.cfg.GlobalConcurrency {
		w := g.nextEligibleLocked()
		if w == nil {
			return
		}
		g.queue.remove(w)
		g.waiting--
		g.counters.waitedMs += time.Since(w.started).Milliseconds()
		g.takeLocked(w.account)
		w.granted = true
		close(w.ready)
	}
}

func (g *Gate) nextEligibleLocked() *waiter {
	for w := g.queue.head; w != nil; w = w.next {
		if g.accountHasRoomLocked(w.account) {
			return w
		}
	}
	return nil
}

// abandonLocked gives up one waiter's claim. When the grant won the race with
// the caller's cancellation the slots are handed straight on, so a caller that
// gave up never holds capacity; otherwise the waiter leaves the queue.
func (g *Gate) abandonLocked(w *waiter) {
	if w.granted {
		g.releaseLocked(w.account)
		return
	}
	g.queue.remove(w)
	g.waiting--
	g.counters.waitedMs += time.Since(w.started).Milliseconds()
}

// queueFullError reports local backpressure as a limit failure. It is not a
// provider error: no provider time was spent, so no health record may be
// penalized because of it (PLAN 9.1).
func queueFullError(depth int) error {
	return domain.WrapError(
		fmt.Errorf("%w: depth %d", ErrQueueFull, depth),
		domain.CategoryLimit,
		domain.CodeRateLimited,
		"the inference wait queue is full, so the request was refused before reaching a provider",
	)
}

// waitExceededError reports the same local refusal for a wait that ended on the
// gate's own bound rather than on the caller's context.
func waitExceededError(maxWait time.Duration) error {
	return domain.WrapError(
		fmt.Errorf("%w: %s", ErrWaitExceeded, maxWait),
		domain.CategoryLimit,
		domain.CodeTimeout,
		"the inference admission wait elapsed, so the request was refused before reaching a provider",
	)
}
