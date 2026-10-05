# Configuration spike (PLAN 11): strict parsing, secret references, atomic reload

Date: 2026-10-03. Scope: PLAN.md 11 (Configuration and prompts), 3.2
(Structured contracts, Configuration rows). Code:
`experiments/config-validation/` (`config.go`, `validate.go`,
`secrets.go`, `catalogue.go`, `loader.go`, `snapshot.go`, tests,
`examples/`). Receipt:
`experiments/config-validation/receipts/config-qualification.log`
(all tests PASS under `go test -race`, gofmt and `go vet` clean).
The nested module is standard-library only; no root `go.mod`/`go.sum`,
`schemas/`, `scripts/`, `containers/`, `cmd/`, `internal/`, or `PLAN.md`
file was modified. `schemas/configuration.schema.json` was read as the
contract version 1 reference.

## Design (observed facts)

- **Strict decoding** (`ParseConfig`): a token-stream scan rejects
  duplicate object keys at every level (encoding/json silently keeps the
  last value for a duplicated key, so the scan runs before decoding),
  rejects non-object roots, malformed JSON, and trailing content;
  `json.Decoder.DisallowUnknownFields` then rejects unknown fields at
  every nesting level; a reflection pass over `required:"true"` struct
  tags reports every missing required field with its JSON path
  (`$.auth.mode`). Required booleans are `*bool` so an explicit `false`
  is distinguishable from an absent field.
- **Semantic validation** (`Validate`) is pure: no I/O, no environment
  reads, no secret resolution. It covers the PLAN 11 groups this spike
  models: identity constants, auth mode with mode/password-file pairing
  rules, sharing policy revision, tier ordering and uniqueness, route
  references against a fixture catalogue, route-to-protocol support,
  duplicate account IDs and tier names, secret-reference grammar,
  persistence durability pairing, and bounds for every duration, size,
  and limit in the extension groups.
