# SSH/world/app acceptance suite

This is the acceptance suite for task D04 (PLAN 15.5): it builds the real
service, runs it in-container, and drives it with the real OpenSSH client.

## Running it

Inside the development container, from the repository root:

```sh
sh tests/e2e/ssh-acceptance.sh
```

The script exits `0` only when every check passed, so it works as a milestone
gate. Options:

| Option | Effect |
| --- | --- |
| `-work DIR` | Use `DIR` for the binaries, configurations, databases, and logs |
| `-keep` | Keep the work directory after the run for inspection |

`go test ./tests/e2e/...` runs exactly the same script, so the ordinary test
loop and the standalone gate execute identical checks.

Both SSH listeners bind `127.0.0.1` inside the container's own network
namespace. No host port is published, so nothing here is reachable from the
host.

## What is covered

| Area | Check |
| --- | --- |
| Public `none` auth | Connect with no password; MOTD and `user@host:dir` prompt |
| MOTD truthfulness | The generated MOTD reports the generator as unreachable instead of pretending |
| Identity commands | `pwd`, `whoami`, `uname`, `uname -a` |
| Unknown command | Reports generation unavailable, names the command, never fabricates a shell result |
| PTY | `pty-req` accepted; the session still serves a prompt |
| Window-change | Accepted after `pty-req`; the session survives it |
| Refused `exec` | Client sees the refusal and a non-zero exit |
| Refused subsystem | Client sees the refusal and a non-zero exit |
| Refused forwarding | `-R` forwarding refused; `ssh -W` `direct-tcpip` channel refused; no byte reaches the shell |
| Ctrl-C | Echoes `^C`, discards the pending line, session keeps working |
| Second session | Two sessions of the public name `alice` map to one durable identity |
| Restart durability | The durable journal survives a restart and grows on reconnect |
| Session close | Every session records a durable `session.end` |
| Secure auth | Correct password accepted; wrong password, unknown user, and disabled user refused |
| Namespace separation | Secure mode refuses the `none` method; secure and public identities do not collide |
| Internal status | `vibeshell status` reports each subsystem without opening a second port |

## What is not covered, and why

No generation path is merged yet. The composition root wires a deterministic
local fallback engine (`cmd/vibeshell/engine.go`) that answers `pwd`, `whoami`,
`uname`, `echo`, and `exit`/`logout` from configured identity facts and reports
**every other command** as truthfully unavailable. Consequences for this suite:

- No invented program, invented path, generated app, app version extension, or
  novel interactive app can be exercised. Those assertions have no path to run
  through yet.
- No world mutation is reachable from the shell, because the wired tool executor
  rejects every batch. So "a second session sees the same committed bytes" is
  not testable as written; what *is* testable, and is tested, is that every
  session of one public name maps to one durable identity and that the durable
  session/turn journal survives a restart and grows on reconnect.
- The window size from `window-change` reaches the transport but the SSH handler
  does not consume `Session.ResizeNotify`, so no application state depends on it
  yet. The check proves the request is accepted and the session survives.
- Cross-session concurrent world edits, scope-policy switching, and top/vi/less
  style full-screen interaction all need C03-C07 services that are not wired
  into `cmd/vibeshell`.

## Layout

| Path | Role |
| --- | --- |
| `ssh-acceptance.sh` | The suite: builds, starts the service, drives the client, prints PASS/FAIL |
| `acceptance_test.go` | Runs the script so `go test ./...` covers the same checks |
| `fixtures/main.go` | `prepare` writes configurations and the password file, `events` reads the durable journal, `resize` sends the scripted window-change |
| `receipts/` | Saved output of real runs |

The fixture tool reads its passwords from the environment; the script generates
a fresh random password per run. No credential material is stored in the
repository, in a log, or in a receipt.

`window-change` uses a scripted `golang.org/x/crypto/ssh` client because the
OpenSSH command line only emits that request in response to `SIGWINCH`, which a
scripted run cannot raise deterministically. Everything else in the suite is
the real `ssh` binary.