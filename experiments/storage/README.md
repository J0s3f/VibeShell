# A05 storage qualification spike

An isolated experiment that qualifies SQLite as durable world state and permanent
research storage for VibeShell, against PLAN 3.2, 5.1-5.3, and 10.2-10.3.

This directory is **not application code**. Nothing here is imported by the shell.
Its purpose is to answer nine questions with tests and leave evidence behind, so
that Phase B can adopt a storage design with the facts already known instead of
assumptions frozen by accident. The findings and the resulting recommendation live
in [`docs/research/2026-10-03-storage-spike.md`](../../docs/research/2026-10-03-storage-spike.md).

The spike is a self-contained Go module. It has its own `go.mod`/`go.sum` and does
not depend on the repository root module, so it can pin a driver version without
touching the root manifest.

## Layout

| File | Role |
| --- | --- |
| `spike.go` | Package overview, connection `Options`/`Pragmas`, the PLAN 10.2 schema, content hashing, and the receipt/mount helpers. |
| `commit.go` | Staged change sets, optimistic revision checks, `ConflictError`, and the atomic commit with its event and index rows. |
| `event.go` | Accepted-event insertion and the search-projection write. |
| `writerqueue.go` | The bounded single-writer queue with backpressure and the shutdown protocol. |
| `lookup.go` | `PathKey`/`SubtreeRange` boundary-aware path keys, `EscapeLikePrefix`, and `NormalizeCommand`. |
| `driver_test.go` | Gate 1: driver, version, schema, corrupt-file handling. |
| `fts_test.go` | Gate 2: FTS5 retrieval and the index-only fallback. |
| `durability_test.go` | Gate 3: pragmas, WAL lifecycle, unclean exit, cross-container persistence. |
| `concurrency_test.go` | Gate 4: one writer, lock-holding cost, readers beside a writer, backpressure. |
| `conflict_test.go` | Gate 5: one-winner revision checks, stale listings, atomic change sets. |
| `content_test.go` | Gate 6: exact bytes including NUL and invalid UTF-8, and dedup. |
| `pathindex_test.go` | Gate 7: subtree boundaries, LIKE equivalence, case, query plans. |
| `backup_test.go` | Gate 8: `VACUUM INTO` backup and restore into a fresh container. |
| `load_test.go` | Gate 9: rough writer-load measurement. |
| `helpers_test.go`, `report_test.go` | Shared fixtures and the receipt/measurement writers. |
| `restart-spike.ps1` | Runs the multi-process gates with container replacements between phases. |
| `phases.sh` | Runs exactly one phase of a gate inside the container. |
| `receipts/` | Evidence. Every claim in the report points at a file here. |

## Where the databases live

Every spike database is created under `/state/spike` inside the container, which is
the named volume on local **ext4** block storage (`/dev/sdd`). Nothing durable is
written to the bind-mounted checkout, so the durability and backup claims describe
the filesystem they will actually run on rather than a host bind mount.
`/state/spike` is deleted by the tests; the volume itself is kept.

## Running it

Everything runs in the project's Podman container, as required by the
container-only rule. From the repository root:

```powershell
& ./scripts/dev.ps1 start
& ./scripts/dev.ps1 exec -Command 'sh','-c','cd /workspace/experiments/storage && go test -count=1 ./...'
& ./scripts/dev.ps1 exec -Command 'sh','-c','cd /workspace/experiments/storage && go test -race -count=1 ./...'
```

`go mod tidy` needs `GOFLAGS` cleared, because the image sets `-mod=readonly`:

```powershell
& ./scripts/dev.ps1 exec -Command 'sh','-c','cd /workspace/experiments/storage && GOFLAGS= go mod tidy'
```

### Gate by gate

Each gate is a set of tests. Run one with `-run`:

| Gate | Question | Tests |
| --- | --- | --- |
| 1 | Which driver, which SQLite, which schema? | `TestDriverAndSQLiteVersion`, `TestStrictTablesAndDeferredForeignKeys`, `TestDriverRejectsCorruptDatabases` |
| 2 | Is FTS5 usable, and what happens without it? | `TestFTS5IsAvailable`, `TestFTS5Retrieval`, `TestFTS5ProjectionTracksItsContent`, `TestIndexOnlySearchFallback` |
| 3 | Does state survive WAL, crash, and container replacement? | `TestPragmasReachEveryPooledConnection`, `TestWALFilesAppearAndDisappear`, `TestSynchronousFullSurvivesForcedExit`, `TestDurableStateAcrossContainerReplacement` |
| 4 | What does one writer cost concurrent readers and writers? | `TestSQLiteAdmitsOnlyOneWriter`, `TestLongWriteTransactionStarvesWriters`, `TestConcurrentReadersBesideOneWriter`, `TestBoundedWriterQueueAppliesBackpressure`, `TestSubmitAfterCloseRefusesWork` |
| 5 | Can concurrent turns be made safe without a server? | `TestGuardedUpdateHasExactlyOneWinner`, `TestStagedChangeSetDetectsAStaleDirectoryRevision`, `TestStagedCreateRejectsAConcurrentCreation`, `TestStagedChangeSetAndEventsCommitAtomically`, `TestConflictReasonsAreDistinguishable`, `TestCommitRejectsMismatchedContent` |
| 6 | Are arbitrary bytes stored exactly, and deduplicated? | `TestContentBytesSurviveExactly`, `TestIdenticalContentIsStoredOnce`, `TestContentHashDistinguishesEveryByte` |
| 7 | Are path lookups boundary-aware and index-backed? | `TestSubtreeLookupRespectsPathBoundaries`, `TestSubtreeLookupAgreesWithTheEscapedLikeVariant`, `TestLookupIsCaseSensitive`, `TestNormalizedCommandLookupGroupsSpacingOnly`, `TestLookupsAreIndexBacked` |
| 8 | Can a world be backed up and restored? | `TestBackupAndRestore` (multi-process, see below) |
| 9 | Is the write path fast enough for a plausible turn rate? | `TestWriterLoadSustainsAPlausibleTurnRate` |

