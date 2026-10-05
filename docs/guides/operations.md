# Operations

How to run the shipped VibeShell service, what its startup/shutdown ordering is,
how readiness and recovery behave, how backup/restore works, and what the
operator surface currently exposes.

> Scope note: this guide describes **merged code** (`cmd/vibeshell`,
> `internal/admin`, `internal/adapters/sqlite`, `internal/observability`,
> `containers/Containerfile`). Deployment hardening (ADR 0008) is **not
> verified** and is called out below.

## 1. Run the container (one public SSH port)

The runtime image stage is built from `containers/Containerfile`:

- `debian:bookworm-slim` pinned by digest, `rootless Podman`, `linux/amd64`;
- a non-root `vibeshell` system user (UID 10001), `USER vibeshell`, `WORKDIR /home/vibeshell`;
- `apt-get install ca-certificates` (TLS roots for outbound provider calls);
- `EXPOSE 2222`, `STOPSIGNAL SIGTERM`, `ENTRYPOINT ["/usr/local/bin/vibeshell"]`;
- the `ENTRYPOINT` runs the default `run` subcommand.

Publish **only** the SSH port. The service makes outbound TLS requests for
permitted provider/catalogue operations; generated applications get no network
capability. Never mount the container-engine socket or unrelated host
directories.

Build/run (Podman-first; the flags are Docker-compatible by design):

```powershell
podman build -f containers/Containerfile --target runtime -t vibeshell .
podman run --name vibeshell -p 2222:2222 \
  -v vibeshell-state:/var/lib/vibeshell \
  -v <config-dir>:/etc/vibeshell:ro \
  vibeshell
```

Configuration default is `/etc/vibeshell/vibeshell.json`; the database path is
`persistence.database_path` (example: `/var/lib/vibeshell/world.db`). Secrets
resolve from mounted files only inside a `-secret-dir` allowed directory.

## 2. Startup and shutdown ordering

Startup order (`cmd/vibeshell/component.go` `build` then `start`), matching
PLAN 12.3:

1. Load and validate the configuration snapshot (`openSnapshot`).
2. Open the database, open the event store, run migrations, build backup and
   recovery adapters.
3. Recover incomplete sessions/turns (`Recovery.RecoverIncomplete`; best-effort
   — a failure is logged but does not prevent serving).
4. Initialize the sandbox engine.
5. Build the provider gateway, routing/health, terminal renderer, presentation.
6. Load or generate the SSH host key; open the password store in secure mode.
7. Bind the SSH listener during construction, so a port conflict fails startup
   before any goroutine runs.
8. `start` marks SSH ready, logs `vibeshell ready`, and serves until a signal.

Shutdown (`component.shutdown`, idempotent), triggered by SIGINT/SIGTERM via
`signal.NotifyContext`:

1. Stop accepting new sessions (readiness for SSH → degraded).
2. Coordinator shutdown with the configured grace period
   (`operations.shutdown_grace_ms`, default 5000 ms).
3. Close the event store (flush queues), close the sandbox engine, close the
   database.

## 3. Readiness

Readiness tracks a fixed set of subsystems (`internal/observability/readiness.go`):
`config`, `storage`, `provider`, `sandbox`, `ssh`, `terminal`, `routing`,
`health`. It deliberately exposes **no** provider/account details. A running
service can be *degraded* rather than unavailable: with no account configured,
`provider` is degraded and generation is unavailable, while locally runnable
app interactions may continue. A new unknown app or MOTD needing inference gets
a bounded, truthful service-unavailable message; it is never presented as a
success (PLAN 12.3).

The `vibeshell status` subcommand is the internal-only status path (no second
port). It reports `config`, `presentation`, `storage`, `provider`, and `ssh`
with a status and a short detail, e.g.:

```
subsystem   status     detail
config      healthy    validated /etc/vibeshell/vibeshell.json
presentation healthy   MOTD prompt /etc/vibeshell/prompts/v1/motd.txt
storage     healthy    schema v1, 0 events
provider    degraded   no account configured; generation unavailable
ssh         ready      listen 0.0.0.0:2222
```

## 4. Backup and restore

Backup uses **`VACUUM INTO`**, so it includes WAL state and every referenced
content blob — never a bare copy of a live `.db` file. A backup is a
self-contained directory (`internal/adapters/sqlite/backup.go`):

