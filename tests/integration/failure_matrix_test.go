package integration

import (
	"context"
	"errors"
	"testing"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/routing"
)

// routingFixture wires a router over a fake gateway, in-memory health store,
// and deterministic clock/random for one tier layout shared by the failure
// matrix tests.
type routingFixture struct {
	router *routing.Router
	gw     *fakeGateway
	health *memoryHealth
	clk    *testClock
	ledger *memoryLedger
	cfg    routing.Config
	sess   domain.SessionID
	turn   domain.TurnID
}

func newRoutingFixture(t *testing.T, cfg routing.Config, seed int64, gw *fakeGateway, ledger *memoryLedger) *routingFixture {
	t.Helper()
	if gw == nil {
		gw = &fakeGateway{}
	}
	clk := newTestClock(1_700_000_000_000)
	health := newMemoryHealth()
	router := routing.NewRouter(gw, health, ledger, clk, newSeededRandom(seed), cfg)
	return &routingFixture{
		router: router,
		gw:     gw,
		health: health,
		clk:    clk,
		ledger: ledger,
		cfg:    cfg,
		sess:   sessionID(t, 1),
		turn:   turnID(t, 1),
	}
}

// turnRequest builds a minimal observing turn with an optional commit hook.
func (f *routingFixture) turnRequest(commit func(context.Context) error) routing.TurnRequest {
	return routing.TurnRequest{
		Session:  f.sess,
		Turn:     f.turn,
		Messages: []domain.Message{{Role: domain.RoleUser, Content: "hello"}},
		Commit:   commit,
	}
}

func oneTier(route domain.RouteID) domain.RoutePolicy {
	return domain.RoutePolicy{
		Tiers:          []domain.TierConfig{{Name: "t0", RouteIDs: []domain.RouteID{route}, Enabled: true}},
		MaxAttempts:    4,
		TurnDeadlineMs: 60_000,
	}
}

func accountFor(id domain.AccountID, group, product string) domain.Account {
	return domain.Account{ID: id, QuotaGroup: group, PermittedProducts: []string{product}, Enabled: true}
}