For example:

```powershell
& ./scripts/dev.ps1 exec -Command 'sh','-c','cd /workspace/experiments/storage && go test -count=1 -v -run TestFTS5 ./...'
```

### Gates 3 and 8 need more than one process

Gates 3 and 8 claim something about what outlives a process, so a single `go test`
run cannot prove them. They select a phase through the `SPIKE_PHASE` environment
variable, and `restart-spike.ps1` runs one phase per process with a **container
replacement** in between, so the second process is a different container reading a
volume the first one left behind:

```
write-unclean     (no replacement, expected exit 1)  commit a row, end without closing
read              (replacement,             exit 0)  read it in a new container
backup-write      (no replacement,           exit 0)  build a world, VACUUM INTO a copy
restore           (replacement,             exit 0)  restore the copy, replace the live database
restore-verify    (replacement,             exit 0)  read the replaced database in a third container
```

Run it from the repository root; it manages `scripts/dev.ps1` itself and stops the
container when it finishes, leaving the volumes in place:

```powershell
pwsh -File experiments/storage/restart-spike.ps1
```

The `write-unclean` phase ends the process without closing the database. A Go test
binary can only do that with a non-zero status, so that phase expects exit 1; every
other phase expects 0. Each phase is appended to `receipts/restart-transcript.txt`.
The script deletes its six expected receipts before it starts, so a stale file from
an earlier run cannot satisfy the check at the end.

Running `go test ./...` without `SPIKE_PHASE` runs the in-process form of gates 3 and
8, which still exercises the WAL, recovery, and `VACUUM INTO` behaviour but cannot
prove cross-container persistence. `restart-spike.ps1` is the receipt for that.

### Environment

| Variable | Meaning |
| --- | --- |
| `SPIKE_STATE_DIR` | Directory holding spike databases. Default `/state/spike`. |
| `SPIKE_PHASE` | Selects one phase of a multi-process gate. Unset runs the in-process form. |
| `SPIKE_DURABLE_DB` | Path of the database shared by the two durability phases. |
| `SPIKE_RUN_LABEL` | Label on measurement receipts. Default `plain`; use `race` for a race-detector run so the numbers do not overwrite each other. |

## Receipts

`receipts/` holds the evidence for every gate, and is committed on purpose. Each
file is plain text written by a test at the moment it observed the behaviour, so a
number in the report can be traced to the run that produced it. There are 48 files:

- `go-test-plain.txt`, `go-test-race.txt` — full test output for both runs, 32
  passing tests each, no data races.
- `cgo-disabled.txt` — `CGO_ENABLED=0` build and test, proving the driver is
  pure Go and needs no cgo toolchain.
- `restart-transcript.txt` — the five phases of `restart-spike.ps1`, with the
  container id of each phase and its exit status.
- `durability-before-restart.txt`, `durability-after-restart.txt`,
  `durability-phase-exit.txt` — gate 3 across the container replacement.
- `backup-before-restore.txt`, `backup-after-restore.txt`,
  `backup-verify-after-restore.txt` — gate 8 across two container replacements.
- `gate1-*.txt` … `gate9-*.txt` — per-gate observations. Gate 4 and gate 9 files
  carry a `-plain` or `-race` suffix so the two runs stay separate.
- `measurements-plain.json`, `measurements-race.json` — gate 9 raw latencies.

The mount type of the state volume (`ext4` on `/dev/sdd`) is recorded inside the
gate 1 and gate 3 receipts by reading `/proc/self/mountinfo`, rather than asserted
in prose.

## Limits

Read this before trusting a number. Details are in the report's limitations
section; the short version:

- Gate 9 is a rough measurement on this container's volume, not a capacity limit.
- The race-detector run instruments every memory access; its timings are not
  representative and are kept separate for that reason.
- The unclean exit is simulated with `os.Exit(1)` inside a test process. It
  exercises SQLite's real recovery from the log next to the database, but it is not
  a machine-level power-loss test.
- Nothing here measures multi-session behaviour at scale, and no conclusion about
  another SQLite version or another platform is drawn.