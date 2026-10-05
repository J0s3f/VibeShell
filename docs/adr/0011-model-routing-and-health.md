# ADR 0011: Tiered model routing, session affinity, and health recovery

Status: accepted.

Date: 2026-10-03.

## Context

PLAN 8.2–8.4 define how VibeShell picks a model and an account, and PLAN 9.1–9.2
define what happens when that choice fails. The constraints that shape the design
are unusually specific:

- An account has a stable administrative ID, provider/product eligibility, a
  quota-group ID, and one or more secret references. Several keys may belong to
  one account, and **keys do not multiply that account's quota**. Separate accounts
  may be mixed in one tier.
- A model route is selected **independently of how many keys are available**;
  only then is a healthy eligible account chosen. Adding a second key must not
  accidentally double a model's probability of selection.
- Selection is uniform at random among distinct eligible routes; the choice is
  pinned to the session until a failure requires a change; a healthy session is
  never migrated back to an earlier tier just because that tier recovered.
- Failover order is fixed: another eligible account for the same route, then
  another randomly ordered unused route in the same tier, and only then the next
  tier. Duplicate entries and keys sharing an exhausted quota group must not
  cause a retry loop. A turn deadline and attempt budget bound the work, and
  expiry ends the turn with a temporary simulated service error rather than
  silently bypassing the tier order.
- Health records move through healthy, cooling-down, probe-eligible, probing, and
  healthy/quarantined, persisting failure reason, affected scope, last attempt,
  consecutive failures, next probe time, and last known success. Authoritative
  `Retry-After` is honoured; otherwise configurable exponential backoff with
  jitter and a maximum applies. Only one probe may run per health key, and health
  checks must not create unlimited paid background traffic.

ADR 0005 already separates provider, product, and wire protocol; this record is
about what happens *after* a route is known to be eligible on paper.

The unresolved question is where these decisions live. They are pure business
rules (deterministic, no I/O) but they also need storage, a gateway call, a clock,
and a spending ledger. Deciding that in one place would put business rules behind
I/O; deciding it in two places without a stated split would produce two
implementations of the same policy.

## Decision

**Two layers with a stated split.** The domain owns the *decisions*; the
application package owns the *effects*.

- `internal/domain/routing_policy.go` and `health_policy.go` hold the pure,
  deterministic policies. They take explicit inputs and an injected
  `RandomIntn` (declared locally in the domain, satisfied structurally by
  `ports.Random` and by test doubles, which preserves the domain's independence
  from the ports package). No clock, no storage, no gateway.
- `internal/routing` holds the orchestration. It depends only on
  `internal/domain` and `internal/ports`, plus two small ports of its own:
  `HealthStore` (Get/Save/List) and `SpendingLedger`
  (Reserve/Reconcile/Release). Both may be absent or doubled in tests.

### Selection and failover (domain policies)

- `TieredCandidates` is tiers of `RouteCandidate` (a route plus an account).
  Callers build each tier from the catalogue and a health snapshot *before* these
  policies run.
- `FirstEligibleTier` returns the earliest non-empty tier.
- `SelectCandidate` draws a route **uniformly among the distinct eligible
  routes**, then an account uniformly among that route's accounts. Candidates are
  sorted by ID first, so the injected random source is the only source of
  variation and a deterministic test reproduces the choice exactly. This is the
  mechanism that satisfies PLAN 8.2: the route draw cannot be weighted by
  account count or key count.
- `DistinctRoutes` removes duplicates so an explicit and an automatic entry naming
  the same route contribute one weight (PLAN 8.3 step 6).
- `EnsureSelection` returns the pinned candidate when it is still eligible and
  otherwise selects from the first eligible tier. A recovered earlier tier never
  migrates a healthy pin.
- `NextFailoverCandidate` implements the fixed order: untried accounts of the same
  route in the same tier, then other routes of that tier in random order, then
  later tiers in order. It never returns a combination present in `tried` and
  never advances past a tier that still has an untried combination. The identity
  used for that check is `candidateKey{route, account}`, which **deliberately
  excludes key references** — keys belong to accounts and do not create new
  combinations, so they can neither multiply a probability nor extend a retry
  loop.
