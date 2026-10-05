# Secure-mode authentication spike (PLAN 4.2)

Date: 2026-10-03. Task: qualify secure-mode authentication — encoded Argon2id
hashes, timing-safe verification, a versioned password file, and bounded
authentication concurrency, before these numbers become contracts at A07.

- Worktree/branch: `.worktrees/auth-argon2`, `agent/auth-argon2`.
- Spike code: `experiments/auth-argon2/` — a self-contained nested Go module
  (`j0s.at/vibeshell/experiments/auth-argon2`) with its own `go.mod`/`go.sum`.
  Nothing outside that directory and this document changed.
- Dependency: `golang.org/x/crypto v0.57.0` (indirect `golang.org/x/sys
  v0.48.0`) pinned in the nested module. It was fetched once from
  `proxy.golang.org` during `go mod tidy`; every test runs offline.
- Container: `vibeshell-auth-argon2-dev` (this checkout's Podman container).
- No real secrets exist anywhere in this spike. Fixture passwords are
  obviously fake strings (for example `correct-horse-battery-staple-fake`),
  salts are `0123456789abcdef`, and receipts contain timings and test output
  only — no password values and no encoded hash values.

These are measurements and recommendations from a spike, not frozen
contracts; A07 decides what the implementation adopts.

## Environment and its caveats

| Property | Observed | Caveat |
| --- | --- | --- |
| Go | `go1.27.1 linux/amd64` in the dev container | pinned by `containers/versions.env` |
| CPUs visible to the container | 16 (`nproc`), no cgroup CPU limit | reference host is 4 vCPU, so measurement runs were pinned with `taskset -c 0-3` (`NumCPU=4`, `GOMAXPROCS=4` in the calibration log) |
| Memory visible to the container | 15.9 GiB, no cgroup memory limit | the 8 GiB reference limit is **not** enforced in-container; per-hash memory is bounded by the parameter cap, and peak heap under burst is reported below |
| Host load | shared with other work; run-to-run timing variance observed | absolute numbers need re-confirmation on a quiet, limited reference host (open question 4) |

## 1. Encoded Argon2id hash format with strict bounds

Format (PHC string style):

```
$argon2id$v=19$m=131072,t=3,p=4$<salt-base64>$<hash-base64>
```

`ParseEncoded` accepts exactly five `$`-separated fields, algorithm
`argon2id` only, `v=19` only, the parameter block exactly `m,t,p` in that
order with no extra parameters, and unpadded canonical standard base64
(re-encoding must match the input, which rejects non-canonical trailing
bits). Every value must pass the bounds; anything else is rejected with
`ErrMalformedHash` (syntax) or `ErrParamsOutOfRange` (bounds).

| Field | Bounds | Rationale |
| --- | --- | --- |
| memory `m` | 8 MiB … 512 MiB | below 8 MiB is not credible work; 512 MiB caps one hash's allocation |
| iterations `t` | 1 … 8 | caps CPU passes per hash |
| parallelism `p` | 1 … 16 | caps lanes per hash |
| salt | 8 … 64 bytes | PHC minimum 8; cap prevents oversized payloads |
| hash | 16 … 64 bytes | ≥128-bit tag; cap prevents oversized payloads |

Aggregate work is bounded separately: the admission controller caps how many
verifications run at once (section 5), so worst-case memory is
`MaxConcurrent × 512 MiB` at the parser cap and `2 × 128 MiB = 256 MiB` at
the recommended parameters.

Evidence (`receipts/`):

- `race-test.log`: 35 rejection table cases + round-trip, bounds, accept
  tests — PASS.
- `fuzz.log`: `FuzzParseEncoded` ran 2,122,211 executions in 20 s with 0
  crashes, 0 failures; the fuzz property asserts that every accepted value is
  in bounds and canonically re-encodable (`parse ∘ String ∘ parse` is
  stable) — PASS.

## 2. Timing-safe verification and dummy verification

Design:

- `Verifier.Verify` derives the candidate key with `argon2.IDKey` and
  compares via `crypto/subtle.ConstantTimeCompare`; the Argon2id work
  dominates the comparison.
- Exactly **one** verification runs per admitted attempt: the stored hash
  when the account exists and is enabled, otherwise the dummy hash (same
  parameters and salt length as the configured default, derived from an
  obviously fake constant password). `DummyVerify` returns nothing, so
  callers cannot branch on its result.
- Unknown user, disabled user with the correct password, wrong password, and
  "no password file loaded" all return the single `ErrAuthFailed` after the
  same amount of work.
- Rejections that need no hashing (attempt budget, abuse penalty, admission
  saturation) happen *before* any Argon2id work — those are deliberately
  distinguishable by cost because they only occur after prior failures.

Measured on `m=131072,t=3,p=4`, 25 interleaved rounds per condition,
`taskset -c 0-3`, without `-race` (instrumentation would dominate):

| Condition | Run 1 mean | Run 2 mean |
| --- | --- | --- |
| known user, correct password | 148.500 ms | 128.552 ms |
| known user, wrong password | 146.394 ms | 128.048 ms |
| unknown user (dummy verify) | 154.431 ms | 128.407 ms |
| naive lookup miss (no dummy) | 0.001 ms | 0.001 ms |
| **gap: unknown vs correct, dummy path** | **5.931 ms (4.0 %)** | **0.145 ms (0.1 %)** |
| gap: unknown vs wrong, dummy path | 8.037 ms (5.4 %) | 0.359 ms (0.3 %) |
| gap: unknown vs correct, naive path | 148.500 ms (100 %) | 128.551 ms (100 %) |

Conclusion: dummy verification reduces the observable unknown-user gap from
the full hash time (~129–149 ms, i.e. everything an attacker needs to
distinguish "no such user") to 0.1–5.9 ms of residual jitter, measured
across two runs (`receipts/timing-gap.log`, `receipts/timing-gap-run2.log`).
The test fails if the dummy-path gap exceeds half the real verification
cost, so deleting or mispricing the dummy work breaks the suite.

## 3. Parameter calibration (4-CPU pin, container)

`TestCalibrationReport`, 2 warmups + 7 timed runs per candidate, run twice
against the final code (`receipts/calibration.log`,
`receipts/calibration-run2.log`). Both runs PASS; medians in ms:

| Parameters | Hash size | Run 1 median | Run 2 median | In 100–250 ms target |
| --- | --- | --- | --- | --- |
| m=16384, t=2, p=1 | 16 MiB | 21.3 | 35.4 | no (fast) |
| m=19456, t=2, p=1 | 19 MiB | 23.9 | 33.1 | no (fast) |
| m=32768, t=3, p=2 | 32 MiB | 39.5 | 78.7 | no (fast) |
| m=65536, t=3, p=3 | 64 MiB | 78.2 | 95.1 | no (fast) |
| m=65536, t=4, p=4 | 64 MiB | 79.9 | 80.7 | no (fast) |
| **m=131072, t=3, p=4** | **128 MiB** | **125.8** | **128.8** | **yes** |
| m=131072, t=4, p=4 | 128 MiB | 162.4 | 163.2 | yes |
| m=196608, t=3, p=4 | 192 MiB | 514.2 (host load) | 236.8 | marginal |
| m=262144, t=3, p=4 | 256 MiB | 555.9 (host load) | 241.7 | top edge / over |

**Recommendation: `m=131072, t=3, p=4` (128 MiB, 3 passes, 4 lanes).**
It lands at ~126–129 ms median in both receipted runs (earlier runs of the
same candidate measured 99–125 ms), comfortably inside 100–250 ms, halves
peak memory against the next in-target candidate, and matches the
parameters used by the timing-gap and burst measurements. The alternative
`m=131072, t=4, p=4` (~162–163 ms, the most run-stable candidate) is the
pick if more work per login is wanted. Candidates at ≥192 MiB showed no
benefit inside the target and one run was pushed far above it by host load;
`p` has little effect on latency because the work is memory-bandwidth bound.

## 4. Versioned password file

JSON schema, version 1 (`schema: "vibeshell/password-file"`):

```json
{
  "schema": "vibeshell/password-file",
  "version": 1,
  "users": [
    {
      "username": "alice",
      "hash": "$argon2id$v=19$m=131072,t=3,p=4$<salt>$<hash>",
      "enabled": true,
      "identityRef": "sec-identity-alice"
    }
  ]
}
```

- Parsing is strict: unknown fields, trailing JSON/garbage, wrong
  schema/version, malformed or out-of-bounds hashes, empty or over-long
  (64 B) or control-character usernames, empty/invalid `identityRef`, and
  **duplicate usernames** (exact byte comparison — no case folding or
  normalization, per PLAN 4.1) are all rejected.
- `FileStore.Load` validates completely before atomically swapping the
  snapshot; a failed load keeps the previous snapshot serving. Entries are
  immutable after parse, so the snapshot can be shared lock-free.
- Disabled entries parse and load normally; at authentication they take the
  dummy path and fail with `ErrAuthFailed` even with the correct password.
- `TestPasswordFileNeverStoresPlaintext` asserts the encoded JSON contains
  neither a `password` field nor the fixture password.
- Empty file (zero users) is currently *valid* — nobody can log in. Whether
  to reject it as an operator mistake is open question 3.

Evidence: `receipts/race-test.log` — round-trip, duplicate rejection,
distinct-username acceptance, schema rejection, JSON-shape rejections,
seven invalid-entry cases, no-plaintext, and atomic-swap tests, all PASS.

## 5. Concurrency and admission prototype

Three independent controls, all transport-agnostic (the SSH adapter supplies
the connection's limiter and source identity):

1. **Admission** — at most `MaxConcurrent` Argon2id computations run at
   once; at most `MaxWaiting` attempts may queue; beyond that `ErrBusy`
   before any hashing. Cancellation while waiting is honored.
2. **Per-connection attempts** — at most `MaxAttempts` failed attempts per
   connection (`ErrAttemptsExceeded`); a successful login resets the budget.
3. **Abusive-rate path** — per-source failure rate: `Threshold` failures
   within `Window` start a `Penalty` during which `ErrAbusive` is returned
   before any hashing. The source table is bounded by `MaxTrackedSources`
   (prune expired, evict least-recently-touched).

Measured burst: 64 attempts (32 correct, 32 wrong passwords) released
simultaneously, `m=131072,t=3,p=4`, `taskset -c 0-3`, no `-race`
(`receipts/burst.log`, PASS):

| Bound | Wall | Throughput | Peak in-flight | Peak heap | Heap after GC |
| --- | --- | --- | --- | --- | --- |
| `MaxConcurrent=4`, waiting 64 | 10.85 s | 5.9 auth/s | 4 (bound held) | ~1.28 GiB | 242 KiB |
| `MaxConcurrent=2`, waiting 64 | 9.04 s | 7.1 auth/s | 2 (bound held) | ~512 MiB | 265 KiB |

Abusive-rate fast path in the same run: after reaching the penalty, 1000
consecutive attempts were rejected in 0.109 ms total (~0.11 µs each — no
Argon2id work).

Findings: each Argon2id hash already runs its `p=4` lanes, so four
concurrent hashes oversubscribe 4 CPUs (16 runnable lanes): the 4-slot
config finished the burst *slower* than 2 slots (10.85 s vs 9.04 s) and
doubled peak heap (1.28 GiB vs 512 MiB, consistent with
`MaxConcurrent × hash memory` plus Go's GC headroom), which is why the
default was lowered to 2 after this measurement. The burst also confirms
the bound itself: sampled peak in-flight never exceeded it, and everything
was reclaimed after GC.

### Recommended bounds (summary)

| Setting | Recommended | Basis |
| --- | --- | --- |
| Argon2id parameters | `m=131072, t=3, p=4`, 16 B salt, 32 B tag | calibration, section 3 |
| parser parameter caps | m ≤ 512 MiB, t ≤ 8, p ≤ 16, salt ≤ 64 B, hash ≤ 64 B | section 1 |
| `MaxConcurrent` | 2 (peak 256 MiB at recommended params) | burst, section 5 |
| `MaxWaiting` | 16, then `ErrBusy` | burst queue was never a bottleneck at 64; 16 absorbs login bursts without a deep queue |
| `MaxAttempts` per connection | 6 | sshd `MaxAuthTries` parity |
| abuse: window / threshold / penalty | 60 s / 10 failures / 300 s | 10 failures ≈ the abuse path becoming cheaper than guessing; long enough not to hit interactive users |
| `MaxTrackedSources` | 1024 | bounded map ≈ a few hundred KiB worst case |
| username / identityRef | ≤ 64 B / ≤ 128 B, valid UTF-8, no control bytes | PLAN 4.1 |

## Commands and receipts

All commands ran inside the container from
`/workspace/experiments/auth-argon2` (worktree root
`D:\code\vibeshell\.worktrees\auth-argon2`), invoked as
`& .\scripts\dev.ps1 exec -Command @('sh','-c','…')`. Receipt files are
complete captured output.

| Command | Result | Receipt |
| --- | --- | --- |
| `go mod init` + `GOFLAGS= go get golang.org/x/crypto@v0.57.0` + `GOFLAGS= go mod tidy` | PASS | `go.mod`/`go.sum` in module |
| `gofmt -l .` (empty) / `go vet ./...` / `go build ./...` | PASS | `receipts/static-check.log` |
| `go test -race -count=1 -v ./...` | PASS (2.2 s) | `receipts/race-test.log` |
| `go test -run=^$ -fuzz=FuzzParseEncoded -fuzztime=20s` | PASS (2,122,211 execs, 0 crashes) | `receipts/fuzz.log` |
| `VIBESHELL_AUTH_MEASURE=1 taskset -c 0-3 go test -run TestCalibrationReport -v -count=1` (×2) | PASS | `receipts/calibration.log`, `receipts/calibration-run2.log` |
| `VIBESHELL_AUTH_MEASURE=1 taskset -c 0-3 go test -run TestTimingGapReport -v -count=1` (×2) | PASS | `receipts/timing-gap.log`, `receipts/timing-gap-run2.log` |
| `VIBESHELL_AUTH_MEASURE=1 taskset -c 0-3 go test -run TestBurstThroughputReport -v -count=1` | PASS | `receipts/burst.log` |
| container environment snapshot | — | `receipts/environment.log` |

The three measurement tests skip unless `VIBESHELL_AUTH_MEASURE=1`, so the
routine `go test -race ./...` stays fast (the receipt shows them SKIP).
Measurements run without `-race` on purpose; the `-race` run covers every
test's correctness instead.

## Open questions

1. **Abuse source identity.** The spike keys the abuse guard on a
   transport-supplied source string (a remote address in practice). Is the
   SSH remote address the right key, and what happens if the service is
   ever fronted by a proxy?
2. **Rehash on parameter change.** The dummy hash has one parameter set.
   Users stored under older parameters keep a different cost until re-hashed;
   an admin rehash operation (PLAN 4.2) or rehash-on-login would close the
   residual difference. Not built in the spike (B08 territory).
3. **Empty password file.** Currently valid (nobody can log in). Reject it
   as an operator mistake, or keep it as an explicit lockout?
4. **Reference-host re-confirmation.** The container sees 16 CPUs/16 GiB
   with no limits, measurements were pinned to 4 CPUs, and the host is
   shared (calibration medians for the same candidate ranged 99–129 ms
   across runs). Repeat calibration in a CPU/memory-limited container on the
   real 4-vCPU/8-GiB host before freezing numbers at A07.
5. **Client-visible error mapping.** `ErrAttemptsExceeded`, `ErrAbusive`,
   and `ErrBusy` are distinct internally; the SSH adapter should map all of
   them to a generic authentication failure so the client cannot probe the
   controls. Confirm with the SSH task (B05).
6. **Queue-wait accounting.** Attempts rejected with `ErrBusy` do not count
   against the per-connection failure budget (no work was done). Confirm
   that an attacker cannot exploit the difference.
