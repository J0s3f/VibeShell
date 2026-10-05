# ADR 0012: Generation artifacts tolerate reasoning output and size budgets for reasoning models

Status: accepted.

Date: 2026-10-04. Amended 2026-10-05 (generation budget and stream event bound).

## Context

The configured provider route serves reasoning-heavy models: the model emits a
chain-of-thought before any artifact, either in a `reasoning_content` field (which
the gateway ignores) or inside the `content` field wrapped in reasoning markup.
Live composed-service runs on 2026-10-04 showed two failures that were not
provider outages:

- The generation budget of 4096 completion tokens was consumed entirely by
  reasoning, so the response finished with `length` and carried no artifact; the
  60-second deadline also cut the slowest attempts.
- The parser assumed the entire response was JSON, so an artifact preceded by a
  reasoning block or prose was rejected even when the JSON was present and valid.
- Generation candidates hardcoded a zero `CommitKey.Turn`, so every failure or
  fallback message was rejected by the coordinator as an invalid commit key and
  the real cause was never shown to the operator.

## Decision

- A generation candidate carries the turn it belongs to; a commit key with a zero
  turn is never produced by the generation engine.
- The generation request budget is 32768 completion tokens and 180 seconds. The
  budget is deliberately larger than the artifact because reasoning models spend
  the same completion budget on their chain of thought. Live-observed
  2026-10-05: the interactive generation prompt made `deepseek-v4.1-flash`
  reason for about 17.3k tokens before emitting the artifact, so 16384 returned
  empty content and 32768 returned a usable artifact in about 95 seconds.
- The streamed-response event bound is 65536 frames. A reasoning model streams
  its chain of thought and the artifact in many small frames, so the same
  32768-token response exceeded the former 4096-frame default and was rejected
  as "too many SSE events"; the default now leaves headroom above the
  one-frame-per-token worst case for the generation budget.
- `parseGenerationProposal` locates the artifact by scanning for the first JSON
  object that decodes and carries `command_names`, `entrypoint`, and `source`.
  Prose, reasoning blocks (with or without braces), and code fences around the
  artifact are ignored. A response without such an object is rejected rather than
  guessed at.
- The engine surfaces the underlying staging-validation issues and activation
  error instead of a generic message.

## Alternatives considered

- Disable reasoning with a provider parameter (`reasoning_effort`, `thinking`,
  and similar): rejected. The parameters are provider- and model-specific, none is
  confirmed for this route, and sending an unknown parameter could silently change
  behavior elsewhere.
- Use only non-reasoning models: rejected. The Go route offered no reliably
  non-reasoning model in the live matrix, and the product must not hardcode a
  model that is permanently available.
- Require the response to be exactly JSON: rejected. It rejects valid artifacts
  that the model wraps in reasoning or prose, which is normal for the served
  models.
- Add a repair loop that re-prompts on a validation failure: deferred. The
  configuration already carries `max_repairs`, but wiring a bounded repair loop is
  a separate change with its own budget and provenance rules.

## Consequences

- A generation attempt costs more tokens and can take up to three minutes; the
  trade is a much higher chance of a usable artifact from reasoning models.
- More provider models become usable without a per-model allowlist.
- The parser is tolerant of surrounding text but still requires a complete,
  well-formed object with the required keys; a truncated artifact is rejected.
- A candidate that fails staging validation still leaves no active version and
  reports the validation issues truthfully. A repair loop remains future work.

## Evidence

- Live receipt:
  `experiments/opencode-live-probe/receipts/2026-10-04T003406-composed-e2e.json`
  (unknown command -> live provider -> parse -> staging validation -> activation ->
  sandbox execution).
- Model matrix observed live on 2026-10-04: `deepseek-v4.1-flash` returned a valid
  artifact at 16384 tokens; several other Go models returned empty content because
  reasoning consumed the whole budget.
- Live-observed 2026-10-05: the interactive generation prompt pushed
  `deepseek-v4.1-flash` reasoning past 16384 tokens (empty content, rejected) and
  the 32768-token response past the 4096-frame SSE bound (`too many SSE events`);
  at 32768 tokens and the 65536-frame bound the same request returned a valid
  artifact in ~95 seconds.
- Commits: `853a3ad` (generation fix), `185beba` (observability and live tests),
  `dd85f1c` (generation budget 32768), `5cf7cc6` (SSE event bound 65536).