// TestFailureMatrixHealthScopes drives every PLAN 9.1 failure class through a
// real turn and asserts the distinct recovery/health scope each one affects:
// the class must cool or quarantine the narrowest evidenced record, and the
// health-neutral classes must leave every record healthy.
func TestFailureMatrixHealthScopes(t *testing.T) {
	r1 := route(t, 1)
	a1 := account(t, 1)

	type testCase struct {
		name          string
		class         domain.FailureClass
		retryAfter    *int64
		wantScope     string // expected mutated health scope: "" means none
		wantState     domain.HealthState
		requestScoped bool // request-scoped: record exists but stays healthy
	}
	retry := int64(90_000)
	cases := []testCase{
		{name: "invalid_credential", class: domain.FailureInvalidCredential, wantScope: routing.ScopeCredential, wantState: domain.HealthCoolingDown},
		{name: "quota_exhausted", class: domain.FailureQuotaExhausted, wantScope: routing.ScopeAccount, wantState: domain.HealthCoolingDown},
		{name: "rate_limited_retry_after", class: domain.FailureRateLimited, retryAfter: &retry, wantScope: routing.ScopeAccount, wantState: domain.HealthCoolingDown},
		{name: "model_not_found", class: domain.FailureModelNotFound, wantScope: routing.ScopeRoute, wantState: domain.HealthQuarantined},
		{name: "provider_outage", class: domain.FailureProviderOutage, wantScope: routing.ScopeRoute, wantState: domain.HealthCoolingDown},
		{name: "network_timeout", class: domain.FailureNetworkTimeout, wantScope: routing.ScopeRoute, wantState: domain.HealthCoolingDown},
		{name: "context_too_long", class: domain.FailureContextTooLong, wantScope: routing.ScopeRoute, wantState: domain.HealthHealthy, requestScoped: true},
		{name: "invalid_arguments", class: domain.FailureInvalidArguments, wantScope: routing.ScopeRoute, wantState: domain.HealthHealthy, requestScoped: true},
		{name: "invalid_response", class: domain.FailureInvalidResponse, wantScope: routing.ScopeRoute, wantState: domain.HealthHealthy, requestScoped: true},
		{name: "content_rejected", class: domain.FailureContentRejected, wantScope: routing.ScopeRoute, wantState: domain.HealthHealthy, requestScoped: true},
		{name: "user_cancelled", class: domain.FailureUserCancelled, wantScope: "", wantState: ""},
		{name: "world_conflict", class: domain.FailureWorldConflict, wantScope: "", wantState: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class := tc.class
			ra := tc.retryAfter
			gw := &fakeGateway{handler: func(_ context.Context, req domain.ModelRequest) (domain.ModelResponse, error) {
				return domain.ModelResponse{}, failure(class, req, ra)
			}}
			cfg := routing.Config{
				Policy:   oneTier(r1),
				Routes:   []routing.RouteSpec{{ID: r1, Product: "p1"}},
				Accounts: []domain.Account{accountFor(a1, "g1", "p1")},
			}
			f := newRoutingFixture(t, cfg, 1, gw, nil)

			commits := 0
			_, err := f.router.ExecuteTurn(context.Background(), f.turnRequest(func(context.Context) error {
				commits++
				return nil
			}))

			// Only the health-neutral classes are fatal; the rest exhaust the
			// bounded attempt/tier space.
			switch class {
			case domain.FailureUserCancelled, domain.FailureWorldConflict:
				if !errors.Is(err, class) {
					t.Fatalf("err=%v, want the class %s", err, class)
				}
			default:
				if !errors.Is(err, routing.ErrTurnExhausted) {
					t.Fatalf("err=%v, want ErrTurnExhausted", err)
				}
			}
			if commits != 0 {
				t.Errorf("commits=%d on failure path, want 0", commits)
			}

			records, _ := f.health.List(context.Background())
			if tc.wantScope == "" {
				if len(records) != 0 {
					t.Fatalf("%s produced health records %+v, want none", class, records)
				}
				return
			}
			if len(records) != 1 {
				t.Fatalf("%s produced %d health records, want exactly 1: %+v", class, len(records), records)
			}
			rec := records[0]
			if rec.Scope != tc.wantScope {
				t.Errorf("%s health scope = %q, want %q", class, rec.Scope, tc.wantScope)
			}
			if rec.State != tc.wantState {
				t.Errorf("%s health state = %q, want %q", class, rec.State, tc.wantState)
			}
			if tc.requestScoped {
				// PLAN 9.1: request-scoped failures must not disable the
				// route on a single occurrence, only count toward the
				// repeat-incompatibility threshold.
				if rec.ConsecutiveFailures != 1 {
					t.Errorf("%s consecutive failures = %d, want 1", class, rec.ConsecutiveFailures)
				}
				if rec.State != domain.HealthHealthy {
					t.Errorf("%s cooled a healthy route: %+v", class, rec)
				}
			}
			if class == domain.FailureRateLimited && tc.retryAfter != nil && rec.CooldownUntil != f.clk.now+*tc.retryAfter {
				t.Errorf("rate limit cooldown until %d, want now+retryafter %d", rec.CooldownUntil, f.clk.now+*tc.retryAfter)
			}
		})
	}
}

// TestQuotaGroupSharedAcrossKeys proves a quota failure on one account marks
// the shared quota group, so a sibling account in the same group is not
// selected as a naive key-cycling fix (PLAN 9.1/9.2).
func TestQuotaGroupSharedAcrossKeys(t *testing.T) {
	r1 := route(t, 1)
	a1 := account(t, 1)
	a2 := account(t, 2)
	retry := int64(120_000)
	gw := &fakeGateway{handler: func(_ context.Context, req domain.ModelRequest) (domain.ModelResponse, error) {
		return domain.ModelResponse{}, failure(domain.FailureQuotaExhausted, req, &retry)
	}}
	cfg := routing.Config{
		Policy: domain.RoutePolicy{
			Tiers:          []domain.TierConfig{{Name: "t0", RouteIDs: []domain.RouteID{r1}, Enabled: true}},
			MaxAttempts:    2,
			TurnDeadlineMs: 60_000,
		},
		Routes:   []routing.RouteSpec{{ID: r1, Product: "p1"}},
		Accounts: []domain.Account{accountFor(a1, "shared", "p1"), accountFor(a2, "shared", "p1")},
	}
	f := newRoutingFixture(t, cfg, 7, gw, nil)

	// Drive one failed attempt against a1 deterministically, then confirm the
	// shared group blocks both member accounts: a sibling account is not a
	// naive fix, and a fresh session finds no eligible route.
	if _, err := f.router.ExecuteTurn(context.Background(), f.turnRequest(nil)); !errors.Is(err, routing.ErrTurnExhausted) {
		t.Fatalf("err=%v, want ErrTurnExhausted", err)
	}
	if _, err := f.router.EnsureBinding(context.Background(), sessionID(t, 2)); !errors.Is(err, routing.ErrNoEligibleRoute) {
		t.Fatalf("EnsureBinding after shared quota exhaustion = %v, want ErrNoEligibleRoute", err)
	}
	rec, ok, _ := f.health.Get(context.Background(), domain.HealthKey{RouteID: r1, AccountID: a1, Scope: routing.ScopeAccount})
	if !ok || rec.State != domain.HealthCoolingDown || rec.CooldownUntil != f.clk.now+retry {
		t.Errorf("a1 account record = %+v ok=%v, want cooling until now+%d", rec, ok, retry)
	}
}

