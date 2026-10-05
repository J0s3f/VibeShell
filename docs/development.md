# Development environment

VibeShell development happens in Podman containers. Nothing is installed on the
host: `podman` and `git` are the only host tools, and every compile, test, and
experiment runs inside this checkout's own container.

The foundation rules are in [AGENTS.md](../AGENTS.md); the toolchain decisions
are in [PLAN.md](../PLAN.md) sections 3.2, 12.1, and 12.2.

## Quick start

```powershell
scripts/dev.ps1 image                      # build the pinned toolchain image
scripts/dev.ps1 start -PublishSsh           # create this checkout's container
scripts/dev.ps1 smoke                      # go version, go build ./..., go test ./...
scripts/dev.ps1 test                       # go test -race ./...
scripts/verify-foundation.ps1              # full acceptance test
```

## Pinned toolchain and images

| Pin | Value |
| --- | --- |
| Go | 1.27.1 |
| Build/test base | `docker.io/library/golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195` |
| Runtime base | `docker.io/library/debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251` |
| Container engine | Podman 5.8, rootless, linux/amd64 |

`containers/versions.env` holds these values as `NAME=VALUE` build arguments and
is the record of what is pinned. `containers/Containerfile` repeats them as ARG
defaults so a plain `podman build` works without the helper script.
`scripts/dev.ps1 image` refuses to build when the two disagree, so the duplication
cannot silently rot.

Both base images are pinned by digest, never by a floating tag. The `golang`
image derives from `buildpack-deps` and already contains gcc, the C headers
needed by the race detector, make, git, and CA roots, so the `dev` stage installs
no packages; its build step probes for each prerequisite and fails with the
missing name if a future base pin drops one. That keeps rebuilds reproducible and
offline.

### Updating a pin

1. Change the value in `containers/versions.env`.
2. Change the matching `ARG` default in `containers/Containerfile`.
3. `scripts/dev.ps1 image`, then `scripts/dev.ps1 smoke` and
   `scripts/dev.ps1 test`.
4. Record the new digest in this table and in the agent handoff.

### Image tags

The development image is content addressed:
`localhost/vibeshell-dev:<sha256[0:12] of Containerfile + versions.env>`.
`scripts/dev.ps1 image` also applies the convenience tag
`localhost/vibeshell-dev:latest`. Checkouts that pin different images therefore
never share a toolchain accidentally, while identical pins share one image.

## Container layout

| Path in container | Source | Purpose |
| --- | --- | --- |
| `/workspace` | bind mount of the checkout | source, read-write |
| `/cache/gomod` | named volume | module cache |
| `/cache/gobuild` | named volume | build cache |
| `/state` | named volume | durable simulated-world data |
| `/out` | container layer | generated binaries and scratch output |

Image environment: `CGO_ENABLED=1` (required by the race detector),
`GOPATH=/cache`, `GOMODCACHE=/cache/gomod`, `GOCACHE=/cache/gobuild`,
`GOBIN=/usr/local/bin`, `GOTOOLCHAIN=local` (no silent toolchain download),
`GOFLAGS=-mod=readonly`. Tools installed with `go install` land in the container
layer and are lost when the container is replaced; declare pinned tools in the
`dev` stage instead.

## `scripts/dev.ps1`

| Command | Effect |
| --- | --- |
| `image` | Builds the `dev` target from this checkout's pins. |
| `start [-PublishSsh]` | Creates and starts this checkout's container. `-PublishSsh` publishes container port 2222 on a dynamically assigned `127.0.0.1` host port; without it no port is published. Creates the ignored `.dev/` directory. Safe to repeat; it starts an existing container, and recreates the container if the published port no longer matches the request. |
| `stop` | Stops and removes the container. Named volumes are retained, so durable state survives a stop/start cycle. |
| `exec -Command <string[]>` | Runs the argument vector in the container through `podman exec` and propagates its exit code unchanged. |
| `smoke` | `go version`, `go build ./...`, `go test ./...` in the container. |
| `test` | `go test -race ./...` in the container. |
| `build` | Compiles `out/vibeshell` in the container, stamped with the short HEAD revision (`-dirty` for a modified tree). |
| `status [-AsJson]` | Reports the container name, its state, and the published SSH port. `-AsJson` prints one line, `{"container":"vibeshell-main-dev","ssh":null}`, and nothing else. |
| `clean [-RemoveVolumes]` | Removes the container; `-RemoveVolumes` also deletes this checkout's three named volumes. |

