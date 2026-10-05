package routing

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/inference"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

type fakeGateway struct {
	mu    sync.Mutex
	hand  func(req domain.ModelRequest) (domain.ModelResponse, error)
	calls []domain.ModelRequest
}

func (f *fakeGateway) Request(ctx context.Context, req domain.ModelRequest) (domain.ModelResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	return f.hand(req)
}

func (f *fakeGateway) requests() []domain.ModelRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.ModelRequest(nil), f.calls...)
}

type fakeClock struct {
	mu  sync.Mutex
	now int64
}

func (c *fakeClock) NowUnixMilli() int64   { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) MonotonicNanos() int64 { return c.NowUnixMilli() * 1e6 }
func (c *fakeClock) advance(ms int64)      { c.mu.Lock(); c.now += ms; c.mu.Unlock() }

type randSource struct{ r *rand.Rand }

func newRandSource(seed int64) *randSource { return &randSource{r: rand.New(rand.NewSource(seed))} }
func (s *randSource) Bytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := s.r.Read(b)
	return b, err
}
func (s *randSource) Intn(n int) int { return s.r.Intn(n) }

type memHealthStore struct {
	mu sync.Mutex
	m  map[string]domain.HealthRecord
}

func newMemHealthStore() *memHealthStore { return &memHealthStore{m: map[string]domain.HealthRecord{}} }

func (s *memHealthStore) Get(ctx context.Context, key domain.HealthKey) (domain.HealthRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.m[key.String()]
	return rec, ok, nil
}

func (s *memHealthStore) Save(ctx context.Context, rec domain.HealthRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := domain.HealthKey{RouteID: rec.RouteID, Scope: rec.Scope}
	if rec.AccountID != nil {
		key.AccountID = *rec.AccountID
	}
	s.m[key.String()] = rec
	return nil
}

func (s *memHealthStore) List(ctx context.Context) ([]domain.HealthRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.HealthRecord
	for _, rec := range s.m {
		out = append(out, rec)
	}
	return out, nil
}

type fakeLedger struct {
	mu       sync.Mutex
	reserved []float64
	settled  []float64
	released int
}

func (l *fakeLedger) Reserve(ctx context.Context, ref SpendingRef, amount float64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reserved = append(l.reserved, amount)
	return nil
}
func (l *fakeLedger) Reconcile(ctx context.Context, ref SpendingRef, actual float64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.settled = append(l.settled, actual)
	return nil
}
func (l *fakeLedger) Release(ctx context.Context, ref SpendingRef) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.released++
	return nil
}

// ---------------------------------------------------------------------------
// Fixture helpers
// ---------------------------------------------------------------------------

const alpha = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// identity builds a regex-valid identity of the given prefix and a small
// integer, suitable for deterministic test fixtures.
func identity(prefix string, n int) string {
	v := make([]byte, 26)
	for i := range v {
		v[i] = alpha[0]
	}
	for i := 25; i >= 0 && n > 0; i-- {
		v[i] = alpha[n%32]
		n /= 32
	}
	return prefix + "_" + string(v)
}

func mustRoute(s string) domain.RouteID { id, err := domain.ParseRouteID(s); must(err); return id }
func mustAccount(s string) domain.AccountID {
	id, err := domain.ParseAccountID(s)
	must(err)
	return id
}
func mustKey(s string) domain.KeyRef { id, err := domain.ParseKeyRef(s); must(err); return id }
func mustSession(s string) domain.SessionID {
	id, err := domain.ParseSessionID(s)
	must(err)
	return id
}
func mustTurn(s string) domain.TurnID { id, err := domain.ParseTurnID(s); must(err); return id }
func must(err error) {
	if err != nil {
		panic(err)
	}
}

func successResp(req domain.ModelRequest, cost *float64) (domain.ModelResponse, error) {
	return domain.ModelResponse{
		RequestID:    req.RequestID,
		RouteID:      req.RouteID,
		AccountID:    req.AccountID,
		Message:      domain.Message{Role: domain.RoleAssistant, Content: "ok"},
		FinishReason: domain.FinishReasonStop,
		Usage:        domain.Usage{TotalTokens: 3, EstimatedCostUSD: cost},
		Timestamp:    time.Now().UnixMilli(),
	}, nil
}

