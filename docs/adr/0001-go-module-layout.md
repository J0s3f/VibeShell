# ADR 0001: Go service with `j0s.at/vibeshell` module layout

Status: accepted.

Date: 2026-10-03.

## Context

VibeShell needs a standalone executable serving many mostly idle SSH
connections with strict control over state commits, scopes, retries,
terminal frames, and transcripts (PLAN 3.1). The implementation language and
repository layout must be fixed before parallel tasks (A02–A06) diverge, or
adapters, storage, and generated-app work will assume incompatible homes.

## Decision

- Implement the service in Go, module path `j0s.at/vibeshell`, pinned to the
  current supported Go patch release (foundation task A01 records the exact
  pin; this task keeps `go 1.27.1` as declared in `go.mod`).
- Use ordinary lowercase Go package names (`domain`, `ports`,
  `application`, `simulation`, `terminal`, `adapters/ssh`, …). Do not
  reproduce Java-style reverse-domain package hierarchies; the `j0s.at`
  identifier appears only in the module path and in schema `$id` URIs, per
  PLAN 3.1.
- Lay out packages hexagonally (PLAN 3.3): `cmd/vibeshell` entry/wiring,
  `internal/domain` invariants, `internal/application` use cases,
  `internal/ports` contracts, `internal/simulation` and `internal/apps`
  orchestration, `internal/adapters/*` I/O, `internal/terminal` rendering
  core, plus `schemas/`, `prompts/`, `tests/`, `containers/`, `scripts/`,
  `docs/`.
- Standard library only for the contract baseline (`internal/domain`,
  `internal/ports`); external dependencies (SSH, SQLite, Wasm engine, HTTP)
  are introduced by later adapter tasks with pinned versions, never by the
  domain.

## Alternatives considered

- TypeScript with ssh2: convenient model integration, but weaker deployment
  story for a standalone executable and less control over lifecycle and
  resource bounds for 100 mostly idle connections.
- Python with AsyncSSH: fast prototyping, but the same lifecycle/packaging
  concerns plus heavier per-connection runtime cost.
- General-purpose coding-agent runtime (e.g. OpenCode server) as the
  execution engine: rejected because it carries filesystem/shell capabilities
  the simulation must not have and cannot enforce VibeShell's commit, scope,
  and transcript rules.

## Consequences

- All A02 contract code compiles with `go build ./...` using only the
  standard library; `go.sum` must not appear until an adapter task justifies
  an external dependency.
- Later tasks add libraries solely for demonstrated need with a recorded
  tradeoff (PLAN 3.2); the domain and ports stay dependency-free.
- Container images build a single static-friendly Go binary, keeping the
  runtime minimal (one public SSH port, non-root process).
