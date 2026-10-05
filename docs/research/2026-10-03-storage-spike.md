# A05 storage qualification spike: SQLite for world state and research storage

Research date: 2026-10-03. Task A05, branch `agent/a05-storage`. Spike code and receipts: [`experiments/storage`](../../experiments/storage); its README has the per-gate commands and layout.

**Status of this document.** Everything below is either a requirement taken from [PLAN.md](../../PLAN.md) (§§3.2, 5.1-5.3, 10.2-10.3), a fact observed on this machine with a receipt, a recommendation, or an open question. The facts are qualified on one pinned toolchain on one container. They are **not** frozen contracts: PLAN 10.2 still says the schema families are "finalized in a migration ADR", and the recommendations below are input to that ADR, not its outcome.

## Decision in one paragraph

Proceed with `database/sql` + `modernc.org/sqlite` (pure Go, no cgo), SQLite in WAL mode with `synchronous=FULL`, a single-writer pool behind a bounded queue, staged change sets committed atomically with optimistic revision checks, content-addressed BLOB storage for exact bytes, a `path_key` index range for subtree lookups, an external-content FTS5 table as a derived projection, and `VACUUM INTO` for backups. All nine gates passed. The one qualification that matters for planning: **the whole design rests on one writer**, so the bounded writer queue is not an optimization but the mechanism that makes concurrent turns safe, and gate 4 measured what happens to other writers when a transaction is held too long.

## What was qualified

| Item | Value |
| --- | --- |
| Toolchain | `go1.27.1`, module `j0s.at/vibeshell/experiments/storage` |
| Driver | `modernc.org/sqlite` v1.60.1, `database/sql` driver name `sqlite` |
| SQLite | `sqlite_version()` 3.53.4, `sqlite_source_id()` 2026-07-24 19:02:57 `bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc` |
| Compile options | `ENABLE_FTS5` true; `ENABLE_FTS4`, `ENABLE_FTS3`, `ENABLE_JSON1` false; `ENABLE_MATH_FUNCTIONS` true |
| Storage | `/state` named volume, **ext4** on `/dev/sdd`, page size 4096, UTF-8. Local block storage; never a network share |

Receipts: [`gate1-driver.txt`](../../experiments/storage/receipts/gate1-driver.txt), [`gate1-schema.txt`](../../experiments/storage/receipts/gate1-schema.txt).

## Commands and results

All commands ran in this checkout's Podman container from the repository root.

| Command | Result |
| --- | --- |
| `& ./scripts/dev.ps1 start` | PASS |
| `& ./scripts/dev.ps1 exec -Command 'sh','-c','cd /workspace/experiments/storage && gofmt -l . && go vet ./...'` | PASS, no output |
| `& ./scripts/dev.ps1 exec -Command 'sh','-c','cd /workspace/experiments/storage && go test -count=1 ./...'` | **PASS**, 32/32 tests, 3.807s → [`go-test-plain.txt`](../../experiments/storage/receipts/go-test-plain.txt) |
| `& ./scripts/dev.ps1 exec -Command 'sh','-c','cd /workspace/experiments/storage && go test -race -count=1 ./...'` | **PASS**, 32/32 tests, 10.754s, no data races → [`go-test-race.txt`](../../experiments/storage/receipts/go-test-race.txt) |
| `& ./scripts/dev.ps1 exec -Command 'sh','-c','cd /workspace/experiments/storage && CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go test -count=1 ./...'` | **PASS** → [`cgo-disabled.txt`](../../experiments/storage/receipts/cgo-disabled.txt) |
| `pwsh -File experiments/storage/restart-spike.ps1` | **PASS**, 5 phases, 3 container replacements, all 6 expected receipts produced → [`restart-transcript.txt`](../../experiments/storage/receipts/restart-transcript.txt) |

No test was skipped, and no test was left failing. Every claim below points at a receipt written by the test that observed it.

