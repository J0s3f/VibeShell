# ADR 0007: Secure-mode authentication with bounded Argon2id

Status: accepted. The mechanism is accepted; the calibrated numbers are
provisional and must be re-confirmed on a limited reference host before they are
frozen.

Date: 2026-10-03.

## Context

PLAN 4.2 requires a versioned password file of encoded Argon2id hashes, hash
parameters calibrated in the container and bounded when read, dummy verification
to reduce obvious timing differences, and bounded authentication concurrency,
attempts per connection, and abusive connection rates. These are transport
controls, independent of AI.

The risk being bounded is twofold. A malformed or hostile hash string must not be
able to request unbounded memory or CPU. An unauthenticated client must not be
able to distinguish "no such user" from "wrong password", or to turn login into
a memory or CPU amplifier.

## Decision

**Hash encoding.** PHC string form
`$argon2id$v=19$m=<m>,t=<t>,p=<p>$<salt>$<hash>`, using
`golang.org/x/crypto/argon2`. Parsing is strict: exactly five `$`-separated
fields, algorithm `argon2id` only, `v=19` only, the parameter block exactly `m,t,p`
in that order with no extra parameters, and unpadded canonical standard base64
whose re-encoding must match the input (which rejects non-canonical trailing
bits). Anything else is a malformed hash or an out-of-range parameter.

**Parameter bounds at parse time.** memory 8 MiB-512 MiB, iterations 1-8,
parallelism 1-16, salt 8-64 bytes, hash 16-64 bytes. A hash below the floor is
not credible work; the ceilings cap one hash's allocation. Aggregate work is
bounded separately by admission control.

**Verification.** Derive the candidate key with `argon2.IDKey` and compare with
`crypto/subtle.ConstantTimeCompare`. Run **exactly one** verification per
admitted attempt: the stored hash when the account exists and is enabled,
otherwise a dummy hash with matching parameters and salt length. `DummyVerify`
returns nothing, so callers cannot branch on it. Unknown user, disabled user
with the correct password, wrong password, and "no password file loaded" all
return one `ErrAuthFailed` after the same work.

**Rejection order.** Checks that need no hashing — attempt budget, abuse
penalty, admission saturation — run *before* any Argon2id work. They are
deliberately cheap and distinguishable, because they only occur after prior
failures.

**Password file.** A versioned JSON document (`schema: "vibeshell/password-file"`,
`version: 1`) with username, encoded hash, enabled flag, and identity reference.
Parsing is strict: unknown fields, trailing content, wrong schema or version,
malformed or out-of-bounds hashes, empty/over-long (64 B) or control-character
usernames, empty or invalid `identityRef`, and **duplicate usernames** compared
by exact bytes with no case folding. Loading validates completely before an
atomic pointer swap, so a failed load leaves the previous snapshot serving.
Disabled entries parse and load normally but take the dummy path and fail even
with the correct password. The file never contains plaintext.

**Concurrency controls.** Three transport-agnostic controls, with the SSH adapter
supplying the connection limiter and source identity: admission (at most
`MaxConcurrent` hashes at once, at most `MaxWaiting` queued, `ErrBusy`
otherwise, cancellation honored while waiting); per-connection failed attempts
(`MaxAttempts`, reset by success); and a per-source abuse rate (failures within
a window start a penalty period during which `ErrAbusive` is returned before
hashing, with a bounded source table).

**Provisional defaults.** `m=131072, t=3, p=4` (128 MiB, 3 passes, 4 lanes),
16 B salt, 32 B tag; `MaxConcurrent=2`; `MaxWaiting=16`; `MaxAttempts=6`
(sshd `MaxAuthTries` parity); abuse window 60 s / threshold 10 failures /
penalty 300 s; `MaxTrackedSources=1024`. Username <= 64 B and `identityRef`
<= 128 B, valid UTF-8, no control bytes.

## Alternatives considered

- No dummy verification: rejected on measurement. A naive lookup miss returned in
  0.001 ms against ~129-149 ms for a real verification — a 100% distinguishing
  gap. Dummy verification reduced the residual to 0.1-5.9% across two runs.
- Higher Argon2id cost for its own sake: rejected. Candidates at 192 MiB and
  above showed no benefit inside the 100-250 ms target and one was pushed far
  above it by host load.
- `MaxConcurrent=4`: rejected on measurement, not intuition. Each hash already
  runs `p=4` lanes, so 4 concurrent hashes oversubscribe 4 CPUs (16 runnable
  lanes): 4 slots finished a 64-attempt burst **slower** than 2 (10.85 s vs
  9.04 s) at double the peak heap (1.28 GiB vs 512 MiB).
