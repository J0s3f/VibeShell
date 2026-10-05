# Configuration

VibeShell is configured by one strict, versioned JSON document, a separate
password file, prompts referenced by path, and secret *references* (never secret
values). This guide describes the shipped configuration boundary. Claims are
traced to merged code; the field list is the one this build actually accepts,
which is narrower than the full PLAN 11 group list (see "Schema coverage").

- Runtime contract: `schemas/configuration.schema.json` (contract version 1)
- Runtime enforcement: `internal/adapters/config` (typed Go structs + semantic
  checks, **no runtime JSON-Schema validator** — ADR 0009)
- Worked example: `examples/vibeshell.json`

## Loading

The executable loads one file; a missing `-config` is an error
(`cmd/vibeshell/main.go`, `cmd/vibeshell/component.go` `openSnapshot`).

```
vibeshell validate [-config PATH] [-secret-dir DIR ...]   # parse + semantic check, print snapshot, exit
vibeshell run      [-config PATH] [-secret-dir DIR ...]   # start the service (default command)
vibeshell status   [-config PATH] [-secret-dir DIR ...]   # local readiness report, exit
```

Default configuration path is `/etc/vibeshell/vibeshell.json`. Relative prompt
paths resolve against the directory of the configuration file
(`config.Loader` constructed with `path.Dir(configPath)`).

`-secret-dir` is repeatable and is **fail-closed**: with no allowed directory,
every `{file:...}` secret reference is refused. File secret references resolve
only inside an allowed directory, including through symlinks
(`filepath.EvalSymlinks` containment; `internal/adapters/config/secrets.go`).

## Strict decoding

`ParseConfig` rejects, with JSON-path diagnostics and before any I/O
(`internal/adapters/config/decode.go`, ADR 0009):

- unknown fields at every nesting level (`DisallowUnknownFields`);
- duplicate object keys at any level (a token-stream scan, because
  `encoding/json` silently keeps the last value);
- wrong scalar kinds, nulls for non-null fields, trailing content, non-object
  roots, and missing required fields (every missing required field is reported
  with its path, e.g. `$.auth.mode`).

Semantic validation is pure (no I/O, no environment reads, no secret
resolution): identity constants, auth mode/password-file pairing, sharing
policy revision, tier ordering/uniqueness, route references against the
catalogue, route-to-protocol support, duplicate account IDs and tier names,
secret-reference grammar, persistence durability pairing, and numeric bounds.

## Secret references

A secret is only ever a reference. `{file:...}` is an absolute, cleaned POSIX
container path; `{env:...}` is a container environment-variable name. Any other
form — including a bare value — is rejected
(`internal/adapters/config/secrets.go`):

```json
"secret_ref": "{file:/etc/vibeshell/secrets/opencode-primary.key}"
"secret_ref": "{env:VIBESHELL_OPENCODE_GO_KEY}"
```

- A resolved value is bounded (`DefaultMaxSecretBytes` 64 KiB), has its trailing
  newline trimmed, and must be non-empty (a mis-mounted secret fails closed).
- `Secret` redacts itself for `%v`, `%s`, `%#v`, and JSON, so a secret does not
  leak through a log line, an error chain, or a marshaled snapshot.
- Error messages name the reference and the reason, never the value.

## Configuration groups

The `Config` struct (`internal/adapters/config/config.go`) accepts the following
groups. `version` is required and must be `1`.