- `GroupAccountsByQuota` groups accounts by quota group (ungrouped accounts kept
  separate, output ordered by group then account for reproducibility), and
  `RepresentativeAccounts` returns one enabled account per non-empty group plus
  every enabled ungrouped account, so a shared quota is counted once at selection
  time while the remaining grouped accounts stay available for failover.
- `BackoffDelayMs` gives `base * 2^attempt`, capped at `max`, perturbed by up to
  `jitterPermille/1000` of itself in either direction and clamped back into range.
  A non-positive base or max yields 0.

### Health (domain records and classification)

- `HealthRecord` carries route, optional account, scope, state, failure class and
  message, consecutive failures, last failure, last success, next probe time,
  cooldown-until, provider `Retry-After`, probe count, and updated-at. `HealthKey`
  is `{RouteID, AccountID, Scope}`; the scopes the router creates are
  `credential`, `account`, and `route`.
- `HealthTransitions` is the explicit transition table. Quarantined records may
  return to probe-eligible, which is the operator/probe recovery path; no other
  state can skip the sequence.
- `RecordFailure` maps class to state and cooldown: invalid credential →
  cooling-down for 5 minutes; quota exhausted or rate limited → cooling-down for
  10 minutes; model not found → **quarantined**; provider outage or network
  timeout → cooling-down for 30 seconds; request-scoped classes
  (context-too-long, invalid arguments, invalid response, content rejected) and
  unknown errors → no state change until three consecutive failures. An
  authoritative `Retry-After` overrides the default for the credential and quota
  cases. `MaxCooldownMs` (24 h) is declared as the ceiling.
- `IsHealthNeutral` covers user cancellation and concurrent world conflict: they
  abort or re-evaluate the turn and must not count as consecutive failures or cool
  any scope. `IsRequestScopedFailure` marks the classes that describe the request
  rather than the route's availability.
- `ProbeDue` admits a half-open record only — cooling-down with an expired
  cooldown, or probe-eligible with `NextProbeAt` arrived. `AdmitProbe` admits at
  most one probe for a key: the first call moves the record to probing, which is
  no longer half-open, so the second call for the same record returns false.
- `SelectableForNewSession` is the admission rule for selection: healthy and
  half-open records are usable, closed and quarantined records are not.

### Orchestration (`internal/routing`)

- `Binding{Tier, Route, Account}` is the session-affinity pin, held in an
  in-memory table keyed by `SessionID`. `EnsureBinding` returns the pin when
  `combinationEligible` still holds; otherwise it walks enabled tiers in order,
  skips tiers with no eligible route, draws uniformly among distinct healthy
  routes, then uniformly among that route's eligible accounts, and pins the result.
- Eligibility of an account requires all of: enabled, permitted for the route's
  product (when a product list is configured), not blocked by an exhausted quota
  group, and healthy at **both** the account scope and the credential scope.
  Requiring both is what stops a single revoked key from stranding an account
  whose remaining keys work, and it never touches unrelated accounts or the route
  record. A missing health record means "no known problem", i.e. healthy;
  quarantined is never selectable; a cooling record whose cooldown expired is
  half-open and selectable.
- `ExecuteTurn` starts at the pinned tier, walks tiers in order, and within a tier
  iterates shuffled routes and shuffled accounts, skipping combinations already
  tried. It enforces `MaxAttempts` and `TurnDeadlineMs` and returns
  `ErrTurnExhausted` — a temporary simulated service error — rather than skipping
  ahead, so tier order is never bypassed. On the first success it runs
  `TurnRequest.Commit` at most once and then re-pins the session. Messages and
  tool definitions are copied by value into each attempt, so a failed attempt
  commits nothing and cannot double-apply a mutation. Cancellation and world
  conflict are fatal for the turn and never touch health.
