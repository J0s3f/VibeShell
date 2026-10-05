# VibeShell Runtime Hardening Qualification Report

**Date:** 2026-10-03
**Worktree:** `D:\code\vibeshell\.worktrees\runtime-hardening`
**Branch:** `agent/runtime-hardening`
**Containerfile:** `containers/Containerfile` (runtime target)
**Image:** `localhost/vibeshell-runtime:hardening`
**Image ID:** `4923b7a157fee86f8f245a6f4647922cdc2dbbe6c1148da6edba8ea60abd2beb`
**Image Digest:** `sha256:d5011467a24cd7613e0353a669abf25ba2db14956e1244a4e70adfe147823d98`
**Host:** rootless Podman (client 5.8.3, server 5.8.7, netavark, WSL2 kernel 6.18.33.2)

This report describes what the orchestrator actually ran and observed. Every
verdict below is owned by `run-hardening-tests.ps1`, which prints the exact
command and observed value for each check. The machine-readable receipt is
`receipts/hardening-receipt.txt`. Two consecutive clean runs from this branch
produced the same result: **10 PASS, 0 FAIL, 1 NOT CHECKED** (exit code 0).

---

## 1. Build and run commands

```bash
podman build --file containers/Containerfile --target runtime --tag localhost/vibeshell-runtime:hardening .

podman run -d --name <hardened> \
  --user 10001 --read-only --tmpfs /tmp \
  --cap-drop=ALL --security-opt no-new-privileges \
  --memory=512m --pids-limit=100 \
  --publish 127.0.0.1::2222 \
  --entrypoint sleep localhost/vibeshell-runtime:hardening 300
```

A control container is started with **identical flags except `--cap-drop=ALL`**.
The capability check compares the two, so a result that is really caused by the
non-root user or the rootless user namespace cannot be reported as a pass.

> The image `ENTRYPOINT` is `/usr/local/bin/vibeshell`, which prints
> `VibeShell on VibeOS version dev` and exits 0. It is a stub with no signal
> handler, so long-running inspection used `--entrypoint sleep` / `--entrypoint sh`.

---

## 2. Check results

| # | Check | Result | Observed evidence |
|---|-------|--------|-------------------|
| 1 | Service process is not root | **PASS** | `id -u` = `10001` (exit 0); inventory shows `user=vibeshell` |
| 2 | Root filesystem is read-only | **PASS** | `touch /vibeshell-ro-probe` fails with `Read-only file system` (`touch_rc=1`); `/proc/mounts` root is `overlay / ro` |
| 3 | Capability set is empty and `--cap-drop=ALL` is attributable | **PASS** | hardened `CapEff=0 CapBnd=0`; control `CapEff=CapBnd=00000000800405fb` (11 caps incl. `CAP_CHOWN`, `CAP_NET_RAW`); `chown` as uid 0 denied in hardened, allowed in control |
| 4 | PID limit is enforced | **PASS** | `/sys/fs/cgroup/pids.max` = `100` |
| 5 | Memory limit is enforced | **PASS** | `/sys/fs/cgroup/memory.max` = `536870912` (512 MiB) |
| 6 | Only the SSH port is published, on loopback | **PASS** | `{"2222/tcp":[{"HostIp":"127.0.0.1","HostPort":"..."}]}`; image `EXPOSEs` = `{"2222/tcp":{}}` |
| 7 | `no-new-privileges` reached the kernel | **PASS** | `/proc/self/status` `NoNewPrivs=1` |
| 8 | No host filesystem/directory/engine socket mounted | **PASS** | 36 mounts, none with a filesystem type outside the container-local allowlist |
| 9 | A process that installs a SIGTERM handler runs it | **PASS** | `exit 0`, output `READY | HANDLER_RAN`, never reached `REACHED_AFTER_SIGNAL` |
| 10 | `podman stop` delivers STOPSIGNAL before SIGKILL timeout | **PASS** | `podman stop --time 3` → container exit code `0`, logs `READY | SIGTERM_HANDLED` |
| 11 | SIGTERM handling of the real service binary | **NOT CHECKED** | Image entrypoint is the stub; it installs no handler. A handler-less PID 1 ignored SIGTERM and was SIGKILLed (`exit 137`) after the 3 s timeout |

### Why the original 8/8 claim did not hold

The first qualification reported 8/8, but two of its checks could not fail:

- **Check 3 (capabilities, "raw socket").** `perl ... SOCK_RAW` fails with
  `Protocol not supported` in **both** the hardened and the control container,
  including the control as uid 0 with `CAP_NET_RAW` effective. The refusal is a
  network-namespace restriction, not a capability denial, so that probe cannot
  distinguish `--cap-drop=ALL`. The check above instead reads `CapEff`/`CapBnd`
  from `/proc/self/status` and confirms the drop with an attributable operation
  (`chown` as uid 0). The raw-socket probe is retained as a labelled observation.
- **Check 7 (SIGTERM).** The old script backgrounded a shell, signalled it, and
  accepted *any* exit status as a pass, so it never asserted that a handler ran.
  The check above requires the handler's own output on `podman stop` and is
  paired with the handler-less PID-1 control (`137`).

The independent re-run's two failures are therefore explained by these two
non-discriminating checks, not by a regression in the image.

---

## 3. Docker compatibility

**Status: UNVERIFIED.** Docker is not installed on this host, so no Docker
command was run and no Docker result is claimed. The equivalent invocation is
documented only as a hypothesis:

```bash
docker run -d --name <hardened> \
  --user 10001 --read-only --tmpfs /tmp \
  --cap-drop=ALL --security-opt no-new-privileges \
  --memory=512m --pids-limit=100 \
  --publish 127.0.0.1::2222 \
  --entrypoint sleep localhost/vibeshell-runtime:hardening 300
```

The Containerfile is a plain multi-stage Dockerfile and every flag used is
Docker-compatible, but nothing here demonstrates a Docker run.

---

## 4. Attack surface (observed inventory)

Present: `apt`/`apt-get`/`dpkg`, shells `bash`/`sh`/`dash`, `perl`, `openssl`,
`tar`/`gzip`; 8 setuid binaries (`chfn`, `umount`, `newgrp`, `mount`, `passwd`,
`su`, `gpasswd`, `chsh`); `/var/tmp` is world-writable. Absent: compilers, VCS,
other package managers, other interpreters, network clients, other archives.
Seccomp is active (`Seccomp=2`, 2 filters) but which syscalls it blocks was not
audited. The inventory script only observes; it prints no PASS/FAIL.

---

## 5. Limitations and open questions

1. **Stub service.** The entrypoint prints a version line and exits; there is no
   long-running supervisor, so service-level SIGTERM handling, SSH transport and
   steady-state behaviour are untested. Check 11 stays NOT CHECKED until the real
   service exists.
2. **Capability semantics in rootless Podman.** `CapEff` is 0 for the configured
   non-root user regardless of `--cap-drop`, which is why the check also compares
   `CapBnd` and uid-0 `chown` against a control container.
3. **Docker unverified** (Section 3).
4. Open questions: remove `apt`/`dpkg`, strip setuid binaries, remove `perl`?
   None of these are decided here.

---

## 6. Receipts

- **Orchestrator:** `experiments/runtime-hardening/run-hardening-tests.ps1`
- **In-container inventory (observe-only):** `experiments/runtime-hardening/test-hardening.sh`
- **Raw output:** `experiments/runtime-hardening/receipts/hardening-receipt.txt`
