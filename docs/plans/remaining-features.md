# Plan: finish the unwired features

Status: complete for the tractable workstreams (2026-10-04).

Wave 1, merged and wired: W1 simulation tools (`798a1a6`), W2 durable apps
(`929f8a2`/`99ded98`), W3 domain-policy routing (`5b69784`), W4 discovery
(`1b7740d`/`e2494fe`).

Wave 2, main-agent work: W8 world write commands and environment (`73e8644`),
AI materialization (`579af51`, fixed by `bcf91ff`), W9 repair budget
(`93ac1ab`), W6 apps/terminal bounds (`0e61c7f`), W7 prompt slots
(`1e43934`), admin app rollback (`fe40975`), metrics (`49f8d80`), frame
delivery (`9f628cf`).

Deliberately not done, with reasons:
- **W5 interactive input routing (A2)**: rendered frames now reach the client,
  but routing keystrokes to an `internal/interactions` machine needs a
  foreground-app input loop, and generated apps are text-mode today.
- **B2 admin due-probe runner**: needs the gateway and credentials in the
  separate CLI admin process.
- **C1 spending**: needs per-model pricing data to estimate cost; the ledger is
  wired but never reserves.
- **C2 exports max bytes**: `export.Options` has no size bound to enforce it.

Source inventories: `docs/research/unwired-capabilities.md` (audit) and the
"Not yet implemented / unverified" table in `FEATURES.md`.

## Rule for this effort

Subagents own packages, not the composition root. Every feature is wired into
`cmd/vibeshell` by the main agent, and **no subagent handoff is merged until the
main agent has verified the code compiles and is actually reachable from the
running app** (the audit's recurring failure was implemented-but-unwired code).
Conflicting composition files (`component.go`, `generation.go`, `builders.go`,
`sshhandler.go`, `routingconfig.go`, `admin.go`, `world.go`) are owned by the
main agent only.

## Workstreams

| WS | Feature (audit id) | Owns | Depends on | Wave |
| --- | --- | --- | --- | --- |
| W1 | Simulation tool/context layer behind `application.ToolExecutor` (A1) | `internal/simulation` | — | 1 |
| W2 | Durable app session-state/pins + durable command index (B1) | `internal/apps`, `internal/ports`, `internal/adapters/sqlite` | — | 1 |
| W3 | Routing uses the pure domain policies instead of duplicating them (A6) | `internal/routing` | — | 1 |
| W4 | Suitable-free discovery / `auto_free` expansion (A3) | `internal/discovery` (new) | — | 1 |
| W5 | Interactive primitives + frame/content delivery to SSH (A2, B4) | `internal/interactions` | — | 2 |
| W6 | Consume the `exports`, `terminal`, `apps`, `discovery` config groups; activate the spending ledger (C1–C5) | main | W4 | 2 |
| W7 | Admin due-probe runner, admin app rollback, metrics transport, prompt slots (B2, B3, A5, B5) | main | — | 2 |
| W8 | AI materialization of a missing path; write commands; environment (`$HOME`/`$PWD`) | main | W1 | 2 |
| W9 | Generation repair loop (`max_repairs`) | main | — | 2 |

The main agent owns all composition wiring for every WS, plus W6–W9.

## Contracts frozen before wave 1

### W1 — `internal/simulation`
```go
// ToolAdapter adapts the server-owned tool registry to the application's
// ToolExecutor seam. The namespace resolver and policy come from trusted
// application context, never from the model.
type ToolAdapter struct {
    Registry   *Registry
    Namespaces NamespaceResolver // resolves session/user/shared/baseline IDs
    Clock      ports.Clock
}
func (a *ToolAdapter) Execute(ctx context.Context, batch application.ToolBatch) (application.ToolBatchResult, error)
```
`NamespaceResolver` maps a principal/session to `CallNamespaces`. The adapter
builds `CallContext` from `batch.Request` (Session, Principal, Turn, Attempt,
Context.CWD, Snapshot.ScopePolicy, now).

### W2 — `internal/apps` + a new port
```go
// ports.AppStateStore persists generated-app session state, pins, and the
// command-name index durably.
type AppStateStore interface {
    SaveSessionState(ctx context.Context, req apps.SaveStateRequest) error
    SessionPin(ctx context.Context, session domain.SessionID, app domain.AppID) (apps.Pin, bool, error)
    PinSession(ctx context.Context, pin apps.Pin) error
    ReleaseSession(ctx context.Context, session domain.SessionID, app domain.AppID) error
    CommandIndex(ctx context.Context) (map[string]domain.AppID, error)
    RecordCommand(ctx context.Context, name string, app domain.AppID) error
}
```
`apps.NewService` takes the store (nil keeps the in-memory behavior for tests).

### W3 — `internal/routing`
Replace the in-package policy copies with calls to `internal/domain`
(`SelectCandidate`, `NextFailoverCandidate`, `GroupAccountsByQuota`,
`BackoffDelayMs`, `FirstEligibleTier`, `ProbeDue`, `AdmitProbe`, …). Behavior
must not change: the existing `internal/routing` tests are the acceptance gate.

### W4 — `internal/discovery` (new)
A service that, given the configured tiers and the OpenCode catalogue cache,
expands `auto_free` tiers into concrete suitable-free routes with explicit
decision reasons. It is pure over an injected catalogue port; the composition
wires it into `buildRoutingConfig`.

## Acceptance for every WS
`gofmt` clean, `go build ./...`, `go vet`, package tests (`-race` where the
package is concurrent), and — before merge — a main-agent check that the new
code is reachable from `cmd/vibeshell` (grep for the constructor/import) and a
combined `go test ./...`.
