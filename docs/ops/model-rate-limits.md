# Free-model rate-limit status

Operational state of the free subagent models. OpenCode surfaces a quota error
only as `AI.Error.QuotaExceeded: Rate limit exceeded. Please try again later.`
with no reset timestamp, so this state is inferred from observed errors and
minimal probes. OpenCode Go monetary usage is visible in the Console; free-model
throttles are not reported by any local API.

Update this file whenever an error or probe changes a model's state. Append the
corresponding detail to [model-rate-limit-history.md](model-rate-limit-history.md).

States: `available` (confirmed usable), `rate-limited` (limit observed, not yet
re-probed), `unavailable` (geoblocked/endpoint down/retired for this region),
`degraded` (transient upstream failures), `unknown` (not exercised recently).

Last updated: 2026-10-05T10:16:00Z

| Agent | Model ID | State | Since (UTC) | Last probe (UTC) | Probe result | Next probe due (UTC) |
| --- | --- | --- | --- | --- | --- | --- |
| free-big-pickle | `opencode/big-pickle` | available | — | 2026-10-04T03:45:00Z | OK (recovered) | — |
| free-fledge | `opencode/fledge-alpha-free` | available | — | 2026-10-04T03:45:00Z | OK (recovered) | — |
| free-ling-3.0 | `opencode/ling-3.0-flash-fin-free` | unavailable (endpoint) | 2026-10-03T14:58:00Z | 2026-10-04T03:45:00Z | "Endpoint is unavailable" | 2026-10-04T08:00:00Z |
| free-ling-3.1 | `opencode/ling-3.1-flash-free` | available | — | 2026-10-04T03:45:00Z | OK (recovered) | — |
| free-longcat | `opencode/longcat-2.5-preview-free` | rate-limited | 2026-10-05T10:16:00Z | 2026-10-05T10:16:00Z | QuotaExceeded | 2026-10-05T14:15:00Z |
| free-mimo-v2.6 | `opencode/mimo-v2.6-flash-free` | available | — | 2026-10-04T03:45:00Z | OK (recovered) | — |
| free-muse-spark | `opencode/muse-spark-1.3-contributor-free` | available | — | 2026-10-04T03:45:00Z | OK (recovered) | — |
| free-nemotron-lightning | `opencode/nemotron-3.5-lightning-free` | available | — | 2026-10-04T03:45:00Z | OK | — |
| free-nemotron-ultra | `opencode/nemotron-3-ultra-free` | available | — | 2026-10-04T03:45:00Z | OK | — |
| free-space-bunny | `opencode/space-bunny-free` | available | — | 2026-10-04T03:45:00Z | OK | — |

## Paid fallback

| Agent | Model ID | State | Constraint |
| --- | --- | --- | --- |
| deepseek-v4.1-flash | `opencode-go/deepseek-v4.1-flash` | standby (free capacity healthy) | Off-peak only (Go docs: peak 01:00–04:00 and 06:00–10:00 UTC Mon–Fri; all other hours and weekends are off-peak). Use only when fewer than two free models are available. Operator authorization: up to 2 concurrent DeepSeek agents until 2026-10-04 08:00 CET, **and only while free models are degraded** (fewer than two healthy); stop starting new DeepSeek agents once free capacity recovers. Free capacity recovered 2026-10-04T03:45Z (9 healthy models), so this fallback is currently on standby. |

## Conclusions so far

- 2026-10-05T10:16:00Z: `free-longcat` limited again after ~6.5h available (previous reset
  from 2026-10-04T03:45Z). Free capacity is otherwise healthy, so the DeepSeek fallback
  stays on standby; do not reassign work to free-longcat until a probe succeeds (~14:15Z).
- Only one free-model limit has been observed (free-longcat during A04); its reset
  duration is not yet measured. See the history file for the per-event findings.
- `free-fledge` is region-blocked here ("This model is not available in your country")
  and must not be selected; this is an availability limit, not a quota one.
- Sustainable operating range (measured 2026-10-03): about 8–12 active subagents, at most two
  sessions per free model, and at most ~6 containers running Go builds/tests at once. A
  18-session push crashed the Podman WSL engine and triggered simultaneous rate limits.
  Wait ~1h for cooldowns before resuming failed work.
- 2026-10-03T16:22:00Z: `free-fledge` limited after just two sessions. Even the 2-per-model
  cap can be hit when both sessions are heavy adapter tasks; keep concurrent heavy work to
  one session per model where possible. B02 remains pending.
- 2026-10-03T16:35:00Z: free-ling-3.1 still limited at 67m, so the free-model reset is not
  uniformly ~1h. free-longcat cleared at 50m48s; ling-3.1 did not. Treat the reset window as
  model- and load-dependent (up to several hours) and do not probe more often than ~hourly.
- 2026-10-03T16:41:00Z: Probe sweep — free-mimo-v2.6, free-big-pickle, free-muse-spark, and
  free-ling-3.1 all still limited at ~1h. Schedule free-model re-probes about every 2h and
  rely on the remaining healthy models meanwhile.
- 2026-10-03T16:42:00Z: free-nemotron-ultra failed a third time with a 504; treat it as
  degraded and avoid it. Healthy capacity is down to free-space-bunny and
  free-nemotron-lightning, so pause new dispatches.
- 2026-10-03T15:50:00Z: `free-longcat` also limited, leaving roughly three healthy free
  models. Stop reassigning failed tasks; wait out the ~1h cooldowns and resume at low,
  spread concurrency. If healthy free models drop below two, use the off-peak DeepSeek
  fallback per the standing rule.
- `free-nemotron-lightning` repeatedly ended long sessions with a truncated or missing final
  response (B05, C04). It remains healthy but should get small, tightly scoped tasks; prefer
  other models for large ones.
