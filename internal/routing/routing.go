// Package routing implements VibeShell's application-level model routing
// and recovery orchestration (PLAN 8.2-8.4, 9.1-9.4). It owns the effects
// only: catalogue and health reads, the health store, gateway calls, spending
// reservations, probe budgets, and turn/commit sequencing. Every selection,
// failover-order, health-admission, and quota-group decision is delegated to
// the pure policies in internal/domain, which this package feeds a
// TieredCandidates snapshot of the configured tiers.
//
// Owned by C06. Concrete adapters and the session coordinator live
// elsewhere; this package depends on internal/domain and internal/ports plus
// its own small ports (HealthStore, SpendingLedger), and on internal/inference
// for the admission gate's refusal sentinels. That last dependency exists only
// so local backpressure can be told apart from a provider fault: the two are
// indistinguishable by domain error category, and misreading one for the other
// cools a healthy provider.
package routing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/inference"
	"j0s.at/vibeshell/internal/ports"
)

// Health record scopes used by this package. The domain HealthKey carries
// RouteID, AccountID, and one of these scope strings.
const (
	ScopeCredential = "credential"
	ScopeAccount    = "account"
	ScopeRoute      = "route"
)

// ProbeDeadlineMs bounds one probe request (PLAN 9.3: health checks must
// not create unbounded paid background traffic).
const ProbeDeadlineMs int64 = 5000

// RouteSpec describes one catalogue route known to the router, distilled
// from the configuration snapshot the session was created under.
type RouteSpec struct {
	ID      domain.RouteID
	Product string // product identifier used for account permission checks
	Paid    bool   // requires a spending reservation before the request
	// Purposes restricts the route to named request purposes (domain.Purpose*).
	// An empty list serves every purpose.
	Purposes []string
}

// serves reports whether the route may serve a request purpose. An empty
// purpose matches every route; a route with no purposes matches every purpose.
func (s RouteSpec) serves(purpose string) bool {
	if purpose == "" || len(s.Purposes) == 0 {
		return true
	}
	for _, p := range s.Purposes {
		if p == purpose {
			return true
		}
	}
	return false
}

// Config is the router's static configuration snapshot.
type Config struct {
	Policy   domain.RoutePolicy
	Routes   []RouteSpec
	Accounts []domain.Account
	// EstimateMaxCost returns the estimated maximum cost (USD) of one
	// request to the route. known=false means the operator has not
	// configured a trustworthy estimate; the ledger is then not asked to
	// reserve and reconciliation labels the usage estimate unknown.
	EstimateMaxCost func(route domain.RouteID) (estimate float64, known bool)
}

// Binding pins a route/account to a session until failure forces a change
// (PLAN 8.4). It is the session-affinity record.
type Binding struct {
	Tier    int
	Route   domain.RouteID
	Account domain.AccountID
}

// TurnRequest is one agent turn submitted to the router. Messages and Tools
// are preserved by value across retries; nothing is committed unless an
// attempt succeeds.
type TurnRequest struct {
	Session  domain.SessionID
	Turn     domain.TurnID
	Messages []domain.Message
	Tools    []domain.ToolDefinition
	// Commit runs exactly once, after a successful attempt and before the
	// turn result is returned. It must be nil for turns that only observe.
	Commit func(ctx context.Context) error
}

// TurnResult reports the accepted attempt. Attempts lists every model
// attempt made, in order, for research recording.
type TurnResult struct {
	Response  domain.ModelResponse
	Binding   Binding
	Attempts  []domain.AttemptRecord
	Committed bool
}

// ErrTurnExhausted ends a turn early when the attempt budget or turn
// deadline expires before the eligible tier space is exhausted. It is a
// temporary simulated service error per PLAN 8.4.
var ErrTurnExhausted = errors.New("turn budget exhausted: simulated service unavailable")

// ErrNoEligibleRoute is returned at connect when no tier offers an eligible
// route/account combination under the current health snapshot.
var ErrNoEligibleRoute = errors.New("no eligible route/account combination")

// Router orchestrates routing decisions. Concurrency model: the session
// binding table, in-process quota-group marks, and probe timestamps are
// guarded by mu; gateway/ledger/health/store I/O happens outside the lock; a
// turn is executed by one caller (the turn coordinator owns turn
// lifecycle), and health records are only mutated by this router.
type Router struct {
	gateway     ports.ModelGateway
	health      HealthStore
	quotaGroups QuotaGroupStore
	ledger      SpendingLedger
	clock       ports.Clock
	rand        ports.Random
	cfg         Config

	mu              sync.Mutex
	sessions        map[domain.SessionID]Binding
	quotaBlocked    map[string]int64 // quota group -> blocked until (unix ms)
	probeTimestamps []int64          // unix ms of probe starts within the last hour
}