- Each attempt runs under its own deadline context. For a **paid** route with a
  known estimate, the router reserves the estimated maximum before the request,
  reconciles with the reported usage afterwards, and releases on failure. When the
  operator configured no trustworthy estimate, nothing is reserved and
  reconciliation leaves the cost labelled unknown — an explicit label rather than
  an assumed zero. No paid route is ever added automatically: only configured
  tiers are eligible.
- Failures update the **narrowest evidenced scope**: invalid credential → the
  credential record; quota exhausted or rate limited → the account record plus a
  quota-group block until the reset time (or `Retry-After`); everything else → the
  route record. Cancellation and world conflict return immediately.
- `RunDueProbes` scans the store, admits a due half-open key, and enforces
  `ProbeBudget.MaxProbesPerHour` against a rolling one-hour window of probe start
  timestamps. A probe is a small fixed request with its own 5-second deadline
  (`ProbeDeadlineMs`); the result returns the key to healthy or re-cools it for one
  minute. `RecordProbeStart` enforces one probe per key. Probes are never
  continuous: only half-open keys are scanned, and the hourly budget stops the run.
- Attempt IDs are derived from a per-turn sequence encoded in a base-32 alphabet,
  so they are stable and regex-valid and tests never depend on real randomness.
- Concurrency is explicit: the binding table, quota-group blocks, and probe
  timestamps are guarded by one mutex; gateway, health, and ledger I/O happens
  outside it; health records are mutated only by this router.

### Domain policies on the router's execution path (resolved 2026-10-04)

`internal/routing` now delegates every selection, failover-order,
health-admission, and quota-group decision to the pure `internal/domain`
policies, feeding them a `TieredCandidates` snapshot built once per request from
the configured tiers and the health snapshot. `EnsureBinding` uses
`FirstEligibleTier` + `SelectCandidate`; `ExecuteTurn` uses
`NextFailoverCandidate`; `RunDueProbes` uses `ProbeDue`; `runProbe` uses
`AdmitProbe`; `recordFailure` uses `IsHealthNeutral`; `eligibleAccounts` uses
`GroupAccountsByQuota` (commit `5b69784`). The duplicated `shuffle`,
`routeAccount`, `probeDue`, `defaultCooldownFor`, and `eligibleRoutes` helpers
were removed.

Two behaviour changes accompanied the refactor and are recorded here rather than
silently accepted:

- `ExecuteTurn`'s first attempt is now the first configured eligible combination
  at the session's pinned tier, not a fresh random draw. PLAN 8.4 requires the
  uniform draw when the session's route and account are *chosen*, which
  `SelectCandidate` performs at connect; a turn then runs on its pinned tier and
  only a failure moves it. The old code re-drew randomly on every turn, so the
  pin was effectively a tier hint either way.
- Eligibility is snapshotted once per request. `NextFailoverCandidate` needs the
  full tier list up front (the end state this section previously described);
  re-reading accounts between attempts is not possible because the route cools
  after a failed attempt and the second attempt would vanish.

`BackoffDelayMs` and `RepresentativeAccounts` remain deliberately unwired: the
router has no retry sleep, and selecting through representatives would remove
sibling accounts from failover — a behaviour change, not a refactor.

## Alternatives considered

- **Select per key rather than per account.** Rejected. It would weight a route by
  its total key count, which PLAN 8.2 forbids, and would let three keys in one
  exhausted quota group produce three identical retries. `candidateKey` excluding
  key references and `RepresentativeAccounts` exist precisely to prevent this.
- **Deterministic first-eligible selection.** Rejected; PLAN 8.4 requires uniform
  selection, and a deterministic first entry would make one configured model
  permanently win. Sorting by ID *before* the draw preserves reproducibility
  without removing the randomness.
- **Round-robin failover instead of uniform random order.** Rejected: it would
  turn the configured tier list into a hard preference ranking and reintroduce
  positional weighting.
- **Migrating a healthy session back to an earlier tier when it recovers.**
  Rejected by PLAN 8.4 — it churns context for no benefit. Recovery makes the route
  eligible for *future* selections; the pin holds until a failure forces a change.
