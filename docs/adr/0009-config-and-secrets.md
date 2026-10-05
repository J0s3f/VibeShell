# ADR 0009: Strict JSON configuration with secret references and atomic reload

Status: accepted.

Date: 2026-10-03.

## Context

PLAN 11 requires versioned strict JSON configuration, a separate password-hash
file, secret references, and separate prompt files, with unknown fields failing
validation. Two operational properties matter as much as the syntax: a secret
value must never reach a log, an error, or an export, and a reload must never
leave a running service on a half-applied configuration.

PLAN 3.2 also constrains the solution: add a library only for a demonstrated
need. That makes "validate with a JSON Schema library at runtime" a decision, not
a default.

## Decision

**Two artifacts, distinct roles.** `schemas/configuration.schema.json` is the
published, versioned contract used for documentation and CI. Typed Go
structures with struct tags plus semantic checks are the **runtime enforcement**.
No runtime JSON-Schema validator is used.

**Strict decoding** (`ParseConfig`): a token-stream scan rejects duplicate object
keys at every level before decoding, because `encoding/json` silently keeps the
last value for a duplicated key. Non-object roots, malformed JSON, and trailing
content are rejected. `json.Decoder.DisallowUnknownFields` then rejects unknown
fields at every nesting level. A reflection pass over `required:"true"` tags
reports **every** missing required field with its JSON path (`$.auth.mode`).
Required booleans are `*bool` so an explicit `false` is distinguishable from an
absent field.

**Semantic validation is pure**: no I/O, no environment reads, no secret
resolution. It covers identity constants, auth mode with mode/password-file
pairing, sharing policy revision, tier ordering and uniqueness, route references
against the catalogue, route-to-protocol support, duplicate account IDs and tier
names, secret-reference grammar, persistence durability pairing, and bounds for
every duration, size, port, and limit.

**Route catalogue is fixture data.** Discovery patterns (`<product>/-free`)
expand to catalogue routes in catalogue order, so tier route lists are
deterministic. Validation reports missing or unsupported routes **without** making
a paid inference call.

**Secret references only.** `{file:...}` and `{env:...}`. Any other form,
including a bare value, is rejected, so an inline secret cannot enter a document.
`{file:...}` resolves only inside one allowed directory, including through
symlinks (`filepath.EvalSymlinks` containment). Errors name the reference and the
reason, never the value.

**Redaction must be structural, not incidental.** `Secret` redacts itself in
every formatting path, and `Snapshot` implements `String()` on a value receiver
so `fmt` calls it at the top level. The spike's redaction test initially failed:
`fmt.Sprintf("%v", snapshot)` printed resolved secret values in full, because
`printValue` calls `String()` only when `value.CanInterface()` is true and
values reached through unexported struct fields are not interfaceable. The
residual, documented on the type: `fmt` prints unexported fields raw when a
snapshot is nested inside another struct's unexported field, so snapshots must be
logged through their `String` method. The guarantee is therefore stated as
"errors and logs never carry values" and tested at both boundaries.

**Atomic reload.** `Loader.Load` produces a fully validated candidate (parse,
semantic validation, prompt-file existence, secret resolution). `Store.LoadFile`
publishes it as an immutable snapshot **only** when the candidate is fully
valid, as a single pointer swap under a lock. A failed load leaves the previous
snapshot serving. A turn that kept a `*Snapshot` keeps its pinned view after
later publications; new sessions and new turns see the current one.

## Alternatives considered

- Full JSON-Schema validation at runtime (e.g. `santhosh-tekuri/jsonschema`):
  rejected for v1. It adds a runtime dependency to every deployment, duplicates
  every semantic rule outside the schema anyway (duplicate-key rejection,
  route-catalogue references, tier ordering, and policy combinations are not
  expressible in JSON Schema), and needs error post-processing to be actionable.
  JSON Schema is also value-based: it cannot see duplicate keys at all, because
  last-wins parsers hand it only the final value — a class of ambiguity that
  matters most for secrets and IDs.