- bcrypt/scrypt: rejected by PLAN 3.2's choice of Argon2id; no comparison was
  run, so this is a selection, not a measured win.
- Reading parameters from the file without bounds: rejected; that is the
  amplification path the bounds exist to close.
- Reloading the password file in place: rejected. Validate-then-swap keeps a
  running service on its last known-good configuration.

## Consequences

- A legitimate login costs roughly 125-130 ms of CPU on the reference shape.
  That is a deliberate, measured latency cost for not leaking account existence.
- Peak memory is `MaxConcurrent x hash memory`: ~256 MiB at the recommended
  parameters, but up to 1 GiB at the parser ceiling if an operator configures
  512 MiB hashes. Admission control, not the parser, is the real ceiling.
- Rejections that skip hashing are observably cheaper than a verification. That
  is intended and acceptable **because** they only occur after prior failures;
  it is worth remembering if the ordering ever changes.
- Bounded admission means an authentication flood is refused rather than queued
  indefinitely, which converts a resource problem into a visible error.
- Users stored under older parameters keep their original cost until rehashed.
  Rehash-on-login and the admin rehash operation are not built.
- The password file is a whole-document swap. Removing a user prevents new
  sessions once a new snapshot is published, but does not disconnect existing
  sessions; that requires an explicit admin operation.

## Evidence

- Research: `docs/research/2026-10-03-auth-argon2-spike.md`.
- Receipts: `experiments/auth-argon2/receipts/` — `race-test.log` (35 rejection
  cases plus round-trip/bounds/accept, PASS), `fuzz.log`
  (`FuzzParseEncoded`, 2,122,211 executions, 0 crashes), `calibration.log` and
  `calibration-run2.log`, `timing-gap.log` and `timing-gap-run2.log`,
  `burst.log`, `static-check.log`, `environment.log`.
- Timing gaps, dummy path: run 1 5.931 ms (4.0%) unknown-vs-correct and
  8.037 ms (5.4%) unknown-vs-wrong; run 2 0.145 ms (0.1%) and 0.359 ms (0.3%).
  Naive path: 148.500 ms (100%) and 128.551 ms (100%).
- Calibration medians for `m=131072,t=3,p=4`: 125.8 ms (run 1) and 128.8 ms
  (run 2), target 100-250 ms.
- Burst, 64 attempts: `MaxConcurrent=4` 10.85 s / 5.9 auth/s / ~1.28 GiB peak;
  `MaxConcurrent=2` 9.04 s / 7.1 auth/s / ~512 MiB peak. Abuse fast path: 1000
  rejections in 0.109 ms total (~0.11 us each, no Argon2id work).
- Dependency: `golang.org/x/crypto v0.57.0` (indirect `golang.org/x/sys
  v0.48.0`), pinned in the nested module; all tests run offline.
- Commits: `2527809` (spike), merged as `e2f502e`.

## Unverified items and limitations

- **Calibration was measured on a shared host with no cgroup limits.** The
  container reported 16 CPUs and 15.9 GiB with no CPU or memory limit applied,
  against a 4 vCPU / 8 GiB reference host. Measurement runs were pinned with
  `taskset -c 0-3` (`NumCPU=4`, `GOMAXPROCS=4`), but the 8 GiB memory limit was
  never enforced in-container. Per-hash memory is bounded by the parameter cap,
  and peak heap under burst is reported, so the risk is bounded — but the
  numbers are not from the deployment target.
- Run-to-run variance is real: the same candidate's calibration medians ranged
  99-129 ms across runs, and two 192 MiB and 256 MiB candidates were pushed far
  above target by host load in run 1. The recommended parameters must be
  re-confirmed in a CPU- and memory-limited container on the real reference host
  before being frozen.
- No live SSH integration was exercised. How `ErrAttemptsExceeded`, `ErrAbusive`,
  and `ErrBusy` map to client-visible authentication failures is **undecided**
  and should be confirmed with the SSH adapter task; the safe default is that all
  three look identical to a client.
- The abuse guard keys on a transport-supplied source string (a remote address in
  practice). What that key should be behind a proxy is unresolved, and whether an
  attacker can exploit `ErrBusy` attempts not counting against the failure budget
  is unconfirmed.
- An empty password file (zero users) is currently **valid** — nobody can log in.
  Whether that is a lockout or an operator mistake to reject is an open question.
- Fuzzing covered the encoded-hash parser only. The password-file and
  admission-control paths were not fuzzed.
- Rehash-on-parameter-change was not built, so a parameter change leaves a
  residual per-user timing and cost difference until users are rehashed.