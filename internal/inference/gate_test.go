package inference_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/inference"
	"j0s.at/vibeshell/internal/ports"
)

// settleTimeout bounds how long a test waits for an observable state to
// change. It is deliberately generous: these tests assert state, not latency.
const settleTimeout = 10 * time.Second

// quietPeriod is how long a test waits before concluding that nothing happened.
// Waiting for the absence of an event is the one place a real delay is needed;
// the gate is otherwise driven through channels and snapshots.
const quietPeriod = 250 * time.Millisecond

var (
	accountA = mustAccount(1)
	accountB = mustAccount(2)
	routeA   = mustRoute(1)
	routeB   = mustRoute(2)
)

// ---------------------------------------------------------------------------
// Behavioral tests
// ---------------------------------------------------------------------------

// TestGlobalConcurrencyCapIsHeld proves that no more than
// Config.GlobalConcurrency requests reach the inner gateway at once, and that a
// caller beyond the cap waits for a slot instead of running in parallel.
func TestGlobalConcurrencyCapIsHeld(t *testing.T) {
	inner := newBlockingGateway()
	gate := mustGate(t, inference.Config{GlobalConcurrency: 2, WaitQueueDepth: 4}, inner)

	holders := []<-chan error{
		callAsync(gate, context.Background(), request(accountA, routeA)),
		callAsync(gate, context.Background(), request(accountB, routeA)),
	}
	inner.waitForStarts(t, 2)

	queued := callAsync(gate, context.Background(), request(accountA, routeB))
	awaitSnapshot(t, gate, func(s inference.Snapshot) bool { return s.Waiting == 1 })
	expectNoResult(t, queued)
	if got := inner.maxInFlightSeen(); got != 2 {
		t.Fatalf("inner gateway ran %d requests concurrently, want the global cap 2", got)
	}

	// Releasing one holder admits exactly one waiter, and only one.
	inner.release()
	inner.waitForStarts(t, 3)
	if got := inner.maxInFlightSeen(); got != 2 {
		t.Fatalf("inner gateway ran %d requests concurrently after a handoff, want 2", got)
	}

	// Two holders and the queued caller each need one release token.
	for range 3 {
		inner.release()
	}
	for i, done := range append(holders, queued) {
		if err := awaitResult(t, done); err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	final := gate.Snapshot()
	if final.InFlight != 0 || final.Waiting != 0 {
		t.Fatalf("gate leaked accounting: in flight %d, waiting %d", final.InFlight, final.Waiting)
	}
	if final.Admitted != 3 {
		t.Fatalf("admitted %d requests, want 3", final.Admitted)
	}
}

// TestPerAccountCapIsHeldAndIndependent proves that one account never exceeds
// Config.MaxAccountConcurrency, and that a saturated account neither consumes
// nor blocks another account's budget.
func TestPerAccountCapIsHeldAndIndependent(t *testing.T) {
	inner := newBlockingGateway()
	gate := mustGate(t, inference.Config{
		GlobalConcurrency:     4,
		MaxAccountConcurrency: 1,
		WaitQueueDepth:        4,
	}, inner)

	first := callAsync(gate, context.Background(), request(accountA, routeA))
	inner.waitForStarts(t, 1)

	secondOnA := callAsync(gate, context.Background(), request(accountA, routeB))
	awaitSnapshot(t, gate, func(s inference.Snapshot) bool { return s.Waiting == 1 })
	expectNoResult(t, secondOnA)

	// A different account still runs while A is saturated: neither the cap nor
	// the queue behind A belongs to another account's budget.
	onB := callAsync(gate, context.Background(), request(accountB, routeA))
	inner.waitForStarts(t, 2)
	if got := inner.maxInFlightSeen(); got != 2 {
		t.Fatalf("inner gateway ran %d requests concurrently, want 2 (one per account)", got)
	}
	awaitSnapshot(t, gate, func(s inference.Snapshot) bool { return s.AccountsInFlight[accountA] == 1 })

	for range 3 {
		inner.release()
	}
	if err := awaitResult(t, first); err != nil {
		t.Fatalf("first request on account A: %v", err)
	}
	if err := awaitResult(t, secondOnA); err != nil {
		t.Fatalf("second request on account A: %v", err)
	}
	if err := awaitResult(t, onB); err != nil {
		t.Fatalf("request on account B: %v", err)
	}
}

// TestAccountBudgetIsNotMultipliedPerRoute pins the unit of the per-account
// bound: the account, never a key or a route. Two requests for one account on
// different routes share the single budget that account was granted, so adding
// routes or keys cannot multiply it.
func TestAccountBudgetIsNotMultipliedPerRoute(t *testing.T) {
	inner := newBlockingGateway()
	gate := mustGate(t, inference.Config{
		GlobalConcurrency:     8,
		MaxAccountConcurrency: 1,
		WaitQueueDepth:        4,
	}, inner)

	first := callAsync(gate, context.Background(), request(accountA, routeA))
	inner.waitForStarts(t, 1)
	second := callAsync(gate, context.Background(), request(accountA, routeB))
	awaitSnapshot(t, gate, func(s inference.Snapshot) bool { return s.Waiting == 1 })
	expectNoResult(t, second)

	for range 2 {
		inner.release()
	}
	if err := awaitResult(t, first); err != nil {
		t.Fatalf("first route on account A: %v", err)
	}
	if err := awaitResult(t, second); err != nil {
		t.Fatalf("second route on account A: %v", err)
	}
	if got := inner.maxInFlightSeen(); got != 1 {
		t.Fatalf("one account ran %d requests at once, want 1", got)
	}
}

// TestWaitQueueDepthIsBounded proves that the wait queue refuses a caller once
// Config.WaitQueueDepth waiters are queued, reports the refusal as local
// backpressure rather than a provider fault, and never lets the refused request
// reach the gateway.
func TestWaitQueueDepthIsBounded(t *testing.T) {
	inner := newBlockingGateway()
	gate := mustGate(t, inference.Config{GlobalConcurrency: 1, WaitQueueDepth: 1}, inner)

	holder := callAsync(gate, context.Background(), request(accountA, routeA))
	inner.waitForStarts(t, 1)
	waiter := callAsync(gate, context.Background(), request(accountB, routeA))
	awaitSnapshot(t, gate, func(s inference.Snapshot) bool { return s.Waiting == 1 })

	refused := callAsync(gate, context.Background(), request(accountA, routeA))
	err := awaitResult(t, refused)
	if !errors.Is(err, inference.ErrQueueFull) {
		t.Fatalf("refused request error = %v, want inference.ErrQueueFull", err)
	}
	if !domain.IsLimitError(err) {
		t.Fatalf("refused request error = %v, want a domain limit error", err)
	}
	if got := inner.callCount(); got != 1 {
		t.Fatalf("the gateway saw %d requests, want 1: a refused request must not reach it", got)
	}

	for range 2 {
		inner.release()
	}
	if err := awaitResult(t, holder); err != nil {
		t.Fatalf("holder: %v", err)
	}
	if err := awaitResult(t, waiter); err != nil {
		t.Fatalf("queued waiter: %v", err)
	}
	final := gate.Snapshot()
	if final.QueueFullRejections != 1 {
		t.Fatalf("recorded %d queue-full rejections, want 1", final.QueueFullRejections)
	}
	if final.Admitted != 2 {
		t.Fatalf("admitted %d requests, want 2", final.Admitted)
	}
}

// TestWaitIsBoundedByContextDeadline proves that a queued caller stops waiting
// when its own context deadline passes, that the error still reports the
// deadline, and that the abandoned waiter leaves the queue clean.
func TestWaitIsBoundedByContextDeadline(t *testing.T) {
	inner := newBlockingGateway()
	gate := mustGate(t, inference.Config{GlobalConcurrency: 1, WaitQueueDepth: 4}, inner)

	holder := callAsync(gate, context.Background(), request(accountA, routeA))
	inner.waitForStarts(t, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	waiter := callAsync(gate, ctx, request(accountB, routeA))

	if err := awaitResult(t, waiter); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired waiter error = %v, want context.DeadlineExceeded", err)
	}
	awaitSnapshot(t, gate, func(s inference.Snapshot) bool { return s.Waiting == 0 })
	if got := inner.callCount(); got != 1 {
		t.Fatalf("the gateway saw %d requests, want 1: an expired waiter must not run", got)
	}

	// The expired waiter left no claim on the freed slot.
	inner.release()
	if err := awaitResult(t, holder); err != nil {
		t.Fatalf("holder: %v", err)
	}
	next := callAsync(gate, context.Background(), request(accountB, routeA))
	inner.waitForStarts(t, 2)
	inner.release()
	if err := awaitResult(t, next); err != nil {
		t.Fatalf("request after the queue drained: %v", err)
	}
}

// TestWaitIsBoundedByMaxWait proves the gate's own bounded wait: with no context
// deadline the caller is refused once Config.MaxWait elapses instead of waiting
// for an unbounded slot.
func TestWaitIsBoundedByMaxWait(t *testing.T) {
	const maxWait = 100 * time.Millisecond
	inner := newBlockingGateway()
	gate := mustGate(t, inference.Config{
		GlobalConcurrency: 1,
		WaitQueueDepth:    4,
		MaxWait:           maxWait,
	}, inner)

	holder := callAsync(gate, context.Background(), request(accountA, routeA))
	inner.waitForStarts(t, 1)

	started := time.Now()
	waiter := callAsync(gate, context.Background(), request(accountB, routeA))
	if err := awaitResult(t, waiter); !errors.Is(err, inference.ErrWaitExceeded) {
		t.Fatalf("expired waiter error = %v, want inference.ErrWaitExceeded", err)
	}
	if elapsed := time.Since(started); elapsed < maxWait {
		t.Fatalf("wait ended after %v, before the configured %v", elapsed, maxWait)
	}
	awaitSnapshot(t, gate, func(s inference.Snapshot) bool { return s.Waiting == 0 })
	if got := inner.callCount(); got != 1 {
		t.Fatalf("the gateway saw %d requests, want 1", got)
	}

	inner.release()
	if err := awaitResult(t, holder); err != nil {
		t.Fatalf("holder: %v", err)
	}
	if got := gate.Snapshot().WaitExpired; got != 1 {
		t.Fatalf("recorded %d expired waits, want 1", got)
	}
}

// TestCancellationFreesTheSlot proves that a cancelled waiter gives up its
// claim instead of consuming a slot: once the running holder leaves, the next
// caller is admitted immediately even though the cancelled caller never ran.
func TestCancellationFreesTheSlot(t *testing.T) {
	inner := newBlockingGateway()
	gate := mustGate(t, inference.Config{GlobalConcurrency: 1, WaitQueueDepth: 4}, inner)

	holder := callAsync(gate, context.Background(), request(accountA, routeA))
	inner.waitForStarts(t, 1)

	ctx, cancel := context.WithCancel(context.Background())
	waiter := callAsync(gate, ctx, request(accountB, routeA))
	awaitSnapshot(t, gate, func(s inference.Snapshot) bool { return s.Waiting == 1 })
	cancel()
	if err := awaitResult(t, waiter); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter error = %v, want context.Canceled", err)
	}

	inner.release()
	if err := awaitResult(t, holder); err != nil {
		t.Fatalf("holder: %v", err)
	}
	next := callAsync(gate, context.Background(), request(accountB, routeA))
	inner.waitForStarts(t, 2)
	inner.release()
	if err := awaitResult(t, next); err != nil {
		t.Fatalf("request after a cancelled waiter: %v", err)
	}
	awaitSnapshot(t, gate, func(s inference.Snapshot) bool { return s.Cancelled == 1 })
}

