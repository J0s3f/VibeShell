# Unwired capabilities audit (2026-10-04)

Read-only inventory of capabilities that are implemented and tested but not
wired into the running product (`cmd/vibeshell`), plus partial wiring and inert
configuration. Observations only; line numbers were valid at the time of the
audit and shift as the tree changes.

Caveat: the tree was being modified during the audit (the world shell moved from
unwired to wired mid-audit); re-verify before acting.

## Pre-tracked examples

| Tracked example | Verdict | Evidence |
| --- | --- | --- |
| `internal/simulation` world/tool layer unwired | Confirmed, still unwired | `cmd/vibeshell/component.go` `Tools: rejectTools{}`; zero importers of the package. |
| Durable app-state / route-health / spending vs in-memory maps | Partially resolved: route-health, spending, and app artifacts/pointer are wired; app session-state and pins remain in-memory | Wired in `cmd/vibeshell/durable.go`; `internal/apps/service.go` still uses maps. |
| Suitable-free discovery (`internal/adapters/opencode/catalog.go`) | Confirmed, still unwired | Only test/experiment callers; `buildRoutingConfig` documents the omission. |

## Master inventory

| # | Capability | Implemented at | Wiring gap | Effort |
| --- | --- | --- | --- | --- |
| A1 | Simulated-world agent tool layer + bounded context assembly + summarization | `internal/simulation/*` | Resolved 2026-10-04 (`798a1a6`): `simulation.ToolAdapter` wires the registry behind `application.ToolExecutor`, and the generation engine requests a `world.list` context batch through it. | done |
| A2 | Declarative interactive primitives (editor/pager/monitor) | `internal/interactions/*` | Partially resolved: rendered frames now reach the client (B4, `9f628cf`) and a generated app can run a **line-based interactive** session — it stays foreground with its own prompt, receives each submitted line, and exits back to the shell (2026-10-05, `c584c38`/`e8338fe`). Routing raw input keys to an app machine (full-screen TUI) remains. | M |
| A3 | Suitable-free discovery, protocol classification, catalogue cache | `internal/adapters/opencode/catalog.go` | Resolved 2026-10-04 (`e2494fe`): `internal/discovery.Planner` expands `auto_free` tiers, wired in `buildRoutingConfig`. | done |
| A4 | Baseline seed description (GNU/Hurd home tree) | `internal/presentation/baseline.go` | No caller of `DefaultBaselineSeed()`; no sqlite `EnsureTree` seeder | M |
| A5 | Operational metrics registry | `internal/observability/metrics.go` | Resolved 2026-10-04 (`49f8d80`): the request-logging gateway populates model-request counters and shutdown records the populated set. | done |
| A6 | Pure domain routing/health policies | `internal/domain/{routing_policy,health_policy}.go` | Resolved 2026-10-04 (`5b69784`): `internal/routing` now delegates to them (see ADR 0011; two behaviour changes recorded there). `BackoffDelayMs`/`RepresentativeAccounts` remain unwired by design. | done |
| B1 | Durable app session-state, pins, activation log | `internal/adapters/sqlite/apppersist.go` | Resolved 2026-10-04 (`99ded98`): `ports.AppStateStore` wired into `apps.NewService`; the command index is seeded from the durable map at startup. | done |
| B2 | Router due-probe execution | `internal/routing/health.go` `RunDueProbes` | `admin.Deps.Probes` never set; `admin probe run` always unavailable | S |
| B3 | Admin app rollback | `internal/admin/bindings.go`; `apps.Service.Rollback` | Resolved 2026-10-04 (`fe40975`): the CLI admin builds the durable app registry and an app service over it. | done |
| B4 | Renderer frame/content delivery to SSH client | `internal/terminal/renderer/*`; `turnflow.go` | Resolved 2026-10-04 (`9f628cf`): the SSH handler fetches `OutputFrame`/`OutputContent` bytes and writes them. | done |
| B5 | Non-MOTD trusted prompt slots | `internal/presentation/prompt.go` | Resolved 2026-10-04 (`1e43934`): `prompts.app_generation` is rendered and prepended to the built-in artifact contract. Other slots remain unwired. | partial |
| C1 | Spending ledger | `sqlite/spending.go`; `internal/routing` | Wired but inert: `Paid:false` always, no `EstimateMaxCost` | M |
| C2 | Exports config group | `config/validate.go` | `buildExporter` hardcodes options; `cfg.Exports` unread | S |
| C3 | Terminal config group | `config/config.go` | Resolved 2026-10-04 (`0e61c7f`): `terminal.max_paste_bytes` bounds the input decoder. | done |
| C4 | Apps resource limits | `config/config.go` | Resolved 2026-10-04 (`0e61c7f`): `apps.execution_deadline_ms`/`heap_limit_bytes` bound the sandbox run. Other `apps` fields remain unwired. | partial |
| C5 | Discovery config group | `config/validate.go` | Never read; tied to A3 | M |

