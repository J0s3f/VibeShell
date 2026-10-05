# Model Routes

Operator-facing explanation of how VibeShell describes and selects models:
provider vs product route vs wire protocol, accounts and quota groups, tiers,
suitable-free discovery, OpenCode Go off-peak windows, and the DeepSeek fallback
rule. Derived from `PLAN.md` 8/11, ADR 0005, ADR 0011, and the merged adapter
and router code. Items that are planned configuration rather than
runtime-verified are labelled.

## 1. Provider vs product vs wire protocol

These are three independent axes. A model ID alone identifies none of them.

| Concept | Definition | Source |
| --- | --- | --- |
| **Provider** | The API service that executes inference. OpenCode is the initial provider. | `internal/adapters/opencode/protocol.go`, ADR 0005 |
| **Product route** | The product behind an endpoint: Console (pay-as-you-go / free) or Go (subscription). | `internal/adapters/opencode/protocol.go` (`ProductConsole`, `ProductGo`); config `providers[].products[]` |
| **Wire protocol** | The transport format: `chat` (OpenAI Chat Completions), `responses` (OpenAI Responses), `messages` (Anthropic Messages), `gemini` (Gemini generateContent). | `internal/adapters/config/config.go` (`ProtocolChat`/`ProtocolResponses`/`ProtocolMessages`/`ProtocolGemini`); ADR 0005 |

A configured **route** names `provider`, `product`, exact `model` ID, and
`protocol`; the protocol must be one the provider's product declares, or
validation fails. A model can exist on Console and Go with different
allowances, so the same ID does not imply the same route.

Protocol classification **fails closed**: an unclassified model resolves to no
protocol and is never routed (ADR 0005).

## 2. Accounts and quota groups

| Concept | Definition | Source |
| --- | --- | --- |
| **Account** | Stable administrative ID, permitted products, a quota-group ID, and one or more secret references. Multiple keys may belong to one account; **keys do not multiply quota**. | config `accounts[]`; `internal/domain/model.go`; ADR 0011 |
| **Quota group** | A shared quota across accounts. An account in an exhausted group is skipped for every route, not only the one that reported exhaustion. | ADR 0011 (`GroupAccountsByQuota`) |
| **Key reference** | An opaque `KeyRef`, never the secret. The account's own ID supplies it; the value travels only to the provider adapter. | `cmd/vibeshell/component.go` (`accountKeyRef`), `internal/domain/identity.go` |