Every command exits non-zero when the underlying work fails, so CI and
`verify-foundation.ps1` can rely on `$LASTEXITCODE`.

## Isolation between agents

Each checkout owns its resources. `scripts/dev.ps1` derives the prefix from its
own location, so no name has to be passed around:

| Checkout | Prefix | Container | Volumes | Published port |
| --- | --- | --- | --- | --- |
| `D:\code\vibeshell` | `vibeshell-main` | `vibeshell-main-dev` | `vibeshell-main-gomod`, `vibeshell-main-gobuild`, `vibeshell-main-state` | dynamic `127.0.0.1` port, none unless `-PublishSsh` |
| `.worktrees/<agent>` | `vibeshell-<agent>` | `vibeshell-<agent>-dev` | `vibeshell-<agent>-*` | its own dynamic port |

The base images and the content-addressed dev image are shared read-only; that
is the only sharing. Two checkouts never share a container name, a volume, a
bind-mounted path, or a host port. Containers carry the labels
`io.vibeshell.role=development` and `io.vibeshell.checkout=<prefix>`, so
`podman ps -a --filter label=io.vibeshell.role=development` lists them all.

Bind mounts carry no SELinux relabel option: the Podman machine mounts Windows
paths through its own translation layer, where `:z` is not meaningful.

## Creating an agent checkout

```powershell
scripts/new-agent.ps1 -Name quickjs-bridge
```

creates `.worktrees/quickjs-bridge` on a new branch `agent/quickjs-bridge` from
the current HEAD, and fails if the path or branch already exists. Run it from the
integration checkout after the tooling the agent needs is committed; a worktree
inherits committed history only. Inside the new checkout,
`scripts/dev.ps1 start -PublishSsh` then gives the agent its own container,
caches, durable state, and port.

Subagents commit their own work on their own branch and hand off; the main agent
merges and validates in the integration checkout.

## Concurrency limits

This environment supports roughly 8–12 active subagent sessions, at most two per free
model, and at most about six containers running `go build`/`go test` at once. Eighteen
concurrent sessions crashed the Podman WSL VM (8 vCPU / 6 GiB; a single active Go build or
race test can use ~1 GiB). See `AGENTS.md` and `docs/ops/model-rate-limits.md`.

## Acceptance test

`scripts/verify-foundation.ps1` is the end-to-end gate for this foundation. It
builds the image, starts the integration container, runs the Go smoke test, then
creates two throwaway agent worktrees and proves the isolation model:

- separate worktrees, containers, volumes, and dynamically assigned SSH ports;
- separate writable storage, by writing probe files in one agent container and
  asserting they are absent in the other (`/state`, `/cache`, `/out`,
  `/workspace/.dev`, `/tmp`);
- retained state across `stop` followed by `start`;
- exact propagation of a non-zero exit code (`sh -c 'exit 23'` must surface 23);
- `status -AsJson` reporting distinct container names and distinct SSH ports.

Success prints:

```
PASS: compiler/race detector, separate worktrees/containers/storage/ports, restart, and exit-code propagation.
```

The run takes about a minute once the toolchain image is cached. It leaves the
smoke worktrees and volumes behind on purpose, so the evidence can be inspected
before cleanup.

## Receipts

Command output for a run belongs in `.dev/receipts/` inside the checkout.
`.dev/` and `out/` are gitignored, and both directories are created on demand by
`scripts/dev.ps1`. Receipts must not contain credentials, API keys, or provider
diagnostics.

## Cleaning up

`stop` and `clean` keep named volumes, which is what makes durable state survive
a stop/start cycle. After preserving the receipts and only then:

```powershell
# Inside each smoke worktree, before removing it:
scripts/dev.ps1 clean -RemoveVolumes

# In the integration checkout:
git worktree remove .worktrees/<agent>
git branch -D agent/<agent>
```

The integration checkout keeps its own container and volumes between sessions;
remove them with `scripts/dev.ps1 clean -RemoveVolumes` when they are no longer
needed. To list leftovers first:

```powershell
podman ps -a --filter label=io.vibeshell.role=development
podman volume ls --filter label=io.vibeshell.role=development
```

## Not part of this foundation yet

- `scripts/dev.ps1 build` compiles the binary into `out/`. The `runtime` stage of
  `containers/Containerfile` is complete and builds, but no command wires it into
  a deployable image yet; that belongs to the deployment task.
- The QuickJS engine build stage from PLAN.md section 12.1 is not implemented.
- Release builds target linux/amd64 only. arm64 has not been built or tested.
