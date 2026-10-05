# Container-Only Development Workflow

This document describes the container-only development workflow as actually shipped in VibeShell. Every capability listed here is verified against merged code in the repository.

## Image Building and Pins

The development image is content-addressed by the hash of `containers/Containerfile` and `containers/versions.env`. The `scripts/dev.ps1` toolchain uses these pins to ensure reproducible builds across worktrees.

- `containers/versions.env` pins `GO_VERSION=1.27.1`, `GO_IMAGE=docker.io/library/golang:1.27.1-bookworm@sha256:...`, and `RUNTIME_IMAGE=docker.io/library/debian:bookworm-slim@sha256:...`.
- `containers/Containerfile` uses `ARG` defaults that mirror `versions.env`; `scripts/dev.ps1 image` fails when the two disagree.
- The dev image tag is computed as `localhost/vibeshell-dev:<sha256-hex-12>` from the combined file contents.
- Build args are passed as `--build-arg NAME=VALUE`; CGO is enabled in the dev stage and disabled in the build stage.

**Source**: `scripts/dev.ps1` (lines 124-149, 302-331), `containers/versions.env`, `containers/Containerfile`

## Per-Agent Worktree Isolation

Each implementation subagent operates in its own Git worktree under `.worktrees/<name>`. The `scripts/new-agent.ps1` tool creates a new branch and worktree:

- Branch: `agent/<name>`
- Worktree path: `.worktrees/<name>`
- Resources (container, module cache, build cache, durable state volume, SSH port) are prefixed with the agent name and never overlap between checkouts.
- Each worktree has its own container named `vibeshell-<agent-slug>-dev`.

**Source**: `scripts/new-agent.ps1`, `scripts/dev.ps1` (lines 76-96, 333-402), `scripts/verify-foundation.ps1`

## Volume and Port Isolation

Each container mounts three named volumes for cache isolation:

- `$CheckoutPrefix-gomod` → `/cache/gomod`
- `$CheckoutPrefix-gobuild` → `/cache/gobuild`
- `$CheckoutPrefix-state` → `/state`

The SSH port is published on `127.0.0.1::2222` when `-PublishSsh` is used; each agent gets a dynamically assigned host port. The container also mounts the checkout as `/workspace` for source access.

**Source**: `scripts/dev.ps1` (lines 197-217, 360-386, 520-525)

## Acceptance Test

The foundation acceptance test is `scripts/verify-foundation.ps1`, which:

1. Builds the dev image (`scripts/dev.ps1 image`)
2. Starts the integration container (`scripts/dev.ps1 start`)
3. Runs Go toolchain smoke test (`scripts/dev.ps1 smoke`)
4. Creates two agent worktrees via `scripts/new-agent.ps1 -Name <name>`
5. Verifies separate writable storage: each agent writes isolated probes to `/state/probe`, `/cache/probe`, `/out/probe`, `/workspace/.dev/probe`, `/tmp/probe`; the other agent's container must not see these files
6. Verifies state retention: after stopping and restarting the first agent, its probe file is still present
7. Verifies separate SSH ports: the two agents have different dynamically assigned ports
8. Reports `PASS: compiler/race detector, separate worktrees/containers/storage/ports, restart, and exit-code propagation`

**Source**: `scripts/verify-foundation.ps1` (entire file)

---
*All claims traceable to merged files/receipts in D:\code\vibeshell.*