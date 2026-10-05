# ADR 0004: SQLite for world state and permanent research storage

Status: accepted.

Date: 2026-10-03.

## Context

PLAN 5.1-5.3 and 10.1-10.3 require exact file bytes, append-only research
records, short atomic commits, derived retrieval indexes, and backups that
survive a container replacement — in a single-process service with roughly 100
mostly idle users (PLAN 13). The storage engine, the commit protocol, and the
search strategy were therefore qualified by task A05 before implementation
relied on them.

The spike's central finding is not "SQLite works"; it is that the whole design
rests on **one writer**, and the bounded writer queue is the mechanism that
turns that fact into safe, observable behavior rather than an accident.

## Decision

- Use `database/sql` with `modernc.org/sqlite` v1.60.1 (pure Go, no cgo) as the
  single durable store for world state and permanent research records, on the
  local `/state` volume. Never on a network share.
- Run SQLite in WAL mode with `synchronous=FULL`, and apply **every** pragma per
  pooled connection through the DSN. Only `journal_mode` is persistent in the
  file; a pragma set once at open reaches one connection only.
- Serialize writes through a single-writer pool with a bounded queue. A full
  queue surfaces as backpressure, never as unbounded memory growth; submitting
  after close refuses work rather than losing it.
- Never hold a transaction across model work. Commit a staged change set in one
  short transaction that carries content, node, event, command index, path index,
  and the search projection together.
- Verify read dependencies at commit with optimistic revision checks: node
  revision, parent-directory membership revision, and absence checks. A conflict
  is a typed, distinguishable result retried after a refresh, never a silent
  overwrite and never a model-health failure.
- Store exact bytes as content-addressed BLOBs inside the database, shared by
  hash, so content and metadata commits stay atomic.
- Use `path_key` (path plus a trailing separator) with a key range for subtree
  lookups, plus exact `command`, whitespace-normalized `command`, and
  session/time indexes.
- Maintain `transcript_fts` as an **external-content** FTS5 projection over
  `search_docs`, trigger-maintained and rebuildable. It is never the source of
  truth. Where FTS5 is unavailable, fall back to index-only lookups and label
  the degraded mode explicitly in the tool result.
- Back up with `VACUUM INTO` and verify a restore by **logical fingerprint**,
  never by file checksum.

## Alternatives considered

- A separate database server (Postgres): rejected. PLAN 3.2 forbids a separate
  database service in v1, and the one-writer limitation is manageable behind a
  queue.
- `contentless` FTS5: rejected on evidence. It cannot be maintained by a plain
  `DELETE` trigger (`cannot DELETE from contentless fts5 table (1)`), so triggers
  cannot keep it in sync.
- `LIKE`-based subtree lookups: rejected on evidence. They need manual wildcard
  escaping with an explicit `ESCAPE '\'` clause (SQLite has no default backslash
  escape) and degrade to a covering-index scan for substrings.
- A separate blob store: deferred. Content and metadata would no longer commit
  atomically; revisit only if measurement justifies it and a crash protocol is
  specified.
- `synchronous=NORMAL`: rejected for v1. It measured ~1376 turns/s against
  ~546 turns/s for `FULL` on the same shape. The 2.5x is real, but at 546
  turns/s `FULL` is far above any plausible interactive rate and PLAN 10.3
  requires permanent records. Revisit only with a measured reason and a
  documented policy; never per transaction.

## Consequences

- Every read-modify-write path carries read dependencies; tools and services
  cannot read without recording what they read. This is more work up front and
  is what makes stale listings unable to validate invalid writes.
- Conflict handling becomes a normal, expected path rather than an error path,
  so the agent loop needs a bounded rebase budget.
- World commits and accepted output/frame references share one transaction, so
  the "committed but never delivered" case (PLAN 10.3) must be recoverable
  without re-applying the change.
- Only one process may write per database. Multiple VibeShell processes or
  replicas against one volume were **not** tested and would contend at the
  database level.
- One FTS5 index plus a fallback means retrieval quality is not uniform; callers
  must be able to say which mode produced a result.
- The schema implemented by the B01 world-store adapter is a **subset** of the
  PLAN 10.2 families. Migration numbering and ownership stay with that adapter's
  owner.

## Evidence

- Research: `docs/research/2026-10-03-storage-spike.md` (nine gates, all PASS).
- Receipts: `experiments/storage/receipts/` — `go-test-plain.txt` (32/32),
  `go-test-race.txt` (32/32), `cgo-disabled.txt`, `restart-transcript.txt`
  (5 phases, 3 container replacements), `measurements-plain.json`,
  `gate5-atomic-commit.txt`, `gate7-subtree-boundary.txt`.
- Environment as measured: SQLite 3.53.4, `ENABLE_FTS5` true, `FTS4`/`FTS3`/
  `JSON1` false; `/state` on ext4, page size 4096.
- Commits: `8c13342` (spike), merged as `83f1ec4`; adapter `c8b35e2`, merged as
  `55fcb0a`.

## Unverified items and limitations

- **Restore is verified by logical fingerprint, not file checksum.** The live
  file's sha256 changes across a restore while the logical content does not.
  Any future check that compares file checksums would fail a correct restore.
- One SQLite version, one driver version, one container, one filesystem, one
  machine. Nothing here is evidence about another SQLite, platform, or the
  deployed image.
- The 546 turns/s figure is a 240-turn burst, not a soak test. Long-run growth,
  vacuum pressure, and multi-session behavior over a real world are unmeasured;
  storage growth over weeks is unknown.
- The unclean-exit evidence uses `os.Exit(1)` inside a test process plus a real
  container replacement. It exercises SQLite's real WAL recovery but is **not** a
  machine-level power-loss or host-kill test.
- Race-detector timings are inflated roughly 5x and are kept in separate
  labelled receipts. Only the plain-run numbers are load-bearing.
- No storage-failure injection. Disk full, read-only volume, and I/O error paths
  are untested, so PLAN 10.3's "stop accepting new semantic work" behavior
  remains unverified.
- Backup scope was partial: world and transcript only. Host keys and app
  versions, which PLAN 10.3 requires the restore test to cover, were not
  restored. Large-database restore time was not measured.
- Concurrency was tested in a single process. `cwd` was `/` in all fixtures, so
  per-session cwd isolation is not covered.
- FTS5 under `unicode61` does not tokenize emoji; CJK tokenization was exercised
  only on the tested input. No non-Latin or stemmed search-quality assessment was
  made.
- Open, unanswered: `node_versions` necessity, `contents` eviction policy,
  whether `synchronous` may ever be per operation class, the payload size that
  needs an external reference, backup cadence and retention location, restore
  time budget, and whether the degraded fallback is surfaced to the model. These
  are recorded in the spike report and remain open.