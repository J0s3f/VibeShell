# A06 OpenCode protocol qualification spike (no key)

Date: 2026-10-03. Worktree: `.worktrees/provider-protocol`, branch
`agent/provider-protocol`. Nested module:
`experiments/opencode-protocol` (own `go.mod`, stdlib only).
All builds/tests ran in container `vibeshell-provider-protocol-dev`.

## 1. Catalogue observation (public GET, no key) — OBSERVED FACT

Fetched in-container on 2026-10-03 (receipts with sha256):

| Product | URL | HTTP | Bytes | Records | sha256 (live bytes) |
| --- | --- | --- | --- | --- | --- |
| Console | `https://opencode.ai/inference/v1/models` | 200 | 6860 | 82 | `3e48d3ea…b988c4` |
| Go | `https://opencode.ai/zen/go/v1/models` | 200 | 3569 | 43 | `3eac61f8…2306435e33` |

JSON shape (both): `{"object":"list","data":[{"id","object","created","owned_by"}]}`.
No pricing, tool capabilities, context sizes, or protocol mappings in the
payloads. ID sets are identical to the vendored snapshots in `testdata/`
(`opencode-models.json` sha256 `d280d20e…2031a3ae`, 82 records;
`opencode-go-models.json` sha256 `bd1641f7…61443963fb32`, 43 records —
byte-identical to `D:\code\vibeshell\.research`). Live-vs-snapshot byte
differences are only server-stamped `created` values (e.g. 1791038968 vs
1791030964); sorted ID sets match exactly.

## 2. Protocol codecs — FIXTURE-QUALIFIED, not live-proven

One canonical `gateway.Result{Text, ToolCalls, FinishReason, Usage}` behind
`DecodeStream(ctx, protocol, body, opts)`, exercised via local `httptest`
SSE servers and `testdata/*.sse` fixtures, with transport reads chunked to
1–7 bytes to prove split-frame tolerance:

- Chat Completions SSE: text deltas, `tool_calls` assembly across chunks
  (index-keyed argument concatenation), `finish_reason`, `usage`,
  `[DONE]` sentinel.
- Responses API: `output_text.delta`, `output_item.added` (function_call),
  `function_call_arguments.delta`, `completed`/`incomplete` + usage.
- Anthropic Messages: `content_block_delta` (text + `input_json_delta`),
  `message_delta` usage/stop_reason mapping, `ping`/unknown events ignored,
  `error` events fail.
- Gemini `generateContent` (`alt=sse`): candidate text parts across chunks,
  `finishReason`, `usageMetadata`.

Boundary cases covered: frames split across reads, multiple events per
read, oversized frame rejection, bounded total body, malformed JSON per
family, HTTP 200 with a JSON error body (reported as `*ProviderError`,
never success), mid-frame truncation, context cancellation, unknown event
types ignored, cleanly dropped terminal sentinel tolerated (documented:
an SSE prefix is indistinguishable from a complete stream).

## 3. Error normalization and redaction — TESTED ON FIXTURES

`CheckStatus`/`NormalizeError` map status + structured fields of the three
documented envelope shapes (OpenAI/Anthropic/Gemini) to a redacted
`ProviderError{Status, Category, Retryable, Message, ProviderCode}`:
401/403→authentication, 402/quota words→quota, 429→rate_limit (retryable),
404→model_not_found, 5xx→provider_outage (retryable), context-length
markers→context_length. `Redact` scrubs `sk-*`, `Bearer`, `api_key=`,
`?key=`, `x-api-key`, `Authorization` shapes; tests assert six fake
credential shapes all become `[REDACTED]` with zero fragment leakage, and
that plain diagnostics (e.g. `model_not_found`) survive.

## 4. Catalogue→metadata matching and suitability — PLAN 8.3

Vendored `testdata/models-dev-opencode.json` (opencode 116 models,
opencode-go 33). Protocol classification from the only observed per-model
signal, models.dev `provider.npm`: `@ai-sdk/anthropic`→Messages,
`@ai-sdk/google`→Gemini, OpenAI family/absent→Chat; unknown models resolve
to empty protocol (fail closed). `EndpointFor` guarantees a non-chat
protocol never resolves to the chat URL; `EndpointForDecision` refuses
ineligible/unclassified models.

With `MinContext=100000`, Console discovery yields **10 eligible**:
big-pickle (explicit-allow), fledge-alpha-free, ling-3.1-flash-free,
longcat-2.5-preview-free, mimo-v2.5-free, mimo-v2.6-flash-free,
muse-spark-1.2-contributor-free, muse-spark-1.3-contributor-free,
nemotron-3.5-lightning-free, space-bunny-free. Cases tested:

- Big Pickle exception: free (zero cost in metadata) despite no `-free`
  suffix → eligible only via explicit allow; ineligible without it.
- `jev-1.13-free` exception: `-free` suffix but documented
  structured-decision model → always denied (`structured-decision-only`).
  `jev-1.13`/`jev-1.13-free` are also absent from models.dev metadata, so
  the unknown-metadata path denies them independently.
- Known nonzero cost on a `-free` ID → excluded; missing cost →
  `needs-operator`, never silently zero; missing metadata → operator
  decision; no tool support / non-text modality / context < minimum /
  non-free suffix without allow → denied with stable reason labels.

## 5. UNVERIFIED (no key — must not be claimed)

Authenticated inference on any family (tool-call acceptance, live
streaming, usage/finish fidelity); Go `x-opencode-session` behavior and
Go-vs-Console billing/allowance differences; inference URL suffixes beyond
the observed catalogue paths (endpoint strings in code are conventional
and unproven); account entitlement and model health; live suitability of
the 10 eligible models (metadata is supplementary, per findings).

## 6. Open questions

1. Exact per-protocol inference paths and required Go headers can only be
   confirmed with a key; which must be supplied via secret file/env, never
   chat or source.
2. Responses-only vs Chat-compatible routing within the OpenAI family has
   no observed per-model signal — needs an explicit override table fed by
   a live probe (B04).
3. `grok-code` (like big-pickle, zero cost without `-free`) was observed
   in metadata but is absent from the live Console catalogue; explicit
   naming should be re-checked at implementation time.
4. Receipts: `experiments/opencode-protocol/receipts/` (live JSON,
   `go-test-race.log` 34 PASS / 0 FAIL, `toolchain.txt` go1.27.1).
   Acceptance: `go test -race ./...` passes in the container.
