package domain_test

import (
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// idAlphabet is Crockford base32 without I, L, O, U, matching the identity
// regex; idValue builds a valid 26-character identity suffix.
const idAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func idValue(i int) string {
	buf := make([]byte, 26)
	for pos := len(buf) - 1; pos >= 0; pos-- {
		buf[pos] = idAlphabet[i%len(idAlphabet)]
		i /= len(idAlphabet)
	}
	return string(buf)
}

func testRouteID(t *testing.T, i int) domain.RouteID {
	t.Helper()
	id, err := domain.ParseRouteID("rte_" + idValue(i))
	if err != nil {
		t.Fatalf("ParseRouteID(%d): %v", i, err)
	}
	return id
}

func testAccountID(t *testing.T, i int) domain.AccountID {
	t.Helper()
	id, err := domain.ParseAccountID("acc_" + idValue(i))
	if err != nil {
		t.Fatalf("ParseAccountID(%d): %v", i, err)
	}
	return id
}

func testKeyRef(t *testing.T, i int) domain.KeyRef {
	t.Helper()
	id, err := domain.ParseKeyRef("key_" + idValue(i))
	if err != nil {
		t.Fatalf("ParseKeyRef(%d): %v", i, err)
	}
	return id
}

// testAccount builds one account with keyCount key references. Key count is
// deliberately variable so tests can prove it never changes selection weight.
func testAccount(t *testing.T, i int, group string, enabled bool, keyCount int) domain.Account {
	t.Helper()
	keys := make([]domain.KeyRef, keyCount)
	for k := range keys {
		keys[k] = testKeyRef(t, i*100+k)
	}
	return domain.Account{ID: testAccountID(t, i), QuotaGroup: group, Enabled: enabled, KeyRefs: keys}
}

func candidate(t *testing.T, tier, routeIdx, accountIdx int) domain.RouteCandidate {
	t.Helper()
	return domain.RouteCandidate{
		Tier:      tier,
		RouteID:   testRouteID(t, routeIdx),
		AccountID: testAccountID(t, accountIdx),
	}
}

// lcgRand is a deterministic linear congruential double for repeated sampling.
type lcgRand struct{ state uint64 }

func newLCGRand(seed uint64) *lcgRand { return &lcgRand{state: seed} }

func (r *lcgRand) Intn(n int) int {
	if n <= 0 {
		panic("Intn requires n > 0")
	}
	r.state = r.state*6364136223846793005 + 1442695040888963407
	return int((r.state >> 33) % uint64(n))
}

// constRand always returns the same index so a test can force a branch.
type constRand int

func (c constRand) Intn(n int) int {
	if n <= 0 {
		panic("Intn requires n > 0")
	}
	return int(c) % n
}

// ---------------------------------------------------------------------------
// Health classification (PLAN 9.1)
// ---------------------------------------------------------------------------

func TestHealthNeutralFailuresNeverDegrade(t *testing.T) {
	route, account := testRouteID(t, 1), testAccountID(t, 1)
	rec := domain.NewHealthRecord(route, account, "account", 1000)

	for i := 0; i < 5; i++ {
		rec.RecordFailure(domain.FailureUserCancelled, "cancelled", nil, 1000+int64(i))
		rec.RecordFailure(domain.FailureWorldConflict, "conflict", nil, 2000+int64(i))
	}
	if rec.State != domain.HealthHealthy {
		t.Fatalf("state after cancellations/conflicts = %v, want healthy", rec.State)
	}
	if rec.ConsecutiveFailures != 0 {
		t.Fatalf("consecutive failures = %d, want 0: cancellations are not outages", rec.ConsecutiveFailures)
	}
	for _, neutral := range []domain.FailureClass{domain.FailureUserCancelled, domain.FailureWorldConflict} {
		if !domain.IsHealthNeutral(neutral) {
			t.Errorf("IsHealthNeutral(%s) = false, want true", neutral)
		}
	}
	for _, outage := range []domain.FailureClass{domain.FailureProviderOutage, domain.FailureQuotaExhausted, domain.FailureInvalidCredential} {
		if domain.IsHealthNeutral(outage) {
			t.Errorf("IsHealthNeutral(%s) = true, want false", outage)
		}
	}
}

func TestRequestScopedFailureNeedsRepetition(t *testing.T) {
	route, account := testRouteID(t, 1), testAccountID(t, 1)
	if !domain.IsRequestScopedFailure(domain.FailureContextTooLong) {
		t.Fatal("context_too_long must be request-scoped, not a general outage")
	}
	if domain.IsRequestScopedFailure(domain.FailureQuotaExhausted) {
		t.Error("quota exhaustion must not be request-scoped")
	}

	rec := domain.NewHealthRecord(route, account, "route", 1000)
	rec.RecordFailure(domain.FailureContextTooLong, "context repair", nil, 1000)
	if rec.State != domain.HealthHealthy {
		t.Fatalf("after one context repair state = %v, want healthy", rec.State)
	}
	rec.RecordFailure(domain.FailureContextTooLong, "context repair", nil, 1001)
	if rec.State != domain.HealthHealthy {
		t.Fatalf("after two context repairs state = %v, want healthy", rec.State)
	}
	rec.RecordFailure(domain.FailureContextTooLong, "context repair", nil, 1002)
	if rec.State != domain.HealthCoolingDown {
		t.Fatalf("after three context repairs state = %v, want cooling_down", rec.State)
	}
}

func TestCredentialFailureIsAccountScoped(t *testing.T) {
	route := testRouteID(t, 1)
	revoked := domain.NewHealthRecord(route, testAccountID(t, 1), "credential", 1000)
	independent := domain.NewHealthRecord(route, testAccountID(t, 2), "credential", 1000)

	revoked.RecordFailure(domain.FailureInvalidCredential, "revoked", nil, 1000)
	if revoked.IsHealthy(1001) {
		t.Fatal("revoked credential record should not be healthy")
	}
	if !independent.IsHealthy(1001) {
		t.Fatal("an independent account must stay healthy when another account's key is revoked")
	}
}

func TestRetryAfterSetsCooldown(t *testing.T) {
	route, account := testRouteID(t, 1), testAccountID(t, 1)
	rec := domain.NewHealthRecord(route, account, "account", 1000)
	retryAfter := int64(5000)
	rec.RecordFailure(domain.FailureRateLimited, "429", &retryAfter, 1000)

	if rec.State != domain.HealthCoolingDown {
		t.Fatalf("state = %v, want cooling_down", rec.State)
	}
	if rec.CooldownUntil != 6000 {
		t.Fatalf("cooldown_until = %d, want 6000 (now + retry_after)", rec.CooldownUntil)
	}
	if rec.IsHealthy(5999) {
		t.Error("record must stay unavailable before the honored Retry-After")
	}
	if !rec.IsHealthy(6000) {
		t.Error("record must become half-open once the cooldown expires")
	}
	if domain.ProbeDue(rec, 5999) {
		t.Error("probe must not be due before the cooldown expires")
	}
	if !domain.ProbeDue(rec, 6000) {
		t.Error("cooling record with expired cooldown must be probe-eligible")
	}
}

func TestProviderOutageUsesBoundedCooldown(t *testing.T) {
	route, account := testRouteID(t, 1), testAccountID(t, 1)
	rec := domain.NewHealthRecord(route, account, "route", 1000)
	// A provider 5xx is handled with the bounded default cooldown and shared
	// outage suppression, not with a provider-supplied reset (PLAN 9.1).
	rec.RecordFailure(domain.FailureProviderOutage, "5xx", nil, 1000)
	if rec.CooldownUntil != 1000+domain.DefaultOutageCooldownMs {
		t.Fatalf("outage cooldown_until = %d, want now + %d", rec.CooldownUntil, domain.DefaultOutageCooldownMs)
	}
}

// ---------------------------------------------------------------------------
// Half-open probes and recovery (PLAN 9.2)
// ---------------------------------------------------------------------------

func TestProbeHalfOpenAdmitsOneProbe(t *testing.T) {
	route, account := testRouteID(t, 1), testAccountID(t, 1)
	rec := domain.NewHealthRecord(route, account, "account", 1000)
	retryAfter := int64(1000)
	rec.RecordFailure(domain.FailureQuotaExhausted, "balance exhausted", &retryAfter, 1000) // cooling until 2000

	if domain.ProbeDue(rec, 1999) {
		t.Fatal("probe must not be due before cooldown expires")
	}
	admitted, ok := domain.AdmitProbe(rec, 2000)
	if !ok {
		t.Fatal("expected the half-open probe to be admitted")
	}
	if admitted.State != domain.HealthProbing || admitted.ProbeCount != 1 {
		t.Fatalf("admitted probe: state=%v count=%d, want probing/1", admitted.State, admitted.ProbeCount)
	}

	second, ok := domain.AdmitProbe(admitted, 2000)
	if ok {
		t.Fatal("a second concurrent probe for the same key must be refused")
	}
	if second.ProbeCount != 1 {
		t.Fatalf("probe count = %d after refused probe, want 1", second.ProbeCount)
	}
}

func TestProbeEligibleRespectsNextProbeAt(t *testing.T) {
	rec := domain.NewHealthRecord(testRouteID(t, 1), testAccountID(t, 1), "account", 1000)
	rec.State = domain.HealthProbeEligible
	rec.NextProbeAt = 5000

	if domain.ProbeDue(rec, 4999) {
		t.Fatal("probe must not be due before NextProbeAt")
	}
	if _, ok := domain.AdmitProbe(rec, 4999); ok {
		t.Fatal("probe before NextProbeAt must be refused")
	}
	if !domain.ProbeDue(rec, 5000) {
		t.Fatal("probe must be due at NextProbeAt")
	}
}

func TestProbeOutcomesRecoverAndReCool(t *testing.T) {
	route, account := testRouteID(t, 1), testAccountID(t, 1)
	rec := domain.NewHealthRecord(route, account, "account", 1000)
	rec.State = domain.HealthProbeEligible
	rec.NextProbeAt = 1000

	probing, ok := domain.AdmitProbe(rec, 1000)
	if !ok {
		t.Fatal("expected probe admission")
	}
	probing.RecordProbeResult(true, 1100)
	if probing.State != domain.HealthHealthy {
		t.Fatalf("after successful probe state = %v, want healthy", probing.State)
	}
	if !domain.SelectableForNewSession(probing, 1100) {
		t.Fatal("recovered route must be selectable for new sessions")
	}

	recooled := domain.NewHealthRecord(route, account, "account", 1000)
	recooled.State = domain.HealthProbeEligible
	probing2, ok := domain.AdmitProbe(recooled, 1000)
	if !ok {
		t.Fatal("expected second probe admission")
	}
	probing2.RecordProbeResult(false, 1200)
	if probing2.State != domain.HealthCoolingDown {
		t.Fatalf("after failed probe state = %v, want cooling_down", probing2.State)
	}
	if probing2.IsHealthy(1200) {
		t.Fatal("a failed probe must keep the record out of service during cooldown")
	}
}

func TestSelectableForNewSession(t *testing.T) {
	route, account := testRouteID(t, 1), testAccountID(t, 1)
	healthy := domain.NewHealthRecord(route, account, "account", 1000)
	if !domain.SelectableForNewSession(healthy, 1000) {
		t.Error("healthy record must be selectable")
	}
	quarantined := domain.NewHealthRecord(route, account, "route", 1000)
	quarantined.State = domain.HealthQuarantined
	if domain.SelectableForNewSession(quarantined, 1000) {
		t.Error("quarantined record must not be selectable")
	}
	cooling := domain.NewHealthRecord(route, account, "account", 1000)
	cooling.State = domain.HealthCoolingDown
	cooling.CooldownUntil = 2000
	if domain.SelectableForNewSession(cooling, 1999) {
		t.Error("cooling record must not be selectable before cooldown expires")
	}
	if !domain.SelectableForNewSession(cooling, 2000) {
		t.Error("half-open record must be selectable once cooldown expires")
	}
}

// ---------------------------------------------------------------------------
// Tier expansion, uniformity, affinity, failover (PLAN 8.2-8.4)
// ---------------------------------------------------------------------------

func TestFirstEligibleTierWins(t *testing.T) {
	tier0 := []domain.RouteCandidate{candidate(t, 0, 10, 10)}
	tier1 := []domain.RouteCandidate{
		candidate(t, 1, 11, 11), candidate(t, 1, 12, 12), candidate(t, 1, 13, 13),
	}
	tiers := domain.TieredCandidates{tier0, tier1}

	idx, ok := domain.FirstEligibleTier(tiers)
	if !ok || idx != 0 {
		t.Fatalf("FirstEligibleTier = (%d,%v), want (0,true)", idx, ok)
	}
	got, ok := domain.EnsureSelection(nil, tiers, constRand(0))
	if !ok {
		t.Fatal("EnsureSelection returned no candidate")
	}
	if got.Tier != 0 {
		t.Fatalf("selected tier = %d, want the first eligible tier 0 even though tier 1 is larger", got.Tier)
	}
}

func TestEnsureSelectionPreservesHealthyAffinity(t *testing.T) {
	pinned := candidate(t, 1, 20, 20)
	tiers := domain.TieredCandidates{
		[]domain.RouteCandidate{candidate(t, 0, 21, 21)}, // earlier tier recovered
		[]domain.RouteCandidate{pinned},
	}
	got, ok := domain.EnsureSelection(&pinned, tiers, constRand(0))
	if !ok {
		t.Fatal("EnsureSelection returned no candidate")
	}
	if got != pinned {
		t.Fatalf("got tier %d route %s, want the pinned tier-1 combination", got.Tier, got.RouteID)
	}
}

func TestEnsureSelectionReselectsWhenPinIneligible(t *testing.T) {
	pinned := candidate(t, 1, 30, 30)
	tiers := domain.TieredCandidates{
		[]domain.RouteCandidate{candidate(t, 0, 31, 31)},
		[]domain.RouteCandidate{candidate(t, 1, 32, 32)},
	}
	got, ok := domain.EnsureSelection(&pinned, tiers, constRand(0))
	if !ok {
		t.Fatal("EnsureSelection returned no candidate")
	}
	if got.Tier != 0 || got.RouteID != testRouteID(t, 31) {
		t.Fatalf("got tier %d, want a fresh selection from tier 0", got.Tier)
	}
}

func TestSelectionUniformOverRoutesIndependentOfAccountAndKeyCount(t *testing.T) {
	routeBig, routeSmall := testRouteID(t, 40), testRouteID(t, 41)
	// Route 40 is backed by three accounts (with 1, 4, and 2 key references);
	// route 41 by a single account with one key. Route selection must still be
	// ~50/50: neither extra accounts nor extra keys raise a route's weight.
	tier := []domain.RouteCandidate{
		{Tier: 0, RouteID: routeBig, AccountID: testAccountID(t, 40)},
		{Tier: 0, RouteID: routeBig, AccountID: testAccountID(t, 41)},
		{Tier: 0, RouteID: routeBig, AccountID: testAccountID(t, 42)},
		{Tier: 0, RouteID: routeSmall, AccountID: testAccountID(t, 43)},
	}
	// Build the backing accounts so the unequal key counts are real inputs.
	for _, acc := range []domain.Account{
		testAccount(t, 40, "", true, 1),
		testAccount(t, 41, "", true, 4),
		testAccount(t, 42, "", true, 2),
		testAccount(t, 43, "", true, 1),
	} {
		if len(acc.KeyRefs) == 0 {
			t.Fatal("test account must carry key references")
		}
	}
	rand := newLCGRand(7)
	const samples = 4000
	counts := map[domain.RouteID]int{}
	for i := 0; i < samples; i++ {
		got, ok := domain.SelectCandidate(tier, rand)
		if !ok {
			t.Fatal("SelectCandidate returned no candidate")
		}
		counts[got.RouteID]++
	}
	if counts[routeBig] < 1600 || counts[routeBig] > 2400 {
		t.Fatalf("route with 3 accounts/7 keys selected %d/%d times, want ~50%%", counts[routeBig], samples)
	}
	if counts[routeSmall] < 1600 || counts[routeSmall] > 2400 {
		t.Fatalf("route with 1 account/1 key selected %d/%d times, want ~50%%", counts[routeSmall], samples)
	}
}

func TestSelectionUniformAcrossAccountsWithinRoute(t *testing.T) {
	route := testRouteID(t, 50)
	tier := make([]domain.RouteCandidate, 3)
	for i := range tier {
		tier[i] = domain.RouteCandidate{Tier: 0, RouteID: route, AccountID: testAccountID(t, 50+i)}
	}
	rand := newLCGRand(11)
	const samples = 3000
	counts := map[domain.AccountID]int{}
	for i := 0; i < samples; i++ {
		got, ok := domain.SelectCandidate(tier, rand)
		if !ok {
			t.Fatal("SelectCandidate returned no candidate")
		}
		counts[got.AccountID]++
	}
	for i := range tier {
		got := counts[tier[i].AccountID]
		if got < 700 || got > 1300 {
			t.Fatalf("account %d selected %d/%d times, want ~1/3", i, got, samples)
		}
	}
}

func TestNextFailoverExhaustsTierBeforeAdvancing(t *testing.T) {
	route0, route1, route2 := testRouteID(t, 60), testRouteID(t, 61), testRouteID(t, 62)
	acc0, acc1, acc2, acc3 := testAccountID(t, 60), testAccountID(t, 61), testAccountID(t, 62), testAccountID(t, 63)
	tiers := domain.TieredCandidates{
		{
			{Tier: 0, RouteID: route0, AccountID: acc0},
			{Tier: 0, RouteID: route0, AccountID: acc1},
			{Tier: 0, RouteID: route1, AccountID: acc2},
		},
		{
			{Tier: 1, RouteID: route2, AccountID: acc3},
		},
	}
	pinned := domain.RouteCandidate{Tier: 0, RouteID: route0, AccountID: acc0}
	tried := []domain.RouteCandidate{pinned}
	rand := newLCGRand(3)

	// Another account for the same route comes first.
	second, ok := domain.NextFailoverCandidate(pinned, tiers, tried, rand)
	if !ok {
		t.Fatal("expected a same-route failover account")
	}
	if second.RouteID != route0 || second.AccountID != acc1 {
		t.Fatalf("second candidate = route %s account %s, want route0/acc1", second.RouteID, second.AccountID)
	}
	tried = append(tried, second)

	// Only after route0 is exhausted may the tier's other route be used.
	third, ok := domain.NextFailoverCandidate(pinned, tiers, tried, rand)
	if !ok {
		t.Fatal("expected a same-tier other-route candidate")
	}
	if third.RouteID != route1 || third.AccountID != acc2 {
		t.Fatalf("third candidate = route %s account %s, want route1/acc2", third.RouteID, third.AccountID)
	}
	tried = append(tried, third)

	// Tier 0 is exhausted, so failover advances to tier 1.
	fourth, ok := domain.NextFailoverCandidate(pinned, tiers, tried, rand)
	if !ok {
		t.Fatal("expected advancement to tier 1")
	}
	if fourth.Tier != 1 || fourth.RouteID != route2 || fourth.AccountID != acc3 {
		t.Fatalf("fourth candidate = tier %d route %s, want tier1/route2", fourth.Tier, fourth.RouteID)
	}
	tried = append(tried, fourth)

	if _, ok := domain.NextFailoverCandidate(pinned, tiers, tried, rand); ok {
		t.Fatal("failover must report exhaustion once every combination was tried")
	}
	for _, c := range tried[1:] {
		if c == pinned {
			t.Fatal("failover must never repeat an already-tried combination")
		}
	}
}

func TestNextFailoverRefusesTriedCombination(t *testing.T) {
	pinned := candidate(t, 0, 70, 70)
	tiers := domain.TieredCandidates{[]domain.RouteCandidate{pinned}}
	if _, ok := domain.NextFailoverCandidate(pinned, tiers, []domain.RouteCandidate{pinned}, constRand(0)); ok {
		t.Fatal("the only candidate was already tried; failover must report exhaustion")
	}
}

func TestDistinctRoutesDeduplicates(t *testing.T) {
	r0, r1 := testRouteID(t, 80), testRouteID(t, 81)
	got := domain.DistinctRoutes([]domain.RouteCandidate{
		candidate(t, 0, 80, 80), candidate(t, 0, 81, 81), candidate(t, 0, 80, 82),
	})
	if len(got) != 2 || got[0] != r0 || got[1] != r1 {
		t.Fatalf("DistinctRoutes = %v, want [r0 r1]", got)
	}
}

// ---------------------------------------------------------------------------
// Quota-group grouping (PLAN 8.2, 8.4)
// ---------------------------------------------------------------------------

func TestQuotaGroupingAvoidsDoubleCounting(t *testing.T) {
	// a0 and a1 share quota group g1 (a1 adds keys but no quota); a2 is g2;
	// a3 is ungrouped and enabled; a4 is disabled.
	a0 := testAccount(t, 90, "g1", true, 2)
	a1 := testAccount(t, 91, "g1", true, 5)
	a2 := testAccount(t, 92, "g2", true, 1)
	a3 := testAccount(t, 93, "", true, 1)
	a4 := testAccount(t, 94, "", false, 1)

	representatives := domain.RepresentativeAccounts([]domain.Account{a0, a1, a2, a3, a4})
	if len(representatives) != 3 {
		t.Fatalf("representatives = %d, want 3 (one per group plus the enabled ungrouped account)", len(representatives))
	}
	for _, acc := range representatives {
		if acc.ID == a4.ID {
			t.Fatal("disabled accounts must not be representatives")
		}
	}

	groupings := domain.GroupAccountsByQuota([]domain.Account{a0, a1, a2, a3, a4})
	byGroup := map[string]int{}
	for _, g := range groupings {
		byGroup[g.Group] += len(g.Accounts)
	}
	if byGroup["g1"] != 2 {
		t.Fatalf("group g1 has %d accounts, want both members retained for failover", byGroup["g1"])
	}
	if byGroup["g2"] != 1 {
		t.Fatalf("group g2 has %d accounts, want 1", byGroup["g2"])
	}
	if byGroup[""] != 2 {
		t.Fatalf("ungrouped accounts = %d, want 2 (each its own grouping)", byGroup[""])
	}
}

// ---------------------------------------------------------------------------
// Backoff (PLAN 9.2)
// ---------------------------------------------------------------------------

func TestBackoffDelayCappedAndExponential(t *testing.T) {
	want := []int64{100, 200, 400, 800, 1000, 1000}
	for attempt, expected := range want {
		got := domain.BackoffDelayMs(100, attempt, 1000, 0, constRand(0))
		if got != expected {
			t.Errorf("attempt %d: delay = %d, want %d", attempt, got, expected)
		}
	}
	if got := domain.BackoffDelayMs(100, 0, 1000, 0, constRand(0)); got != 100 {
		t.Errorf("fractional-free attempt 0 = %d, want 100", got)
	}
	if got := domain.BackoffDelayMs(0, 3, 1000, 0, constRand(0)); got != 0 {
		t.Errorf("zero base = %d, want 0", got)
	}
	if got := domain.BackoffDelayMs(100, -1, 1000, 0, constRand(0)); got != 0 {
		t.Errorf("negative attempt = %d, want 0", got)
	}
	// Deep exponentiation must stay capped, not overflow.
	if got := domain.BackoffDelayMs(3, 100, 60_000, 0, constRand(0)); got != 60_000 {
		t.Errorf("high attempt = %d, want the 60000 cap", got)
	}
}

func TestBackoffJitterStaysWithinBounds(t *testing.T) {
	rand := newLCGRand(13)
	for i := 0; i < 500; i++ {
		got := domain.BackoffDelayMs(1000, 0, 1000, 200, rand)
		if got < 800 || got > 1000 {
			t.Fatalf("jittered delay = %d, want within [800,1000] (20%% jitter, capped)", got)
		}
	}
}