// TestModelNotFoundQuarantineUnselectable proves a removed model quarantines
// the route and the next connect under that snapshot refuses to bind
// (PLAN 9.1 model removed/not found).
func TestModelNotFoundQuarantineUnselectable(t *testing.T) {
	r1 := route(t, 1)
	a1 := account(t, 1)
	gw := &fakeGateway{handler: func(_ context.Context, req domain.ModelRequest) (domain.ModelResponse, error) {
		return domain.ModelResponse{}, failure(domain.FailureModelNotFound, req, nil)
	}}
	cfg := routing.Config{
		Policy: domain.RoutePolicy{
			Tiers:          []domain.TierConfig{{Name: "t0", RouteIDs: []domain.RouteID{r1}, Enabled: true}},
			MaxAttempts:    2,
			TurnDeadlineMs: 60_000,
		},
		Routes:   []routing.RouteSpec{{ID: r1, Product: "p1"}},
		Accounts: []domain.Account{accountFor(a1, "g1", "p1")},
	}
	f := newRoutingFixture(t, cfg, 3, gw, nil)
	_, _ = f.router.ExecuteTurn(context.Background(), f.turnRequest(nil))

	rec, ok, _ := f.health.Get(context.Background(), domain.HealthKey{RouteID: r1, Scope: routing.ScopeRoute})
	if !ok || rec.State != domain.HealthQuarantined {
		t.Fatalf("route record = %+v ok=%v, want quarantined", rec, ok)
	}
	// A fresh session under the quarantined snapshot must not bind.
	if _, err := f.router.EnsureBinding(context.Background(), sessionID(t, 2)); !errors.Is(err, routing.ErrNoEligibleRoute) {
		t.Fatalf("EnsureBinding after quarantine = %v, want ErrNoEligibleRoute", err)
	}
}

// TestContentRejectionNotQuota proves a content rejection is recorded and
// handled as a request/capability failure, never counted as a quota outage or
// a faked success (PLAN 9.1 content rejection).
func TestContentRejectionNotQuota(t *testing.T) {
	r1 := route(t, 1)
	a1 := account(t, 1)
	gw := &fakeGateway{handler: func(_ context.Context, req domain.ModelRequest) (domain.ModelResponse, error) {
		return domain.ModelResponse{}, failure(domain.FailureContentRejected, req, nil)
	}}
	cfg := routing.Config{
		Policy:   oneTier(r1),
		Routes:   []routing.RouteSpec{{ID: r1, Product: "p1"}},
		Accounts: []domain.Account{accountFor(a1, "g1", "p1")},
	}
	f := newRoutingFixture(t, cfg, 1, gw, nil)
	res, err := f.router.ExecuteTurn(context.Background(), f.turnRequest(nil))
	if !errors.Is(err, routing.ErrTurnExhausted) {
		t.Fatalf("err=%v, want ErrTurnExhausted", err)
	}
	if len(res.Attempts) != 1 {
		t.Fatalf("attempts=%d, want 1", len(res.Attempts))
	}
	if res.Attempts[0].Result != domain.AttemptResultContentRejected {
		t.Errorf("attempt result = %q, want content_rejected", res.Attempts[0].Result)
	}
	// A content rejection is request-scoped and must not quarantine.
	rec, ok, _ := f.health.Get(context.Background(), domain.HealthKey{RouteID: r1, Scope: routing.ScopeRoute})
	if ok && rec.State == domain.HealthQuarantined {
		t.Errorf("content rejection quarantined the route: %+v", rec)
	}
}