// Option configures an optional router dependency. Optional dependencies keep
// the zero configuration meaningful: a router built without them behaves
// entirely in process.
type Option func(*Router)

// WithQuotaGroups installs the durable quota-group store. Without it,
// quota-group exhaustion marks live in process memory and a restart forgets
// them, so every account of an exhausted group is retried again immediately.
func WithQuotaGroups(store QuotaGroupStore) Option {
	return func(r *Router) {
		if store != nil {
			r.quotaGroups = store
		}
	}
}

// NewRouter wires a router. gw and health are required. ledger may be nil
// when no spending limits are configured. EstimateMaxCost may be nil. Options
// add the optional persistence ports.
func NewRouter(gw ports.ModelGateway, health HealthStore, ledger SpendingLedger, clock ports.Clock, rnd ports.Random, cfg Config, opts ...Option) *Router {
	r := &Router{
		gateway:      gw,
		health:       health,
		ledger:       ledger,
		clock:        clock,
		rand:         rnd,
		cfg:          cfg,
		sessions:     map[domain.SessionID]Binding{},
		quotaBlocked: map[string]int64{},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// ---------------------------------------------------------------------------
// Connect-time selection with session affinity (PLAN 8.4)
// ---------------------------------------------------------------------------

// EnsureBinding returns the session's pinned binding, selecting and pinning
// a new one on first contact or when the pinned combination became
// ineligible. Selection expands tiers in order and delegates the choice to the
// domain policies: the first tier that offers an eligible combination, then
// SelectCandidate's uniform draw over distinct eligible routes and their
// eligible accounts (PLAN 8.4). Route choice is independent of how many key
// references each account holds (PLAN 8.2). A healthy pinned session is never
// migrated back to an earlier tier just because that tier recovered
// (PLAN 8.4).
func (r *Router) EnsureBinding(ctx context.Context, session domain.SessionID) (Binding, error) {
	if pinned, ok := r.Binding(session); ok && r.combinationEligible(ctx, pinned.Route, pinned.Account, r.clock.NowUnixMilli()) {
		return pinned, nil
	}

	tiers := r.tieredCandidates(ctx, "")
	tierIdx, ok := domain.FirstEligibleTier(tiers)
	if !ok {
		return Binding{}, ErrNoEligibleRoute
	}
	candidate, ok := domain.SelectCandidate(tiers[tierIdx], r.rand)
	if !ok {
		return Binding{}, ErrNoEligibleRoute
	}
	b := Binding{Tier: tierIdx, Route: candidate.RouteID, Account: candidate.AccountID}
	r.mu.Lock()
	r.sessions[session] = b
	r.mu.Unlock()
	return b, nil
}

// Binding returns the pinned binding without selecting.
func (r *Router) Binding(session domain.SessionID) (Binding, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.sessions[session]
	return b, ok
}

// tieredCandidates expands the configured tiers into the domain's
// TieredCandidates snapshot: one entry per configured tier, in configuration
// order, so an index into the result is also the tier index a Binding reports.
// A disabled tier stays empty and is therefore skipped by the domain policies.
// Each entry holds only combinations that are eligible right now, which is the
// catalogue-plus-health projection the pure policies are defined against
// (ADR 0011); reading it once per request keeps one walk on a stable candidate
// set instead of re-deciding eligibility between attempts.
func (r *Router) tieredCandidates(ctx context.Context, purpose string) domain.TieredCandidates {
	now := r.clock.NowUnixMilli()
	tiers := make(domain.TieredCandidates, len(r.cfg.Policy.Tiers))
	for i, tier := range r.cfg.Policy.Tiers {
		if !tier.Enabled {
			continue
		}
		tiers[i] = r.tierCandidates(ctx, i, now, purpose)
	}
	return tiers
}

// tierCandidates returns one enabled tier's eligible combinations in
// configuration order: the tier's declared routes, duplicates collapsed so a
// route named twice is never selected twice, each followed by its eligible
// accounts in pool order.
func (r *Router) tierCandidates(ctx context.Context, tierIdx int, now int64, purpose string) []domain.RouteCandidate {
	tier := r.cfg.Policy.Tiers[tierIdx]
	seen := map[domain.RouteID]bool{}
	var out []domain.RouteCandidate
	for _, id := range tier.RouteIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		spec, ok := r.routeSpec(id)
		if !ok || !spec.serves(purpose) || !r.routeHealthy(ctx, id, now) {
			continue
		}
		for _, accID := range r.eligibleAccounts(ctx, tierIdx, id, now) {
			out = append(out, domain.RouteCandidate{Tier: tierIdx, RouteID: id, AccountID: accID})
		}
	}
	return out
}

// eligibleAccounts returns accounts of the tier's pool that are enabled,
// permitted for the route's product, not blocked by an exhausted quota group,
// and healthy at the account/credential scope.
func (r *Router) eligibleAccounts(ctx context.Context, tierIdx int, routeID domain.RouteID, now int64) []domain.AccountID {
	spec, ok := r.routeSpec(routeID)
	if !ok {
		return nil
	}
	pool := r.poolAccounts(r.cfg.Policy.Tiers[tierIdx])
	// The domain groups accounts that share one quota, so an exhausted group is
	// resolved once for the tier instead of once per account in it.
	blocked := make(map[string]bool)
	for _, group := range domain.GroupAccountsByQuota(pool) {
		if r.quotaBlockedGroup(ctx, group.Group, now) {
			blocked[group.Group] = true
		}
	}
	var out []domain.AccountID
	for _, acc := range pool {
		if !acc.Enabled || blocked[acc.QuotaGroup] {
			continue
		}
		if len(acc.PermittedProducts) > 0 && !contains(acc.PermittedProducts, spec.Product) {
			continue
		}
		if r.accountHealthy(ctx, routeID, acc, now) {
			out = append(out, acc.ID)
		}
	}
	return out
}

func (r *Router) combinationEligible(ctx context.Context, route domain.RouteID, account domain.AccountID, now int64) bool {
	if !r.routeHealthy(ctx, route, now) {
		return false
	}
	acc, ok := r.account(account)
	if !ok || !acc.Enabled || r.quotaBlockedGroup(ctx, acc.QuotaGroup, now) {
		return false
	}
	return r.accountHealthy(ctx, route, acc, now)
}

func (r *Router) accountPool(tier domain.TierConfig) []domain.AccountID {
	if tier.AccountPool != "" {
		return r.cfg.Policy.AccountPools[tier.AccountPool]
	}
	var out []domain.AccountID
	for _, a := range r.cfg.Accounts {
		out = append(out, a.ID)
	}
	return out
}

// poolAccounts resolves the tier's account pool into full accounts, in pool
// order. A pool that names an account the router does not know contributes
// nothing, exactly as an unknown account ID in the pool never did.
func (r *Router) poolAccounts(tier domain.TierConfig) []domain.Account {
	var out []domain.Account
	for _, id := range r.accountPool(tier) {
		if acc, ok := r.account(id); ok {
			out = append(out, acc)
		}
	}
	return out
}

func (r *Router) routeSpec(id domain.RouteID) (RouteSpec, bool) {
	for _, s := range r.cfg.Routes {
		if s.ID == id {
			return s, true
		}
	}
	return RouteSpec{}, false
}

func (r *Router) account(id domain.AccountID) (domain.Account, bool) {
	for _, a := range r.cfg.Accounts {
		if a.ID == id {
			return a, true
		}
	}
	return domain.Account{}, false
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// quotaBlockedGroup reports whether a quota group is still exhausted. With a
// configured store the durable mark is the truth; an unreadable mark falls
// back to the in-process one, matching routeHealthy's treatment of an
// unreadable health record as "no known problem" so a storage fault does not
// strand every account of the group.
func (r *Router) quotaBlockedGroup(ctx context.Context, group string, now int64) bool {
	if group == "" {
		return false
	}
	if r.quotaGroups != nil {
		if blocked, err := r.quotaGroups.QuotaGroupBlocked(ctx, group, now); err == nil {
			return blocked
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	until, ok := r.quotaBlocked[group]
	return ok && now < until
}

// markQuotaGroup blocks a quota group until the given time. The in-process mark
// is always recorded so this process stops selecting the group even when the
// durable write fails; only the durability across restarts is lost in that case.
func (r *Router) markQuotaGroup(ctx context.Context, group string, until int64) {
	if group == "" {
		return
	}
	r.mu.Lock()
	r.quotaBlocked[group] = until
	r.mu.Unlock()
	if r.quotaGroups == nil {
		return
	}
	_ = r.quotaGroups.BlockQuotaGroup(ctx, group, until)
}

// ---------------------------------------------------------------------------
// Turn execution with failover, budgets, and commit-once semantics
// ---------------------------------------------------------------------------

// ExecuteTurn runs one turn. It starts at the tier the session is pinned to and
// then delegates the order to the domain failover policy: after a failure it
// exhausts the other eligible accounts of the current route, then another
// randomly ordered unused route of the same tier, and only then the next tier
// (PLAN 8.4). The session binding is pinned on success and is not migrated back
// when a higher tier recovers. Failed or cancelled attempts preserve messages
// and discard mutations: Commit runs at most once, only after success, and a
// late response from an interrupted attempt cannot update the turn result.
func (r *Router) ExecuteTurn(ctx context.Context, req TurnRequest) (TurnResult, error) {
	if err := ctx.Err(); err != nil {
		return TurnResult{}, err
	}
	deadline := r.clock.NowUnixMilli() + r.cfg.Policy.TurnDeadlineMs
	attemptsLeft := r.cfg.Policy.MaxAttempts

	startTier := 0
	if b, ok := r.Binding(req.Session); ok {
		startTier = b.Tier
	}

	tiers := r.tieredCandidates(ctx, "")
	candidate, ok := firstCandidate(tiers, startTier)
	if !ok {
		return TurnResult{}, ErrNoEligibleRoute
	}

	var attempts []domain.AttemptRecord
	tried := []domain.RouteCandidate{candidate}
	attemptSeq := 0

	for {
		if attemptsLeft <= 0 || r.clock.NowUnixMilli() >= deadline {
			return TurnResult{Attempts: attempts}, fmt.Errorf("%w (after %d attempts)", ErrTurnExhausted, len(attempts))
		}
		attemptSeq++
		attemptsLeft--
		rec, resp, err := r.attempt(ctx, attemptInput{
			session:  req.Session,
			turn:     req.Turn,
			messages: req.Messages,
			tools:    req.Tools,
			deadline: deadline,
			seq:      attemptSeq,
		}, candidate.RouteID, candidate.AccountID)
		attempts = append(attempts, rec)
		if err == nil {
			if req.Commit != nil {
				if cerr := req.Commit(ctx); cerr != nil {
					return TurnResult{Attempts: attempts}, fmt.Errorf("commit: %w", cerr)
				}
			}
			b := Binding{Tier: candidate.Tier, Route: candidate.RouteID, Account: candidate.AccountID}
			r.mu.Lock()
			r.sessions[req.Session] = b
			r.mu.Unlock()
			return TurnResult{Response: resp, Binding: b, Attempts: attempts, Committed: req.Commit != nil}, nil
		}
		if isFatalForTurn(err) {
			return TurnResult{Attempts: attempts}, err
		}
		next, ok := domain.NextFailoverCandidate(candidate, tiers, tried, r.rand)
		if !ok {
			break
		}
		candidate = next
		tried = append(tried, next)
	}
	return TurnResult{Attempts: attempts}, fmt.Errorf("%w (%d attempts)", ErrTurnExhausted, len(attempts))
}

// firstCandidate returns the combination a turn starts from: the first
// configured combination of the first tier at or after startTier that offers
// one. PLAN 8.4 asks for the uniform random draw when the session's route and
// account are chosen, which SelectCandidate performs at connect; a turn then
// runs on the tier it was pinned to, and only a failure moves it. What follows
// a failure is the domain's randomly ordered failover policy.
func firstCandidate(tiers domain.TieredCandidates, startTier int) (domain.RouteCandidate, bool) {
	if startTier < 0 {
		startTier = 0
	}
	if startTier > len(tiers) {
		startTier = len(tiers)
	}
	offset, ok := domain.FirstEligibleTier(tiers[startTier:])
	if !ok {
		return domain.RouteCandidate{}, false
	}
	return tiers[startTier+offset][0], true
}

// ExecuteRequest is one routed model request that is not a turn: it carries no
// commit and pins no session binding. Purpose restricts candidate routes to
// those that serve it; Accept validates a successful response before it is
// accepted. A nil Accept accepts every successful response.
type ExecuteRequest struct {
	Session   domain.SessionID
	Turn      domain.TurnID
	Purpose   string
	Messages  []domain.Message
	Tools     []domain.ToolDefinition
	MaxTokens int
	// DeadlineMs is the absolute wall-clock deadline for the whole request.
	// Zero uses the configured turn deadline.
	DeadlineMs int64
	Accept     func(domain.ModelResponse) error
}

// ExecuteResult reports the accepted response and every attempt made.
type ExecuteResult struct {
	Response domain.ModelResponse
	Binding  Binding
	Attempts []domain.AttemptRecord
}

// Execute runs one routed model request with failover across the eligible
// routes of the configured tiers, filtered by purpose. A provider failure or
// an Accept rejection cools the route and the next candidate is tried, so a
// broken or unusable model does not end the request while another eligible
// model remains. It pins no session binding and commits nothing.
//
// Unlike ExecuteTurn this walk keeps the operator's configured order: tiers in
// order, then each tier's declared routes and accounts in order. A one-shot
// request has no session pin to preserve, and the domain failover policy
// deliberately randomizes route order within a tier, which here would move a
// configured preference at random on every MOTD probe.
func (r *Router) Execute(ctx context.Context, req ExecuteRequest) (ExecuteResult, error) {
	if err := ctx.Err(); err != nil {
		return ExecuteResult{}, err
	}
	deadline := req.DeadlineMs
	if deadline <= 0 {
		deadline = r.clock.NowUnixMilli() + r.cfg.Policy.TurnDeadlineMs
	}
	attemptsLeft := r.cfg.Policy.MaxAttempts

	tiers := r.tieredCandidates(ctx, req.Purpose)
	var tried []domain.RouteCandidate
	var attempts []domain.AttemptRecord
	attemptSeq := 0

	for _, tier := range tiers {
		for _, candidate := range tier {
			if !untried(tried, candidate) {
				continue
			}
			if attemptsLeft <= 0 || r.clock.NowUnixMilli() >= deadline {
				return ExecuteResult{Attempts: attempts}, fmt.Errorf("%w (after %d attempts)", ErrTurnExhausted, len(attempts))
			}
			attemptSeq++
			attemptsLeft--
			tried = append(tried, candidate)
			rec, resp, err := r.attempt(ctx, attemptInput{
				session:   req.Session,
				turn:      req.Turn,
				messages:  req.Messages,
				tools:     req.Tools,
				maxTokens: req.MaxTokens,
				deadline:  deadline,
				seq:       attemptSeq,
			}, candidate.RouteID, candidate.AccountID)
			attempts = append(attempts, rec)
			if err == nil {
				if req.Accept != nil {
					if aerr := req.Accept(resp); aerr != nil {
						// A response the caller cannot use is a route
						// failure: cool the route and try the next model.
						env := domain.NewErrorEnvelope(domain.FailureInvalidResponse, aerr.Error(), candidate.RouteID, candidate.AccountID, r.clock.NowUnixMilli())
						r.recordFailure(ctx, candidate.RouteID, candidate.AccountID, env)
						continue
					}
				}
				return ExecuteResult{Response: resp, Binding: Binding{Tier: candidate.Tier, Route: candidate.RouteID, Account: candidate.AccountID}, Attempts: attempts}, nil
			}
			if isFatalForTurn(err) {
				return ExecuteResult{Attempts: attempts}, err
			}
		}
	}
	if len(attempts) == 0 {
		return ExecuteResult{}, ErrNoEligibleRoute
	}
	return ExecuteResult{Attempts: attempts}, fmt.Errorf("%w (%d attempts)", ErrTurnExhausted, len(attempts))
}

// untried reports whether a combination has not been attempted in this request
// yet. The identity is route plus account; key references take no part (PLAN
// 8.2), so several keys behind one account cannot produce a second identical
// attempt, and a route configured into more than one tier is attempted in the
// first tier only. It is the same identity the domain's candidateKey uses,
// restated here because the configured-order walk above is deliberately not
// the domain's random-order failover policy.
func untried(tried []domain.RouteCandidate, candidate domain.RouteCandidate) bool {
	for _, t := range tried {
		if t.RouteID == candidate.RouteID && t.AccountID == candidate.AccountID {
			return false
		}
	}
	return true
}

func isFatalForTurn(err error) bool {
	return errors.Is(err, domain.FailureUserCancelled) || errors.Is(err, domain.FailureWorldConflict)
}

// attemptInput is the request data one model attempt needs, independent of
// whether it came from a turn or a one-shot routed request.
type attemptInput struct {
	session   domain.SessionID
	turn      domain.TurnID
	messages  []domain.Message
	tools     []domain.ToolDefinition
	maxTokens int
	deadline  int64
	seq       int
}

// attempt runs one model attempt with the given deadline, spending
// reservation/reconciliation, and health-record updates. The returned
// AttemptRecord is always populated; the response is meaningful only when
// err is nil.
func (r *Router) attempt(ctx context.Context, in attemptInput, route domain.RouteID, account domain.AccountID) (domain.AttemptRecord, domain.ModelResponse, error) {
	now := r.clock.NowUnixMilli()
	rec := domain.AttemptRecord{
		AttemptID: attemptID(in.seq),
		RouteID:   route,
		AccountID: account,
		StartTime: now,
		Result:    domain.AttemptResultFailed,
	}

	attemptCtx, cancel := context.WithDeadline(ctx, time.UnixMilli(in.deadline))
	defer cancel()

	// Spending reservation before a paid request (PLAN 9.3): reserve the
	// estimated maximum, reconcile with reported usage, release on failure.
	reserved := false
	ref := SpendingRef{Session: in.session, Turn: in.turn, Route: route, Account: account}
	var estimate float64
	if spec, ok := r.routeSpec(route); ok && spec.Paid && r.ledger != nil && r.cfg.EstimateMaxCost != nil {
		if est, known := r.cfg.EstimateMaxCost(route); known && est > 0 {
			estimate = est
			if err := r.ledger.Reserve(attemptCtx, ref, est); err != nil {
				rec.EndTime = r.clock.NowUnixMilli()
				rec.Error = envelopePtr(domain.FailureProviderOutage, "spending reservation rejected", route, account, now)
				return rec, domain.ModelResponse{}, rec.Error
			}
			reserved = true
		}
	}

	mreq := domain.ModelRequest{
		RouteID:    route,
		AccountID:  account,
		Messages:   append([]domain.Message(nil), in.messages...),
		Tools:      append([]domain.ToolDefinition(nil), in.tools...),
		MaxTokens:  in.maxTokens,
		DeadlineMs: in.deadline,
		RequestID:  attemptID(in.seq).String(),
	}
	resp, err := r.gateway.Request(attemptCtx, mreq)
	rec.EndTime = r.clock.NowUnixMilli()
	if err != nil {
		switch {
		case isAdmissionRefusal(err):
			// A local admission refusal is request-scoped, not a provider
			// fault: the gate turned the request away before any provider time
			// was spent, so no route or account record may cool or quarantine
			// because of it (PLAN 9.1). The attempt is still recorded so
			// research keeps the refusal and its reason.
			rec.Result = domain.AttemptResultFailed
			rec.Error = envelopePtr(domain.FailureUnknown, err.Error(), route, account, rec.EndTime)
		default:
			var env domain.ErrorEnvelope
			if errors.As(err, &env) {
				rec.Error = &env
				rec.Result = attemptResultFor(env.Class, err)
				r.recordFailure(ctx, route, account, env)
			} else {
				rec.Result = domain.AttemptResultFailed
				env := domain.NewErrorEnvelope(domain.FailureUnknown, err.Error(), route, account, rec.EndTime)
				rec.Error = &env
				r.recordFailure(ctx, route, account, env)
				err = env
			}
		}
		if ctx.Err() != nil || err == context.DeadlineExceeded {
			rec.Result = domain.AttemptResultTimeout
		}
		if reserved {
			_ = r.ledger.Release(attemptCtx, ref)
		}
		return rec, domain.ModelResponse{}, err
	}

	rec.Result = domain.AttemptResultSuccess
	rec.Usage = resp.Usage
	if reserved {
		actual := estimate
		if resp.Usage.EstimatedCostUSD != nil {
			actual = *resp.Usage.EstimatedCostUSD
		}
		_ = r.ledger.Reconcile(attemptCtx, ref, actual)
	}
	r.recordSuccess(route, account)
	return rec, resp, nil
}

func (r *Router) recordSuccess(route domain.RouteID, account domain.AccountID) {
	now := r.clock.NowUnixMilli()
	for _, key := range []domain.HealthKey{
		{RouteID: route, Scope: ScopeRoute},
		{RouteID: route, AccountID: account, Scope: ScopeAccount},
		{RouteID: route, AccountID: account, Scope: ScopeCredential},
	} {
		rec, ok, err := r.health.Get(context.Background(), key)
		if err != nil || !ok {
			continue
		}
		rec.RecordSuccess(now)
		_ = r.health.Save(context.Background(), rec)
	}
}

// isAdmissionRefusal reports whether err is the inference admission gate
// refusing the request locally rather than a provider failing.
//
// Both refusals reach here wrapped as a domain limit error, which is exactly
// how a provider's own rate limit or exhausted quota arrives, so the error
// category alone cannot separate them: cooling the route or account because
// VibeShell turned away its own request would penalize a healthy provider and
// shrink the eligible route space for every later turn. The gate's sentinel
// errors are what make a refusal local, and a provider error never carries one.
//
// The decision is made here, in attempt, because the sentinel survives only on
// the original error: recordFailure is handed an ErrorEnvelope, which keeps the
// failure class but not the wrapped cause.
func isAdmissionRefusal(err error) bool {
	return errors.Is(err, inference.ErrQueueFull) || errors.Is(err, inference.ErrWaitExceeded)
}

// recordFailure classifies the failure and updates the narrowest evidenced
// health scope (PLAN 9.1). Credential failures cool the credential record
// of that account only; quota/rate-limit failures additionally mark the
// shared quota group; model-not-found/provider/transport failures cool or
// quarantine the route record. User cancellation and world conflicts
// carry no health penalty, which is the domain's health-neutral classification
// and the same guard HealthRecord.RecordFailure applies.
func (r *Router) recordFailure(ctx context.Context, route domain.RouteID, account domain.AccountID, env domain.ErrorEnvelope) {
	if domain.IsHealthNeutral(env.Class) {
		return
	}
	now := r.clock.NowUnixMilli()
	switch env.Class {
	case domain.FailureInvalidCredential:
		r.updateRecord(domain.HealthKey{RouteID: route, AccountID: account, Scope: ScopeCredential}, env, now)
	case domain.FailureQuotaExhausted, domain.FailureRateLimited:
		r.updateRecord(domain.HealthKey{RouteID: route, AccountID: account, Scope: ScopeAccount}, env, now)
		if acc, ok := r.account(account); ok {
			// The same default the account record's cooldown uses, so the
			// shared group is released exactly when the account is.
			resetAt := now + domain.DefaultQuotaCooldownMs
			if env.RetryAfter != nil {
				resetAt = now + *env.RetryAfter
			}
			r.markQuotaGroup(ctx, acc.QuotaGroup, resetAt)
		}
	default:
		r.updateRecord(domain.HealthKey{RouteID: route, Scope: ScopeRoute}, env, now)
	}
}

func (r *Router) updateRecord(key domain.HealthKey, env domain.ErrorEnvelope, now int64) {
	rec, ok, err := r.health.Get(context.Background(), key)
	if err != nil {
		return
	}
	if !ok {
		rec = domain.NewHealthRecord(key.RouteID, key.AccountID, key.Scope, now)
	}
	rec.RecordFailure(env.Class, env.Message, env.RetryAfter, now)
	_ = r.health.Save(context.Background(), rec)
}

func attemptResultFor(class domain.FailureClass, err error) domain.AttemptResult {
	if errors.Is(err, context.DeadlineExceeded) || class == domain.FailureNetworkTimeout {
		return domain.AttemptResultTimeout
	}
	if class == domain.FailureContentRejected {
		return domain.AttemptResultContentRejected
	}
	return domain.AttemptResultFailed
}

func envelopePtr(class domain.FailureClass, msg string, route domain.RouteID, account domain.AccountID, now int64) *domain.ErrorEnvelope {
	env := domain.NewErrorEnvelope(class, msg, route, account, now)
	return &env
}

// attemptID derives a deterministic, regex-valid AttemptID from the per-turn
// attempt sequence so tests never depend on real randomness.
func attemptID(seq int) domain.AttemptID {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	buf := make([]byte, 26)
	n := seq
	for i := len(buf) - 1; i >= 0; i-- {
		if n > 0 {
			buf[i] = alphabet[n%32]
			n /= 32
		} else {
			buf[i] = alphabet[0]
		}
	}
	id, err := domain.ParseAttemptID("att_" + string(buf))
	if err != nil {
		panic(err)
	}
	return id
}