- **One health record per route.** Rejected. A single revoked credential would
  disable a route that other accounts can still serve, and a quota exhaustion on
  one account would quarantine a shared route. Credential, account, and route
  scopes are the minimum that keeps the blast radius at the narrowest evidenced
  level.
- **Treating every provider error as a route failure.** Rejected by PLAN 9.1.
  Context-too-long must trigger context repair rather than disabling a healthy
  model, content rejection is not a quota failure, and cancellation or a world
  conflict carries no health penalty at all.
- **In-memory health state only.** Rejected: PLAN 9.2 requires persisted failure
  reason, scope, next probe time, and last success so a restart recovers a cooldown
  rather than forgetting it. Durable wall-clock timestamps in the record make that
  possible; the `HealthStore` port keeps the adapter a separate decision.
- **Continuous background health checks, or probing every model.** Rejected by
  PLAN 9.2 — it would create unbounded paid traffic. Probes are restricted to
  half-open keys and capped per hour, and prefer a due candidate during a real
  request.
- **Caching health inside the router's own map instead of a port.** Rejected for
  the same reason as in-memory-only state: it would make restart recovery
  impossible and would couple recovery to process lifetime.
- **Spending limits as a hard refusal rather than a reservation.** Rejected:
  parallel requests would each see the same remaining budget. Reserve-then-
  reconcile-then-release is the model that actually bounds concurrent spend.

## Consequences

- Selection, failover, backoff, classification, and probe admission are all
  unit-testable with an injected clock and random source and a test health store.
  No live provider, no API key, no network.
- Only configured tiers make a route — including a paid one — eligible, and a paid
  route always goes through a reservation. The router never invents a fallback.
- A quota-group block is a **shared, coarse gate**: an account in an exhausted
  group is skipped for every route, not only the one that reported the quota
  exhaustion. That is correct for account-level quota, and it also means a free
  route sharing that group is skipped, which is the conservative direction.
- The attempt budget and turn deadline can end a turn while a later tier is still
  nominally eligible. That is deliberate: PLAN 8.4 prefers a temporary simulated
  service error over bypassing the configured tier order.
- A paid route with no configured estimate reserves nothing and therefore has no
  in-process ceiling. This is an operator obligation, not a solved problem.
- Health mutation is single-writer inside the router, and `HealthStore` is
  get-then-save, which is not atomic across processes. A multi-process deployment
  would need a stronger store; the current architecture is a single cohesive
  process.
- The binding table is in memory, so a restart re-pins sessions. Affinity is
  session-scoped, and PLAN 6.4 already starts a new connection with a fresh shell
  rather than reattaching to old foreground state.
- Because `RecordFailure` treats unknown errors as cooling only after three
  consecutive failures, a novel provider error degrades gracefully instead of
  quarantining a working route on first sight.
- Adding a provider or a protocol family does not touch this package: it
  implements `ports.ModelGateway`, consistent with ADR 0002 and ADR 0005.

## Evidence

Read for this record (documentation change only; no build or test run):

- `internal/domain/routing_policy.go`, `health_policy.go`, `routing.go`.
- `internal/routing/routing.go`, `health.go`, `ports.go`.
- `PLAN.md` sections 8.2–8.4 and 9.1–9.2, and section 9.3 for the reservation
  model referenced by the router.
