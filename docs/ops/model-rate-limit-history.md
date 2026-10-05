# Free-model rate-limit history

Append one entry per observed quota/rate-limit event, and record each probe result.
After every addition, re-read that model's entries and write a conclusion (inferred
window and measured reset duration). Keep all times in UTC.

## Events

### 2026-10-03T14:25:42Z — free-longcat
- Model: `opencode/longcat-2.5-preview-free` (OpenCode)
- Task: A04 sandbox qualification spike
- Session: `ses_efdf2c2dcffeZaHWy4G1TfpfuU`
- Observed: `AI.Error.QuotaExceeded: Rate limit exceeded. Please try again later.`
  after many model steps in a single long agentic loop.
- Probe 2026-10-03T14:32:34Z: still rate-limited (6m52s after the failure).
- Reset observed at: 2026-10-03T15:16:30Z (probe OK).
- Elapsed to reset: 50m48s.
- Inferred window: roughly one hour; not sub-minute, not a daily cap. Treat a free-model
  limit as ~hourly and re-probe about an hour after the failure.

### 2026-10-03T15:28:00Z — free-ling-3.1
- Model: `opencode/ling-3.1-flash-free`
- Sessions affected: C03 app registry, C07 MOTD/presentation, and the opencode-live-probe harness —
  three concurrent sessions on one model.
