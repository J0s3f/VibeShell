# ADR 0008: Runtime container hardening

Status: **Accepted.** The qualification was corrected and merged (`1186933`,
spike commit `8ab827e`): 10 verdict-bearing checks PASS, 0 FAIL, and the
real-service SIGTERM item — previously NOT CHECKED because the entrypoint was a
stub — is now checked and **PASS** (`agent/runtime-real`, see
`experiments/runtime-hardening/run-real-service-sigterm.ps1`, 4 PASS / 0 FAIL on
two consecutive clean runs). An independent re-run by the main agent reproduced
the original result. Docker execution remains unverified.

Date: 2026-10-03 (updated 2026-10-04).

## Context

PLAN 12.2 requires a non-root process under rootless Podman with a read-only
root filesystem, bounded temporary storage, dropped capabilities, configured
CPU/memory/PID limits, persistent database/content/host-key mounts, read-only
configuration/prompt/password/secret mounts, only the SSH port published, direct
signal forwarding, and Podman-first commands with Docker-compatible equivalents.

The first spike claimed 8/8 PASS but was not reproducible and was reverted
(`43a3dc1`). A corrected spike replaced the unreliable checks — the capability
set is now read from `/proc/self/status` (`CapEff`/`CapBnd`) with an attributable
control container, and SIGTERM is verified with a real handler plus `podman stop`
— and produced a reproducible receipt.

## Decision

- Target the runtime image in `containers/Containerfile` (`runtime` stage):
  `debian:bookworm-slim` pinned by digest, a `vibeshell` system user at UID 10001,
  `USER vibeshell`, `EXPOSE 2222`, `STOPSIGNAL SIGTERM`, and a `CGO_ENABLED=0`
  static binary copied from the build stage.
- Demonstrated under rootless Podman: non-root UID 10001; read-only root
  (`overlay / ro`, `touch` fails); **empty capability set** with `--cap-drop=ALL`
  (control container shows the non-empty baseline, and a uid-0 `chown` is denied
  only when hardened); `pids.max=100`; `memory.max=536870912` (512 MiB); only
  `2222/tcp` published on `127.0.0.1`; `NoNewPrivs=1`; no host filesystem or
  engine-socket mounts; SIGTERM delivered to a handler and handled before the
  `podman stop` SIGKILL timeout.
- Require the equivalent `docker run` flag set and record it as **unverified**
  unless Docker is actually executed; the container-only rule keeps Docker off
  the host.
- The **real service binary's** SIGTERM handling is **checked and passing**: the
  composed `cmd/vibeshell` runtime image, started with a minimal valid config
  under the hardened flags, installs a `signal.NotifyContext(SIGINT,SIGTERM)`
  handler, exits 0 (not 137) on `podman stop --time 5`, and durably records
  `session.end` for a session open at signal time.

## Alternatives considered

- Accepting the reverted 8/8 report: rejected. A result that could not be
  reproduced is not evidence, and the report itself disclosed that the service
  entrypoint was a stub and that SIGTERM was tested through a shell trap.
- Distroless or scratch runtime: evaluated as a measurement artifact
  (`experiments/runtime-hardening/Containerfile.minimal`), not adopted. A
  `scratch` image with only the binary and the CA bundle and a numeric UID is
  16.3 MiB versus 99.5 MiB and removes `apt`/`dpkg`, the shells, `perl`,
  `openssl`, `tar`/`gzip`, and all setuid binaries by construction. It still
  starts and SIGTERMs cleanly, but it loses the `/etc/passwd` account, any
  in-container shell for operations/debugging, and package-level CA refresh.
  Tradeoff recorded; the shipped Containerfile is unchanged.
- Multi-stage copy of only `ca-certificates` and the binary: demonstrated by the
  same artifact; the CA bundle is copied from the build stage's
  `/etc/ssl/certs/ca-certificates.crt`.

## Consequences

- Deployment hardening is verified for the enumerated properties, including the
  real service binary's clean SIGTERM shutdown and durable session end. The
  residual items — Docker execution, seccomp coverage, and attack-surface
  reduction — are unverified or unresolved and must not be overstated.
- Present attack surface in the image: `apt`/`apt-get`/`dpkg`, `bash`/`sh`/`dash`,
  `perl`, `openssl`, `tar`/`gzip`, and 8 setuid binaries. Mitigations in place:
  non-root, `--cap-drop=ALL`, read-only rootfs, `no-new-privileges`, PID and
  memory limits.

## Evidence

- Requirement source: `PLAN.md` 12.2, 12.3, 17; `AGENTS.md`.
- Artifact: `containers/Containerfile` (`runtime` target).
- Spike: `experiments/runtime-hardening/**`; receipt
  `experiments/runtime-hardening/receipts/hardening-receipt.txt`; corrected report
  `experiments/runtime-hardening/receipts/hardening-report.md`; merged at
  `1186933` (spike `8ab827e`). Superseded first attempt `7e4e580`, reverted by
  `43a3dc1`.
- Real-service re-check: `experiments/runtime-hardening/run-real-service-sigterm.ps1`;
  receipt `experiments/runtime-hardening/receipts/real-service-sigterm-receipt.txt`;
  attack-surface artifact `experiments/runtime-hardening/Containerfile.minimal`;
  branch `agent/runtime-real` (report in
  `docs/research/2026-10-03-runtime-hardening-spike.md`).

## Unverified items and limitations

- Docker compatibility is documented but has never been executed.
- Seccomp filter syscall coverage is unaudited (`Seccomp=2`, 2 filters observed).
- Attack-surface reduction is **evaluated but unresolved**: the `scratch` variant
  is measured (16.3 MiB, no `apt`/shells/`perl`/setuid) but not adopted; the
  decision to change the shipped image remains open because it costs the
  `/etc/passwd` account, in-container troubleshooting shell, and package-level CA
  refresh.
- No live SSH session was exercised inside the hardened image at the earlier
  spike; the real-service re-check does open one (`echo runtime-real-probe`) and
  confirms the session end is durable, but it does not exercise a full
  interactive command/turn lifecycle.
