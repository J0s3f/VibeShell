# VibeShell Documentation

Index of the documentation tree. All paths are relative to the repository root.

## Guides

- [guides/foundation.md](guides/foundation.md) — Container-only development
  workflow as shipped: pinned toolchain and images, `scripts/dev.ps1` commands,
  per-agent worktree/container/volume/port isolation, and the acceptance test.
- [guides/contracts.md](guides/contracts.md) — Overview of merged contracts
  (`internal/domain`, `internal/ports`, `schemas/` + fixtures) and the
  dependency-direction rule, with ADR pointers.
- [guides/configuration.md](guides/configuration.md) — Configuration groups,
  strict validation, `{file:...}`/`{env:...}` secret references, the password
  file and Argon2id defaults, prompt paths, and atomic reload.
- [guides/model-routes.md](guides/model-routes.md) — Provider vs product route
  vs wire protocol, accounts/quota groups, tiers, suitable-free discovery, Go
  off-peak windows, and the DeepSeek fallback rule.
- [guides/operations.md](guides/operations.md) — Running the container, startup
  and shutdown ordering, readiness, backup/restore, recovery, admin surface, and
  observability/redaction.
- [guides/exports.md](guides/exports.md) — JSONL bundle schema and checksums,
  transcript lossiness, asciicast v2, incomplete-session handling, and
  redaction.

## Root documents

- [../FEATURES.md](../FEATURES.md) — Shipped behavior traceable to merged code
  and tests, with a separated "Not yet implemented / unverified" section.
- [../AGENTS.md](../AGENTS.md) — General engineering rules (do not modify).
- [../PLAN.md](../PLAN.md) — Product and success criteria, scope, architecture
  decisions, and work breakdown (do not modify).

## Decision records, research, and operations

- [adr/](adr/) — Architecture decision records (0001–0011) for significant
  design decisions.
- [research/](research/) — Independently owned research snapshots and design
  implications (do not modify).
- [ops/](ops/) — Free-model rate-limit status and history for subagent
  scheduling.
- [development.md](development.md) — Development environment details for the
  life of the project.

---
*Each guide contains the full content for its topic with source references. Items
that are planned or unverified are labelled in the guide and in `FEATURES.md`.*
