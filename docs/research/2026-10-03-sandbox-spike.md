# Sandbox spike (A04): QuickJS Wasm in wazero — executable qualification

Date: 2026-10-03. Scope: PLAN.md 3.2, 5.7, 12.1, 15.2 (task A04).
Code: `experiments/sandbox/` (`sandbox.go`, `wasi.go`, `sandbox_test.go`,
`PROVENANCE.md`, `cmd/inspectimports`, `cmd/debugeval`).
Receipt: `experiments/sandbox/receipts/sandbox-qualification.log`
(all tests PASS, rated below). No application code was written; no root
`go.mod`/`go.sum`, `scripts/`, `containers/`, `cmd/`, `internal/`, or
`PLAN.md` files were modified.

## Engine provenance (observed facts)

- Artifact `experiments/sandbox/qjs.wasm`, 1038767 bytes, SHA-256
  `88fcbeead33cca3ec8394feb0fb3eec8047ec9ac5062aba7a63799cee11989b7`.
- Byte-identical to the `qjs.wasm` committed in
  https://github.com/fastschema/qjs at tag `v0.0.6`
  (commit `461716f4f380f81ffd09378751f1812919cddbca`, 2025-10-28).
- Guest sources: `qjswasm/*.c` over submodule `qjswasm/quickjs` →
  https://github.com/quickjs-ng/quickjs.git at `d01ca44`.
- Upstream build flags (`Makefile`): `cmake -DQJS_BUILD_LIBC=ON
  -DQJS_BUILD_CLI_WITH_MIMALLOC=OFF` with the wasi-sdk CMake toolchain,
  then `wasm-opt -O3`. The exact wasi-sdk release is not pinned upstream
  (only the `/opt/wasi-sdk` path); the artifact is pinned by
  tag+commits+sha256 instead. Full record: `PROVENANCE.md`.
- Licenses: adapter glue MIT © 2025 Nguyen Ngoc Phuong and Contributors;
  QuickJS-ng Bellard MIT. No upstream source copied; only the binary plus
  this record. wazero `v1.9.0` (same version upstream uses).
- Inference (not a fact): the blob is the project's released build, not a
  locally reproduced one. Mitigation: sha256 match to a tagged release plus
  recorded source revisions; a from-source rebuild with a pinned wasi-sdk
  remains possible before promoting the artifact.

## Harness design (observed facts)

- One wazero runtime per `Engine`: `WithMemoryLimitPages(256)` = 16 MiB
  guest cap, `WithCloseOnContextDone(true)`, compiled module shared.
- Host `env`/`wasi_snapshot_preview1` modules instantiated exactly once per
  engine. Denied: all `path_*`, `fd_*` beyond 0/1/2, `environ_*` (empty),
  `args_*` (empty), prestat/readdir/seek. Implemented: `fd_write` to fd 1/2
  into a per-instance 1 MiB capped buffer routed via an engine registry keyed
  by calling guest module; `fd_read` EOF on stdin; `fd_fdstat_get` char-dev
  for 0/1/2; `clock_time_get` fixed simulated time; `poll_oneoff` zero events.
- No directory mounts, no walltime/nanosleep passthrough, no sockets (the
  module imports none: 24 imports = 23 WASI + `env.jsFunctionProxy`).
- Per instance: QuickJS heap 8 MiB (`JS_SetMemoryLimit`), interpreter stack
  512 KiB, GC threshold 1 MiB, `QJS_UpdateStackTop` before eval.
- Two guest-ABI facts found by failing tests, fixed in `sandbox.go`:
  1. `QJS_ToCString`/`QJS_JSONStringify` return a guest *pointer* to an
     8-byte `(address<<32|length)` struct (`qjswasm/helpers.c`), not the
     packed value. The old code read the pointer as the value (megabyte
     garbage strings). Now dereferenced, length-bounded (4 MiB), and both
     the QuickJS string and the wrapper are freed.
  2. i32 results (notably `QJS_IsException`) carry garbage in the high 32
     bits, turning every pointer-valued result into a phantom exception.
     Results are now masked by declared result type.