// TestCancellationRacingAHandoffKeepsAccountingConsistent drives cancellation
// into the handoff of a slot, where the grant and the caller give up at the
// same moment. The outcome depends on the interleaving, so the assertions are
// invariants: the cap still holds, nothing leaks, no caller is admitted twice,
// and no request reaches the gateway without a slot.
func TestCancellationRacingAHandoffKeepsAccountingConsistent(t *testing.T) {
	const callers = 16
	inner := newBlockingGateway()
	gate := mustGate(t, inference.Config{
		GlobalConcurrency:     2,
		MaxAccountConcurrency: 2,
		WaitQueueDepth:        callers,
	}, inner)

	holder := callAsync(gate, context.Background(), request(accountA, routeA))
	inner.waitForStarts(t, 1)

	ctx, cancel := context.WithCancel(context.Background())
	waiters := make([]<-chan error, 0, callers)
	for range callers {
		waiters = append(waiters, callAsync(gate, ctx, request(accountA, routeA)))
	}
	awaitSnapshot(t, gate, func(s inference.Snapshot) bool { return s.Waiting == callers-1 })

	// Cancel the whole queue while the holder's slot is being handed off.
	go cancel()
	for range callers + 2 {
		inner.release()
	}
	if err := awaitResult(t, holder); err != nil {
		t.Fatalf("holder: %v", err)
	}
	granted := int64(0)
	for i, done := range waiters {
		err := awaitResult(t, done)
		switch {
		case err == nil, errors.Is(err, context.Canceled):
			if err == nil {
				granted++
			}
		default:
			t.Fatalf("waiter %d: %v", i, err)
		}
	}

	snapshot := gate.Snapshot()
	if snapshot.InFlight != 0 || snapshot.Waiting != 0 {
		t.Fatalf("gate leaked accounting: in flight %d, waiting %d", snapshot.InFlight, snapshot.Waiting)
	}
	if snapshot.MaxInFlight > 2 || inner.maxInFlightSeen() > 2 {
		t.Fatalf("peak concurrency %d/%d exceeded the global cap 2", snapshot.MaxInFlight, inner.maxInFlightSeen())
	}
	// A slot is granted once per admitted request, and no request runs without
	// one: admissions cannot exceed the callers, the gateway cannot have seen
	// more requests than admissions, and every completed caller was backed by a
	// gateway request.
	if snapshot.Admitted > callers+1 {
		t.Fatalf("admitted %d slots for %d callers: a slot was granted twice", snapshot.Admitted, callers+1)
	}
	if int64(inner.callCount()) > snapshot.Admitted {
		t.Fatalf("the gateway ran %d requests but only %d were admitted", inner.callCount(), snapshot.Admitted)
	}
	if granted > int64(inner.callCount()) {
		t.Fatalf("%d callers completed but the gateway ran only %d requests", granted, inner.callCount())
	}
}