## Gate 1 — driver, version, and schema · PASS

Registered drivers: exactly one, `sqlite`. The schema applies cleanly: `contents`, `nodes`, `directory_revisions`, `events`, `command_index`, `path_index`, `search_docs`, all `STRICT`. `integrity_check` is `ok` and `foreign_key_check` returns 0 rows.

`STRICT` tables reject type confusion at the boundary: storing an integer in `contents.bytes` fails with `cannot store INT value in BLOB column (3091)`, storing text in `nodes.revision` fails likewise. Foreign keys are `DEFERRABLE INITIALLY DEFERRED`, so a namespace root and its children can be inserted in one transaction and still pass the check at commit; a genuinely dangling parent is still rejected (`FOREIGN KEY constraint failed (787)`).

A corrupt file is refused rather than silently misread: `SELECT 1` against random bytes returns `file is not a database (26)`.

*Fact:* the driver is pure Go, so no cgo toolchain is required (`cgo-disabled.txt`).

*Fact for Phase B:* `PRAGMA threadsafe` and `PRAGMA case_sensitive_like` return **no rows** in v1.60.1 — the driver consumes both itself. Configuration must therefore be verified by behaviour, not by reading the pragma back. This is the shape of problem to expect for other driver-consumed settings.

## Gate 2 — FTS5, and the fallback without it · PASS

`ENABLE_FTS5` is true and `transcript_fts` is created as an **external-content** FTS5 table over `search_docs` (`unicode61 remove_diacritics 2`), maintained by insert/delete/update triggers. Retrieval works: bare terms, column filters (`command:grep`, `path:report`), column+term conjunction, phrases (`"ls -la"`), prefix tokens (`ca*`), diacritic folding (`zoe` → `Zoë`), CJK tokens, punctuation, and `bm25` ranking all return the expected event ids. Snippets are produced. Emoji are **not** tokenized (`unicode61` gives no token for them), so a search for an emoji-only command finds nothing.

*Fact that chose the design:* a **contentless** FTS5 table cannot be maintained by a plain `DELETE` trigger — `cannot DELETE from contentless fts5 table (1)`. The external-content form is therefore the one to build: `search_docs` stays authoritative, the index is derived, `'rebuild'` remains available for repair, and projection updates and deletes propagate (verified true, true, and 7 hits after a deliberate rebuild).

*Fact:* the FTS5 table exposes only the indexed columns plus `rowid`; the content lookup is a separate covering-index search on `search_docs (event_id=?)`.

Fallback when FTS5 is unavailable is index-only and honest about its limits. Exact command, normalized command, and subtree path queries stay index-backed:

```
SEARCH command_index USING INDEX command_index_exact (command=?)
SEARCH command_index USING INDEX command_index_normalized (normalized_command=? AND cwd=?)
SEARCH path_index USING INDEX path_index_key (path_key>? AND path_key<?)
```

A mid-token substring degrades to `SCAN command_index USING COVERING INDEX`, with no ranking, stemming, or phrase support. *Recommendation:* ship the fallback as a degraded mode that is explicitly labelled as such in the tool result, so the agent knows it is not receiving ranked full-text results.

## Gate 3 — durability across a container replacement · PASS

Pragmas are applied **per pooled connection** and reach all four connections: `journal_mode=wal`, `synchronous=2` (FULL), `busy_timeout=5000`, `foreign_keys=1`. This matters because `database/sql` hands out several connections and only `journal_mode` is persistent in the file — a pragma set once at open would apply to one connection only.

WAL lifecycle: after a seed commit `wal.db 4096 B`, `wal.db-shm 32768 B`, `wal.db-wal 193672 B`; after a `TRUNCATE` checkpoint `wal.db 102400 B`, `wal.db-wal 0 B`; after close only `wal.db 102400 B` remains.

The cross-container proof ([`restart-transcript.txt`](../../experiments/storage/receipts/restart-transcript.txt)):