```
<dest>/manifest.json    format version, schema version, logical fingerprint
<dest>/vibeshell.db     the VACUUM INTO snapshot
<dest>/extras/<name>    sidecar host files (e.g. the SSH host key)
```

The manifest carries `logical_fingerprint`: for every authoritative table, the
sorted per-row hashes folded into one digest, so it is independent of physical
row order (which `VACUUM` legitimately changes).

- **Restore is accepted only when the rebuilt database's logical fingerprint
  matches the manifest, never on a file checksum.** A tampered backup is
  rejected even when SQLite reports the file structurally intact.
- `VerifyBackup` reopens the backup, checks `PRAGMA integrity_check`,
  foreign-key violations, schema version, and the fingerprint, and verifies each
  sidecar against its recorded hash/size/mode.
- `RestoreBackup` verifies **before** touching the destination; on any
  fingerprint mismatch it deletes the rebuilt database, so a partial restore is
  never left behind.
- Default backup extras include the SSH host key (`cmd/vibeshell/builders.go`).

`IntegrityCheck` runs `PRAGMA integrity_check`, `pragma_foreign_key_check`,
schema version, logical fingerprint, and per-table row counts.

## 5. Recovery

Startup recovery finds sessions with a start event but no end/recovery marker
and marks them (and their recorded turns) with a durable `session.recovered`
event; repeated runs are no-ops (`internal/adapters/sqlite/backup_recovery.go`,
and `internal/admin/recovery.go`). Crash/consistency behavior is verified by
`tests/integration/receipts`:

- staged-but-uncommitted changes leave no node;
- a committed change survives a process stop and reopen with exact bytes;
- partial emission (committed world, no transport-write outcome) recovers
  truthfully with no fabricated success and the mutation applied exactly once;
- a conflicting re-drive commits no duplicate mutation.

## 6. Admin operations — current surface

`internal/admin` is an application service that performs no I/O of its own and
is **not reachable from the simulated shell**. It exposes: configuration
validation, user/hash maintenance, session listing, research export delegation,
route/account status, one bounded due-probe admission, an integrity check,
backup/restore, startup recovery, and app-version rollback. Operations whose
dependency is nil report a typed unavailable error instead of pretending to run.

**Admin CLI:** `cmd/vibeshell` exposes an internal-only `admin` subcommand —
`admin config validate`, `admin user list|add|set-password|enable|disable|remove`,
`admin sessions list`, `admin export`, `admin status`, `admin probe trigger|run`,
`admin integrity`, `admin backup`, `admin restore`, and `admin app rollback` —
invocable through `podman exec` and never as a simulated shell command. Passwords
are read from a TTY/stdin, never from argv. Operations whose dependency is nil
report a typed unavailable error instead of pretending to run. The health store
used at composition remains in-memory (`memoryHealthStore`), so route/account
health does not survive a restart (ADR 0011).

## 7. Observability and redaction

Operational logs and metrics are separate from the permanent research-event
stream (`internal/observability/doc.go`): structured, low-cardinality,
redacted logs via `slog`; metrics with bounded label values; readiness naming
failing subsystems only. Logs never contain raw commands, usernames, file
contents, or credentials. `Secret`/`PasswordHash` redact themselves in every
string and JSON form. Research events (a separate concern) may contain
user-supplied content as data and are redacted only for known secret shapes;
exports redact according to `domain.RedactionPolicy` and the bundle records a
`secrets_excluded` decision.

Log level is `operations.log_level` (`debug`/`info`/`warn`/`error`).

## 8. Not verified

- **Deployment hardening (ADR 0008) is unverified.** The runtime stage exists in
  `containers/Containerfile`, but no reproducible check of non-root/read-only
  rootfs/dropped capabilities/PID+memory limits/SIGTERM delivery against the
  **real service binary** has passed. Do not describe the hardened posture as
  proven.
- Docker compatibility is documented but has not been executed.
- Hot-reload vs restart-required configuration is not classified.
- Disk-full / storage-failure behavior (PLAN 10.3 "stop accepting new semantic
  work") is untested (ADR 0004).

---
*Traceable to: `cmd/vibeshell/{main.go,commands.go,component.go,builders.go}`, `internal/admin/*`, `internal/adapters/sqlite/{backup.go,backup_recovery.go}`, `internal/observability/*`, `containers/Containerfile`, `tests/integration/receipts/README.md`, ADR 0004, ADR 0008, ADR 0009, ADR 0011.*