## Gate verdicts (all executed in `vibeshell-a04-sandbox-dev`)

Command: `cd experiments/sandbox && go test -v -timeout 600s -count=1 .`
→ all PASS (receipt). `go test -race` → PASS (53.8 s). Root
`scripts/dev.ps1 test` → PASS.

| Gate | Result | Evidence |
| --- | --- | --- |
| Infinite loop + deadline/cancel | PASS | `while(true){}` killed after 501 ms (500 ms ctx); `Close` prompt; fresh instance unaffected |
| Allocation exhaustion (8→4 MiB heap test) | PASS | Guest OOM error, host stable, instance reusable for small eval |
| Wasm memory cap without JS heap limit | PASS | 48-page engine OOMs the hog; error, no host effects |
| Deep recursion | PASS with nuance | 6 ms to `wasm error: out of bounds memory access` (wazero native-stack guard fires before QuickJS's own stack limit at every tested StackSize 64–512 KiB); host/peers fine; trapped instance fails fast, fresh instance works |
| Malformed JSON boundary | PASS | 5 malformed inputs rejected; circular `stringify` rejected; valid `{"a":1,"b":[2,3]}` canonicalized |
| Forbidden capabilities | PASS | 24-import audit: no `sock_*`, only `wasi_snapshot_preview1`+`env`; `os.open`/`std.loadFile`/`os.stat` denied (offender exits 71, instance closes by design); env reads empty; `Socket`/`fetch`/`XMLHttpRequest`/`require`/`process` all `undefined`; fresh instance works after denials |
| Instance isolation | PASS | Globals/memory invisible across instances; close/interrupt of one never affects peers |
| Cold vs warm + memory | PASS | Compile once 518 ms; cold instance 2 ms; 100 warm in 208 ms (avg 2.08 ms); guest 320 KiB after small alloc |
| Bounded concurrency (100 simultaneous) | PASS | 100 instances in 57 ms, 0 errors; host heap 3→17 MiB (Δ13 MiB) |

## ~100-user reading (inferences, labelled)

- Compile-once/instantiate-per-app amortizes the 518 ms cost; steady-state
  instance creation (~2 ms) and 13 MiB host heap per 100 live instances fit
  the 4-vCPU/8-GiB reference host with large headroom for 100 mostly-idle
  users plus an admitted execution pool. Not yet measured: 100 *simultaneous
  long-running* guests, eviction/restore from explicit state (design only),
  and provider-coupled load — those belong to the phase-B/C benchmarks.
- Termination-by-instance (loop/recursion/capability-abuse) makes the
  per-offender cost one cheap re-instantiation; the adapter must treat any
  trapped/exited instance as disposable and never reuse it.

## Recommendation

**Accept QuickJS/wazero behind the sandbox port** (`internal/adapters/sandbox`
in phase B) with the isolation contract as tested: no mounts/env/sockets,
both memory caps, close-on-done, bounded JSON bridge, disposable instances,
admission limits still to be set from phase-B/C benchmarks. Do not weaken
isolation and do not fall back to native execution. Optional follow-ups, not
blockers: pin a wasi-sdk release and reproduce the artifact from source;
tune for catchable JS `RangeError` on recursion (currently a wasm trap —
safe but instance-fatal); add compiled-bytecode caching tied to engine
version. No gate is UNVERIFIED; every gate above ran with receipts.

## Delta from the previous partial (honest handoff)

The inherited tree embedded the same blob but could not evaluate anything:
(1) host `env`/WASI modules were instantiated per instance, so the 2nd–nth
instance, isolation, latency, and concurrency tests all failed
(`module[env] has already been instantiated`); fixed by engine-once stubs
plus a calling-module instance registry. (2) The packed-pointer ABI was
misread (pointer taken as value), failing even `1+1`; fixed by
dereference + bounds + frees. (3) i32 high-bit garbage made every
pointer result look exceptional; fixed by result-type masking. (4) No
provenance record, receipts, or report existed; added. (5) `TestDeepRecursion`
and forbidden-capability tests now codify the true contract (trapped
instance dies fast; fresh instances per denial probe) instead of asserting
behavior the engine does not have.
