# ADR 0002: Hexagonal port contracts owned by the domain

Status: accepted.

Date: 2026-10-03.

## Context

SSH transport, model providers, SQLite storage, the terminal renderer, and
the generated-application sandbox must all evolve independently (spikes
A03–A06 run in parallel), yet agree on sessions, turns, world commits,
events, retrieval, and exports. Without a fixed boundary, business behavior
leaks into the SSH handler or provider codecs, and parallel agents block on
each other's internals.

## Decision

- `internal/domain` owns identities, scope policy, world/read-dependency and
  change-set invariants, event envelopes, app artifact/event/result shapes,
  canonical model request/result types, failure classes, route/health
  transitions, retrieval queries, and typed errors. It performs no I/O, reads
  no clock or randomness (timestamps and seeds are injected), and imports
  only the standard library.
- `internal/ports` declares small inbound/outbound interfaces using only
  domain types: `ModelGateway`, `WorldStore`, `EventStore`,
  `RetrievalStore`, `ContentStore`, `AppRegistry`, `AppSandbox`,
  `TerminalRenderer`, `Clock`, `Random`, `Exporter`, `AdminOps`. Each method
  documents its bounds and failure vocabulary; secrets cross only as
  `AccountID`/`KeyRef` references.
- Versioned JSON Schemas in `schemas/` (draft 2020-12, one `version`
  constant each, mirrored by `domain` version constants) plus representative
  fixtures in `schemas/fixtures/` are the cross-implementation conformance
  target: every schema file is valid JSON, and every fixture unmarshals into
  its Go type and round-trips.
- Dependency direction is inward only: adapters → ports → domain. An
  architecture guard test fails the build on any non-stdlib or
  adapter/SSH/SQL/HTTP import under `internal/domain` or `internal/ports`.

## Alternatives considered

- Sharing provider SDK or `ssh.Session` types through the core: rejected;
  swapping a protocol or transport would then rewrite shell behavior, and
  unit tests could not run without network/transport doubles.
- One wide `Store`/`Service` interface: rejected; small ports keep fakes
  cheap, bound responsibilities, and let B/C tasks proceed against doubles
  before adapters land.
- Protobuf/gRPC IDL instead of JSON Schema: rejected for v1; SQLite-backed
  JSON payloads, JSONL exports, and the JSON sandbox bridge already commit to
  JSON, and strict schemas plus typed Go structs give the needed validation.

## Consequences

- B/C tasks implement adapters against these ports with deterministic fakes;
  contract changes go through the main agent (this baseline), never through
  unilateral edits.
- Adding a provider requires only a new `ModelGateway` adapter plus route
  configuration; SSH, storage, and sandbox code are untouched.
- The guard test and fixture round-trip tests run in the standard
  `go test -race ./...` gate, so contract drift fails fast in every
  checkout's container.