| Group | Key fields | Notes |
| --- | --- | --- |
| `version` | `1` | Only accepted document version. |
| `identity` | `system_name`, `shell_name`, `hostname`, `presentation_seed` | `system_name`/`shell_name` are fixed by PLAN to VibeOS/VibeShell; the example sets them explicitly. |
| `ssh` | `listen_address`, `listen_port`, `host_key_file`, `max_connections`, `idle_timeout_ms`, `handshake_timeout_ms`, `max_terminal_rows`, `max_terminal_cols`, `max_input_bytes` | The one public port. Host key is loaded or generated in the durable volume. |
| `auth` | `mode` (`public`\|`secure`), `password_file`, `max_attempts_per_connection`, `max_concurrent_auth`, `failed_attempts_per_minute` | No default mode; it must be explicit. |
| `sharing` | `enabled`, `policy_revision` | Hard switch enforced at the tool boundary. |
| `world` | `baseline_version`, `max_objects`, `max_content_bytes`, `materialization_budget_bytes`, `max_staged_changes`, `conflict_retries` | Staged-change and conflict bounds. |
| `apps` | `engine_abi_version`, `max_source_bytes`, `max_state_bytes`, `heap_limit_bytes`, `stack_limit_bytes`, `execution_deadline_ms`, `instance_pool_size`, `max_generations`, `max_repairs` | Generated-application bounds. |
| `terminal` | `scrollback_lines`, `max_paste_bytes`, `redraw_policy`, `refresh_budget_lines`, `completion_cache` | Rendering/input budgets. |
| `prompts` | `motd`, `shell_behavior`, `app_generation`, `app_extension`, `world_materialization`, `summary`, `repair` | Paths to prompt files; relative to the configuration directory. Each must exist, be regular, and be readable (except the optional `app_extension`). |
| `providers` | `name`, `kind` (`http`\|`cli`), `products[]` (`name`, `base_url`, `protocols`, `default_protocol`, `endpoint_overrides`) | Products and the protocols they accept. `kind` defaults to `http`: `base_url` is required and must be an https URL. `kind: "cli"` runs the OpenCode CLI instead: `base_url` is then the optional CLI binary path/name (no URL rules) and every product's `protocols` must be exactly `["chat"]`, because the CLI returns text. |
| `routes` | `id`, `provider`, `product`, `model`, `protocol` | A concrete model route; provider/product/protocol must agree with the provider declaration. |
| `accounts` | `id`, `quota_group`, `permitted_products`, `secret_ref` | Keys do not multiply quota; several keys may share one account. `secret_ref` may be omitted **only** for a credential-less account: non-empty `permitted_products` where every listed product is declared by a `kind: "cli"` provider (which ignores secrets); every other account requires it. |
| `account_pools` | `{ poolName: [accountID...] }` | Named pools a tier may reference. |
| `tiers` | `name`, `routes[]`, `auto_free`, `account_pool` | Ordered routing tiers. |
| `discovery` | `refresh_interval_ms`, `stale_age_ms`, `metadata_url`, `min_context_tokens`, `capabilities`, `explicit_allow`, `protocol_overrides` | Free-model discovery policy. |
| `health` | `max_concurrent_probes`, `probe_interval_ms`, `max_probes_per_hour`, `initial_backoff_ms`, `max_backoff_ms`, `jitter_ratio`, `reset_grace_ms` | Retry/backoff/probe budgets. |
| `limits` | `turn_deadline_ms`, `max_attempts`, `max_output_bytes`, `max_content_bytes` | Request/turn limits. |
| `inference` | `request_deadline_ms`, `max_steps`, `max_output_tokens`, `global_concurrency`, `max_account_concurrency`, `wait_queue_depth` | Per-turn model work bounds. |
| `spending` | `instance_limit_usd`, `account_limit_usd`, `user_limit_usd`, `session_limit_usd`, `unknown_cost_policy` | Optional; no shipped default ceiling. Zero/absent means "no limit configured". |
| `persistence` | `database_path`, `durability`, `backup_dir`, `writer_queue_depth`, `backup_interval_ms`, `event_retention`, `event_retention_days` | Durable database and writer queue. `database_path` changes require a restart. |
| `exports` | `output_dir`, `formats`, `max_concurrent_jobs`, `max_export_bytes`, `redact_secrets` | Research exports. |
| `operations` | `log_level`, `health_check_interval_ms`, `shutdown_grace_ms`, `instrumentation` | Service logging and shutdown. |

Groups are pointers, so a present-but-empty group and an absent group are
distinguishable; absent optional groups inherit documented defaults elsewhere in
the code.

## Password file (secure mode)

`auth.password_file` points at a versioned JSON password file
(`schema`/`version` — this build accepts version 1). It is never plaintext and
must be mode `0600` (group/world access is refused). Each entry is
`{username, hash, enabled, identity}` where `hash` is a PHC Argon2id string
(`$argon2id$v=19$m=...,t=...,p=...$salt$hash`) and `identity` is a `usr_...`
user identity (`internal/adapters/config/password.go`).

Verification (`Verify`) performs exactly one Argon2id verification per admitted
attempt, using a dummy hash of matching cost for an unknown or disabled user, so
unknown-user, disabled-user, wrong-password, and no-file cases cost the same.
`PasswordHash` redacts itself in every string/JSON form.

Default hash parameters: `m=131072` (128 MiB), `t=3`, `p=4`, 16-byte salt,
32-byte tag (`config.DefaultParams`; ADR 0007). Parsing bounds are memory
8 MiB–512 MiB, iterations 1–8, parallelism 1–16. Passwords are read from a TTY
or stdin (`ReadPassword`), never a command-line argument. User
add/set-password/enable/remove operations exist on the admin service
(`internal/admin`) and the `PasswordStore`; see the operations guide.

## Atomic reload

`config.Store` publishes a fully validated snapshot by a single pointer swap; a
failed load leaves the previous snapshot serving. A turn that pinned a
`*Snapshot` keeps it for its lifetime; new sessions and new turns see the
current one. Hot-reloadability is **not** classified per field: bind address,
engine binary, and storage migrations require a restart (ADR 0009, PLAN 11).

## Schema coverage (what this build does *not* accept yet)

`schemas/configuration.schema.json` is contract version 1 with the baseline
groups; PLAN 11 lists more. `providers`, `prompts`, `discovery`, `inference`,
and `operations` are modelled in Go as optional groups and are **schema-v2
candidates**, not part of the published contract (ADR 0009). There is currently
**no CI check that `examples/vibeshell.json` validates against the schema**
(ADR 0009, "Unverified items").

---
*Traceable to: `internal/adapters/config/{config.go,decode.go,validate.go,loader.go,secrets.go,password.go,store.go,doc.go}`, `schemas/configuration.schema.json`, `examples/vibeshell.json`, ADR 0009, ADR 0007.*
