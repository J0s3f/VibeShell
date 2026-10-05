# D05 failure and research acceptance suite — receipts

Task: PLAN 15.5 D05. Owned path: `tests/integration/**`.
Branch: `agent/d05-failure`. Container: `vibeshell-d05-failure-dev` (Podman, dedicated worktree).

## Commands and results

| Command (run in `/workspace` in the dev container) | Result |
| --- | --- |
| `go vet ./tests/integration/...` | PASS |
| `go test -race -count=1 ./tests/integration/...` | PASS (`ok j0s.at/vibeshell/tests/integration 1.852s`) |
| `go test -race -count=1 ./...` | PASS (all 25 packages `ok`) |

Raw logs: `go-test-integration.log` (verbose), `go-test-all.log`.

## Coverage

### PLAN 9.1 failure matrix (`failure_matrix_test.go`, via `internal/routing` + a fake gateway)

`TestFailureMatrixHealthScopes` drives every class through a real turn and asserts the distinct health scope:

| PLAN 9.1 row | Class | Asserted scope/state |
| --- | --- | --- |
| Invalid/revoked credential | `invalid_credential` | credential cooling, route unaffected |
| Account/product quota exhausted | `quota_exhausted` | account cooling |
| Provider rate limit (Retry-After) | `rate_limited` | account cooling at `now+RetryAfter` |
| Model removed/not found | `model_not_found` | route quarantined |
| Provider outage/5xx | `provider_outage` | route cooling |
| Network timeout | `network_timeout` | route cooling |
| Context too long | `context_too_long` | route record, stays healthy (request-scoped) |
| Invalid tool args | `invalid_arguments` | route record, stays healthy |
| Invalid response schema | `invalid_response` | route record, stays healthy |
| Content rejection | `content_rejected` | route record, stays healthy; attempt result `content_rejected` |
| User cancellation | `user_cancelled` | fatal, zero commits, zero health records |
| Concurrent world conflict | `world_conflict` | fatal, zero commits, zero health records |

Supporting tests: `TestQuotaGroupSharedAcrossKeys` (shared quota group blocks sibling account and fresh binding), `TestModelNotFoundQuarantineUnselectable`, `TestContentRejectionNotQuota`, `TestCancellationStopsPromptlyAndDiscardsStaging`, `TestWorldConflictFatalAndHealthNeutral`, `TestRetryAfterHonoredAndProbeBudget`.

### Crash/consistency (`crash_consistency_test.go`, real SQLite + `internal/admin`)

- `TestCrashAtStagedCheckpointLeavesNoMutation` — staged but uncommitted change leaves no node; persisted content is reusable.
- `TestCrashAtCommittedCheckpointIsDurableAndExact` — commit, process stop (DB close), reopen: exact bytes, revision 1, single version row.
- `TestCrashAtPartialEmissionRecoversTruthfully` — committed world + accepted frame, no transport-write outcome; `admin.RecoverIncomplete` marks the session recovered, idempotently, world mutation intact exactly once, no fabricated success.
- `TestRedrivingTurnCommitsNoDuplicateMutation` — conflicting re-drive yields a conflict, leaves one node/version and original content.

### Export completeness (`export_completeness_test.go`, real SQLite events/content through `internal/export`)

- `TestExportJSONLBundleChecksums` — `export.VerifyBundle` reports no problems; manifest marks the session `incomplete_disconnected`; emitted blob equals recorded bytes.
- `TestExportTranscriptLabels` — lossy-projection header, user input, terminal output, alt-screen snapshot, failure, and "NO recorded end" status.
- `TestExportAsciicastReplay` — v2 header 80x24, replayed output contains both committed frames, resize recorded.
- `TestExportRedactsSecrets` — `sk-*`/Bearer secret absent from every file of all three formats after redaction; redacted bundle checksums still valid.

### Failed sandbox candidate (`app_candidate_test.go`, `internal/apps` + fake sandbox)

- `TestFailedSandboxCandidateLeavesPriorVersionIntact` — extension with a simulated staging compile failure is registered with `Passed=false` and `ActivatedAt=0`, cannot be activated, current pointer stays on the prior version, fresh session resolves it.
- `TestRuntimeSandboxFailureDoesNotActivateCandidate` — runtime sandbox failure does not move the pointer or rewrite accepted source.

## Unverified / limits

- Fake-gateway classification is asserted at the routing seam; no live provider classification was probed (by design, no live calls).
- The `context_too_long`, `invalid_arguments`, `invalid_response`, and `content_rejected` classes create a route health record that stays healthy on first occurrence (the implementation only cools at the repetition threshold); the suite asserts that documented behavior.
