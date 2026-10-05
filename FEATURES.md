# FEATURES — Verified Behavior

This file lists only behavior that is **actually shipped and traceable** to
merged code, schemas, or test receipts in this repository (baseline commit
`91c7beb`, "Merge D05 failure/research acceptance suite"). Items that are
planned or unverified are in the clearly separated
["Not yet implemented / unverified"](#not-yet-implemented--unverified) section
and must not be presented as product features.

Test receipts referenced below: `tests/integration/receipts/` (`go-test-all.log`
— every package `ok` under `go test -race -count=1 ./...`; and
`go-test-integration.log` — the D05 suite PASS).

## Foundation and toolchain

| Feature | Source |
| --- | --- |
| Content-addressed dev image keyed by `containers/Containerfile` + `containers/versions.env` hash | `scripts/dev.ps1`; `containers/versions.env`; `containers/Containerfile` |
| `scripts/dev.ps1` commands: `image`, `start`, `stop`, `exec`, `smoke`, `test`, `build`, `status`, `clean` | `scripts/dev.ps1`; `docs/development.md` |
| Per-agent worktree/container/volume/port isolation (`vibeshell-<agent>-*`) | `scripts/new-agent.ps1`; `scripts/dev.ps1`; `scripts/verify-foundation.ps1` |
| Acceptance test passes: image build, container start, Go smoke, two-agent isolation, state retention, separate SSH ports | `scripts/verify-foundation.ps1` |
| Container-only: nothing installed on host; Podman used from the start | `AGENTS.md`; `containers/Containerfile` |
| Runtime image stage: non-root user UID 10001, one `EXPOSE 2222`, `STOPSIGNAL SIGTERM`, CA roots only | `containers/Containerfile` (`runtime` stage) |
| Build identity stamped via linker flags (`version`, `revision`) and guarded for typos | `internal/buildinfo/buildinfo.go`; `internal/buildinfo/buildinfo_test.go` |

## Domain contracts and hexagonal boundaries

| Feature | Source |
| --- | --- |
| Dependencies point inward; `internal/domain` imports only the standard library (no I/O, clock, or randomness) | `internal/domain/contracts_guard_test.go`; `internal/domain/doc.go` |
| Identity types (UserID, SessionID, TurnID, AttemptID, EventID, ContentID, NodeID, NamespaceID, AppID, AppVersionID, RouteID, AccountID, KeyRef) with base32 Crockford patterns | `internal/domain/identity.go` |
| Scope types (Session/User/Shared/Baseline) with `AuthorizeRead`/`AuthorizeWrite` policy checks | `internal/domain/scope.go` |
| Failure classes and `ErrorEnvelope` with optional route/account references | `internal/domain/model.go` |
| Versioned JSON schemas (configuration, route-policy, app-abi, event-envelope, model-request-result, world-changeset), each with `version`/`$id` and `additionalProperties: false` | `schemas/*.json`; `schemas/fixtures/*.json` |
| Pure routing/health policies (tier selection, uniform route draw independent of key count, quota grouping, failover order, backoff, probe admission) with deterministic tests | `internal/domain/routing_policy.go`; `internal/domain/health_policy.go`; ADR 0011 |

## Configuration, secrets, and authentication

| Feature | Source |
| --- | --- |
| Strict versioned JSON config: unknown fields, duplicate keys, wrong kinds, trailing content, and missing required fields all rejected with JSON paths | `internal/adapters/config/{decode.go,config.go}`; ADR 0009 |
| Pure semantic validation (tiers, route references, protocol support, bounds); never makes an inference call | `internal/adapters/config/validate.go`; ADR 0009 |
| Secret **references** only: `{file:...}` (absolute, symlink-contained, `-secret-dir` fail-closed) and `{env:...}`; inline values rejected; `Secret` self-redacts | `internal/adapters/config/secrets.go`; ADR 0009 |
| Atomic reload: a failed load keeps the previous snapshot; a turn pins its snapshot | `internal/adapters/config/store.go`; ADR 0009 |
| Secure-mode password file: versioned JSON, PHC Argon2id, mode `0600`, dummy verification for unknown/disabled users, constant-time compare, bounded parameters | `internal/adapters/config/password.go`; ADR 0007 |
| Passwords read from a TTY/stdin, never an argv | `internal/adapters/config/password.go` (`ReadPassword`) |
| Public or secure SSH auth mode, explicitly required | `cmd/vibeshell/component.go`; `internal/adapters/ssh`; `internal/adapters/config/config.go` |

## Provider protocol and routing

| Feature | Source |
| --- | --- |
| Provider vs product vs wire protocol are separate axes; protocol classification **fails closed** | `internal/adapters/opencode/protocol.go`; ADR 0005 |
| Four protocol families behind one `ModelGateway`: Chat Completions, Responses, Messages, Gemini | `internal/adapters/opencode`; ADR 0005 |
| Multiple provider implementations behind one `ModelGateway`: a route-to-provider `Registry` dispatches per route and fails closed (`model_not_found`) when a route has no provider; `providers[].kind` selects the implementation (`http` default, `cli`) | `internal/provider`; `cmd/vibeshell/providers.go`; ADR 0013 |
| OpenCode CLI provider: runs `opencode run -m <provider>/<model> --format json`, collects the NDJSON text parts, and maps failures onto the canonical envelopes; reaches the Console free models the direct API rejects with `403 FreeTierError`, and the CLI owns its own credential | `internal/adapters/opencodecli`; ADR 0013 |
| Credential-less accounts for CLI providers: an account may omit `secret_ref` only when its non-empty `permitted_products` are all served by `cli`-kind providers; it participates in routing but is not registered with the HTTP gateway | `internal/adapters/config/validate.go`; `cmd/vibeshell/providers.go`; ADR 0013 |
| SSE framing treated as untrusted (split frames, multiple events per read, bounded bodies, dropped sentinel tolerated) | `internal/adapters/opencode`; ADR 0005 |
| Typed, redacted provider errors; HTTP 200 with a JSON error body is an error, never success | `internal/adapters/opencode`; ADR 0005 |
| Credential redaction in every diagnostic path (`sk-*`, `Bearer`, `api_key=`, `?key=`, `x-api-key`, `Authorization`) | `internal/adapters/opencode`; ADR 0005 |
| Suitable-free discovery: `-free` expansion, capability intersection, pricing check, allow/deny, explicit decision reasons | `internal/adapters/opencode/catalog.go`; `internal/discovery`; `cmd/vibeshell/discovery.go`; `docs/guides/model-routes.md` |
| `auto_free` tiers expand into suitable-free routes: the composition refreshes models.dev metadata and each product's catalogue (best effort, fail-closed) and mints a route per eligible model through `discovery.Planner` | `internal/discovery`; `cmd/vibeshell/{discovery.go,routingconfig.go}` |
| Session affinity: route/account pinned until failure; recovered earlier tier never migrates a healthy pin | `internal/routing`; `internal/domain`; ADR 0011 |
| Per-purpose model selection with response failover: a request declares a purpose (`motd`, `generation`), routes carry an optional purposes list, and `Router.Execute` tries each purpose-eligible route across tiers, cooling one whose response the caller rejects (empty welcome, unparseable artifact) before trying the next model | `internal/routing` (`Execute`); `internal/domain`; `internal/adapters/config`; `cmd/vibeshell`; ADR 0012 |
| Chain-of-thought blocks (` thinking`, `<thinking>`, `<reasoning>`) are stripped from decoded content for every protocol, so reasoning never reaches a consumer as the answer | `internal/adapters/opencode/reasoning.go` |
| Failure matrix with distinct health scopes (credential/account/route), Retry-After cooldown, quarantine, and health-neutral cancellation/conflict — verified against a fake gateway | `tests/integration` (`failure_matrix_test.go`); D05 receipt |

## Storage, world, and generated apps

| Feature | Source |
| --- | --- |
| SQLite (modernc.org/sqlite) with WAL, `synchronous=FULL`, single-writer bounded queue | `internal/adapters/sqlite`; ADR 0004 |
| Immutable content-addressed BLOBs; content and metadata commit atomically | `internal/adapters/sqlite`; ADR 0004 |
| Optimistic revision checks: world commits verify read dependencies and expected revisions; conflicts are typed and rebased | `internal/adapters/sqlite`; ADR 0004 |
| Storage-failure guard (PLAN 10.3): durable-write failures (read-only FS, disk full, I/O error, WAL/corruption) are classified distinctly from ordinary conflicts; once permanent recording fails, new semantic appends/commits/content writes are refused with the typed `recording_unavailable` unavailable error, reads of recorded history still work, and one successful durable probe clears the condition | `internal/adapters/sqlite/storagefail.go`; `internal/adapters/sqlite/storagefail_test.go` |
| QuickJS/Wasm guest embedded in wazero with a bounded JSON bridge; no host mounts, env, sockets, process execution, or native modules | `internal/adapters/sandbox`; `internal/adapters/sandbox/PROVENANCE.md`; ADR 0003 |
| Both guest-memory and JS-heap/stack limits, execution deadlines, bounded serialization/output, instance admission | `internal/adapters/sandbox`; ADR 0003 |
| Declarative interactive primitives bound to eight approved actions; no program-name universe (hermetic AST guard) | `internal/interactions`; ADR 0010 |
| Generated-app registry with immutable versions, validation-before-activation, and rollback; a failed candidate leaves the prior version intact | `internal/apps`; D05 `app_candidate_test.go` |
| Generated-app input event: the guest receives the app event with the payload's fields promoted to the top level, so a program reads `event.command`, `event.line` (the full submitted line), `event.args`, and `event.cwd` directly while `event.event_type` and the nested `event.payload` stay available; the generation prompt documents that shape | `internal/adapters/sandbox/appsandbox.go`; `internal/simulation/apprun.go`; `cmd/vibeshell/generation.go` |
| Line-based interactive generated apps: the model chooses one-shot or interactive at generation time; an interactive app returns `awaiting_input` with a short `prompt`, stays foreground across lines with its session state carried forward, and returns to the shell on `exited` or end of input; the app prompt is rendered as one sanitized printable line, the foreground app runs its pinned version, and the shell's own exit words do not end the session while an app is foreground | `internal/domain/apps.go`; `internal/application/{session,sessionstate,turnstate,sessionapi}.go`; `internal/apps/service.go` (`ResolvePinned`); `cmd/vibeshell/{generation.go,sshhandler.go}`; `schemas/app-abi.schema.json` |
| World-backed shell filesystem: `ls`, `cat`, `cd`, and `pwd` read the durable per-user simulated world; a fresh user namespace is seeded with the `presentation.DefaultBaselineSeed` skeleton and the home tree (conventional subdirectories and a `notes.txt`) through the sqlite `EnsureTree` seeder | `cmd/vibeshell/world.go`; `internal/adapters/sqlite/world_seed.go`; `internal/presentation/baseline.go` |
| World write commands: `mkdir`, `touch`, and `rm` stage creates/deletes in the turn's change set (committed by the coordinator), and `echo`/`env` expose `$HOME`/`$PWD`; a missing file under an existing directory is materialized with model-generated content | `cmd/vibeshell/{world.go,worldgen.go}` |
| Rendered frames and content reach the client: the SSH handler fetches `OutputFrame`/`OutputContent` bytes from the content store and writes them instead of dropping them | `cmd/vibeshell/sshhandler.go` |
| Configured generation brief: `prompts.app_generation` is rendered from the trusted invocation facts and prepended to the built-in artifact contract | `cmd/vibeshell/{prompts.go,generation.go}`; `internal/presentation/prompt.go` |
| Operational metrics are populated: the request-logging gateway increments `model.requests.total`/`model.request.errors`, and shutdown records the populated set | `cmd/vibeshell/{builders.go,component.go}`; `internal/observability/metrics.go` |
| `vibeshell admin app rollback` works: the CLI admin process builds the durable app registry and an app service over it | `cmd/vibeshell/admin.go` |
| Generated-app world reads: an app that returns `world_reads` has them resolved against the world (file content or directory entries) and handed back in a bounded `world_change` round-trip, so a generated program can read the simulated filesystem | `internal/simulation/{worldread.go,apprun.go}`; `cmd/vibeshell/generation.go` |
| Simulation tool layer behind the application seam: a turn can request the server-owned world/fact/history/app tools, executed by `simulation.Registry` through `simulation.ToolAdapter` with trusted namespace IDs, and the generation engine gathers the working directory's world context through it before generating | `internal/simulation/{registry.go,tooladapter.go}`; `cmd/vibeshell/tools.go`; `cmd/vibeshell/generation.go` |
| World scopes and deterministic path resolution: user > shared > baseline, tombstones, `..` clamp, symlink depth bound, byte-exact unicode names | `internal/domain`; ADR 0006 |
| Terminal input decoder, line editor, screen model, renderer, and fuzz tests: the SSH session edits commands with the editor, giving command history (Up/Down), cursor motion, Home/End, Ctrl-A/E/U/K/W, bracketed paste, and Ctrl-C/D; every line written to the client is CRLF-normalized (the interactive client's terminal is in raw mode, so the service supplies the carriage return itself) | `internal/terminal/*`; `cmd/vibeshell/sshhandler.go` |
| MOTD/presentation: per-session generated MOTD with a truthful service-unavailable fallback; sanitized identity and seed | `internal/presentation`; `cmd/vibeshell/motd.go` |
| Trusted prompt rendering: `{{variable_name}}` placeholders are substituted from session facts (per-slot empty renderings) before the prompt is sent, and an undocumented placeholder fails closed; the MOTD slot is wired, the other slots are not yet wired | `internal/presentation/prompt.go`; `internal/presentation/motd.go`; `tests/contract/prompts_test.go` |

## Backup, recovery, admin, and observability

| Feature | Source |
| --- | --- |
| Consistent live backup via `VACUUM INTO`, including content and version rows and optional sidecar host files (hash/size/mode in the manifest) | `internal/adapters/sqlite/backup.go`; `internal/admin/backup.go` |
| Restore accepted only on a matching **logical fingerprint** (order-independent per-row hashes), never a file checksum; a failed restore leaves no database | `internal/adapters/sqlite/backup*.go`; ADR 0004 |
| Integrity check: `PRAGMA integrity_check`, `pragma_foreign_key_check`, schema version, fingerprint, per-table row counts | `internal/adapters/sqlite/backup.go` |
| Startup recovery marks sessions with a start but no end/recovery marker via a durable `session.recovered` event; repeated runs are no-ops | `internal/adapters/sqlite/backup_recovery.go`; `internal/admin/recovery.go`; D05 crash tests |
| Admin application service (validation, user/hash maintenance, session listing, export delegation, route status, one bounded due-probe admission, integrity, backup/restore, recovery, app rollback) | `internal/admin/service.go` |
| Admin operations are reachable only through the container executable/service, never the simulated shell | `internal/admin/doc.go`; PLAN 10.5 |
| Three research projections from one canonical stream: JSONL bundle with per-file sha256 + manifest, lossy transcript, asciicast v2 | `internal/export`; `docs/guides/exports.md`; D05 export tests |
| Redaction of secret shapes from every exported format with valid post-redaction checksums | `internal/export`; `internal/observability`; D05 `TestExportRedactsSecrets` |
| Operational logs/metrics separated from research events; bounded label values; readiness names subsystems without provider/account detail | `internal/observability/{doc.go,logger.go,metrics.go,readiness.go}` |
| `vibeshell` subcommands: `run`, `validate`, `status`, `version`, internal-only `admin <op>`, `help` (default `run`) | `cmd/vibeshell/{main.go,admin.go}` |

## Not yet implemented / unverified

*Planned or researched but not shipped/verified. Do not claim these as product
features.*

| Item | Status | Notes |
| --- | --- | --- |
| AI materialization of a missing world path | Verified | `cat` on a missing file under an existing directory generates plausible content through the routed model executor, returns it, and stages the creation for the coordinator to commit, so an invented file gets an AI-generated presentation and then persists (`579af51`, `bcf91ff`). |
| Live authenticated provider inference | Verified for OpenCode Go chat and the OpenCode CLI; other protocols unverified | Live composed-service runs authenticated and streamed from `https://opencode.ai/zen/go/v1` (chat) on 2026-10-04; MOTD and generation requests returned 200 and decoded. The Zen free tier rejects non-OpenCode clients (`invalid_credential: ... can only be used from within OpenCode`), so the OpenCode CLI provider is the client for those models: a MOTD request reached the `cli` route and completed in 12194 ms via `mimo-v2.6-flash-free`. Receipts: `experiments/opencode-live-probe/receipts/2026-10-04T003406-composed-e2e.json`, `experiments/opencode-live-probe/receipts/2026-10-04T035600-opencode-cli-provider.json`. |
| Generation path behind the turn engine | Verified live end-to-end | A novel command over real SSH reached the composed generation engine, which asked the live provider for an artifact, parsed it, staging-validated it in the bounded sandbox, activated it, and ran it, printing the generated program's view. ADR 0012; receipt: `experiments/opencode-live-probe/receipts/2026-10-04T003406-composed-e2e.json`. |
| Reasoning-model fit (token budgets, reasoning in content) | Mitigated by selection, filtering, and failover | Generation uses a 16384-token / 180s budget and received a valid artifact live; chain-of-thought is stripped from decoded content for every protocol; per-purpose routing plus `Router.Execute` failover switches models when one returns an empty or unparseable answer. The MOTD bound was raised from 256 to 1024 tokens: with streaming, a reasoning model emitted its chain of thought as `reasoning_content` and produced empty content at 256 tokens (so the MOTD fell back), while 1024 leaves room for the reasoning and a short welcome. The truthful fallback and the next eligible model are still used when every route fails. |
| Durable app-state and route/account health persistence | Verified | `internal/adapters/sqlite/{apppersist,healthstore,spending}.go` persist apps/versions/state/pins/activations, route health, quota groups, and spending. `apps.Service` now writes state, pins, and the command-name index through `ports.AppStateStore` (satisfied by `sqlite.Apps`), so they survive a restart, and the shell seeds its command index from the durable map at startup (`929f8a2`, `99ded98`). |
| Full ~100-user benchmark | Shipped with a finding | D06 (`tests/load`, gated by `VIBESHELL_LOAD`) established 100/100 idle sessions at ~552 KiB/session (53.9 MiB peak) on a 16-vCPU host; the mixed-workload local event **p95 was 430 ms vs the 50 ms PLAN 13 goal** — recording-bound (`synchronous=FULL` per-event), not SSH. Durable-recording group commit then cut concurrent acknowledged append latency ~15× (≈5 ms → ≈0.33 ms), so an end-to-end re-measure is pending. See `tests/load/receipts/`. |
| Inference admission limits (`inference.global_concurrency` / `max_account_concurrency` / `wait_queue_depth`) | Implemented | `internal/inference` enforces global/per-account concurrency and a bounded wait queue behind `ports.ModelGateway`; composition wiring + health-neutral classification are the gate-wiring follow-up. |
| Docker execution | Unverified | Documented compatible but never executed; container-only rule keeps Docker off the host (ADR 0008). |
| Deployment hardening (non-root / read-only rootfs / dropped caps / PID+mem limits / no-new-privileges / single loopback port / SIGTERM to a handler) | Verified (ADR 0008) | Reproducible receipt: 10 PASS, 0 FAIL, plus the real-service SIGTERM check now PASS under the hardened flags. Docker execution remains unverified. |
| Sandbox penetration test / adversarial audit | Not performed | ADR 0003: forbidden-capability evidence is a source/import audit plus targeted denial probes. |
| 100 simultaneous long-running guests, idle eviction/restore, provider-coupled load | Not measured | ADR 0003 limitation. |
| Hot-reload vs restart-required classification per config field | Implemented (ADR 0009) | `internal/adapters/config/reload.go`: `ClassifyField`/`ClassifyConfig` + per-reload `ReloadReport{HotApplied, PendingRestart}`; the hot subset swaps atomically, restart-required fields are reported. |
| Real Unix execution, real kernel, full Bash/POSIX, SFTP/SCP, forwarding, X11, client-key auth, web admin UI, clustering | Out of scope (v1) | PLAN.md §2, §56; AGENTS.md. |
| Live DeepSeek V4.1 Flash / Go off-peak verification | Planned configuration | `docs/ops/model-rate-limit-history.md`; not runtime-verified. |

---
*Verified claims trace to merged files/receipts at baseline `91c7beb`. Research
findings (`docs/research/`) are observations, not runtime-verified capabilities.
The "Not yet implemented / unverified" table is explicitly separated and may
not be shipped as features.*

**Last verified**: 2026-10-05 against commit `af9f7e8` (live line-based interactive app over real SSH: `todo --interactive` generated, kept state across lines — `add` then `list` showed the item — and exited to the shell; receipt `experiments/opencode-live-probe/receipts/2026-10-05T114338-line-based-interactive.json`).