- ADRs 0002 (port contracts) and 0005 (provider/product/protocol axes).
- Tests present in the tree (names read, **not executed** for this documentation
  task): `internal/domain/routing_policy_test.go` (24 tests, including
  `TestSelectionUniformOverRoutesIndependentOfAccountAndKeyCount`,
  `TestSelectionUniformAcrossAccountsWithinRoute`,
  `TestEnsureSelectionPreservesHealthyAffinity`,
  `TestNextFailoverExhaustsTierBeforeAdvancing`,
  `TestNextFailoverRefusesTriedCombination`,
  `TestQuotaGroupingAvoidsDoubleCounting`,
  `TestHealthNeutralFailuresNeverDegrade`, `TestRetryAfterSetsCooldown`,
  `TestProbeHalfOpenAdmitsOneProbe`, `TestProbeOutcomesRecoverAndReCool`,
  `TestBackoffDelayCappedAndExponential`, `TestBackoffJitterStaysWithinBounds`);
  `internal/domain/domain_test.go` (`TestHealthTransitions`);
  `internal/routing/routing_test.go` (9 tests, including
  `TestUniformSelectionIndependentOfKeyCount`, `TestTierExhaustionOrder`,
  `TestRevokedKeyIsolation`, `TestCooldownResetProbe`, `TestBudgetReservation`,
  `TestBudgetReleaseOnFailure`, `TestAttemptBudgetExhaustion`,
  `TestNoDuplicateCommitAndContextPreserved`).
- Commits: `279bff4` "Add pure routing and health domain policies with
  deterministic tests", merged as `f5a4a77` "Merge B03 routing/health domain
  policies"; `95d7a97` "Add C06 model routing/recovery orchestration in
  internal/routing" plus `dfa99cc`, merged as `ac1fbb2` "Merge C06 model
  routing/recovery orchestration".

## Unverified items and limitations

- **The domain policies are on the router's execution path** as of
  `5b69784` (see the section above); the former duplication is removed. The two
  behaviour changes that accompanied it are recorded there.
- **No authenticated inference was ever performed.** Every cooldown, quota,
  `Retry-After`, and probe-success behaviour is proven only against a fake gateway
  and a deterministic clock. Live streaming, error-envelope shapes, and real
  `Retry-After` values are unverified, consistent with ADR 0005.
- **No production `HealthStore` or `SpendingLedger` implementation exists in the
  tree.** The only `HealthStore` implementation found is the in-memory
  `memHealthStore` test double, and `routing.NewRouter` has no production caller
  yet. Durability of health records across a restart, and any real spending
  enforcement, are therefore unproven.
- **Probe scheduling is pull-based.** `RunDueProbes` must be called; no scheduler
  loop drives it, and `ProbeBudget.ProbeIntervalMs` and
  `ProbeBudget.MaxConcurrentProbes` are not enforced by the code read. Only
  `MaxProbesPerHour` is applied.
- **`BackoffDelayMs` is unused by the router.** Per-attempt work is currently
  bounded by the attempt budget and the turn deadline rather than by a sleep
  between retries, so the "exponential backoff with jitter and a maximum"
  requirement is provided as a policy but not yet applied. Provider-supplied
  `Retry-After` is recorded and honoured only as a cooldown on the health record
  and quota-group block, not as a wait inside a turn.
- **`MaxCooldownMs` (24 h) is not enforced.** A provider-supplied `Retry-After`
  longer than the ceiling would be taken verbatim into `CooldownUntil`.
- **Shared-outage suppression is incomplete.** `product` and `provider` scopes
  exist in the domain's scope vocabulary, but the router only creates
  `credential`, `account`, and `route` records, so a Console-wide outage is
  handled per route rather than once per product.
- **Catalogue refresh on `model_not_found` is not implemented here.** PLAN 9.1
  requires a refresh plus quarantine; the router quarantines and leaves refresh to
  the catalogue layer.
- The router holds no state about which routes were tried across *turns* — the
  `tried` set is per turn — so a persistently failing route in a lower tier can be
  re-attempted on a later turn while its health record is cooling. That is
  intentional (recovery must eventually be observable) but it means "avoid retry
  loops" is enforced within a turn, not across turns.
- Open questions: whether `TierConfig.MinHealthy` and `RoutePolicy.TurnDeadlineMs`
  defaults have documented operational values; whether `Account.PermittedProducts`
  should be authoritative or advisory when an account is eligible for a product
  it has never used; and whether the quota-group block should also be persisted,
  since it currently lives only in the router's memory and is therefore lost on
  restart while the account-scope health record survives.