func envFor(class domain.FailureClass, route domain.RouteID, account domain.AccountID, retryAfter *int64) error {
	env := domain.NewErrorEnvelope(class, string(class), route, account, time.Now().UnixMilli())
	env.RetryAfter = retryAfter
	return env
}

var (
	r1 = mustRoute(identity("rte", 1))
	r2 = mustRoute(identity("rte", 2))
	r3 = mustRoute(identity("rte", 3))
	a1 = mustAccount(identity("acc", 4))
	a2 = mustAccount(identity("acc", 5))
	a3 = mustAccount(identity("acc", 6))
)

var basePolicy = domain.RoutePolicy{
	Tiers: []domain.TierConfig{
		{Name: "t0", RouteIDs: []domain.RouteID{r1}, Enabled: true},
	},
	MaxAttempts:    8,
	TurnDeadlineMs: 60_000,
	ProbeBudget:    domain.ProbeBudget{MaxProbesPerHour: 4},
}

func accountWithKeys(id domain.AccountID, group string, product string, keys int) domain.Account {
	acc := domain.Account{ID: id, QuotaGroup: group, PermittedProducts: []string{product}, Enabled: true}
	for i := 0; i < keys; i++ {
		acc.KeyRefs = append(acc.KeyRefs, mustKey(identity("key", 10+i)))
	}
	return acc
}

