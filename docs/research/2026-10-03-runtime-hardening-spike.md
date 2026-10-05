# Runtime Hardening Spike (2026-10-03)

**Scope:** qualify the hardened `runtime` image from `containers/Containerfile`
under rootless Podman, and determine why the first qualification (8/8 PASS) was
reported as 6/8 on an independent re-run.

**Environment:** rootless Podman 5.8.3/5.8.7 (netavark), WSL2 kernel 6.18.33.2.
Image `localhost/vibeshell-runtime:hardening`, digest
`sha256:d5011467a24cd7613e0353a669abf25ba2db14956e1244a4e70adfe147823d98`.
Worktree `.worktrees/runtime-hardening`, branch `agent/runtime-hardening`.

## Method

`experiments/runtime-hardening/run-hardening-tests.ps1` is the orchestrator and
owns every verdict. It starts a hardened container and a control container that
differs only by the omitted `--cap-drop=ALL`, prints the exact command and
observed value for each check, and writes `receipts/hardening-receipt.txt`.
`test-hardening.sh` runs inside the container and only observes (no PASS/FAIL).

## Result

Two consecutive clean runs: **10 PASS, 0 FAIL, 1 NOT CHECKED** (exit 0).
See `receipts/hardening-report.md` for the per-check evidence table.

## Findings

1. **The original 8/8 claim was not reproducible, and neither was the 6/8
   re-run.** The two failing checks in the re-run were not measuring what they
   claimed:
   - *Capabilities (raw socket):* `perl` `SOCK_RAW`/`IPPROTO_ICMP` returns
     `Protocol not supported` for **both** the hardened and the control
     container, including the control as uid 0 with `CAP_NET_RAW` effective. It
     is a network-namespace refusal, not a capability denial, so it can neither
     pass nor fail `--cap-drop=ALL`.
   - *SIGTERM:* the old test signalled a backgrounded shell and treated any exit
     status as a pass; it never required a handler's output to be observed.
2. **Attributable capability check.** Read `CapEff`/`CapBnd` from
   `/proc/self/status`. Hardened: `CapEff=CapBnd=0`. Control:
   `CapEff=CapBnd=00000000800405fb` (11 capabilities including `CAP_CHOWN`,
   `CAP_NET_RAW`, `CAP_SYS_CHROOT`). `chown` as uid 0 is denied in the hardened
   container and allowed in the control, so the drop is attributable to the flag.
3. **SIGTERM is handled correctly by the runtime.** `podman stop --time 3` on a
   container whose shell entrypoint installs a `TERM` handler yields exit code 0
   and `SIGTERM_HANDLED` in the log (no SIGKILL escalation). A handler-less PID 1
   is SIGKILLed (`137`) after the timeout, which is the documented namespace
   behaviour, not a container defect.
4. **Remaining unverified.** The image entrypoint is a stub; real service-level
   SIGTERM handling is NOT CHECKED. Docker was never executed (not installed):
   Docker compatibility is explicitly UNVERIFIED.

## Deliverable

- `experiments/runtime-hardening/run-hardening-tests.ps1` (fixed orchestrator)
- `experiments/runtime-hardening/test-hardening.sh` (observe-only inventory)
- `experiments/runtime-hardening/receipts/hardening-report.md` and
  `hardening-receipt.txt`

No file under `containers/`, `scripts/`, `cmd/`, `internal/`, or the root
`go.mod` was modified.

---

## Re-check: real service binary under the hardened flags (2026-10-04)

**Scope:** close ADR 0008's NOT-CHECKED item against the composed service.

**Environment:** rootless Podman 5.8.3, WSL2. Runtime image rebuilt from the
current `containers/Containerfile` (`runtime` target):
`localhost/vibeshell-runtime:real`, id
`a54e5b20f0f5aa94393a55d2946909f4c13d72cf7b53e818f130998c33197bf0`, digest
`sha256:3dc0dbf7a49b5125c91630cf6ba337f5d40cff160281424a975814d78147033a`,
`Stopsignal=SIGTERM`. Worktree `.worktrees/runtime-real`, branch
`agent/runtime-real`.

**Method:** `experiments/runtime-hardening/run-real-service-sigterm.ps1` is the
orchestrator. It writes a minimal valid public-mode config (one
provider/product/route/tier, no accounts, no prompts), mounts it read-only at
`/etc/vibeshell/vibeshell.json`, and runs the image's real entrypoint
`vibeshell run` under `--user 10001 --read-only --tmpfs /tmp --cap-drop=ALL
--security-opt no-new-privileges --memory=512m --pids-limit=100 --publish
127.0.0.1::2222`, with a fresh named volume at `/var/lib/vibeshell` for the host
key and database (PLAN 12.2 persistent storage). It opens a real SSH session
with the Windows OpenSSH client (stdin held open so the channel stays
established), then sends SIGTERM with `podman stop --time 5` and reopens the
database from a new process to inspect durable state.

**Result — 4 PASS, 0 FAIL (exit 0), reproduced on two consecutive clean runs:**

| # | Check | Result | Observed |
|---|-------|--------|----------|
| 1 | Service starts and reaches readiness under the hardened flags | **PASS** | `vibeshell ready` logged; `127.0.0.1:<port> -> 2222/tcp` published |
| 2 | An open SSH session is recorded before shutdown | **PASS** | `admin sessions list` shows one row `turns=1 ended=false` |
| 3 | Real binary handles SIGTERM and exits cleanly (not 137) | **PASS** | exit code `0`; `podman stop` returned in 0.72–0.83 s; logs `"shutdown":"begin"` then `"shutdown":"complete"` |
| 4 | Session open at SIGTERM is durably ended | **PASS** | same session now `ended=true` after reopening the volume DB |

**Findings:**

1. **The real binary installs a handler and shuts down cleanly.** `runCommand`
   uses `signal.NotifyContext(SIGINT, SIGTERM)`; on SIGTERM it runs
   `component.shutdownAll` (stop admission, wait for in-flight handlers within
   `operations.shutdown_grace_ms`, end live sessions, flush and close the event
   store). Exit code is `0`, never the `137` a handler-less PID 1 produces.
2. **Session ends survive the shutdown.** The session opened before SIGTERM is
   recorded with `session.end` and shows `ended=true` when the database is
   reopened, which is PLAN 12.3's durability requirement.
3. **Attack-surface reduction measured, not adopted.** A `scratch` variant
   (`Containerfile.minimal`: multi-stage copy of only the binary and
   `/etc/ssl/certs/ca-certificates.crt`, numeric `USER 10001:10001`) builds to
   **16.3 MiB** versus **99.5 MiB** for the shipped image and removes
   `apt`/`apt-get`/`dpkg`, `bash`/`sh`/`dash`, `perl`, `openssl`,
   `tar`/`gzip`, and all 8 setuid binaries by construction. It starts, reaches
   readiness, and SIGTERMs cleanly. Cost: no `/etc/passwd` account, no
   in-container shell for operations/debugging, and no package-level CA refresh
   (the bundle is copied from the build stage). The shipped Containerfile is
   unchanged.

**Remaining uncertainty:** Docker was not executed. Seccomp syscall coverage is
unaudited. The session check exercises `echo` (identity/fallback path), not a
full interactive turn lifecycle. The `scratch` variant was not validated for
outbound provider TLS against a live endpoint.