1. `write-unclean` — commit a row, then end the process **without closing the database**. Exit 1 as expected. On disk: `vibeshell.db 4096 B`, `vibeshell.db-wal 201912 B` — the committed frames are still only in the log.
2. `stop` / `start` — the container is replaced.
3. `read` — a **different container** opens the same ext4 volume. Bytes identical to what was written, size 18, `sha256 97e8d54f8a7255106f66cf1b213c4a4d7b558d21310b1c2bfedc3d0d526dddda`, `integrity_check: ok`, `foreign_key_check` 0 rows.

This is SQLite's own recovery, driven by the log next to the database. *Fact:* state on `/state` survives a container replacement including an unclean exit, so the `/state` named volume is a sound place for permanent records.

## Gate 4 — concurrency · PASS

SQLite admits **one writer at a time**. The contender is refused with `database is locked (5)` (`SQLITE_BUSY`), surfaced as a `*sqlite.Error` with code 5 — so the driver gives an inspectable error code rather than an opaque string. It is accepted immediately once the writer releases.

Cost of holding the write lock: a writer holding it for **250 ms** made a patient writer wait **332.9 ms**. Every second of lock holding is a second of delay for all other writers. *Consequence for PLAN 5.3:* a turn must never hold a transaction across model work — this measurement is the evidence behind that requirement.

Readers beside one writer, 8 readers / 4 submitters / queue capacity 8, 120 commits:

| | plain | `-race` |
| --- | --- | --- |
| commits completed | 120/120, 0 refused, 0 failures | 120/120, 0 refused, 0 failures |
| writer throughput | **272.4 commits/s** | 135.2 commits/s |
| mean / longest commit | 3.83 ms / 22.56 ms | 7.65 ms / 25.77 ms |
| submit→accepted p50 / p95 | 7.51 ms / 88.08 ms | 16.91 ms / 76.87 ms |
| reader query samples | 73644 | 7992 |
| reader query p50 / p95 / p99 | 31 µs / **128 µs** / 298 µs | 767 µs / 1.72 ms / 2.54 ms |
| reader failures | 0 | 0 |

Readers are not blocked by the writer, which is the WAL behaviour the design depends on.

The bounded writer queue applies real backpressure: capacity 4 → 4 accepted, 9 refused, depth pinned at 4, 0 out-of-order applications, 0 failures. Submitting after close refuses work rather than losing it silently. *Fact:* backpressure is implementable and observable; a full queue must surface as a bounded-retry conflict, not as unbounded memory growth.

## Gate 5 — concurrency safety without a server · PASS

Optimistic revision checking: two writers both read revision 1 of `seed-0000`; exactly one changed a row (1→2) and the other changed nothing. The guarded `UPDATE` matches one row for exactly one writer.

A stale directory listing is a **conflict, not a silent overwrite**: revision read 1, revision now 2 → `conflict on dir (directory listing): membership revision is 2, the turn read 1`; after a refresh the rebase is accepted. A concurrent create of the same name yields 1 accepted / 1 refused. Reasons are distinguishable in the message: revision from the future (`revision is 1, the turn read 100`), node removed by another turn, parent directory with no membership revision.

Atomicity holds across the whole staged change set — content, node, event, command index, path index, and the search projection are each exactly 1 row. A commit forced to fail on `UNIQUE constraint failed: events.session_id, events.sequence (2067)` left **0** content, node, event, and projection rows, and the directory revision unchanged at 2. Content whose hash does not match its bytes is rejected.

*Recommendation:* adopt this as the PLAN 5.3 commit shape, with the typed `ConflictError` carrying the scope and reason, so a conflict is retried after a refresh rather than being treated as a model-health failure.

## Gate 6 — exact bytes and dedup · PASS

Every payload came back byte-identical, with `typeof=blob` and `length()` equal to the stored size, identical again after a checkpoint:

| Payload | Bytes | Result |
| --- | --- | --- |
| empty | 0 | identical, `blob` |
| single NUL | 1 | identical, `blob` |
| single `0xff` (invalid UTF-8) | 1 | identical, `blob` |
| damage pattern | 26 | identical, `blob` |
| UTF-8 text | 29 | identical, `blob` |
| pseudo-random | 262144 (256 KiB) | identical, `blob` |

NUL bytes, invalid UTF-8, and page-crossing payloads survive exactly. Note the empty payload must be stored as `[]byte{}`; a nil blob is rejected by `NOT NULL`.

Dedup: three files share **one** 65536-byte blob (1 content row, 2 nodes referencing it); re-presenting the same content in a rebase is accepted; a single changed byte produces a **separate** blob (2 content rows, 131072 distinct bytes stored). The content key separated all 128 single-bit mutations of a 64-byte probe with **0** collisions, and `sha256` of the empty blob is `e3b0c442…`, of a single NUL `6e340b9c…`.

*Recommendation:* keep content-addressed BLOB storage in the database for v1, which keeps content and metadata commits atomic (PLAN 5.1). Revisit a separate blob store only if measurement justifies it and its crash protocol is specified.

## Gate 7 — boundary-aware, index-backed lookups · PASS

`path_key` is the path plus a trailing separator, so every descendant of `p` starts with `p + "/"` and a subtree is a contiguous key range — no `LIKE`, no case folding, no wildcard escaping:

```
path_key >= PathKey(prefix) AND path_key < SubtreeRange(prefix)
```

Against 15 fixtures, 10 prefixes return boundary-correct results. `/some` → 4 paths and excludes siblings `/somewhere`, `/some-else`, `/sometext`, `/some_thing`; `/some/place` → 3; `/` → all 15. Wildcards and underscores in real paths are not treated as patterns: `/100%` → 1, `/100%file` → 1, `/a_b` → 1.

The range agrees exactly with escaped `LIKE` on all 10 prefixes (0 differing result sets), which is a useful cross-check: SQLite has **no default backslash escape** in `LIKE`, so the variant needs an explicit `ESCAPE '\'` — `/100%` → `/100\%/`, `/a_b` → `/a\_b/`.

Lookups are case-sensitive by construction: 4 paths under `/some`, 0 under `/SOME`; `cat /x.txt` matches, `CAT /x.txt` does not. `case_sensitive_like` is effective even though the driver reports no value for it.

Normalized command lookup trims and collapses whitespace only, preserving case (`"  cat  /some/file.txt "` → `cat /some/file.txt`), grouping 4 spacing variants under one key.

Query plans confirm index use:

```
subtree range        SEARCH path_index USING INDEX path_index_key (path_key>? AND path_key<?)
exact path           SEARCH path_index USING INDEX path_index_path (path=?)
exact command        SEARCH command_index USING INDEX command_index_exact (command=?)
normalized command   SEARCH command_index USING INDEX command_index_normalized (normalized_command=?)
session transcript   SEARCH events USING INDEX events_user_time (session_id=?)
substring LIKE       SCAN path_index USING COVERING INDEX path_index_path
```

*Fact:* prefix, exact, and session lookups are index searches; a substring `LIKE` falls back to a covering-index scan. That is the measurement behind preferring the key range. Note neither `command` nor `path` is unique — many events touch the same path — so only `(event_id, command)` and `(event_id, path)` are unique.

## Gate 8 — backup and restore into fresh containers · PASS

Backup (container 1): the world is checkpointed, then copied with `VACUUM INTO`. The copy's logical fingerprint equals the live database's; `integrity_check` is `ok` in both; 3 events, 3 contents, 3 full-text hits, 3 subtree hits. Both files occupy 159744 B **including** the 32768-byte shared-memory file, because the measurement accounts for the space a database uses while open — the copy is a compacted 126976 B database, not a byte-for-byte image.

