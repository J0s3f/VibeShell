# ADR 0003: QuickJS/Wazero engine with a capability-free embedding

Status: accepted.

Date: 2026-10-03.

## Context

PLAN 5.7 requires a bounded runtime for AI-generated application logic.
Generated code may only reach the simulated world through proposals, never the
host, and the source-inspected reference adapter
(`fastschema/qjs`) mounts the process working directory into WASI and exposes
wall-clock time, sleep, and host stdout/stderr. That default is unusable here,
and PLAN 5.7 forbids weakening the isolation contract or falling back to native
execution.

Before implementation relied on the engine, PLAN 5.7 required an
execution-level qualification covering infinite loops, allocation exhaustion,
recursion, malformed JSON, forbidden imports and filesystem/network attempts,
cancellation, state leakage, and 100-instance behavior. Task A04 ran that
qualification; its results, not the library's marketing, are the basis here.

## Decision

- Embed a pinned QuickJS Wasm guest (`qjs.wasm`) in `wazero` v1.9.0 and expose
  it only through `ports.AppSandbox`. Persist application **source** as the
  long-term artifact; any compiled-bytecode cache is derived and tied to the
  exact engine version.
- Ship the upstream released artifact `qjs.wasm`, sha256
  `88fcbeead33cca3ec8394feb0fb3eec8047ec9ac5062aba7a63799cee11989b7`
  (1038767 bytes), byte-identical to tag `v0.0.6`
  (commit `461716f4f380f81ffd09378751f1812919cddbca`) over the QuickJS-ng
  submodule `d01ca4491fb24ccfeccb4c7394e28a3b21fd5986`, with its provenance
  recorded beside it.
- Provide **no** host capabilities: no directory mounts, no environment, no
  arguments, no sockets, no process execution, no native modules, and no
  unrestricted Go object/function proxies. Deny `path_*`, `fd_*` beyond
  0/1/2, `environ_*`, `args_*`, and directory inspection; supply EOF stdin, a
  fixed simulated clock, a non-blocking poll, and stdout/stderr capture capped
  at 1 MiB per instance.
- Apply **both** limits to every instance: a Wasm memory cap
  (`WithMemoryLimitPages(256)`, 16 MiB) and a guest-side heap/stack limit
  (QuickJS heap 8 MiB, interpreter stack 512 KiB, GC threshold 1 MiB).
- Compile the module once per engine and instantiate an isolated runtime per
  application instance; close on context done; run under an execution deadline.
  Any trapped or exited instance is **disposable and never reused**: termination
  by instance re-instantiation is what bounds per-offender cost.
- Keep the JSON bridge bounded in both directions, with length-bounded reads of
  guest-allocated strings and explicit frees.
- Admit instances only through an admission limit; the concrete limit stays
  unset until the phase-B/C benchmarks measure it.

## Alternatives considered

- Native execution of generated code: rejected by PLAN 5.7 and the project
  rules; it would give generated code exactly the host capabilities the
  simulation must deny.
- A Linux VM per user: rejected in PLAN 17; it adds a real OS, process, and
  network model inconsistent with the confirmed simulation design.
- The upstream adapter's own defaults (`WithDirMount(CWD, "/")`, host time,
  sleep, stdio): rejected on inspection. Accepting the library's defaults because
  they are the defaults is exactly the failure PLAN 5.7 and PLAN 17 call out.
- Another Wasm embedding behind the same port: retained as the escape hatch if
  a future qualification fails, not chosen now. Javy was inspected during
  research and not selected.

## Consequences

- `internal/adapters/sandbox` owns the bridge; the domain and ports stay free of
  Wasm concepts (ADR 0002).
- An instance that hits a resource limit, a forbidden capability, or deep
  recursion dies with the instance rather than degrading gracefully. That is a
  deliberate trade: guest aborts are recoverable because instances are cheap and
  disposable.
- Deep recursion currently surfaces as a Wasm trap (`out of bounds memory
  access`), not a catchable JavaScript `RangeError`. Safe, but instance-fatal and
  harder for an application to handle. Tuning for a catchable error is a
  follow-up, not a blocker.
- Engine compilation costs roughly half a second once per process; instance
  creation is on the order of milliseconds, so per-session cost is amortized.
- The shipped blob is a binary dependency carried in the repository. Its
  licenses (MIT adapter glue, MIT-style Bellard license for QuickJS-ng) must be
  preserved if the artifact is redistributed.
- Admission limits remain unset; a 100-user figure measured with short-lived
  instances cannot be extrapolated to 100 simultaneously running guests.

## Evidence

- Research: `docs/research/2026-10-03-sandbox-spike.md`.
- Provenance: `experiments/sandbox/PROVENANCE.md` (spike) and
  `internal/adapters/sandbox/PROVENANCE.md` (promoted artifact).
- Receipt: `experiments/sandbox/receipts/sandbox-qualification.log`, UTC
  2026-10-03T14:38:12Z, `go1.27.1 linux/amd64`; every gate PASS, including
  `go test -race` and the root `scripts/dev.ps1 test` gate.
- Commits: `1355899` (spike), merged as `dde7c99`; adapter `49ce35a`, merged as
  `8c0f51a`.

## Unverified items and limitations

- The artifact is the **upstream released build, not a local rebuild**. No
  from-source reproduction was attempted. Upstream does not pin a wasi-sdk
  release (only the `/opt/wasi-sdk` path), so the blob is pinned by tag,
  submodule commit, and sha256 rather than by toolchain. Reproducing it
  reproducibly requires choosing a wasi-sdk release and re-recording the
  checksum. This is inference about reproducibility, not a measured fact.
- 100 **simultaneous long-running** guests, idle-instance eviction and restore,
  and provider-coupled load were not measured. The 100-instance receipt
  exercised short-lived instances.
- The report's summary table and the committed receipt report slightly
  different values for the same runs (compile 518 ms vs 529 ms; 100 concurrent
  instances 57 ms with a 13 MiB host-heap delta vs 64 ms with a 26 MiB delta).
  These are separate runs of the same harness; the receipt is the authoritative
  capture, and the host-heap delta is not a stable measurement because GC
  timing dominates it. No capacity number should be derived from either.
- No penetration test or adversarial audit was performed; the forbidden-
  capability evidence is a 24-import audit plus targeted denial probes.
- License texts were not copied into the checkout at spike time.