## Group A — fully implemented, completely unwired

### A1. `internal/simulation` tool/context/summary layer (highest value)
- `NewRegistry` registers 11 tools (`world.lookup`, `world.list`, `content.read`,
  `fact.lookup`, `world.stage`, `world.materialize`, `history.search`,
  `history.context`, `interaction.propose`, `app.lookup`, `app.candidate`), all
  with handlers and tests.
- `ContextAssembler.Assemble` implements budgeted, scoped, redacted context with
  summarization; `ModelSummarizer` and `DefaultRedactor` are production-shaped.
- `simulation.Registry.Execute(ctx, CallContext, ToolCall)` is
  signature-incompatible with `application.ToolExecutor.Execute(ctx, ToolBatch)`,
  so a small adapter is also missing.
- `GenerationLoop` is implemented but untested and unused; `cmd/vibeshell` has its
  own generation path.

### A2. `internal/interactions`
Full declarative primitive machine (spec parse, buffer/search/scroll/mode/status,
frame, hermetic AST guard), 30+ tests, no importers. The SSH `readLoop` owns the
editor locally and never hands keys to an app machine; frames are dropped at the
transport (see B4).

### A3. `internal/adapters/opencode/catalog.go`
`SuitableFree`, `ExpandFree`, `EndpointForDecision`, `ProtocolFor`, and `Cache`
are exercised only by tests. `buildRoutingConfig` documents the omission;
`tierConfig.AutoFree` is copied but never expanded.

### A4. `presentation.DefaultBaselineSeed`
A versioned directory seed with `Validate`, tested, no caller. Directly relevant
to the simulated-world seeding work: the seeder should consume it.

### A5. `observability` metrics
Allow-listed metric set, registry, counters/gauges, `Snapshot`; only test
callers; `cmd/vibeshell` builds only `ReadinessReporter` and `Logger`.

### A6. Pure domain routing/health policies
`EnsureSelection`, `SelectCandidate`, `NextFailoverCandidate`, `DistinctRoutes`,
`RepresentativeAccounts`, `GroupAccountsByQuota`, `BackoffDelayMs`,
`FirstEligibleTier`, `ProbeDue`, `AdmitProbe`, `SelectableForNewSession`,
`IsRequestScopedFailure` are tested but not called from `internal/routing`
(`domain.AdmitProbe` is used by admin). ADR 0011 records this as a deliberate
follow-up.

## Group B — implemented but partially wired / stubbed at the boundary

- **B1** Durable app session-state/pins: `sqlite.Apps` implements the operations
  and tests them, but `apps.Service` keeps maps and `NewService` has no state-store
  parameter; a restart loses app state/pins while artifacts persist.
- **B2** Router due-probe execution: `Router.RunDueProbes` is implemented and
  tested, but `admin.Deps.Probes` is never populated, so `admin probe run` always
  reports unavailable.
- **B3** Admin app rollback: the binding and `Rollback` are tested, but the
  reachable CLI service omits `Apps`.
- **B4** Renderer frames dropped at SSH: the coordinator renders views/content,
  but `sshHandler.writeOutput` handles only `OutputText`/`OutputPrompt` and drops
  the rest as `WriteDropped`.
- **B5** Non-MOTD prompt slots: seven prompt files validate, only `motd` is read;
  the generation prompt is hardcoded.

## Group C — implemented but inert / behind unset config

- **C1** Spending ledger wired but never exercised (`Paid:false`, no estimator).
- **C2–C5** `exports`, `terminal`, `apps`, and `discovery` config groups are
  validated but not consumed; sandbox run limits and editor limits are hardcoded.

## Verified as genuinely wired (negative results)

`internal/adapters/{config,opencode,opencodecli,sandbox,sqlite,ssh}`,
`internal/admin` (storage/password/backup/export paths), `internal/application`,
`internal/apps` (artifact lifecycle), `internal/buildinfo`, `internal/domain`
(types; policy functions excepted), `internal/export`, `internal/inference`,
`internal/observability` (logger/readiness), `internal/ports`,
`internal/presentation` (identity/MOTD/sanitize), `internal/provider`,
`internal/routing` (the router itself), `internal/system`,
`internal/terminal/{editor,input,screen,renderer}`, and the
`cmd/vibeshell` world shell.

## Ranked by product value if wired

1. A1 — agent tool layer, bounded context assembly, summarization/redaction.
2. B1 — durable app state/pins (silent state loss across restart).
3. A2 + B4 — interactive generated-app UI and its transport delivery.
4. A3 — suitable-free discovery / `auto_free` expansion.
5. B2 + B3 — admin probe execution and app rollback.
6. C1 — spending limits (safety control currently inert).
7. B5 — configured prompt slots.
8. A4 — baseline world seed (blocked on a seeder).
9. A5 — operational metrics.
10. A6 — domain policy reuse (maintainability).
11. C2–C5 — config groups with no effect.
