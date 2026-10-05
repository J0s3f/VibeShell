# Merged Contracts Overview

This document provides an overview of the merged contracts (`internal/domain`, `internal/ports`, `schemas/` + fixtures) and the dependency-direction rule, with pointers to the ADRs.

## Dependency Direction Rule

> **Dependencies point inward: adapters depend on application ports and the domain; the domain does not depend on adapters.**

This is enforced by the architecture guard test (`internal/domain/contracts_guard_test.go`):

- `internal/domain` imports only the Go standard library; it performs no I/O, reads no clock, and references no SSH, SQL, HTTP, provider SDK, or terminal types.
- `internal/ports` may import `j0s.at/vibeshell/internal/domain` but nothing else from the module.
- Adapters (`internal/adapters/*`) implement the port interfaces and may import the domain and ports, but never the reverse.

**Source**: `internal/domain/contracts_guard_test.go`, `internal/domain/doc.go`, `internal/ports/ports.go` (license header)

## `internal/domain` — Contract Vocabulary

The domain package owns VibeShell's contract vocabulary. Key types and their verified roles:

| Type | Purpose | Source |
| --- | --- | --- |
| `Identity` (and prefixes `usr`, `ses`, `trn`, `att`, `evt`, `cnt`, `nod`, `nsp`, `app`, `av`) | Strongly-typed identities for users, sessions, turns, attempts, events, content, nodes, namespaces, apps, and app versions. Pattern: `2-3 letter prefix` + `_` + 26-char Crockford base32. | `internal/domain/identity.go` |
| `Scope` (Session/User/Shared/Baseline) | Durability and visibility scope of world state. Session = cwd/shell vars; User = home files; Shared = global changes (when enabled); Baseline = immutable seed. | `internal/domain/scope.go` |
| `RouteID`, `AccountID`, `AppID`, `ContentID`, etc. | Opaque references traveling through ports; never carry secret values. | `internal/domain/identity.go` |
| `HealthState` / `HealthRecord` | Health tracking for route/account combinations (healthy/cooling_down/probe_eligible/probing/quarantined). Transitions documented in `routing.go`. | `internal/domain/routing.go` |
| `FailureClass` / `ErrorEnvelope` | PLAN 9.1 failure classification (invalid_credential, quota_exhausted, rate_limited, model_not_found, provider_outage, network_timeout, context_too_long, invalid_arguments, invalid_response, content_rejected, user_cancelled, world_conflict, unknown). | `internal/domain/model.go` |
| `ModelRequest` / `ModelResponse` | Canonical provider-neutral request/response format. Route and account travel as opaque references. | `internal/domain/model.go` |
| `EventEnvelope` / `EventKind` | Immutable research event envelope (PLAN 10.1). 60+ event kinds covering sessions, inputs, terminals, model I/O, tools, routing, world state, apps, context, config, shutdown, export, backup. | `internal/domain/events.go` |
| `WorldStore`, `EventStore`, `RetrievalStore`, `ContentStore`, `AppRegistry`, `AppSandbox`, `TerminalRenderer`, `Clock`, `Random`, `Exporter` | Outbound port interfaces (small contracts) that adapters implement. | `internal/ports/ports.go` |
| `RoutePolicy`, `TierConfig`, `ProbeBudget` | Routing configuration with ordered tiers, account pools, attempt budgets, and probe budgets. | `internal/domain/routing.go` |
| `ValidPath`, `ChangeSet`, `ReadDependency`, `ContentRef`, `Mutation` | World mutation contracts with read dependencies, expected revisions, and content references for atomic commit. | `internal/domain/events.go` / `schemas/world-changeset.schema.json` |

**Source**: `internal/domain/` directory (all `.go` files), `schemas/` JSON schemas

## `internal/ports` — Outbound Port Contracts

The ports package declares small inbound/outbound contracts that use only domain types plus standard-library plumbing (`context`). No port references SSH sessions, SQL rows, HTTP clients, provider SDK structures, or terminal libraries; adapters translate those into and out of these interfaces.

Key interfaces:

