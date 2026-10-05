# QuickJS Wasm engine provenance

The sandbox spike embeds exactly one opaque binary: `experiments/sandbox/qjs.wasm`.
This file records where it came from so the blob is never unattributed.

## Artifact

- File: `experiments/sandbox/qjs.wasm`
- Size: 1038767 bytes
- SHA-256: `88fcbeead33cca3ec8394feb0fb3eec8047ec9ac5062aba7a63799cee11989b7`
- Verified: byte-identical (`sha256sum`) to the `qjs.wasm` committed in the
  upstream repository at the pinned revision below. No local rebuild was
  needed; a rebuild path is documented for auditability.

## Upstream source

- Repository: https://github.com/fastschema/qjs
- Pinned tag: `v0.0.6` (resolves to commit `461716f4f380f81ffd09378751f1812919cddbca`, 2025-10-28)
- Go module: `github.com/fastschema/qjs`, `go 1.22.0`, requires
  `github.com/tetratelabs/wazero v1.9.0` (same wazero version this spike uses)
- Guest C sources: `qjswasm/*.c` (`eval.c`, `function.c`, `helpers.c`,
  `proxy.c`, `qjs.c`, `qjs.h`, `qjswasm.cmake`) on top of the QuickJS submodule
- QuickJS submodule: `qjswasm/quickjs` → https://github.com/quickjs-ng/quickjs.git
  at `d01ca4491fb24ccfeccb4c7394e28a3b21fd5986`

## Build flags (upstream `Makefile`, `build` target)

- `cmake -B build -DQJS_BUILD_LIBC=ON -DQJS_BUILD_CLI_WITH_MIMALLOC=OFF`
  `-DCMAKE_TOOLCHAIN_FILE=/opt/wasi-sdk/share/cmake/wasi-sdk.cmake`
  `-DCMAKE_PROJECT_INCLUDE=../qjswasm.cmake`
- `make -C qjswasm/quickjs/build qjswasm`
- `cp qjswasm/quickjs/build/qjswasm qjs.wasm && wasm-opt -O3 qjs.wasm -o qjs.wasm`
- wasi-sdk release: not pinned anywhere upstream (only the `/opt/wasi-sdk`
  toolchain path and the CI matrix `go 1.22.x–1.25.x` are recorded). The
  artifact is therefore pinned by (tag, commit, submodule commit, sha256)
  rather than by toolchain version. Rebuilding reproducibly would require
  choosing a wasi-sdk release and re-recording the checksum here.

## Licenses

- `qjs.wasm` adapter/guest glue: MIT, Copyright (c) 2025 Nguyen Ngoc Phuong
  and Contributors (upstream `LICENSE`).
- QuickJS-ng (submodule): MIT-style Bellard license (see
  `qjswasm/quickjs/LICENSE` upstream). Preserve both notices if the artifact
  is promoted into the application build.
- Neither license was copied into this checkout; no upstream Go or C source
  was copied either — only the released binary artifact plus this record.
  `go.sum` pins only `github.com/tetratelabs/wazero v1.9.0`.

## What this spike does NOT adopt from upstream

Upstream `runtime.go` instantiates WASI with `WithDirMount(CWD, "/")` and
full walltime/nanotime/sleep plus host stdout/stderr. This spike provides
none of that: capability-free stubs (denied fs/paths/env/args, EOF stdin,
fixed simulated clock, non-blocking poll, bounded stdout/stderr capture),
`WithMemoryLimitPages`, and `WithCloseOnContextDone`. See `wasi.go` and the
research spike report.