- Permissive decoding with defaults: rejected by PLAN 11 ("unknown fields fail
  validation").
- Environment-only secrets: rejected; files mounted into the container are the
  primary path, and PLAN 12.2 mounts read-only secret files.
- Mutable in-place reload: rejected. A turn mid-flight would see a mixture of two
  documents, and a bad reload could leave the service partly configured.
- Generating one artifact from the other (schema from Go types, or vice versa)
  to eliminate drift: open question, not decided. The current mitigation is
  reviewing schema and struct changes together and validating the shipped example
  and fixtures against the schema in CI.

## Consequences

- Schema and structs can drift, and bounds exist in both places. This is a real,
  accepted cost with a named mitigation, not a solved problem.
- Validation is unit-testable with no filesystem, environment, or secrets.
- Secrets never exist inside a configuration document, so a configuration file
  can be backed up, exported, and attached to a bug report without a redaction
  pass. The residual `fmt` nesting hazard is a caller obligation.
- A reload that fails leaves the service running on its last known-good
  configuration, and the failure is reported rather than silently ignored.
- Snapshot pinning means a turn's auth mode, tier routes, and secrets are stable
  for its duration; a key removal takes effect for new requests after
  activation, not retroactively.
- Hot-reloadability is **not** yet classified. Bind address, engine binary, and
  storage migrations require a restart; the snapshot mechanism publishes any
  validated document without distinguishing those fields.

## Evidence

- Research: `docs/research/2026-10-03-config-validation-spike.md`.
- Receipt: `experiments/config-validation/receipts/config-qualification.log`, UTC
  2026-10-03T15:22:59Z, `go test -race -count=1 -v ./...` PASS (13 tests, ~90
  subtests, 1.4 s); `gofmt -l .` empty, `go vet ./...` clean; standard-library
  only, no dependencies.
- Covered: strict decoding (unknown top-level/nested/deep fields, duplicate keys
  at root and inside `identity`, `auth`, tiers and accounts, missing required
  fields with JSON paths, wrong types, trailing content, non-object roots);
  semantic validation (50-case table covering auth mode pairing, duplicate IDs,
  discovery tier placement, route/protocol mismatch, durability pairing, and
  bounds); secret references (grammar, inline-value rejection, traversal,
  absolute-path and symlink escapes, missing/empty/unreadable targets, and a
  marker-value test asserting no secret appears in any error message); atomic
  reload (three invalid candidates rejected without replacing the active
  snapshot, a pinned snapshot surviving a later reload, 8 concurrent readers
  during 25 reloads under `-race`).
- Reference contract: `schemas/configuration.schema.json` (contract version 1,
  eight groups: `identity`, `ssh`, `auth`, `sharing`, `tiers`, `accounts`,
  `limits`, `persistence`, plus `version`).
- Commits: `026bae4` (spike), merged as `640bd52`.

## Unverified items and limitations

- The committed schema is contract version 1 with eight groups; PLAN 11 lists
  seventeen. The spike modelled `providers`, `prompts`, `discovery`,
  `inference`, and `operations` as optional pointer groups so a version 1
  document stays valid, but they are **schema-v2 candidates** and are not yet
  part of the published contract.
- The route catalogue is a fixture. Live route availability, per-product endpoint
  behavior, and quota-group health (multiple accounts sharing a quota group) are
  unverified. PLAN 11 only requires reporting missing or unsupported routes
  without a paid call.
- Hot-reloadable versus restart-required classification **is implemented**
  (`internal/adapters/config/reload.go`: `ClassifyField`/`ClassifyConfig` plus a
  per-reload `ReloadReport{HotApplied, PendingRestart}`), satisfying PLAN 11's
  boundary-documentation requirement. The hot subset (tiers, routes, accounts,
  account pools, providers, prompts, sharing, discovery, health, limits,
  inference, spending, world, apps, terminal, exports, operations) swaps
  atomically; `ssh.listen_address`, `ssh.listen_port`, `ssh.host_key_file`,
  `apps.engine_abi_version`, `persistence.database_path`, and
  `persistence.durability` require a restart.
- Secret rotation and revocation beyond snapshot replacement are not covered:
  removing a key prevents new requests only once a new snapshot is published.
  Password-file content, Argon2 verification, and host-key file format were out
  of scope.
- No fuzzing of the configuration parser, and **no CI check that the shipped
  example still validates against `schemas/configuration.schema.json`** — so the
  named drift mitigation is currently only partially in place.
- Open questions: how `account_pool` membership is declared (tiers name a pool,
  but accounts carry no pool field in schema version 1, so membership cannot be
  cross-checked); whether one artifact should be generated from the other;
  whether prompt paths must stay inside the configuration directory (the spike
  enforces containment, absolute paths elsewhere are not yet permitted); whether
  `persistence.durability` should default to FULL, which the spike follows from
  the schema but which therefore requires `backup_dir`; and whether the five
  extension groups ship as a full schema version 2 or incrementally per owning
  task.