- `ModelGateway` — Single provider-neutral route to OpenCode product routes. `Request(ctx, req)` → `ModelResponse`.
- `WorldStore` — Persists current world state; `Commit` applies mutations atomically after verifying read dependencies and expected revisions.
- `EventStore` — Append-only research record; events are immutable once appended.
- `RetrievalStore` — Agent's scoped window into history; policy argument injected by trusted application context.
- `ContentStore` — Immutable exact bytes addressed by content hash; metadata and content commits are atomic.
- `AppRegistry` — Tracks immutable application artifacts; `RegisterCandidate`/`Activate`/`Rollback` manage version activation.
- `AppSandbox` — Executes generated JavaScript with capabilities over the simulated world only.
- `TerminalRenderer` — Consumes validated declarative views and emits terminal control sequences.
- `Clock` / `Random` — Injectable time and randomness sources so tests never depend on real delays.
- `Exporter` — Streams research projections (versioned JSONL, readable transcript, asciinema-compatible replay).
- `AdminOps` — Operator commands: config validation, route/status, probe triggers, backup/restore.

**Source**: `internal/ports/ports.go`, `internal/adapters/opencode/gateway.go` (implementation), `internal/adapters/sandbox/appsandbox.go`

## `schemas/` — Versioned JSON Schemas

The `schemas/` directory contains versioned JSON schemas for all major contracts. Each schema has a `version` field and `$id` for discoverability. Key schemas:

| Schema | Purpose | Verified Constraints |
| --- | --- | --- |
| `configuration.schema.json` | VibeShell strict service configuration (PLAN 11). | `version: 1` required; `identity.system_name: "VibeOS"`, `identity.shell_name: "VibeShell"`; `ssh.listen_port` (1-65535); `auth.mode: "public"\|"secure"`; `sharing.enabled`; `tiers[].name` + `routes[]`; `accounts[].id`, `quota_group`, `secret_ref` (file path or env name, never value). No `additionalProperties` allowed. |
| `route-policy.schema.json` | Route policy and health record (PLAN 8.4, 9.2). | Ordered tiers with `auto_free`, `account_pool`, `min_healthy`. Health record with `state` enum and `failure_class` enum. `consecutive_failures` minimum 0. |
| `app-abi.schema.json` | Generated-application artifact / event / result ABI (PLAN 5.6, 6.2). | Artifact with `app_id`, `version_id`, `manifest` (abi_version 1, command_names, description, state_schema, capabilities, entrypoint, version). App view modes: text/form/table/editor/pager/status. App result with `new_state`, `view`, `exited`. |
| `event-envelope.schema.json` | Immutable envelope for every research event (PLAN 10.1). | `schema_version: 1`; `event_id`, `session_id`, `sequence`, `timestamp`, `monotonic_offset`, `kind` enum (60+ kinds). Provenance with `source` enum (ssh/model/tool/sandbox/admin/system). |
| `model-request-result.schema.json` | Canonical model request / result + failure envelope (PLAN 9.1, contract 14). | `request` with `route_id`, `account_id`, `messages`, `max_tokens`, `deadline_ms`, `request_id`. `result` with `request_id`, `route_id`, `account_id`, `finish_reason`, `usage`, `latency_ms`, `timestamp`. `errorEnvelope` with `class` enum and `retry_after_ms`. |
| `world-changeset.schema.json` | Staged world mutations with read dependencies (PLAN 5.3, contract 14). | `mutations` array max 256 items. `read_dependencies` max 1024 items. Per-mutation: `type` (create/update/delete/tombstone), `namespace_id`, `path`, `expected_rev`, `kind`, `metadata` (mode/uid/gid/times), `contentRef` (hash/size/media_type). |

**Source**: `schemas/*.json` files

## Pointers to ADRs

The following ADRs (Architecture Decision Records) are in `docs/adr/` and document significant design decisions:

- `docs/adr/0001-go-module-layout.md` — Go module path `j0s.at/vibeshell`, normal lowercase package names, no Java-style reverse-domain hierarchies.
- `docs/adr/0002-hexagonal-port-contracts.md` — Hexagonal boundaries, dependency direction, port/adapter separation.

**Source**: `docs/adr/0001-go-module-layout.md`, `docs/adr/0002-hexagonal-port-contracts.md`

---
*All claims traceable to merged files/receipts in D:\code\vibeshell.*