// TestConfigValidationRejectsUnusableLimits keeps a bad configuration from
// reaching the request path, where it would silently mean "no bound at all".
func TestConfigValidationRejectsUnusableLimits(t *testing.T) {
	cases := []struct {
		name string
		cfg  inference.Config
	}{
		{"no global cap", inference.Config{GlobalConcurrency: 0}},
		{"negative global cap", inference.Config{GlobalConcurrency: -1}},
		{"negative account cap", inference.Config{GlobalConcurrency: 2, MaxAccountConcurrency: -1}},
		{"account cap above global", inference.Config{GlobalConcurrency: 2, MaxAccountConcurrency: 3}},
		{"negative queue depth", inference.Config{GlobalConcurrency: 2, WaitQueueDepth: -1}},
		{"negative max wait", inference.Config{GlobalConcurrency: 2, MaxWait: -time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := inference.NewGate(newBlockingGateway(), tc.cfg)
			if err == nil {
				t.Fatal("NewGate accepted an invalid configuration")
			}
			if !domain.IsValidationError(err) {
				t.Fatalf("error = %v, want a domain validation error", err)
			}
		})
	}

	if _, err := inference.NewGate(nil, inference.Config{GlobalConcurrency: 1}); err == nil {
		t.Fatal("NewGate accepted a nil inner gateway")
	}
	// Zero means "not configured" for the two optional bounds: an account cap of
	// zero disables the per-account bound, and a queue depth of zero refuses
	// instead of waiting.
	if _, err := inference.NewGate(newBlockingGateway(), inference.Config{GlobalConcurrency: 1}); err != nil {
		t.Fatalf("NewGate rejected a valid configuration: %v", err)
	}
}

