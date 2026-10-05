# ADR 0006: World scopes, path resolution, and permission semantics

Status: accepted, with the flagged ambiguities left open. The resolution,
overlay, and permission semantics below were qualified by tests; the questions
the spike referred onward were not decided here and are listed under
"Unverified items and limitations".

Date: 2026-10-03.

## Context

PLAN 5.2-5.3 requires deterministic path resolution from session cwd across
three durable layers (baseline, shared, user), Unix-like permission facts, a
hard sharing switch enforced at the tool boundary, and conflict-free concurrent
materialization. These rules are product-visible — a user sees a path, a
permission denial, or a "someone else created this" outcome — so they must be
decided, not discovered during implementation.

Task A05's world-paths spike qualified the semantics against
`internal/domain` in a nested module with injected IDs and timestamps. No clock,
randomness, or filesystem access was involved.

## Decision

**Scope precedence.** Three layers resolve as user > shared > baseline. Within a
layer, either a live entry or a tombstone is present; a tombstone hides lower
layers without erasing their bytes.

**Deletion and re-materialization.** A delete removes a path from the current
view only. Re-materializing a hidden path is a **new** creation with a fresh ID
and initial revision, never a resurrection of the old node. Historical events
retain the deletion and the original object.

**Containment.** Resolution is absolute or relative to session cwd, normalized
for `.` and `..`, with `..` above `/` clamping at `/`. Symlink targets — absolute,
relative, `..`-containing, self-referential — re-enter the component stack with a
depth bound of 40 and loop errors otherwise. Output is always a valid domain
path, never a host path.

**Validation.** Reject empty input, NUL, any C0/C1 control byte or DEL, invalid
UTF-8, empty components after `//` collapse, storable names of `.` or `..`, and
overruns of `MaxPathLen` (4096) or `MaxNameLen` (255).

**Unicode.** Names are byte-exact keys with no normalization. `U+00E9` and
`U+0065 U+0301` are distinct and both valid. Display may normalize; storage keys
must not.

**Permissions.** Unix dispatch is owner, then group (effective or supplementary),
then other. A simulated root (EUID 0) bypasses read/write and needs any execute
bit to execute. Simulated root is never a real host privilege and **never**
bypasses the scope-policy gate.

**Scope policy precedes permissions.** The trusted `ScopePolicy` is checked first;
with sharing disabled, cross-scope reads and writes are refused regardless of
mode bits or simulated root. Scope filters come from trusted application context,
never from model-supplied identifiers.

**Concurrency.** The first creator wins only if it presents absence plus the
current parent-directory membership revision. Every other outcome is a typed
conflict requiring a rebase onto the winner's ID and revision.

**Determinism.** Path resolution is a pure function of cwd, the effective view,
and the input. `cd` is session-local state and a function argument; parallel
sessions' cwd never overwrite one another.

## Alternatives considered

- Keying layers by path string: rejected for the real store. The spike's `View`
  used path strings, but the durable store keys by `(namespace, parent, name)`
  with opaque IDs, which also makes rename a real operation.
- Normalizing names to NFC for stable keys: rejected. It would silently collapse
  two distinct names a user created, which is exactly what PLAN 4.1 forbids for
  usernames and is equally wrong for filenames.
- Using the host filesystem for world storage: rejected by the project's
  simulation rules; the world is not the container.
- Letting simulated root bypass scope policy for convenience: rejected. It would
  make the hard sharing switch advisory.
- `..` above root as an error rather than a clamp: rejected in favor of clamping,
  matching the Unix-like presentation; confirmation was requested from the main
  agent and is recorded under open items.

## Consequences

- Every read that could justify a later write must be expressible as a read
  dependency, including directory membership and absence.
- Deleting does not reclaim storage; content and history both accumulate, which
  is consistent with PLAN 10.3's prohibition on automatic expiry but leaves the
  `contents` eviction question open (ADR 0004).
- Three layers multiply the cases a permission or conflict bug can hide in. The
  qualified test set is the guard, and it is small; it must grow with the real
  store.
- Validation is deliberately strict and rejects inputs a real filesystem would
  accept, so the simulated machine will occasionally refuse a name that would work
  on Linux. That is a visible, intentional difference.

## Evidence

- Research: `docs/research/2026-10-03-world-paths-spike.md`.
- Receipt: `experiments/world-paths/receipts/go-test-race.log` — 13 tests PASS
  under `go test -race ./...`, 1.021 s.
- Tests cover: determinism, overlay precedence, tombstone deletion and
  re-materialization, shared tombstone, the Unix permission table, sharing-off
  denial, absolute/relative resolution, root containment, symlink loop and
  depth, bad-input rejection, Unicode distinctness, concurrent first
  materialization, and stale-listing rejection.
- Commits: `29f3a79` (spike), merged as `27e49f6`.

## Unverified items and limitations

- The spike's commit model (`SimStore`) is **in-memory only**. Real atomicity,
  FTS, and index behavior belongs to the SQLite adapter and is not proven here
  (see ADR 0004).
- The spike's `View` keys are path strings, unlike the real store's
  `(namespace, parent, name)` opaque IDs. Overlay semantics are proven in the
  spike's model, not against the production keying.
- `domain.ParsePath` currently rejects NUL, `.`, `..`, and length overruns but
  **not** other control bytes; the spike enforced a stricter boundary at its own
  edge. Adopting the stricter rule in domain validation versus at the adapter
  edge is **not decided here**.
- There is no domain-level `Resolve(cwd, input)` or symlink-aware resolver; the
  spike's resolver is the proposed semantic, pending promotion.
- Whether tombstone ownership is `MutationTombstone` or `MutationDelete` needs
  the persistence owner's confirmation.
- The simulated-root "any execute bit" rule follows POSIX and is unconfirmed for
  V1.
- **Clamping `..` above `/` is the spike's chosen behavior, not a confirmed
  product decision.** The spike implemented the clamp and flagged it for
  confirmation against erroring instead; it is recorded as the decision here
  because it matches the Unix-like presentation, but it has not been ratified.
- Open items referred to the main agent: home-path encoding for unusual
  usernames (`/home/<user>` display vs storage key), shared-scope tombstone
  authorship and provenance fields, and the exact directory-membership
  `ReadDependency` shape for nested parents (the spike tracked only the immediate
  parent).