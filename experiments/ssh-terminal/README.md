# SSH and terminal qualification spike (A03)

An isolated spike, not application code. It answers three questions with real
execution rather than with a reading of a library's documentation:

1. Can `golang.org/x/crypto/ssh` carry the SSH channel contract in PLAN.md
   section 4.3, including the refusals, and without ever starting a process?
2. What does a real OpenSSH 9.2 client actually send on a pty session, and what
   does the server have to accept for a resize and a Ctrl-C to work?
3. Which terminal primitives work as pure byte-stream code, given that
   "terminal" here means the far end of an SSH channel?

The findings, the numbers, and the remaining unknowns are in
`docs/research/2026-10-03-ssh-terminal-spike.md`.

## Layout

| Path | Purpose |
| --- | --- |
| `go.mod`, `go.sum` | This directory is its own Go module. Nothing here is a dependency of the repository's root module. |
| `internal/sshserver` | The SSH transport: authentication, session channels, request handling, bounded output, cancellation. |
| `internal/termdecode` | A byte-stream terminal input decoder with bounded buffers: UTF-8, control keys, fragmented escape sequences, bracketed paste, in-band resize. |
| `internal/termrender` | Output-side primitives built on `github.com/charmbracelet/x/ansi`: frame validation and cell measurement. |
| `cmd/spike-server` | The spike server as a standalone service, so a real `ssh` client can drive it. |
| `cmd/terminal-primitives` | Prints the evidence for the terminal-primitive decision. |
| `cmd/transport-probe` | Measures round-trip latency, session start, cancellation, and back pressure. |
| `ptyclient.py`, `forwardcheck.py`, `askpass.py`, `agentsocket.py` | Helpers that make a real `ssh` client behave the way a person would drive it. |
| `Containerfile` | Builds the spike binaries on the pinned Go base image and installs `openssh-client`. |
| `run-spike.ps1` | Builds that image, runs every check, writes the receipts, and exits non-zero on any failure. |
| `receipts/` | Captured output of the runs. |

## Running it

From the checkout root, inside this checkout's container:

```sh
scripts/dev.ps1 exec -Command go -C experiments/ssh-terminal test -race ./...
```

The real-client run needs Podman on the host and nothing else:

```powershell
pwsh -File experiments/ssh-terminal/run-spike.ps1
```

It writes `receipts/real-openssh.txt`, `receipts/go-test-race.txt`, and
`receipts/probes.txt`, and returns a non-zero exit code if any check fails. The
container and image are named from the checkout path, so a run never collides
with the development container or with another agent's spike.

## What is deliberately not here

There is no shell, no interpreter, no world model, and no simulated application.
The spike session served by `cmd/spike-server` is an observation session: it
reports what the transport delivered, one line per fact, and acts on exactly two
words, `exit` and `exit <n>`, which only choose the session's own SSH exit
status. `TestNoProcessSpawningPrimitives` fails the test run if any file in the
module gains an import or call that could start an operating-system process.