// TestQueueDepthZeroRefusesImmediately pins the documented meaning of a zero
// wait queue: no waiting at all, so a saturated gate refuses rather than
// holding the caller.
func TestQueueDepthZeroRefusesImmediately(t *testing.T) {
	inner := newBlockingGateway()
	gate := mustGate(t, inference.Config{GlobalConcurrency: 1}, inner)

	holder := callAsync(gate, context.Background(), request(accountA, routeA))
	inner.waitForStarts(t, 1)

	refused := callAsync(gate, context.Background(), request(accountB, routeA))
	if err := awaitResult(t, refused); !errors.Is(err, inference.ErrQueueFull) {
		t.Fatalf("error = %v, want inference.ErrQueueFull", err)
	}
	if got := gate.Snapshot().Waiting; got != 0 {
		t.Fatalf("waited %d callers with a zero queue depth, want 0", got)
	}
	inner.release()
	if err := awaitResult(t, holder); err != nil {
		t.Fatalf("holder: %v", err)
	}
}

// TestCancelledContextIsRefusedBeforeQueueing proves a caller that is already
// cancelled consumes neither queue space nor a slot.
func TestCancelledContextIsRefusedBeforeQueueing(t *testing.T) {
	inner := newBlockingGateway()
	gate := mustGate(t, inference.Config{GlobalConcurrency: 2, WaitQueueDepth: 4}, inner)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gate.Request(ctx, request(accountA, routeA)); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if got := inner.callCount(); got != 0 {
		t.Fatalf("the gateway saw %d requests, want 0", got)
	}
	snapshot := gate.Snapshot()
	if snapshot.Waiting != 0 || snapshot.InFlight != 0 {
		t.Fatalf("gate accounted for a refused request: in flight %d, waiting %d", snapshot.InFlight, snapshot.Waiting)
	}
}

