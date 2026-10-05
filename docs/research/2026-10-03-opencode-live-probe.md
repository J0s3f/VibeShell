# E02 keyed live-probe harness (OpenCode integration gate)

Date: 2026-10-03. Worktree: `.worktrees/opencode-live-probe`, branch
`agent/opencode-live-probe`. Nested module:
`experiments/opencode-live-probe` (own `go.mod`, requires
`j0s.at/vibeshell/experiments/opencode-protocol v0.0.0` with
`replace => ../opencode-protocol`; reuses its tested codecs, catalogue
parsing, PLAN 8.3 suitability filtering, and `Redact`).
All builds/tests ran in container `vibeshell-opencode-live-probe-dev`.

## Deliverable

`cmd/opencode-live-probe` plus package `probe`:

- Without `--live` (or with key flags but no `--live`, which exits 2):
  prints `SKIPPED: dry-run; ...` and exits 0 making **no network
  request**.
- With `--live` but no resolvable key: prints `SKIPPED: no key ...`,
  exits 0, no network request. A key is only accepted via `--key-env`
  (environment variable name) or `--key-file` (secret file path); there
  is deliberately **no flag accepting a key literal**.
- With `--live` and a key: performs GET `/models` on both product routes
  (Console, Go), records status/bytes/sha256/record count, then runs one
  minimal bounded streaming probe per product × protocol family
  (chat/responses/messages/gemini) against the first suitable-free
  model (PLAN 8.3 `ExpandFree` over the vendored models.dev snapshot
  given via `--metadata`). Each probe POSTs a `stream:true` request that
  also carries tool definitions, with a per-request timeout. Recorded:
  HTTP status, latency (ms), whether the SSE stream decoded
  (`stream_decode_ok`), whether tool parameters were accepted
  (`tool_params_accepted`), observed tool calls, and redacted errors.
- Credentials travel only as the `Authorization: Bearer` header value.
  Every user-facing string (errors, receipt errors, stdout) passes
  through `gateway.Redact`; stdout and the receipt are asserted not to
  contain a fake key in tests.
- Successful live runs write a dated receipt
  `receipts/<UTC-timestamp>-live-probe.json` (mode `live`, redacted:true).

Exit-code contract: 0 = skipped / dry-run / completed probe matrix
(individual probe failures are recorded, not fatal); 1 = metadata load
failure, both catalogues unreachable, or receipt write failure; 2 = key
source flags without `--live`.

Note: the Gemini probe endpoint uses `/models/{model}:streamGenerateContent?alt=sse`
(model in path), unlike the suffix-only `EndpointFor` string qualified
fixture-only in A06 — live confirmation remains an open question.

## Verification (offline, `go test -race ./...`)

- `TestDryRunMakesNoNetwork`: dry-run output contract, exit 0, counting
  httptest server records zero hits.
- `TestLiveWithoutKeyMakesNoNetwork`: `--live` + missing env key →
  `SKIPPED: no key`, exit 0, zero hits.
- `TestLiveProbeRecordsAndRedacts`: full live path against local
  httptest → exit 0, Authorization header carried the fake key, stdout
  and receipt contain no key, receipt records `stream_decode_ok: true`.
- `TestRedactionOfKeyEchoingError`: server 401 echoing the Bearer value;
  receipt and stdout both free of the fake key, exit 0.
- `TestCatalogueFailureExitCode`: 502 on catalogue → exit 1.

`go vet` clean; `gofmt -l` clean; `go test -race ./...` PASS (probe
1.124s). No live run was performed: no API key was available or
requested, and `go test` never issues network calls outside local
httptest servers.

## What remains for E02

1. A real credentialed run: `--live --key-env/--key-file --metadata
   ../opencode-protocol/testdata/models-dev-opencode.json`, reviewing the
   resulting dated receipt. This is the actual integration gate.
2. Verify per-protocol inference suffixes, Go `x-opencode-session`
   header behavior, and Responses-vs-Chat routing per model from the live
   receipt, then feed overrides back into the adapter.
3. Tool-call acceptance in live probe is currently "request accepted";
   strengthen to requiring an actual tool call only when a model reliably
   emits one — otherwise false negatives.
4. Wire key provisioning (mounted secret file/env at the composition
   root) instead of hand-passed flags for the deployment path.

## Open questions

- Exact Gemini streaming URL (model-in-path vs. `/models:streamGenerateContent`)
  unconfirmed without a key.
- Catalog derivation picks the first suitable-free model per family
  deterministically; live free-tier entitlement per model is unverified
  and expected to produce recorded 401/402/429 probes.