func newTestRouter(gw *fakeGateway, hs *memHealthStore, ledger SpendingLedger, clk *fakeClock, seed int64, cfg Config) *Router {
	return NewRouter(gw, hs, ledger, clk, newRandSource(seed), cfg)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestUniformSelectionIndependentOfKeyCount(t *testing.T) {
	gw := &fakeGateway{hand: func(req domain.ModelRequest) (domain.ModelResponse, error) { return successResp(req, nil) }}
	hand := func(req domain.ModelRequest) (domain.ModelResponse, error) { return successResp(req, nil) }
	gw.hand = hand
	cfg := Config{
		Policy: domain.RoutePolicy{
			Tiers:       []domain.TierConfig{{Name: "t0", RouteIDs: []domain.RouteID{r1, r2}, Enabled: true}},
			MaxAttempts: 4, TurnDeadlineMs: 60_000,
		},
		Routes: []RouteSpec{{ID: r1, Product: "p1"}, {ID: r2, Product: "p2"}},
		Accounts: []domain.Account{
			accountWithKeys(a1, "g1", "p1", 3), // weighted 3x by raw key count
			accountWithKeys(a2, "g2", "p2", 1),
		},
	}
	counts := map[domain.RouteID]int{}
	for i := 0; i < 600; i++ {
		h := newMemHealthStore()
		clk := &fakeClock{now: 1_000}
		router := newTestRouter(gw, h, nil, clk, int64(i+1), cfg)
		b, err := router.EnsureBinding(context.Background(), mustSession(identity("ses", 100+i)))
		if err != nil {
			t.Fatal(err)
		}
		counts[b.Route]++
	}
	if c := counts[r1]; c < 200 || c > 400 {
		t.Errorf("r1 selected %d/600, want ~300 (uniform, not key-weighted)", c)
	}
	if c := counts[r2]; c < 200 || c > 400 {
		t.Errorf("r2 selected %d/600, want ~300", c)
	}
}

func TestTierExhaustionOrder(t *testing.T) {
	gw := &fakeGateway{}
	gw.hand = func(req domain.ModelRequest) (domain.ModelResponse, error) {
		switch req.AccountID {
		case a1:
			return domain.ModelResponse{}, envFor(domain.FailureProviderOutage, req.RouteID, req.AccountID, nil)
		case a2:
			return domain.ModelResponse{}, envFor(domain.FailureQuotaExhausted, req.RouteID, req.AccountID, nil)
		default:
			return successResp(req, nil)
		}
	}
	h := newMemHealthStore()
	clk := &fakeClock{now: 10_000}
	cfg := Config{
		Policy: domain.RoutePolicy{
			Tiers: []domain.TierConfig{
				{Name: "t0", RouteIDs: []domain.RouteID{r1}, Enabled: true, AccountPool: "p0"},
				{Name: "t1", RouteIDs: []domain.RouteID{r3}, Enabled: true, AccountPool: "p1"},
			},
			AccountPools:   map[string][]domain.AccountID{"p0": {a1, a2}, "p1": {a3}},
			MaxAttempts:    8,
			TurnDeadlineMs: 60_000,
		},
		Routes: []RouteSpec{{ID: r1, Product: "p1"}, {ID: r3, Product: "p1"}},
		Accounts: []domain.Account{
			accountWithKeys(a1, "g1", "p1", 1),
			accountWithKeys(a2, "g2", "p1", 1),
			accountWithKeys(a3, "g3", "p1", 1),
		},
	}
	router := newTestRouter(gw, h, nil, clk, 3, cfg)
	commits := 0
	_, err := router.ExecuteTurn(context.Background(), TurnRequest{
		Session: mustSession(identity("ses", 20)), Turn: mustTurn(identity("trn", 20)),
		Messages: []domain.Message{{Role: domain.RoleUser, Content: "hi"}},
		Commit:   func(ctx context.Context) error { commits++; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if commits != 1 {
		t.Errorf("commits=%d, want 1", commits)
	}
	calls := gw.requests()
	if len(calls) != 3 {
		t.Fatalf("gateway calls=%d, want 3", len(calls))
	}
	if calls[0].AccountID != a1 && calls[0].AccountID != a2 {
		t.Errorf("first call unexpected %v", calls[0].AccountID)
	}
	if calls[0].RouteID != r1 || calls[1].RouteID != r1 {
		t.Errorf("first two attempts must stay in tier0 route r1: %v %v", calls[0].RouteID, calls[1].RouteID)
	}
	if calls[0].AccountID == calls[1].AccountID {
		t.Errorf("second attempt must switch account within route r1")
	}
	if calls[2].RouteID != r3 || calls[2].AccountID != a3 {
		t.Errorf("third attempt must advance to tier1 r3/a3, got %v/%v", calls[2].RouteID, calls[2].AccountID)
	}
}

func TestRevokedKeyIsolation(t *testing.T) {
	gw := &fakeGateway{}
	gw.hand = func(req domain.ModelRequest) (domain.ModelResponse, error) {
		if req.AccountID == a1 {
			return domain.ModelResponse{}, envFor(domain.FailureInvalidCredential, req.RouteID, req.AccountID, nil)
		}
		return successResp(req, nil)
	}
	h := newMemHealthStore()
	clk := &fakeClock{now: 5_000}
	cfg := Config{
		Policy: domain.RoutePolicy{
			Tiers:       []domain.TierConfig{{Name: "t0", RouteIDs: []domain.RouteID{r1}, Enabled: true}},
			MaxAttempts: 4, TurnDeadlineMs: 60_000,
		},
		Routes:   []RouteSpec{{ID: r1, Product: "p1"}},
		Accounts: []domain.Account{accountWithKeys(a1, "g1", "p1", 2), accountWithKeys(a2, "g2", "p1", 1)},
	}
	router := newTestRouter(gw, h, nil, clk, 2, cfg)
	// Drive a1's credential failure deterministically through one attempt.
	_, _, _ = router.attempt(context.Background(), attemptInput{
		session:  mustSession(identity("ses", 21)),
		turn:     mustTurn(identity("trn", 21)),
		messages: []domain.Message{{Role: domain.RoleUser, Content: "hi"}},
		deadline: clk.now + 60_000,
		seq:      1,
	}, r1, a1)
	// The full turn path must reach a2: a1's credential cools, so the
	// failure never disables the route or other accounts.
	res, err := router.ExecuteTurn(context.Background(), TurnRequest{
		Session: mustSession(identity("ses", 29)), Turn: mustTurn(identity("trn", 29)),
		Messages: []domain.Message{{Role: domain.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Binding.Account != a2 {
		t.Errorf("winning account = %v, want a2", res.Binding.Account)
	}
	// a1's credential is cooling; route record must remain healthy and a2 stays eligible.
	rec, ok, _ := h.Get(context.Background(), domain.HealthKey{RouteID: r1, AccountID: a1, Scope: ScopeCredential})
	if !ok || rec.State == domain.HealthHealthy {
		t.Errorf("a1 credential record missing/healthy: %+v", rec)
	}
	routeRec, ok, _ := h.Get(context.Background(), domain.HealthKey{RouteID: r1, Scope: ScopeRoute})
	if ok && routeRec.State != domain.HealthHealthy {
		t.Errorf("route record must not be harmed: %+v", routeRec)
	}
	// A fresh session must never bind to the revoked account.
	b, err := router.EnsureBinding(context.Background(), mustSession(identity("ses", 30)))
	if err != nil {
		t.Fatal(err)
	}
	if b.Account != a2 {
		t.Errorf("fresh binding account = %v, want a2 (a1 revoked)", b.Account)
	}
}

func TestCooldownResetProbe(t *testing.T) {
	gw := &fakeGateway{}
	hand := func(req domain.ModelRequest) (domain.ModelResponse, error) { return successResp(req, nil) }
	gw.hand = hand
	h := newMemHealthStore()
	clk := &fakeClock{now: 1_000}
	cfg := Config{
		Policy: domain.RoutePolicy{
			Tiers:       []domain.TierConfig{{Name: "t0", RouteIDs: []domain.RouteID{r1}, Enabled: true}},
			MaxAttempts: 4, TurnDeadlineMs: 60_000, ProbeBudget: domain.ProbeBudget{MaxProbesPerHour: 4},
		},
		Routes:   []RouteSpec{{ID: r1, Product: "p1"}},
		Accounts: []domain.Account{accountWithKeys(a1, "g1", "p1", 1)},
	}
	router := newTestRouter(gw, h, nil, clk, 1, cfg)

	retry := int64(60_000)
	gw.hand = func(req domain.ModelRequest) (domain.ModelResponse, error) {
		return domain.ModelResponse{}, envFor(domain.FailureRateLimited, req.RouteID, req.AccountID, &retry)
	}
	_, err := router.ExecuteTurn(context.Background(), TurnRequest{
		Session: mustSession(identity("ses", 23)), Turn: mustTurn(identity("trn", 23)),
		Messages: []domain.Message{{Role: domain.RoleUser, Content: "hi"}},
	})
	if !errors.Is(err, ErrTurnExhausted) {
		t.Fatalf("err=%v", err)
	}
	rec, ok, _ := h.Get(context.Background(), domain.HealthKey{RouteID: r1, AccountID: a1, Scope: ScopeAccount})
	if !ok || rec.State != domain.HealthCoolingDown {
		t.Fatalf("account record = %+v ok=%v", rec, ok)
	}
	if rec.CooldownUntil != 1_000+retry {
		t.Errorf("cooldown until %d, want %d", rec.CooldownUntil, 1_000+retry)
	}
	if rec.State == domain.HealthCoolingDown && clk.now < rec.CooldownUntil {
	}

	// Not due yet.
	probeCalls := len(gw.requests())
	if n, _ := router.RunDueProbes(context.Background()); n != 0 {
		t.Errorf("probes before cooldown = %d", n)
	}
	if len(gw.requests()) != probeCalls {
		t.Errorf("gateway must not be probed early")
	}

	// Cooldown expired: a bounded probe runs and heals the key.
	clk.advance(retry)
	gw.hand = hand
	if n, _ := router.RunDueProbes(context.Background()); n != 1 {
		t.Errorf("probes after cooldown = %d", n)
	}
	if got := len(gw.requests()) - probeCalls; got != 1 {
		t.Errorf("probe requests = %d", got)
	}
	rec, _, _ = h.Get(context.Background(), domain.HealthKey{RouteID: r1, AccountID: a1, Scope: ScopeAccount})
	if rec.State != domain.HealthHealthy {
		t.Errorf("state after successful probe = %v", rec.State)
	}
}

func TestBudgetReservation(t *testing.T) {
	cost := 0.03
	gw := &fakeGateway{hand: func(req domain.ModelRequest) (domain.ModelResponse, error) { return successResp(req, &cost) }}
	h := newMemHealthStore()
	clk := &fakeClock{now: 2_000}
	ledger := &fakeLedger{}
	cfg := Config{
		Policy: domain.RoutePolicy{
			Tiers:       []domain.TierConfig{{Name: "t0", RouteIDs: []domain.RouteID{r1}, Enabled: true}},
			MaxAttempts: 4, TurnDeadlineMs: 60_000,
		},
		Routes:          []RouteSpec{{ID: r1, Product: "p1", Paid: true}},
		Accounts:        []domain.Account{accountWithKeys(a1, "g1", "p1", 1)},
		EstimateMaxCost: func(id domain.RouteID) (float64, bool) { return 0.05, true },
	}
	router := newTestRouter(gw, h, ledger, clk, 1, cfg)
	_, err := router.ExecuteTurn(context.Background(), TurnRequest{
		Session: mustSession(identity("ses", 24)), Turn: mustTurn(identity("trn", 24)),
		Messages: []domain.Message{{Role: domain.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.reserved) != 1 || ledger.reserved[0] != 0.05 {
		t.Errorf("reserved=%v", ledger.reserved)
	}
	if len(ledger.settled) != 1 || ledger.settled[0] != 0.03 {
		t.Errorf("settled=%v", ledger.settled)
	}
	if ledger.released != 0 {
		t.Errorf("released=%d", ledger.released)
	}
}

func TestBudgetReleaseOnFailure(t *testing.T) {
	gw := &fakeGateway{hand: func(req domain.ModelRequest) (domain.ModelResponse, error) {
		return domain.ModelResponse{}, envFor(domain.FailureProviderOutage, req.RouteID, req.AccountID, nil)
	}}
	h := newMemHealthStore()
	clk := &fakeClock{now: 2_000}
	ledger := &fakeLedger{}
	cfg := Config{
		Policy: domain.RoutePolicy{
			Tiers:       []domain.TierConfig{{Name: "t0", RouteIDs: []domain.RouteID{r1}, Enabled: true}},
			MaxAttempts: 1, TurnDeadlineMs: 60_000,
		},
		Routes:          []RouteSpec{{ID: r1, Product: "p1", Paid: true}},
		Accounts:        []domain.Account{accountWithKeys(a1, "g1", "p1", 1)},
		EstimateMaxCost: func(id domain.RouteID) (float64, bool) { return 0.05, true },
	}
	router := newTestRouter(gw, h, ledger, clk, 1, cfg)
	_, err := router.ExecuteTurn(context.Background(), TurnRequest{
		Session: mustSession(identity("ses", 25)), Turn: mustTurn(identity("trn", 25)),
		Messages: []domain.Message{{Role: domain.RoleUser, Content: "hi"}},
	})
	if !errors.Is(err, ErrTurnExhausted) {
		t.Fatalf("err=%v", err)
	}
	if len(ledger.reserved) != 1 || ledger.released != 1 || len(ledger.settled) != 0 {
		t.Errorf("reserved=%v settled=%v released=%d", ledger.reserved, ledger.settled, ledger.released)
	}
}

func TestAttemptAndDeadlineExhaustion(t *testing.T) {
	clk := &fakeClock{now: 100_000}
	gw := &fakeGateway{hand: func(req domain.ModelRequest) (domain.ModelResponse, error) {
		clk.advance(40_000) // each attempt consumes most of the turn deadline
		return domain.ModelResponse{}, envFor(domain.FailureProviderOutage, req.RouteID, req.AccountID, nil)
	}}
	h := newMemHealthStore()
	cfg := Config{
		Policy: domain.RoutePolicy{
			Tiers:       []domain.TierConfig{{Name: "t0", RouteIDs: []domain.RouteID{r1, r2}, Enabled: true}},
			MaxAttempts: 8, TurnDeadlineMs: 70_000,
		},
		Routes:   []RouteSpec{{ID: r1, Product: "p1"}, {ID: r2, Product: "p1"}},
		Accounts: []domain.Account{accountWithKeys(a1, "g1", "p1", 1), accountWithKeys(a2, "g2", "p1", 1)},
	}
	router := newTestRouter(gw, h, nil, clk, 1, cfg)
	res, err := router.ExecuteTurn(context.Background(), TurnRequest{
		Session: mustSession(identity("ses", 26)), Turn: mustTurn(identity("trn", 26)),
		Messages: []domain.Message{{Role: domain.RoleUser, Content: "hi"}},
	})
	if !errors.Is(err, ErrTurnExhausted) {
		t.Fatalf("err=%v", err)
	}
	if len(res.Attempts) != 2 {
		t.Errorf("attempts=%d, want 2 (deadline)", len(res.Attempts))
	}
}

func TestAttemptBudgetExhaustion(t *testing.T) {
	clk := &fakeClock{now: 0}
	gw := &fakeGateway{hand: func(req domain.ModelRequest) (domain.ModelResponse, error) {
		return domain.ModelResponse{}, envFor(domain.FailureProviderOutage, req.RouteID, req.AccountID, nil)
	}}
	h := newMemHealthStore()
	cfg := Config{
		Policy: domain.RoutePolicy{
			Tiers:       []domain.TierConfig{{Name: "t0", RouteIDs: []domain.RouteID{r1, r2}, Enabled: true}},
			MaxAttempts: 2, TurnDeadlineMs: 60_000,
		},
		Routes:   []RouteSpec{{ID: r1, Product: "p1"}, {ID: r2, Product: "p1"}},
		Accounts: []domain.Account{accountWithKeys(a1, "g1", "p1", 1), accountWithKeys(a2, "g2", "p1", 1)},
	}
	router := newTestRouter(gw, h, nil, clk, 1, cfg)
	commits := 0
	res, err := router.ExecuteTurn(context.Background(), TurnRequest{
		Session: mustSession(identity("ses", 27)), Turn: mustTurn(identity("trn", 27)),
		Messages: []domain.Message{{Role: domain.RoleUser, Content: "hi"}},
		Commit:   func(ctx context.Context) error { commits++; return nil },
	})
	if !errors.Is(err, ErrTurnExhausted) {
		t.Fatalf("err=%v", err)
	}
	if len(res.Attempts) != 2 {
		t.Errorf("attempts=%d, want 2", len(res.Attempts))
	}
	if commits != 0 {
		t.Errorf("commits=%d on failure path", commits)
	}
}

func TestNoDuplicateCommitAndContextPreserved(t *testing.T) {
	clk := &fakeClock{now: 0}
	var msgs [][]domain.Message
	var mu sync.Mutex
	calls := 0
	gw := &fakeGateway{hand: func(req domain.ModelRequest) (domain.ModelResponse, error) {
		mu.Lock()
		msgs = append(msgs, req.Messages)
		calls++
		mu.Unlock()
		if calls == 1 {
			return domain.ModelResponse{}, envFor(domain.FailureProviderOutage, req.RouteID, req.AccountID, nil)
		}
		return successResp(req, nil)
	}}
	h := newMemHealthStore()
	cfg := Config{
		Policy: domain.RoutePolicy{
			Tiers:       []domain.TierConfig{{Name: "t0", RouteIDs: []domain.RouteID{r1}, Enabled: true}},
			MaxAttempts: 4, TurnDeadlineMs: 60_000,
		},
		Routes:   []RouteSpec{{ID: r1, Product: "p1"}},
		Accounts: []domain.Account{accountWithKeys(a1, "g1", "p1", 1), accountWithKeys(a2, "g2", "p1", 1)},
	}
	router := newTestRouter(gw, h, nil, clk, 1, cfg)
	commits := 0
	_, err := router.ExecuteTurn(context.Background(), TurnRequest{
		Session: mustSession(identity("ses", 28)), Turn: mustTurn(identity("trn", 28)),
		Messages: []domain.Message{{Role: domain.RoleUser, Content: "keep me"}},
		Commit:   func(ctx context.Context) error { commits++; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if commits != 1 {
		t.Errorf("commits=%d, want exactly 1", commits)
	}
	if len(msgs) != 2 {
		t.Fatalf("gateway calls=%d", len(msgs))
	}
	if fmt.Sprint(msgs[0]) != fmt.Sprint(msgs[1]) {
		t.Errorf("messages differ across retry: %v vs %v", msgs[0], msgs[1])
	}
}

// TestAdmissionRefusalIsRequestScoped verifies that both admission-gate
// refusals leave every health record untouched (PLAN 9.1). The gate refuses the
// request before any provider time is spent, so cooling or quarantining the
// route or account would penalize a healthy provider and shrink the eligible
// route space for every later turn.
func TestAdmissionRefusalIsRequestScoped(t *testing.T) {
	cases := []struct {
		name string
		// gate configures the refusal the test provokes: a zero queue depth
		// refuses at once, while a wait queue plus a short gate-level wait
		// refuses after the bounded wait elapsed.
		gate inference.Config
		want error
	}{
		{
			name: "queue full",
			gate: inference.Config{GlobalConcurrency: 1, WaitQueueDepth: 0},
			want: inference.ErrQueueFull,
		},
		{
			name: "wait exceeded",
			gate: inference.Config{GlobalConcurrency: 1, WaitQueueDepth: 4, MaxWait: time.Millisecond},
			want: inference.ErrWaitExceeded,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// One admitted request occupies the gate's only slot until the
			// test releases it, so the second attempt is the one refused.
			entered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			gw := &fakeGateway{hand: func(req domain.ModelRequest) (domain.ModelResponse, error) {
				once.Do(func() { close(entered) })
				<-release
				return successResp(req, nil)
			}}
			gate, err := inference.NewGate(gw, tc.gate)
			if err != nil {
				t.Fatalf("build gate: %v", err)
			}

			h := newMemHealthStore()
			// The clock base is real so the attempt deadline is a live context:
			// the gate refuses on an already-ended context instead of on the
			// bound under test.
			clk := &fakeClock{now: time.Now().UnixMilli()}
			router := NewRouter(gate, h, nil, clk, newRandSource(1), Config{
				Policy: domain.RoutePolicy{
					Tiers:       []domain.TierConfig{{Name: "t0", RouteIDs: []domain.RouteID{r1}, Enabled: true}},
					MaxAttempts: 4, TurnDeadlineMs: 60_000,
				},
				Routes:   []RouteSpec{{ID: r1, Product: "p1"}},
				Accounts: []domain.Account{accountWithKeys(a1, "g1", "p1", 1)},
			})

			holder := make(chan struct{})
			go func() {
				defer close(holder)
				_, _, _ = router.attempt(context.Background(), attemptInput{
					session:  mustSession(identity("ses", 40)),
					turn:     mustTurn(identity("trn", 40)),
					messages: []domain.Message{{Role: domain.RoleUser, Content: "hi"}},
					deadline: clk.now + 60_000,
					seq:      1,
				}, r1, a1)
			}()
			<-entered

			rec, _, refused := router.attempt(context.Background(), attemptInput{
				session:  mustSession(identity("ses", 41)),
				turn:     mustTurn(identity("trn", 41)),
				messages: []domain.Message{{Role: domain.RoleUser, Content: "hi"}},
				deadline: clk.now + 60_000,
				seq:      2,
			}, r1, a1)
			if !errors.Is(refused, tc.want) {
				t.Fatalf("refusal error = %v, want it to wrap %v", refused, tc.want)
			}
			// The refusal is still recorded, with its reason, so research keeps
			// the attempt that the gate turned away.
			if rec.Error == nil || !strings.Contains(rec.Error.Message, tc.want.Error()) {
				t.Fatalf("refusal attempt error = %+v, want it to carry the gate refusal", rec.Error)
			}

			// No scope may change: not the route, not the account, not the
			// credential.
			records, err := h.List(context.Background())
			if err != nil {
				t.Fatalf("list health records: %v", err)
			}
			if len(records) != 0 {
				t.Errorf("a local admission refusal wrote %d health record(s): %+v", len(records), records)
			}
			for _, key := range []domain.HealthKey{
				{RouteID: r1, Scope: ScopeRoute},
				{RouteID: r1, AccountID: a1, Scope: ScopeAccount},
				{RouteID: r1, AccountID: a1, Scope: ScopeCredential},
			} {
				if record, ok, _ := h.Get(context.Background(), key); ok {
					t.Errorf("%s record must not exist, got %+v", key.Scope, record)
				}
			}
			// The refused route must still be eligible for the next turn.
			if !router.routeHealthy(context.Background(), r1, clk.now) {
				t.Error("route must stay healthy after a local admission refusal")
			}
			if _, err := router.EnsureBinding(context.Background(), mustSession(identity("ses", 42))); err != nil {
				t.Errorf("binding after a local admission refusal: %v", err)
			}

			close(release)
			<-holder
		})
	}
}

// TestExecuteSelectsByPurpose proves a purpose-eligible route is chosen and a
// route that does not serve the purpose is never called.
func TestExecuteSelectsByPurpose(t *testing.T) {
	seen := map[domain.RouteID]bool{}
	gw := &fakeGateway{hand: func(req domain.ModelRequest) (domain.ModelResponse, error) {
		seen[req.RouteID] = true
		return successResp(req, nil)
	}}
	clk := &fakeClock{now: 1_000}
	cfg := Config{
		Policy: domain.RoutePolicy{
			Tiers:       []domain.TierConfig{{Name: "t0", RouteIDs: []domain.RouteID{r1, r2}, Enabled: true}},
			MaxAttempts: 4, TurnDeadlineMs: 60_000,
		},
		Routes: []RouteSpec{
			{ID: r1, Product: "p1", Purposes: []string{domain.PurposeMOTD}},
			{ID: r2, Product: "p1", Purposes: []string{domain.PurposeGeneration}},
		},
		Accounts: []domain.Account{accountWithKeys(a1, "g1", "p1", 1)},
	}
	router := newTestRouter(gw, newMemHealthStore(), nil, clk, 1, cfg)

	res, err := router.Execute(context.Background(), ExecuteRequest{
		Purpose:  domain.PurposeGeneration,
		Messages: []domain.Message{{Role: domain.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Execute(generation): %v", err)
	}
	if res.Binding.Route != r2 {
		t.Fatalf("route = %v, want the generation route r2", res.Binding.Route)
	}
	if seen[r1] {
		t.Fatal("a route that does not serve the purpose was called")
	}
}

// TestExecuteNoEligiblePurpose proves a purpose with no eligible route is
// reported rather than silently falling back to an unrelated model.
func TestExecuteNoEligiblePurpose(t *testing.T) {
	gw := &fakeGateway{hand: func(req domain.ModelRequest) (domain.ModelResponse, error) { return successResp(req, nil) }}
	clk := &fakeClock{now: 1_000}
	cfg := Config{
		Policy: domain.RoutePolicy{
			Tiers:       []domain.TierConfig{{Name: "t0", RouteIDs: []domain.RouteID{r1}, Enabled: true}},
			MaxAttempts: 4, TurnDeadlineMs: 60_000,
		},
		Routes:   []RouteSpec{{ID: r1, Product: "p1", Purposes: []string{domain.PurposeGeneration}}},
		Accounts: []domain.Account{accountWithKeys(a1, "g1", "p1", 1)},
	}
	router := newTestRouter(gw, newMemHealthStore(), nil, clk, 1, cfg)

	if _, err := router.Execute(context.Background(), ExecuteRequest{
		Purpose:  domain.PurposeMOTD,
		Messages: []domain.Message{{Role: domain.RoleUser, Content: "hi"}},
	}); !errors.Is(err, ErrNoEligibleRoute) {
		t.Fatalf("err = %v, want ErrNoEligibleRoute", err)
	}
}

// TestExecuteAcceptFailureFailsOver proves a response the caller cannot use
// cools its route and the next eligible model is tried, so a broken or
// unusable model does not end the request.
func TestExecuteAcceptFailureFailsOver(t *testing.T) {
	gw := &fakeGateway{hand: func(req domain.ModelRequest) (domain.ModelResponse, error) {
		resp, _ := successResp(req, nil)
		if req.RouteID == r1 {
			resp.Message.Content = "" // unusable
		} else {
			resp.Message.Content = "good"
		}
		return resp, nil
	}}
	clk := &fakeClock{now: 1_000}
	cfg := Config{
		Policy: domain.RoutePolicy{
			Tiers:       []domain.TierConfig{{Name: "t0", RouteIDs: []domain.RouteID{r1, r2}, Enabled: true}},
			MaxAttempts: 4, TurnDeadlineMs: 60_000,
		},
		Routes:   []RouteSpec{{ID: r1, Product: "p1"}, {ID: r2, Product: "p1"}},
		Accounts: []domain.Account{accountWithKeys(a1, "g1", "p1", 1)},
	}
	router := newTestRouter(gw, newMemHealthStore(), nil, clk, 1, cfg)

	res, err := router.Execute(context.Background(), ExecuteRequest{
		Messages: []domain.Message{{Role: domain.RoleUser, Content: "hi"}},
		Accept: func(resp domain.ModelResponse) error {
			if resp.Message.Content == "" {
				return errors.New("empty answer")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Response.Message.Content != "good" || res.Binding.Route != r2 {
		t.Fatalf("result = %+v, want r2's usable response", res)
	}
	if len(res.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 (unusable then usable)", len(res.Attempts))
	}
}