Restore (container 2, replaced): the copy is read, rebuilt with `VACUUM INTO` into a fresh file (fingerprints equal, `integrity_check: ok`), and put in place with an atomic rename within the volume.

**Caveat worth carrying into the ADR:** the live file's own `sha256` changes across a restore — `0a5064b0debc…` before, `94277b8a885d…` after — while the logical fingerprint is unchanged. A restore must be verified by logical content, never by comparing file checksums.

Verify (container 3, replaced again): the replaced database matches the fingerprint recorded beside it, `integrity_check: ok`, `foreign_key_check` 0 rows, 3 events / 3 contents / 3 full-text hits / 3 subtree hits, `live.db 126976 B`, `live.db-wal 0 B`.

*Fact:* `VACUUM INTO` is SQLite's consistent snapshot mechanism and it restores into a container that never saw the original. It requires the target to be absent and accepts a bound parameter.

## Gate 9 — rough writer-load measurement · PASS (rough)

Shape: one 4096-byte file + one event + its command and path index rows per turn; 240 turns; 4 concurrent submitters; queue capacity 8.

| | `synchronous=FULL` | `synchronous=NORMAL` |
| --- | --- | --- |
| plain run | **546.0 turns/s** (240 in 0.440 s) | **1376.2 turns/s** (240 in 0.174 s) |
| commit latency | mean 1.81 ms, max 7.37 ms | mean 0.71 ms, max 13.42 ms |
| turn body p50 / p95 / p99 | 0.28 / 0.40 / 0.55 ms | 0.20 / 0.35 / 0.61 ms |
| `-race` run | 96.8 turns/s | 111.1 turns/s |

Both settings: 0 rejections, 0 failures, 1531904 B, 366 pages. `FULL` costs roughly 60% of `NORMAL`'s throughput here (a 2.5× difference).

*Recommendation:* keep `synchronous=FULL` for v1. At 546 turns/s it is far above any plausible interactive rate, and PLAN 10.3 requires permanent records; the measured price is not worth weakening the guarantee. Revisit only with a measured reason, and never per-transaction without a documented policy. The `-race` numbers are recorded separately precisely because race instrumentation inflates timings by ~5× and would be misleading if mixed in.

## Recommendation (input to the storage ADR)

**Adopt** `database/sql` + `modernc.org/sqlite` for durable world state and permanent research storage in v1, on the `/state` volume, with:

1. **WAL + `synchronous=FULL`**, applied per connection through the DSN, because only `journal_mode` is persistent and the pool has several connections.
2. **A single-connection writer pool plus a bounded writer queue.** One writer at a time is SQLite's model, not an accident; the queue converts lock contention into observable backpressure. A turn must not hold a transaction across model work (a 250 ms hold cost another writer 332.9 ms).
3. **Staged change sets committed in one short transaction** with optimistic revision checks on the node and the directory listing revision, plus absence checks. Conflicts are typed, distinguishable, and rebased after a refresh inside a bounded retry budget.
4. **Content-addressed BLOBs in the database** for exact bytes, shared by hash, which keeps content and metadata commits atomic.
5. **`path_key` subtree ranges** for path lookups, with `path`, `normalized_command`, and the session/time indexes as measured.
6. **External-content FTS5** (`transcript_fts` over `search_docs`, trigger-maintained, rebuildable) as a derived projection that is never the source of truth, plus the index-only fallback as an explicitly labelled degraded mode.
7. **`VACUUM INTO` for backups**, verified by logical fingerprint after restore — not by file checksum.
8. **Storage failure treated as first-class**: if recording cannot continue, stop accepting new semantic work rather than continuing silently (PLAN 10.3). The schema supports this; the failure path itself was **not** exercised by this spike.

