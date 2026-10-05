package domain

import "sort"

// Pure, deterministic routing policies (PLAN 8.2-8.4): tier expansion,
// uniform route selection, attempt deduplication, session affinity, and
// failover ordering. Every function takes its decisions from explicit inputs
// and an injected RandomIntn, so tests are reproducible and no policy reads a
// global clock or the ambient random source. Catalogue I/O, health storage,
// and model calls live in application/adapters, not here.

// RandomIntn is the injectable randomness routing policies consume. It is
// satisfied structurally by ports.Random (which adds Bytes and Float64) and by
// deterministic test doubles; keeping the interface local preserves the
// domain's independence from the ports package.
type RandomIntn interface {
	// Intn returns a uniform value in [0, n) for n > 0.
	Intn(n int) int
}

// TieredCandidates holds eligible route/account combinations grouped in tier
// expansion order. A tier with no eligible candidate is empty; callers build
// each tier from the catalogue and health snapshot before these policies run.
type TieredCandidates [][]RouteCandidate

// FirstEligibleTier returns the index of the earliest tier with at least one
// eligible candidate (PLAN 8.4). It reports false when every tier is empty.
func FirstEligibleTier(tiers TieredCandidates) (int, bool) {
	for i, tier := range tiers {
		if len(tier) > 0 {
			return i, true
		}
	}
	return 0, false
}

// DistinctRoutes returns the candidate routes in deterministic first-seen
// order with duplicates removed. Tier expansion and attempt deduplication
// depend on distinct routes so duplicate entries and extra keys never multiply
// selection weight (PLAN 8.3 step 6, 8.4).
func DistinctRoutes(candidates []RouteCandidate) []RouteID {
	seen := make(map[RouteID]bool, len(candidates))
	var out []RouteID
	for _, c := range candidates {
		if seen[c.RouteID] {
			continue
		}
		seen[c.RouteID] = true
		out = append(out, c.RouteID)
	}
	return out
}

// SelectCandidate chooses one candidate from tier: a route uniformly at random
// among the distinct eligible routes, then an account uniformly at random
// among that route's eligible accounts (PLAN 8.4). The route draw is
// independent of how many accounts or key references back a route (PLAN 8.2),
// and the deterministic ordering by ID keeps the injected random source as the
// only source of variation. It reports false for an empty tier.
func SelectCandidate(tier []RouteCandidate, rand RandomIntn) (RouteCandidate, bool) {
	if len(tier) == 0 {
		return RouteCandidate{}, false
	}
	byRoute := groupCandidatesByRoute(tier)
	routes := make([]RouteID, 0, len(byRoute))
	for route := range byRoute {
		routes = append(routes, route)
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].Value() < routes[j].Value() })

	accounts := byRoute[routes[rand.Intn(len(routes))]]
	sortCandidates(accounts)
	return accounts[rand.Intn(len(accounts))], true
}

// EnsureSelection returns the session's pinned candidate when it is still
// eligible, otherwise it selects from the first eligible tier. A recovered
// earlier tier never migrates an already-healthy pin, which preserves session
// affinity and avoids unnecessary context churn (PLAN 8.4). A nil pin means
// the session has no affinity yet.
func EnsureSelection(pinned *RouteCandidate, tiers TieredCandidates, rand RandomIntn) (RouteCandidate, bool) {
	if pinned != nil && tiersContain(tiers, *pinned) {
		return *pinned, true
	}
	tierIdx, ok := FirstEligibleTier(tiers)
	if !ok {
		return RouteCandidate{}, false
	}
	return SelectCandidate(tiers[tierIdx], rand)
}

// NextFailoverCandidate returns the next candidate to try after pinned failed
// (PLAN 8.4): first another untried account for the same route in the same
// tier, then a randomly ordered untried route of that tier, then the next
// tier. It refuses to advance past a tier that still has an untried
// combination and never returns a combination present in tried, so duplicate
// entries and keys in an exhausted quota group cannot cause a retry loop. It
// reports false only when no untried candidate remains.
func NextFailoverCandidate(pinned RouteCandidate, tiers TieredCandidates, tried []RouteCandidate, rand RandomIntn) (RouteCandidate, bool) {
	triedSet := make(map[candidateKey]bool, len(tried))
	for _, c := range tried {
		triedSet[candidateKeyOf(c)] = true
	}
	untried := func(c RouteCandidate) bool { return !triedSet[candidateKeyOf(c)] }

	// Same tier, same route: exhaust the other eligible accounts first.
	sameTier := tierAt(tiers, pinned.Tier)
	for _, c := range sortedCopy(sameTier) {
		if c.RouteID != pinned.RouteID || !untried(c) {
			continue
		}
		return c, true
	}

	// Same tier, other routes in random order.
	for _, route := range shuffledRoutes(sameTier, pinned.RouteID, rand) {
		accounts := candidatesForRoute(sameTier, route)
		for _, c := range accounts {
			if untried(c) {
				return c, true
			}
		}
	}

	// Later tiers in order.
	for tierIdx := pinned.Tier + 1; tierIdx < len(tiers); tierIdx++ {
		if c, ok := selectUntried(tiers[tierIdx], untried, rand); ok {
			return c, true
		}
	}
	return RouteCandidate{}, false
}

// QuotaGrouping collects the accounts that share one quota group so callers
// can count shared quota once (PLAN 8.2). Accounts without a quota group are
// each returned as their own grouping with an empty Group.
type QuotaGrouping struct {
	Group    string
	Accounts []Account
}

