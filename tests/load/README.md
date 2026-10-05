# VibeShell capacity and soak harness

Task D06 of PLAN 15.5. It measures the shipped service under the PLAN 13
planning workload for roughly 100 concurrent users and writes machine-readable
receipts under `receipts/`.

## What it exercises

Every session scenario drives the real layers: the SSH transport adapter, the
session coordinator, the terminal renderer, and the SQLite world and event
stores. The harness's turn engines are deterministic doubles standing in for
simulation generation, because no production generation engine is wired into
the composition root yet. Nothing here is product behaviour.

| Scenario | What it measures |
| --- | --- |
| `idle-sessions-and-churn` | Concurrent idle sessions with SSH keepalives, plus connect/disconnect churn; achieved connections, RSS, CPU, database size, and the time for the coordinator to drain to zero sessions |
| `interactive-input-mix` | Command submission, editor keystrokes, pager navigation, top-like refreshes, large output, large bracketed paste, and real window changes; local event latency per input class |
| `admitted-model-requests` | Concurrent turns through the shipped routing policy into a bounded admission gate and a deterministic delayed provider double |
| `world-contention` | Concurrent first-time materialisation of one private and one shared path, shared reads during writes, and conflicting saves from one stale revision |
| `sandbox-and-state-soak` | Repeated sandbox instance open/close with app state evicted and restored, checking resident memory, file descriptors, and goroutines for growth |

## Two facts the receipts never blur

- **Local event latency is measured at the client boundary** (final input byte to
  the prompt that follows the turn) and reported per input class. Provider time
  is recorded separately and is an injected delay: no live call is ever made and
  no credential is used. The mix scenario's provider column is exactly zero.
- **Every receipt names its host**: Go version, CPU count, `GOMAXPROCS`, total
  memory, cgroup bounds, and the harness revision. A latency number without its
  host is not a benchmark.

## Running it

Nothing runs by default: `go test ./...` skips the harness, so the normal suite
stays fast and hermetic.

Inside the development container:

```sh
# full planning workload (the default sizing)
go run ./tests/load/cmd/loadharness

# a smaller host, or a faster iteration
go run ./tests/load/cmd/loadharness -sessions 40 -hold 15s -rounds 2

# one scenario at a time
go run ./tests/load/cmd/loadharness -skip idle-sessions-and-churn,sandbox-and-state-soak
```

From the host, through the checkout's container:

```powershell
$revision = git rev-parse --short=12 HEAD
& .\scripts\dev.ps1 exec -Command @('sh','-c',"go run ./tests/load/cmd/loadharness -revision $revision")
```

`-revision` exists because a Linux container cannot read a Windows worktree's
git metadata; without it the receipt records `unknown` and says why.

Through `go test`:

```sh
VIBESHELL_LOAD=1 go test ./tests/load -run TestCapacityHarness -v -timeout 60m
```

Checking the harness itself under the race detector, at a reduced scale:

```sh
go run -race ./tests/load/cmd/loadharness -receipts /tmp/racecheck \
  -sessions 12 -hold 6s -rounds 1 -soak-rounds 3
```

## Exit status and results

The command exits non-zero when an executed scenario missed one of its bounds,
so a capacity regression is visible to a pipeline instead of being read as a
pass. Bounds are deliberately honest rather than aspirational:

- An idle scenario that cannot establish the requested number of sessions fails
  and reports the ceiling it did reach. The receipt is the result; the target is
  not a promise.
- The mix scenario records how many inputs exceeded the PLAN 13 goal of 50 ms
  p95 local handling without failing on it, so a host that cannot meet the goal
  still produces comparable numbers.
- The soak fails on per-round resident-memory growth above 8 MiB or on file
  descriptor growth above 16, judged between the first and last round so a
  warm-up cost is not mistaken for a leak.

## Known gaps the receipts carry as findings

- **Admission control is harness-owned.** `inference.global_concurrency`,
  `inference.max_account_concurrency`, and `inference.wait_queue_depth` are
  validated by the configuration adapter but enforced by no runtime package, so
  the bound measured in `admitted-model-requests` is the harness gate's, not the
  service's. The receipt states this rather than implying the service throttles.
- **Concurrent first-time creation of a deep path** needs its parent chain to
  exist first: the world adapter fills a missing parent chain inside the writing
  transaction, which two concurrent transactions cannot both observe. The
  scenario therefore pre-creates the chain and races on the leaf, which is the
  first-materialisation race it is meant to measure.

## Layout

- `doc.go` — package contract
- `harness.go` — options, scenario orchestration, verdict
- `receipt.go`, `latency.go`, `environment.go` — receipt schema, percentiles, host and process sampling
- `service.go` — the assembled stack and the SSH byte-decoding handler
- `sshclient.go` — the client that drives the transport, including prompt-counted latency measurement
- `provider.go` — the delayed provider double, the admission gate, and the health store
- `engines.go`, `identity.go`, `ids.go` — the deterministic turn engines and identities
- `scenario_*.go` — one file per scenario
- `cmd/loadharness` — the command that writes receipts
- `receipts/` — receipts produced by runs
