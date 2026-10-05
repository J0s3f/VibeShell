# Plan: simulated-world reads for the shell and generated apps

Status: stages 1 and 2 complete (2026-10-04); AI materialization of a missing
path (stage 3) remains.

Shipped: the shell's filesystem commands (`ls`, `cat`, `cd`, `pwd`) read and
write the durable per-user world (`2c6681f`, `a28b85c`); the baseline skeleton
is seeded from `presentation.DefaultBaselineSeed` plus the user's home tree
through the sqlite `EnsureTree` seeder; generated apps fulfil `world_reads`
through `simulation.AppRunner` in a bounded `world_change` round-trip
(`b3a8567`, `8b839c4`). A missing path currently returns a truthful
"no such file"; generating plausible content for it is stage 3.

## Goal

Make the simulated filesystem real. Today `ls` lists a fabricated directory and
`cat notes.txt` cannot read anything because generated apps have no world access
and the world store is empty. After this work:

- The shell's filesystem commands (`ls`, `cat`, `cd`, `pwd`) read and write the
  durable, per-user simulated world.
- A generated app can request a world read and receive the result within its run.
- A path that does not exist yet is materialized once with plausible
  AI-generated content and then persists (the "invented files get an
  AI-generated presentation" rule).

## Current state (facts)

- `internal/simulation` already implements the agent-facing world tool layer
  (`world.lookup`, `world.list`, `content.read`, `world.stage`,
  `world.materialize`) over `ports.WorldStore` / `ports.ContentStore`, but it is
  **not wired**: the coordinator's `Tools` is `rejectTools{}`.
- The turn engine (`cmd/vibeshell/generation.go`) generates an app for an unknown
  command and runs it in the sandbox; it **ignores** the app's `world_reads`.
- The world store (`internal/adapters/sqlite`) is wired as `World`/`Content`, but
  the world is empty: `EnsureNamespace` creates a namespace and its root node,
  and nothing seeds a home tree.
- `domain.WorldReadRequest{RequestID, Scope, Path, NodeID, MaxBytes}` is the
  app-facing read request. `domain.AppEvent{EventType, Payload, Timestamp}` is the
  event envelope; the sandbox promotes payload fields to the top level.
- `TurnCandidate.Changes domain.ChangeSet` is the only path by which a turn's
  world mutation is committed, atomically, by the coordinator.

## Design

### 1. Namespaces and baseline seeding

- One user namespace per principal, ensured on first use:
  `db.EnsureNamespace(ctx, domain.NamespaceForUser(user, "user:"+user.Value()))`.
- A baseline home tree seeded idempotently when the home node is absent:
  `/home/<user>` with the conventional empty directories (`Desktop`, `Documents`,
  `Downloads`, `Music`, `Pictures`, `Public`, `Templates`, `Videos`) and a couple
  of deterministic files (for example `notes.txt`). Seeding is an adapter
  operation, not a turn mutation.

### 2. World-backed shell commands

The turn engine answers the filesystem primitives directly against the world,
resolved in the session's user namespace:

- `pwd` -> the session cwd (already deterministic).
- `cd <dir>` -> resolve the directory; on success return a `SessionPatch{CWD}`.
- `ls [path]` -> list the directory entries (name + kind + size), sorted.
- `cat <file>` -> read the file content (bounded).

Unknown commands still go to generation. This is the shell's own filesystem
layer (as in a real shell), not a hardcoded catalogue of invented programs.

### 3. AI materialization of a missing path

When `cat`/`ls` names a path that does not exist under the home tree, the engine
generates plausible content through the model, stages the creation in the turn's
`ChangeSet`, returns the generated content to the user this turn, and the
coordinator commits it so it persists. A directory is materialized as a
directory; a file with generated text.

### 4. App world-read round-trip

A generated app may return `world_reads` requests. The runner resolves each
against the world (file content or directory entries), then re-invokes the app
with a `world_change` event carrying the results, up to a bounded number of
rounds. The generation prompt documents both shapes.

Request (returned by the app):
```json
{"world_reads": [{"request_id": "r1", "scope": "user", "path": "/home/alice/notes.txt"}]}
```
Response event (`event_type: "world_change"`):
```json
{"results": [{"request_id": "r1", "path": "/home/alice/notes.txt", "found": true,
              "kind": "file", "content": "...", "entries": []}]}
```
A directory read returns `kind: "dir"` and `entries: [{"name": "...", "kind": "file", "size": 12}]`.

## Contracts (frozen before parallel work)

### `internal/adapters/sqlite` — baseline seeder

```go
// SeedNode is one node to create in a namespace tree.
type SeedNode struct {
    Path    domain.ValidPath
    Kind    domain.NodeKind // NodeKindDir or NodeKindFile
    Content []byte          // files only
    Mode    uint32          // optional; default 0o755 dir, 0o644 file
}

// EnsureTree idempotently creates every missing node in nodes within ns, parents
// before children, and returns how many it created. Existing nodes are left
// unchanged.
func (db *DB) EnsureTree(ctx context.Context, ns domain.NamespaceID, nodes []SeedNode) (int, error)
```

### `internal/simulation` — world reader and app runner

```go
// WorldReader resolves app world-read requests against the world store.
type WorldReader struct {
    World        ports.WorldStore
    Content      ports.ContentStore
    MaxReadBytes int64 // default MaxContentReadBytes
}

type WorldEntry struct {
    Name string          `json:"name"`
    Kind domain.NodeKind `json:"kind"`
    Size int64           `json:"size,omitempty"`
}

type WorldReadOutcome struct {
    RequestID string           `json:"request_id"`
    Path      domain.ValidPath `json:"path"`
    Found     bool             `json:"found"`
    Kind      domain.NodeKind  `json:"kind,omitempty"`
    Content   []byte           `json:"content,omitempty"`
    Entries   []WorldEntry     `json:"entries,omitempty"`
    Truncated bool             `json:"truncated,omitempty"`
}

// Resolve fulfills one request within a namespace. A missing path is not an
// error: it returns Found=false.
func (r *WorldReader) Resolve(ctx context.Context, ns domain.NamespaceID, req domain.WorldReadRequest) (WorldReadOutcome, error)

type AppRunRequest struct {
    Artifact     domain.AppArtifact
    State        domain.AppState
    Command      string
    Args         []string
    CWD          domain.ValidPath
    Namespace    domain.NamespaceID
    NowUnixMilli int64
    Limits       ports.SandboxLimits
}

// AppRunner runs an app and fulfils its world reads in bounded rounds.
type AppRunner struct {
    Sandbox   ports.AppSandbox
    Reader    *WorldReader
    MaxRounds int // default DefaultWorldReadRounds
}

func (r *AppRunner) Run(ctx context.Context, req AppRunRequest) (domain.AppResult, error)
```

`Run` builds the input event `{command, args, cwd}`, runs the sandbox, and while
the result carries `WorldReads` (up to `MaxRounds`) resolves them and re-runs
with a `world_change` event whose payload is `{"results": [...]}`. A final result
that still requests reads is returned as-is; the caller decides.

## Task split

| Task | Owner scope | Deliverable |
| --- | --- | --- |
| A (subagent) | `internal/adapters/sqlite/**` | `EnsureTree` + tests (idempotent, parents-first, files+dirs). |
| B (subagent) | `internal/simulation/**` | `WorldReader` + `AppRunner` + tests over fakes. |
| C (main) | `cmd/vibeshell/**` | Namespace resolution, home seeding, world-backed `ls`/`cat`/`cd`/`pwd`, AI materialization, AppRunner wiring, prompt doc. |

A and B are independent and compile against only `internal/domain` and
`internal/ports`. C depends on A and B and is owned by the main agent.

## Acceptance

- `go test -race` for the new packages; full `go test ./...` green.
- Live: over SSH, `ls` lists the seeded home; `cat notes.txt` prints its content;
  `cat <missing>` materializes and persists a plausible file; a generated app can
  read a file it requests.