// TestCancellationStopsPromptlyAndDiscardsStaging proves a cancelled context
// aborts before any provider call, records no health penalty, and runs no
// commit (PLAN 9.1 user cancellation).
func TestCancellationStopsPromptlyAndDiscardsStaging(t *testing.T) {
	r1 := route(t, 1)
	a1 := account(t, 1)
	gw := &fakeGateway{}
	cfg := routing.Config{
		Policy:   oneTier(r1),
		Routes:   []routing.RouteSpec{{ID: r1, Product: "p1"}},
		Accounts: []domain.Account{accountFor(a1, "g1", "p1")},
	}
	f := newRoutingFixture(t, cfg, 1, gw, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	commits := 0
	_, err := f.router.ExecuteTurn(ctx, f.turnRequest(func(context.Context) error { commits++; return nil }))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if commits != 0 {
		t.Errorf("commits=%d on cancellation, want 0", commits)
	}
	if n := gw.callCount(); n != 0 {
		t.Errorf("gateway calls=%d on pre-cancelled turn, want 0", n)
	}
	records, _ := f.health.List(context.Background())
	if len(records) != 0 {
		t.Errorf("cancellation produced health records: %+v", records)
	}
}

// TestWorldConflictFatalAndHealthNeutral proves a concurrent world conflict
// ends the turn immediately (no other route consumes the turn), carries no
// health penalty, and runs no commit so the caller re-evaluates (PLAN 9.1
// concurrent world conflict).
func TestWorldConflictFatalAndHealthNeutral(t *testing.T) {
	r1 := route(t, 1)
	r2 := route(t, 2)
	a1 := account(t, 1)
	a2 := account(t, 2)
	conflict := domain.NewErrorEnvelope(domain.FailureWorldConflict, "world conflict", r1, a1, f0())
	gw := &fakeGateway{handler: func(_ context.Context, req domain.ModelRequest) (domain.ModelResponse, error) {
		if req.RouteID == r1 {
			return domain.ModelResponse{}, conflict
		}
		return successResponse(req), nil
	}}
	cfg := routing.Config{
		Policy: domain.RoutePolicy{
			Tiers:          []domain.TierConfig{{Name: "t0", RouteIDs: []domain.RouteID{r1, r2}, Enabled: true}},
			MaxAttempts:    8,
			TurnDeadlineMs: 60_000,
		},
		Routes:   []routing.RouteSpec{{ID: r1, Product: "p1"}, {ID: r2, Product: "p1"}},
		Accounts: []domain.Account{accountFor(a1, "g1", "p1"), accountFor(a2, "g2", "p1")},
	}
	f := newRoutingFixture(t, cfg, 1, gw, nil)
	commits := 0
	_, err := f.router.ExecuteTurn(context.Background(), f.turnRequest(func(context.Context) error { commits++; return nil }))
	if !errors.Is(err, domain.FailureWorldConflict) {
		t.Fatalf("err=%v, want world_conflict", err)
	}
	if commits != 0 {
		t.Errorf("commits=%d on conflict, want 0", commits)
	}
	records, _ := f.health.List(context.Background())
	if len(records) != 0 {
		t.Errorf("world conflict produced health records: %+v", records)
	}
}

// TestRetryAfterHonoredAndProbeBudget proves Retry-After sets the cooldown and
// that the bounded probe budget admits at most one probe per half-open key
// (PLAN 9.2).
func TestRetryAfterHonoredAndProbeBudget(t *testing.T) {
	r1 := route(t, 1)
	a1 := account(t, 1)
	retry := int64(45_000)
	fail := true
	gw := &fakeGateway{handler: func(_ context.Context, req domain.ModelRequest) (domain.ModelResponse, error) {
		if fail {
			return domain.ModelResponse{}, failure(domain.FailureRateLimited, req, &retry)
		}
		return successResponse(req), nil
	}}
	cfg := routing.Config{
		Policy: domain.RoutePolicy{
			Tiers:          []domain.TierConfig{{Name: "t0", RouteIDs: []domain.RouteID{r1}, Enabled: true}},
			MaxAttempts:    1,
			TurnDeadlineMs: 60_000,
			ProbeBudget:    domain.ProbeBudget{MaxProbesPerHour: 1},
		},
		Routes:   []routing.RouteSpec{{ID: r1, Product: "p1"}},
		Accounts: []domain.Account{accountFor(a1, "g1", "p1")},
	}
	f := newRoutingFixture(t, cfg, 1, gw, nil)
	_, _ = f.router.ExecuteTurn(context.Background(), f.turnRequest(nil))

	key := domain.HealthKey{RouteID: r1, AccountID: a1, Scope: routing.ScopeAccount}
	rec, ok, _ := f.health.Get(context.Background(), key)
	if !ok || rec.CooldownUntil != f.clk.now+retry {
		t.Fatalf("account cooldown = %+v, want now+%d", rec, retry)
	}
	if n, _ := f.router.RunDueProbes(context.Background()); n != 0 {
		t.Fatalf("probes before cooldown = %d, want 0", n)
	}
	f.clk.advance(retry)
	fail = false
	if n, _ := f.router.RunDueProbes(context.Background()); n != 1 {
		t.Fatalf("probes after cooldown = %d, want 1", n)
	}
	// Budget exhausted: a second due key must not be probed.
	f.clk.advance(1)
	if n, _ := f.router.RunDueProbes(context.Background()); n != 0 {
		t.Errorf("probes after budget = %d, want 0", n)
	}
}

func f0() int64 { return 1_700_000_000_000 }