**Alternatives considered.** A separate database server (Postgres) was rejected: PLAN 3.2 forbids a separate database service in v1, and the one-writer limitation is manageable with a queue. `contentless` FTS5 was rejected on evidence — it rejects plain `DELETE`, so triggers cannot maintain it. `LIKE`-based subtree lookups were rejected on evidence — they need manual wildcard escaping with an explicit `ESCAPE` clause and degrade to a scan for substrings. A separate blob store was deferred pending measurement.

## Limitations and unverified items

- **Pinned and narrow.** One SQLite (3.53.4) and one driver version (v1.60.1), one container, one filesystem (ext4), one machine. Nothing here is evidence about another SQLite version, another platform, or the deployed image.
- **Gate 9 is a rough figure**, not a capacity limit. It is a 240-turn burst, not a soak test. No long-running growth, vacuum-pressure, or many-session behaviour was measured. Storage growth over a real multi-week world is **unmeasured**.
- **The unclean exit is simulated** with `os.Exit(1)` inside a test process. This exercises SQLite's real recovery from the log next to the database and a real container replacement, but it is **not** a machine-level power-loss or `SIGKILL`-of-the-host test.
- **Race-detector timings are inflated** (~5×) and are kept in separate labelled receipts for that reason. Only the plain-run numbers are load-bearing.
- **No storage-failure injection.** Disk full, read-only volume, and I/O error paths are untested; PLAN 10.3's requirement to degrade explicitly remains unverified.
- **Backup scope is partial.** The spike restores a world and its transcript, not host keys or app versions as PLAN 10.3 requires of the restore test. Restore time for a large database was not measured.
- **FTS5 emoji are not tokenized** under `unicode61`; CJK tokenization worked for the tested input but was not exercised broadly. No stemming or non-Latin search quality assessment was made.
- **Concurrency was tested with one process.** Multiple VibeShell processes or replicas against one volume were not tested, and the single-writer assumption is a *per-database* property, so a second process would contend at the database level too.
- **`cwd` is recorded as `/`** in the fixtures, so per-session cwd isolation (PLAN 5.3) is not covered by this spike.
- **The queue has no per-request result channel** in this spike: completion is observed by the submitter, which is enough to measure backpressure but not a full request/response API.
- **Driver behaviour that is consumed rather than queryable** (`threadsafe`, `case_sensitive_like`) can only be verified behaviourally; a configuration regression would show up as a failing behaviour test, not a changed pragma.
- **The schema is a subset** of PLAN 10.2, not the final schema. Tables such as `apps`, `facts`, `summaries`, `route_health`, and `export_jobs` were out of scope here.

## Open questions for Phase B

1. Do we need `node_versions` (PLAN 10.2) as a real table, or are events plus the `search_docs` projection sufficient to reconstruct history? The spike stored nodes and events only.
2. What is the eviction policy for `contents`? Content is immutable and shared, but the spike says nothing about when unreferenced blobs may be dropped, and PLAN 10.3 forbids automatic retention expiry.
3. Should `synchronous=NORMAL` ever be configurable per operation class (terminal input vs. permanent records), and who authorizes the change?
4. How large may a single event payload be before it needs the bounded external payload reference of PLAN 10.2? The spike's largest payload was 256 KiB.
5. What is the backup cadence and retention, and where do snapshots live — a separate volume, or off-host object storage? A network share was explicitly out of scope for this spike.
6. Does the terminal-input grouping of PLAN 10.3 ("short bounded transactions") need its own measurement? This spike only measured whole-turn commits.
7. What is the restore-time budget? A large world restored via `VACUUM INTO` was not timed.
8. Should the degraded search fallback be surfaced to the model as a labelled result, or hidden behind a single interface that always reports its mode? This is a product decision, not a storage one.

## Repository impact

None. This spike is an isolated nested module under `experiments/storage` with its own `go.mod`/`go.sum`; the root `go.mod`/`go.sum`, `scripts/`, `containers/`, `cmd/`, `internal/`, `PLAN.md`, and `FEATURES.md` are unchanged. Nothing here is application code, and no spike assumption has been written into PLAN as a decided contract.