// GroupAccountsByQuota groups accounts by quota group, with ungrouped accounts
// kept separate. Groups are ordered by group name and each group's accounts by
// ID; the ungrouped accounts follow in ID order, so output is reproducible.
func GroupAccountsByQuota(accounts []Account) []QuotaGrouping {
	byGroup := make(map[string][]Account)
	order := make([]string, 0, len(accounts))
	for _, acc := range accounts {
		if acc.QuotaGroup == "" {
			continue
		}
		if _, ok := byGroup[acc.QuotaGroup]; !ok {
			order = append(order, acc.QuotaGroup)
		}
		byGroup[acc.QuotaGroup] = append(byGroup[acc.QuotaGroup], acc)
	}
	sort.Strings(order)

	groupings := make([]QuotaGrouping, 0, len(order))
	for _, group := range order {
		members := byGroup[group]
		sort.Slice(members, func(i, j int) bool { return members[i].ID.Value() < members[j].ID.Value() })
		groupings = append(groupings, QuotaGrouping{Group: group, Accounts: members})
	}
	for _, acc := range sortedAccounts(accounts) {
		if acc.QuotaGroup == "" {
			groupings = append(groupings, QuotaGrouping{Accounts: []Account{acc}})
		}
	}
	return groupings
}

// RepresentativeAccounts returns one enabled account per non-empty quota group
// plus every enabled account without a quota group, preserving input order.
// Selecting through representatives counts a shared quota once even when
// several accounts or keys belong to it, while the remaining grouped accounts
// stay available for failover (PLAN 8.2, 8.4). Disabled accounts are dropped.
func RepresentativeAccounts(accounts []Account) []Account {
	seenGroup := make(map[string]bool)
	var out []Account
	for _, acc := range accounts {
		if !acc.Enabled {
			continue
		}
		if acc.QuotaGroup == "" {
			out = append(out, acc)
			continue
		}
		if seenGroup[acc.QuotaGroup] {
			continue
		}
		seenGroup[acc.QuotaGroup] = true
		out = append(out, acc)
	}
	return out
}

// BackoffDelayMs returns the exponential backoff delay for one retry, capped
// at maxMs and perturbed by up to jitterPermille/1000 of itself in either
// direction using the injected randomness (PLAN 9.2). attempt is 0-based and
// negative values are treated as 0. A non-positive base or max yields 0.
func BackoffDelayMs(baseMs int64, attempt int, maxMs int64, jitterPermille int, rand RandomIntn) int64 {
	if baseMs <= 0 || maxMs <= 0 || attempt < 0 {
		return 0
	}
	delay := baseMs
	for i := 0; i < attempt && delay < maxMs; i++ {
		delay *= 2
	}
	if delay > maxMs {
		delay = maxMs
	}
	if jitterPermille > 0 {
		span := 2*jitterPermille + 1
		delta := rand.Intn(span) - jitterPermille
		delay = delay * int64(1000+delta) / 1000
		if delay < 0 {
			delay = 0
		}
		if delay > maxMs {
			delay = maxMs
		}
	}
	return delay
}

// candidateKey is the identity used to detect a repeated route/account
// attempt. Key references deliberately do not take part: keys belong to
// accounts and do not create new combinations (PLAN 8.2, 8.4).
type candidateKey struct {
	route   RouteID
	account AccountID
}

func candidateKeyOf(c RouteCandidate) candidateKey {
	return candidateKey{route: c.RouteID, account: c.AccountID}
}

func groupCandidatesByRoute(candidates []RouteCandidate) map[RouteID][]RouteCandidate {
	byRoute := make(map[RouteID][]RouteCandidate)
	for _, c := range candidates {
		byRoute[c.RouteID] = append(byRoute[c.RouteID], c)
	}
	return byRoute
}

func tierAt(tiers TieredCandidates, idx int) []RouteCandidate {
	if idx < 0 || idx >= len(tiers) {
		return nil
	}
	return tiers[idx]
}

func tiersContain(tiers TieredCandidates, want RouteCandidate) bool {
	for _, tier := range tiers {
		for _, c := range tier {
			if candidateKeyOf(c) == candidateKeyOf(want) {
				return true
			}
		}
	}
	return false
}

func candidatesForRoute(candidates []RouteCandidate, route RouteID) []RouteCandidate {
	var out []RouteCandidate
	for _, c := range candidates {
		if c.RouteID == route {
			out = append(out, c)
		}
	}
	sortCandidates(out)
	return out
}

func shuffledRoutes(candidates []RouteCandidate, exclude RouteID, rand RandomIntn) []RouteID {
	routes := DistinctRoutes(candidates)
	filtered := routes[:0]
	for _, route := range routes {
		if route != exclude {
			filtered = append(filtered, route)
		}
	}
	for i := len(filtered) - 1; i > 0; i-- {
		j := rand.Intn(i + 1)
		filtered[i], filtered[j] = filtered[j], filtered[i]
	}
	return filtered
}

func selectUntried(tier []RouteCandidate, untried func(RouteCandidate) bool, rand RandomIntn) (RouteCandidate, bool) {
	var remaining []RouteCandidate
	for _, c := range tier {
		if untried(c) {
			remaining = append(remaining, c)
		}
	}
	return SelectCandidate(remaining, rand)
}

func sortCandidates(candidates []RouteCandidate) {
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].RouteID.Value() != candidates[j].RouteID.Value() {
			return candidates[i].RouteID.Value() < candidates[j].RouteID.Value()
		}
		return candidates[i].AccountID.Value() < candidates[j].AccountID.Value()
	})
}

func sortedCopy(candidates []RouteCandidate) []RouteCandidate {
	out := append([]RouteCandidate(nil), candidates...)
	sortCandidates(out)
	return out
}

func sortedAccounts(accounts []Account) []Account {
	out := append([]Account(nil), accounts...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID.Value() < out[j].ID.Value() })
	return out
}