// ---------------------------------------------------------------------------
// Test doubles and helpers
// ---------------------------------------------------------------------------

// blockingGateway is a ports.ModelGateway double that holds every request open
// until the test releases it. It records how many requests overlapped, which is
// the evidence that the gate really bounds concurrency rather than merely
// reporting that it did.
type blockingGateway struct {
	mu          sync.Mutex
	inFlight    int
	maxInFlight int
	calls       int

	// observed counts the start signals the test has already consumed.
	observed int
	started  chan struct{}
	tokens   chan struct{}
}

func newBlockingGateway() *blockingGateway {
	return &blockingGateway{
		started: make(chan struct{}, 128),
		tokens:  make(chan struct{}, 128),
	}
}

func (f *blockingGateway) Request(ctx context.Context, req domain.ModelRequest) (domain.ModelResponse, error) {
	f.mu.Lock()
	f.calls++
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	f.mu.Unlock()

	f.started <- struct{}{}
	select {
	case <-f.tokens:
	case <-ctx.Done():
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
		return domain.ModelResponse{}, ctx.Err()
	}

	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()
	return domain.ModelResponse{
		RequestID: req.RequestID,
		RouteID:   req.RouteID,
		AccountID: req.AccountID,
	}, nil
}

func (f *blockingGateway) maxInFlightSeen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInFlight
}

func (f *blockingGateway) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// waitForStarts blocks until the gateway has accepted at least n requests in
// total, counting the requests an earlier wait already observed. Only the test
// goroutine reads and writes observed.
func (f *blockingGateway) waitForStarts(t *testing.T, n int) {
	t.Helper()
	for f.observed < n {
		select {
		case <-f.started:
			f.observed++
		case <-time.After(settleTimeout):
			t.Fatalf("the gateway started %d of %d expected requests", f.observed, n)
		}
	}
}

// release lets one blocked request finish. Tokens may be pushed ahead of the
// requests that consume them, so a test can drain a known number of callers.
func (f *blockingGateway) release() { f.tokens <- struct{}{} }

// callAsync runs one request in the background so a test can hold a slot open
// while it inspects the gate.
func callAsync(gw ports.ModelGateway, ctx context.Context, req domain.ModelRequest) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := gw.Request(ctx, req)
		done <- err
	}()
	return done
}

// awaitResult waits for a caller's outcome.
func awaitResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(settleTimeout):
		t.Fatal("a gateway call did not return")
		return nil
	}
}

// expectNoResult fails when a caller returns within the quiet period.
func expectNoResult(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("a call returned while it should have been waiting: %v", err)
	case <-time.After(quietPeriod):
	}
}

// awaitSnapshot waits until the gate reports a state the test needs.
func awaitSnapshot(t *testing.T, gate *inference.Gate, want func(inference.Snapshot) bool) {
	t.Helper()
	deadline := time.Now().Add(settleTimeout)
	for time.Now().Before(deadline) {
		if want(gate.Snapshot()) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the gate never reported the expected state; last snapshot: %+v", gate.Snapshot())
}

func mustGate(t *testing.T, cfg inference.Config, inner ports.ModelGateway) *inference.Gate {
	t.Helper()
	gate, err := inference.NewGate(inner, cfg)
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	return gate
}

func request(account domain.AccountID, route domain.RouteID) domain.ModelRequest {
	return domain.ModelRequest{
		AccountID: account,
		RouteID:   route,
		RequestID: account.Value() + "/" + route.Value(),
	}
}

// identity builds a deterministic, valid identity. The identity grammar is a
// fixed-width base32 value, so a shorthand would not parse.
func identity(prefix string, n int) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	value := make([]byte, 26)
	for i := range value {
		value[i] = alphabet[0]
	}
	for i := 25; i >= 0 && n > 0; i-- {
		value[i] = alphabet[n%32]
		n /= 32
	}
	return prefix + "_" + string(value)
}

func mustAccount(n int) domain.AccountID {
	return domain.MustParseAccountID(identity(domain.PrefixAccount, n))
}
func mustRoute(n int) domain.RouteID { return domain.MustParseRouteID(identity(domain.PrefixRoute, n)) }
