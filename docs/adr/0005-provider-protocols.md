# ADR 0005: Provider, product, and wire protocol are separate axes

Status: accepted.

Date: 2026-10-03.

## Context

PLAN 8.1 requires OpenCode's protocol families behind one `ModelGateway` port
without advertising a Messages/Responses/Gemini model while silently sending it
to a chat endpoint. The same model ID can exist on more than one product route
with different allowances, so a model name alone does not identify provider,
product, or protocol (research findings, OpenCode APIs).

Task A06 qualified the protocol codecs, error normalization, redaction, and the
catalogue-to-suitability rules **without an API key**. Fixture qualification is
the only evidence that exists, and this record must not claim more than it has.

## Decision

- Model provider, product route (Console vs Go), and wire protocol are three
  independent, separately tracked values. A protocol alone never selects an
  endpoint, and a model ID never implies a protocol.
- Implement the four families behind one canonical result type
  (`Result{Text, ToolCalls, FinishReason, Usage}`) and one stream decoder:
  Chat Completions, Responses, Anthropic Messages, and Gemini `generateContent`
  (`alt=sse`).
- Treat SSE framing as untrusted: tolerate frames split across reads and multiple
  events per read, bound frame and total body size, ignore unknown event types,
  and tolerate a dropped terminal sentinel because an SSE prefix is
  indistinguishable from a complete stream.
- Normalize provider failures into a typed, redacted error carrying status,
  category, retryability, message, and provider code: 401/403 authentication,
  402/quota words quota, 429 rate limit (retryable), 404 model not found, 5xx
  provider outage (retryable), context-length markers context length. An HTTP
  200 with a JSON error body is an error, never a success.
- Scrub credentials in every diagnostic path (`sk-*`, `Bearer`, `api_key=`,
  `?key=`, `x-api-key`, `Authorization`) while leaving useful diagnostics intact.
- Classify a model's protocol from the only observed per-model signal, the
  models.dev `provider.npm` field, and **fail closed**: an unclassified model
  resolves to no protocol and is never routed. Endpoint selection must refuse an
  ineligible or unclassified model outright.
- Select suitable-free models by intersecting catalogue, text generation, tool
  support, protocol support, and a minimum context. Unknown cost is an explicit
  operator decision (`needs-operator`), never silently zero; a documented
  structured-decision-only model is always denied even with a `-free` suffix; a
  zero-cost model without the suffix is eligible only through an explicit allow.

## Alternatives considered

- Assuming Chat Completions for every OpenAI-family model: rejected. It has no
  observed per-model signal behind it and would silently misroute a non-chat
  model. Responses-vs-Chat routing needs an explicit override table fed by a live
  probe.
- Inferring protocol or price from the model ID suffix alone: rejected on
  observed evidence. `big-pickle` and `grok-code` are free without a `-free`
  suffix, and `jev-1.13-free` carries the suffix but is not a shell text
  generator.
- Using a provider SDK: rejected. ADR 0002 keeps provider protocols behind an
  outbound port; the B04 adapter is stdlib `net/http` only and adds no
  dependency.
- Validating against the live API during qualification: not possible without a
  key. This is recorded as a limitation rather than worked around.

## Consequences

- Catalogue discovery works with no key and does not prove entitlement, health,
  or inference compatibility. Those stay separate concepts.
- Metadata is supplementary. A model can pass every static rule and still fail at
  inference time, which is why health and cooldown live outside this decision.
- Adding a protocol family or a new provider touches only the gateway adapter and
  route configuration; shell behavior and the SSH adapter are unaffected.
- Failing closed on unknown classification means an unclassified model is
  unavailable rather than misrouted. Operators need a way to classify it, and
  that override must be explicit and administrator-controlled.
- Endpoint paths beyond the observed catalogue bases remain conventional guesses
  in code until a live probe confirms them.

## Evidence

- Research: `docs/research/2026-10-03-opencode-protocol-spike.md`.
- Receipts: `experiments/opencode-protocol/receipts/` —
  `go-test-race.log` (34 PASS / 0 FAIL), `live-console-models.json`
  (82 records, sha256 `3e48d3ea…b988c4`), `live-go-models.json`
  (43 records, sha256 `3eac61f8…2306435e33`), `toolchain.txt` (`go1.27.1`).
- Fixtures: `experiments/opencode-protocol/testdata/` — five SSE transcripts and
  four error envelopes, including `error-200-envelope.json`.
- Observed 2026-10-03 with `MinContext=100000`: 10 eligible Console models.
- Commits: `0ebc5df` (spike), merged as `436c595`; adapter `9938d85`, merged as
  `a0f5c55`.

## Unverified items and limitations

- Authenticated inference is verified for the **OpenCode Go chat** protocol only
  (2026-10-04): the composed service authenticated, returned 200, and decoded
  live streams for MOTD and generation. Tool-call acceptance, usage/finish-reason
  fidelity across families, the Responses/Messages/Gemini families, and
  Go-vs-Console billing remain untested. Receipt:
  `experiments/opencode-live-probe/receipts/2026-10-04T003406-composed-e2e.json`.
- The Go chat inference path (`/chat/completions`) and the `x-opencode-session`
  request header are confirmed live; other protocol paths remain conventional
  guesses. The key arrived through a container environment variable, never chat or
  committed source.
- Go-versus-Console billing and allowance differences are unverified. Do not
  route a Go model to the Console pay-as-you-go endpoint.
- The models.dev snapshot is a dated observation (opencode 116 models,
  opencode-go 33). Model lists, prices, and availability change; the shipped
  configuration must not hardcode a permanently free model.
- `grok-code` appeared in metadata but was absent from the live Console
  catalogue; an explicit naming rule for it needs re-checking.
- Responses-vs-Chat routing inside the OpenAI family has no observed per-model
  signal and still needs an explicit override table from a live probe.
- Catalogue payloads carry only `id`, `object`, `created`, and `owned_by`. No
  pricing, tool capability, context size, or protocol mapping is available from
  the provider; all of that comes from supplementary third-party metadata.
- The redaction tests use fabricated credential shapes. They prove the scrubber,
  not that every real provider error shape is covered.