Secrets resolve from mounted files or the container environment (see
[configuration](configuration.md#secret-references)). Never copy key contents
into the catalogue, research events, configuration exports, or error messages.

## 3. Tiers, selection, and affinity

Configured `tiers[]` are ordered, each with explicit `routes[]`, an optional
`auto_free` expansion, and an optional `account_pool`.

- Selection draws a route **uniformly among the distinct eligible routes**, then
  an account among that route's eligible accounts. Selection is independent of
  how many keys the account has (`internal/domain` routing policies; ADR 0011).
- The chosen route/account is **pinned to the session** until a failure forces a
  change. A recovered earlier tier never migrates a healthy pin (session
  affinity).
- Failover order is fixed: another eligible account for the same route, then
  another randomly ordered unused route in the same tier, then later tiers.
  A turn deadline and attempt budget bound the work; if they expire before the
  tier is exhausted, the turn ends with a temporary simulated service error
  rather than silently bypassing tier order (`PLAN` 8.4, ADR 0011).
- The router (`internal/routing`) currently duplicates several domain policies
  rather than calling them — recorded as a refactor follow-up in ADR 0011, not a
  correctness defect.

## 4. Suitable-free discovery

`internal/adapters/opencode/catalog.go` implements automatic-free selection:

1. Refresh the product catalogue on a configurable interval; retain the
   last-successful snapshot.
2. Expand an `-free`-suffixed entry when a tier sets `auto_free`.
3. Intersect with text generation, tool/structured support, protocol support,
   and a minimum context (`discovery.min_context_tokens`, `discovery.capabilities`).
4. Check pricing metadata: known nonzero cost excludes the model; missing or
   contradictory metadata requires an explicit operator decision — never treated
   as zero.
5. Apply administrator allow/deny rules (`discovery.explicit_allow`,
   `denied:*` reasons).
6. Deduplicate explicit and automatic entries naming the same route within a
   tier.

Documented decision reasons include `eligible:automatic-free`,
`eligible:explicit-allow`, `denied:not-free-suffix`,
`denied:known-nonzero-cost`, `needs-operator:unknown-metadata`,
`denied:no-tool-support`, `denied:non-text-modality`,
`denied:context-too-small`, and `denied:structured-decision-only`.

An explicitly named route may include a free model without the suffix (the
observed Big Pickle exception). Subscription-included Go usage is distinct from
genuinely zero-price free routes; do not classify all Go models as free.

Catalogue discovery succeeds **without an API key**, but proves neither account
entitlement nor model health (`PLAN` 8.3, ADR 0005).

## 5. OpenCode Go off-peak windows

From `docs/ops/model-rate-limit-history.md`: peak hours are **01:00–04:00 and
06:00–10:00 UTC, Monday–Friday**; all other hours and weekends are off-peak.

This window is the operator rule for the DeepSeek V4.1 Flash fallback (section
6). It is a **planned configuration**, not runtime-verified: the Go monetary
usage is visible in the Console, but free-model throttles are not reported by
any local API.

## 6. DeepSeek V4.1 Flash fallback rule

Per `AGENTS.md` and `docs/ops/model-rate-limits.md`: free subagent capacity is
limited. When **fewer than two free models** are healthy, DeepSeek V4.1 Flash
subagents may additionally be launched on the OpenCode Go route
(`opencode-go/deepseek-v4.1-flash`), **only during DeepSeek off-peak pricing**
(all hours and weekends except the peak window above). The operator authorization
currently recorded in `docs/ops/model-rate-limits.md` limits this to at most two
concurrent DeepSeek agents and only while free capacity is degraded.

For VibeShell's own provider routing, a Go route is selected only when a
configured tier makes it eligible; no route is ever added automatically
(`PLAN` 8.4, 9.3).

## 7. Model endpoints and free models (live-observed 2026-10-04)

The supplied key is an **OpenCode Go** key. Its models are served from
`https://opencode.ai/zen/go/v1` on three protocol families:

* Chat Completions: `/chat/completions` — GLM, Kimi, LongCat, DeepSeek, MiMo,
  Hy, Space Bunny, LongCat 2.5 Preview Free.
* Responses: `/responses` — Grok, GPT, Muse Spark.
* Messages: `/messages` — MiniMax, Qwen.

The Console pay-as-you-go catalogue (`https://opencode.ai/zen/v1/...`) is a
separate provider. Its free models (Big Pickle, Fledge Alpha, MiMo Free, Ling
Free, Nemotron Free, Muse Spark Contributor Free, Jev Free) reject non-OpenCode
clients with `403 FreeTierError` even with the Go key. The OpenCode CLI provider
(section 8) is the client for them: it runs the authenticated CLI, which reaches
the Console free tier. Paid Console models require Console credits.

Free models reachable with this key on the Go route are **`space-bunny-free`**
and **`longcat-2.5-preview-free`** (both unlimited for a limited time). Live
observations: `space-bunny-free` answers the short MOTD in ~1-8s but is
reasoning-heavy for generation; `longcat-2.5-preview-free` often returns empty
content or times out. Per-purpose routing therefore uses the free models for
MOTD and the authorized `deepseek-v4.1-flash` for generation, with failover
between them.

Privacy of the configured models (Go table): Space Bunny Free, LongCat 2.5
Preview Free, and DeepSeek V4.1 Flash are 0-day retention and not used for
training. This project's use case accepts training/retention, so that is not a
selection constraint here.

## 8. Provider implementations: HTTP and the OpenCode CLI

A configured provider's `kind` selects how its routes are served. Every
implementation sits behind one `ports.ModelGateway`; `internal/provider.Registry`
dispatches each request to the provider that owns its route and fails closed
(`model_not_found`) when a route has none (ADR 0013).

* `kind: "http"` (default): the direct provider API
  (`internal/adapters/opencode`). Products carry an https `base_url`; accounts
  carry a `secret_ref`.
* `kind: "cli"`: the OpenCode CLI (`internal/adapters/opencodecli`). It runs
  `opencode run -m <provider>/<model> --format json <prompt>` and collects the
  NDJSON `text` parts. The CLI owns its own login, so its accounts are
  credential-less: an account may omit `secret_ref` only when its non-empty
  `permitted_products` are all served by `cli` providers. A cli provider's
  product `base_url` is the CLI executable path (optional; default `opencode`),
  and its products must declare exactly `["chat"]`.

Adding a provider is a composition and configuration change
(`cmd/vibeshell/providers.go`); the shell, the SSH adapter, and the routing
policy do not change. Live-verified 2026-10-04: a MOTD request reached a `cli`
route and completed in 12194 ms via `mimo-v2.6-flash-free`
(`experiments/opencode-live-probe/receipts/2026-10-04T035600-opencode-cli-provider.json`).

---
*Traceable to: `internal/adapters/opencode/{protocol.go,catalog.go}`, `internal/adapters/opencodecli`, `internal/provider`, `internal/adapters/config/config.go`, `internal/domain`, `internal/routing`, `cmd/vibeshell/{component.go,providers.go}`, `docs/adr/0005-*`, `docs/adr/0011-*`, `docs/adr/0012-*`, `docs/adr/0013-*`, `docs/ops/model-rate-limit-history.md`, `docs/ops/model-rate-limits.md`. Live authenticated inference is verified for the Go chat protocol and the OpenCode CLI provider (2026-10-04); other protocol families remain unverified.*