- **Route catalogue** (`catalogue.go`) is fixture data, as PLAN 11
  anticipates ("configuration validation can report missing/unsupported
  routes without making a paid inference call"). Discovery patterns
  (`<product>/-free`) expand to catalogue routes in catalogue order, so
  tier route lists are deterministic.
- **Secret references** (`secrets.go`): `{file:...}` and `{env:...}`
  only; any other form — including a bare value — is rejected, so an
  inline secret cannot enter a document. `{file:...}` resolves only
  inside one allowed directory, including through symlinks
  (`filepath.EvalSymlinks` containment). `Secret` redacts itself in
  every formatting path; errors name the reference and the reason, never
  the value.
- **Atomic reload** (`snapshot.go`): `Loader.Load` produces a fully
  validated `Candidate` (parse + semantic validation + prompt-file
  existence + secret resolution); `Store.LoadFile` publishes it as an
  immutable `Snapshot` only when the candidate is fully valid, as a
  single pointer swap under a lock. `Active()` returns the current
  snapshot; a turn that kept a `*Snapshot` keeps seeing its pinned view
  after later publications.

The model covers the eight contract-version-1 groups of
`schemas/configuration.schema.json` field-for-field, plus five PLAN 11
groups the version 1 schema does not carry yet (`providers`, `prompts`,
`discovery`, `inference`, `operations`), modelled as optional pointer
groups so a version 1 document stays valid without them.

## Gate verdicts (all executed in `vibeshell-config-validation-dev`)

Command: `cd experiments/config-validation && go test -race -count=1 -v ./...`
→ PASS (13 tests, ~90 subtests, 1.4 s). `gofmt -l .` empty,
`go vet ./...` clean. Root `scripts/dev.ps1` validation unchanged.

| Deliverable | Result | Evidence |
| --- | --- | --- |
| 1. Strict decoding | PASS | Unknown top-level/nested/deep fields rejected; duplicate keys rejected at root, in `identity`, in `auth`, in tiers and accounts; missing `version`/`identity`/`auth.mode`/`sharing.enabled`/`tiers`/`persistence.database_path` reported with JSON paths; wrong types rejected (`"2222"` for a port, string for a bool, number for a route); trailing content, non-object roots, malformed JSON rejected |
| 2. Semantic validation | PASS | 50-case table: explicit auth mode enum plus public/secure pairing rules; duplicate account IDs and tier names rejected; discovery tier may not be first or repeat, may not set `account_pool`; unknown routes, unknown discovery products, patterns in plain tiers, explicit routes in discovery tiers rejected; disabled provider or protocol mismatch reports `requires protocol "zen", which no enabled provider supports`; persistence FULL requires a backup dir; bounds checked for ports, connections, attempts, deadlines, token limits, concurrency, queue depth, grace period, log level |
| 3. Secret references | PASS | `{file:...}`/`{env:...}` grammar; inline values, malformed refs, `.`/`..` targets rejected; allowed-directory enforcement rejects traversal, absolute paths outside, and symlink escapes; missing/empty/unreadable (directory-as-file) refs fail with actionable errors; a marker-value test asserts neither secret appears in any error message, including a validation failure after a successful resolution |
| 4. Atomic reload snapshot | PASS | Valid candidate publishes versioned immutable snapshots; three invalid candidates (unknown field, duplicate account ID, unresolvable secret) each fail without replacing the active snapshot; a pinned snapshot keeps its auth mode, tier routes, and secret across a later reload while `Active()` advances; 8 concurrent readers during 25 reloads pass under `-race`; the shipped example loads and demonstrates tier 1 named free routes, tier 2 `opencode/-free` discovery expansion, tier 3 Go-native routes, tier 4 cheap pay-as-you-go routes |
| 5. Recommendation | below | Struct tags + semantic checks recommended; full JSON-Schema validation rejected for the runtime path |

## Redaction finding (observed fact, not a plan)

The spike's redaction test initially failed: `fmt.Sprintf("%v", snapshot)`
printed resolved secret values in full. Cause, verified with a minimal
probe against the container's Go 1.27.1 `fmt`: `printValue` calls
`String()` only when `value.CanInterface()` is true, and values reached
through unexported struct fields are not interfaceable, so `Secret.String()`
is bypassed whenever a snapshot is printed as a whole struct. Fix:
`Snapshot.String()` (value receiver, so both `Snapshot` and `*Snapshot`
are Stringers) renders a redacted summary, and `fmt` always calls it at
the top level. Residual, documented on the type: fmt prints unexported
fields raw when a snapshot is nested inside another struct's unexported
field, so snapshots must be logged through their `String` method — the
same property every Go type with unexported state has. This is why the
guarantee is stated as "errors and logs never carry values" and tested at
both the error and formatting boundaries.

## Recommendation

**Adopt Go struct tags plus semantic checks as the runtime validation
mechanism; keep the JSON Schema as the published, versioned contract for
documentation and CI, not as a runtime dependency.**

Tradeoffs, both ways:

- **Struct tags + semantic checks (recommended).** Zero dependencies,
  which PLAN 3.2 requires ("add a library only for a demonstrated
  need"); compile-time type safety; `DisallowUnknownFields` is stdlib;
  errors carry typed causes and JSON paths; the pure `Validate` is
  trivially unit-testable. Costs: the schema and the structs can drift,
  and bounds exist twice (schema and named Go constants). Mitigations:
  validate the shipped example and fixtures against the schema in CI, and
  review schema changes and struct changes in the same PR; whether one
  artifact should be generated from the other is an open question below.
- **Full JSON-Schema validation at runtime (rejected for v1).** A
  draft-2020-12 validator (e.g. `santhosh-tekuri/jsonschema`) would
  make the committed schema the single source of truth, but it adds a
  runtime dependency to every deployment, duplicates every semantic rule
  outside the schema anyway (duplicate-key rejection, route-catalogue
  references, tier ordering, and policy combinations are not expressible
  in JSON Schema), and needs error post-processing to be actionable.
  JSON Schema is also value-based: it cannot see duplicate keys at all,
  because last-wins parsers hand it only the final value.

The split therefore matches PLAN 3.2's own wording ("versioned JSON
schemas and typed Go structures"): the schema is the contract document,
the Go structures are the enforcement.

## What is not covered

- The committed schema is contract version 1 with eight groups; PLAN 11
  lists seventeen. The five extension groups here are schema-v2
  candidates, optional in the loader until the schema carries them.
- The route catalogue is a fixture. Live route availability, per-product
  endpoint behaviour, and quota-group health sharing (multiple accounts
  in one quota group) are not verified; PLAN 11 only requires that
  validation report missing or unsupported routes without a paid call.
- Hot-reload scope: PLAN 11 requires that bind address, engine binary,
  and storage migrations need a restart. The snapshot mechanism publishes
  any validated document; it does not yet classify fields into
  hot-reloadable versus restart-required.
- Secret rotation and revocation beyond snapshot replacement (a removed
  account prevents new requests only once a new snapshot is published);
  password-file content and Argon2 verification; host-key file format.
- No fuzzing yet (PLAN 3.2 lists fuzzing as a test direction), and no
  CI check that the example document still validates against
  `schemas/configuration.schema.json`.

## Open questions

1. How is `account_pool` membership declared? A tier names a pool, but
   accounts carry no pool field in schema version 1, so pool membership
   cannot be cross-checked yet.
2. Should the schema be generated from the Go types (or vice versa) to
   eliminate the drift risk between the two bound representations?
3. Is enforcing prompt paths to stay inside the configuration directory
   the right product rule, or should absolute prompt paths elsewhere be
   allowed? The spike enforces containment.
4. Is `persistence.durability` defaulting to FULL (and therefore
   requiring `backup_dir`) the intended default, or should the default
   be NORMAL until an operator opts into durability? The spike follows
   the schema's documented default.
5. Should the five extension groups ship as schema version 2 in full,
   or incrementally per group as their owning tasks land?