- Observed: `AI.Error.QuotaExceeded: Rate limit exceeded. Please try again later.`
- Reset observed at: pending (expect ~1h, based on free-longcat's 50m48s).
- Inferred window: consistent with the ~1h free-model window; almost certainly aggravated by
  running three concurrent sessions on a single model.
- Probe 2026-10-03T16:35:00Z: still rate-limited (67m after the failure); the ~1h expectation
  did not hold. Next probe ~17:35Z.

### 2026-10-03T15:40:00Z — cascade: free-mimo-v2.6, free-big-pickle, free-muse-spark
- Sessions affected: B08 (mimo), B02 reassignment (mimo), C01 (big-pickle), runtime rework
  (big-pickle), B03 reassignment (muse-spark), B05 (muse-spark).
- Observed: `AI.Error.QuotaExceeded: Rate limit exceeded. Please try again later.` on all
  at once.
- Context: coincided with the Podman WSL engine becoming unreachable (socket refused)
  under 18 concurrent dev containers.
- Reset observed at: pending (~1h).
- Inferred window: ~1h, but simultaneous limits across three models show that very high
  concurrency overruns both the host and free-quota capacity.

### 2026-10-03T15:50:00Z — free-longcat
- Model: `opencode/longcat-2.5-preview-free`
- Sessions affected: C02 (tools/context) and C03 (app registry), both reassigned onto it
  after earlier failures.
- Observed: `AI.Error.QuotaExceeded: Rate limit exceeded. Please try again later.`
- Reset observed at: pending (its earlier reset took 50m48s).
- Inferred window: ~1h. Near-total free-model exhaustion now — pause dispatching until the
  cooldowns clear.

### 2026-10-03T16:22:00Z — free-fledge
- Model: `opencode/fledge-alpha-free`
- Sessions affected: B02 resume (event store) and B08 (config/secrets). B08 completed
  successfully immediately before; B02 failed.
- Observed: `AI.Error.QuotaExceeded: Rate limit exceeded. Please try again later.`
- Reset observed at: pending (~1h).
- Inferred window: ~1h. Two sessions were enough to trip the limit under resumed load.

### 2026-10-05T10:16:00Z — free-longcat
- Model: `opencode/longcat-2.5-preview-free` (OpenCode)
- Task: L3a pinned app-version resolver (`internal/apps`)
- Session: `ses_ef48b1a7effegvM0aKmK60y1sD`
- Observed: `AI.Error.QuotaExceeded: Rate limit exceeded. Please try again later.`
- Context: one session on the model. An earlier L3a attempt on `free-space-bunny` ended with
  a streaming (`unprocessable entity`) error, not a quota one, so this is free-longcat's
  first limit in this workstream.
- Reset observed at: pending.
- Inferred window: the 2026-10-04T03:45Z sweep had every free model available, and
  free-longcat has served work since, so this is a fresh limit. Free-model resets measured
  so far are multi-hour, so the next probe is scheduled ~4h out (2026-10-05T14:15Z).

## Non-quota availability events

- 2026-10-03T23:13:00Z — `opencode/nemotron-3.5-lightning-free` (free-nemotron-lightning):
  a 504 idle timeout ended the config hot-reload task mid-work (uncommitted partial state).
  Marked `degraded`; the task was reassigned to DeepSeek.

- 2026-10-03T16:42:00Z — `opencode/nemotron-3-ultra-free` (free-nemotron-ultra): 504 idle
  timeouts ended the C02, B03 (twice), and C01 resumes. Despite a successful probe at 16:10Z,
  the endpoint is unstable; marked `degraded` and no longer selected.

### 2026-10-03T16:40:00Z — probe sweep: still limited
- free-mimo-v2.6 (~60m), free-big-pickle (~58m), free-muse-spark (~57m), free-ling-3.1 (~67m)
  all still returned `AI.Error.QuotaExceeded`.
- Conclusion: the reset window is not uniformly ~1h; free-longcat's 50m48s was not
  representative. Treat exhausted free models as unavailable for several hours and prefer
  waiting for currently-running work to free healthy models.

- 2026-10-03T16:10:00Z — `opencode/nemotron-3-ultra-free` (free-nemotron-ultra): probe returned
  OK; the two 504s were transient. State → `available`, and it was used to resume B03 and C01.

- 2026-10-03T15:40:00Z — Podman WSL engine (`podman-machine-root`) became unreachable
  (socket refused) under the 18-container load; a stray `podman-machine-default` machine
  appeared and the connection was rewritten. Recovered by stopping/starting the root
  machine, shutting WSL down, and re-adding the SSH connection; container/image state was
  preserved.

- 2026-10-03T14:42:00Z — `opencode/fledge-alpha-free` (free-fledge): the harness returned
  "This model is not available in your country". Geographic restriction, not a quota
  issue; the model is unusable from this region and must not be selected. The
  auth-argon2 spike was reassigned to `free-mimo-v2.6`.

- 2026-10-03T14:44:00Z — `opencode/fledge-alpha-free` (free-fledge): re-probed after the
  operator enabled a VPN; the minimal probe returned OK, so the earlier geo-block is
  bypassed. State returned to `available` and the model was put back into use.

- 2026-10-03T15:00:00Z — `opencode/ling-3.0-flash-fin-free` (free-ling-3.0): B01 and B03
  failed with "Upstream request failed: Endpoint is unavailable"; a minimal probe
  reproduced it, so this model's endpoint is down (NOT a quota). Both tasks were
  reassigned to other models; retry after the scheduled time.

- 2026-10-03T15:24:08Z — `opencode/nemotron-3-ultra-free` (free-nemotron-ultra): the C02
  session ended with "Streaming response failed: [504] Upstream idle timeout exceeded".
  Transient upstream idle timeout, not a quota; the worktree had no partial work, so C02
  was reassigned to `free-longcat`. The model state is left `available` (its B03 session
  was still running), but a second failure would justify a cooldown.
- 2026-10-03T15:37:00Z — `opencode/nemotron-3-ultra-free` (free-nemotron-ultra): a second
  504 idle timeout ended B03 mid-work (uncommitted partial state). Two consecutive upstream
  504s → state `degraded`, next probe ~16:08Z; B03 reassigned to `free-muse-spark`.

## Conclusions

### 2026-10-05T10:16:00Z — free-longcat re-limited after ~6.5h of availability
- Re-reading free-longcat's entries: first limit 2026-10-03T14:25:42Z, reset 15:16:30Z
  (50m48s); re-limited 15:50:00Z, recovered by the 2026-10-04T03:45Z sweep (≈12h); available
  until 2026-10-05T10:16:00Z, then limited again.
- Inferred window: still consistent with a sustained/multi-hour cap rather than a per-minute
  throttle; one reset was ~51m but the others were ≥7h. No reset duration measured for this
  event yet; probe ~4h out and record it.
- Policy: do not reassign more work to free-longcat until a probe succeeds; spread remaining
  work over other free models, and use the off-peak DeepSeek fallback if fewer than two free
  models stay healthy.

### 2026-10-04T03:45:00Z — probe sweep: free models recovered
- Probed via the authenticated OpenCode CLI (`opencode run -m opencode/<model>`), which is a
  valid client for the Console free tier.
- Available: big-pickle, fledge-alpha-free, ling-3.1-flash-free, longcat-2.5-preview-free,
  mimo-v2.6-flash-free, muse-spark-1.3-contributor-free, nemotron-3.5-lightning-free,
  nemotron-3-ultra-free, space-bunny-free.
- Unavailable: ling-3.0-flash-fin-free (endpoint unavailable; not a quota).
- Conclusion: the models limited at 2026-10-03T15:40–16:22Z had recovered by ~03:45Z
  (~11–12h later). Combined with the 22:42Z sweep (still limited at ≥7h), the free-model
  reset window is multi-hour and consistent with a sustained/daily cap, not ~1h. Re-probe
  no more often than every few hours, and spread work across models.

### 2026-10-03T22:42:00Z — probe sweep: free models still limited (~7h)
- free-big-pickle, free-fledge, free-ling-3.1, free-longcat, free-mimo-v2.6, and
  free-muse-spark all still returned `AI.Error.QuotaExceeded`.
- Conclusion: free-model limits persist well beyond an hour (≥7h here) and are effectively
  daily/sustained. Free capacity stays degraded, so the conditional off-peak DeepSeek
  fallback (window until 2026-10-04 06:00 UTC) is justified.

- 2026-10-03T14:31:54Z: First free-model limit observed. Free-longcat had already been
  replaced on A04, so the event caused no lost work. The failure came after a long
  agentic loop, consistent with a per-minute token/request throttle rather than a
  hard monthly cap. No reset duration measured yet, so no window can be inferred.
  Next step: probe at the scheduled time and record the result here.
- 2026-10-03T14:32:34Z: A probe 6m52s after the first failure returned the same quota
  error, so this is not a sub-minute per-minute throttle. The next probe is scheduled
  about 30 minutes out; if that also fails, the window is likely hourly or daily, which
  means a `-free` model can stay unavailable for a long stretch and the fallback pool
  should not depend on any single free model.
- 2026-10-03T15:16:30Z: First measured reset. free-longcat cleared after 50m48s, consistent
  with a rolling ~1-hour window rather than a per-minute throttle or a daily cap. Policy:
  after a free-model quota error, defer the next probe ~1 hour; escalate toward daily only
  if an hour passes without recovery. Because a limited free model can return within the
  same working session, re-check the ledger hourly.
- 2026-10-03T15:30:00Z: Three concurrent sessions on one free model (free-ling-3.1) triggered
  a rate limit at once. Cap concurrent subagent sessions at two per free model and spread
  work across models; expect ~1h to recovery.
