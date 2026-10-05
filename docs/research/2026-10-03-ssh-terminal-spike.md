# SSH and terminal qualification spike

Task A03, run on 2026-10-03. This records what was executed, what was observed,
and what remains open for the SSH transport and the terminal layer. It is a
qualification spike: the code lives in `experiments/ssh-terminal/` as its own Go
module and is not application code.

Every number below was measured in a container on this host. Nothing was
installed on the host; Podman and Git were the only host tools used.

## Summary of conclusions

| Question | Answer |
| --- | --- |
| Can `golang.org/x/crypto/ssh` carry the PLAN 4.3 channel contract? | Yes, with one contract amendment: a client that requests a pty without usable dimensions must be accepted with the documented default size, and refusing an optional capability (agent or X11 forwarding) must not close the session channel. |
| Does a real OpenSSH 9.2 client work against it? | Yes, for public `none` auth, password auth, pty sessions, resize, Ctrl-C, EOF, exit status, and every refusal the contract requires. |
| Which terminal primitive? | `github.com/charmbracelet/x/ansi` as a pure byte-stream library, plus a small owned input decoder. Not `ultraviolet`'s `Terminal`. |
| No OS process is spawned | Enforced mechanically: no file in the module imports `os/exec`, `syscall`, `golang.org/x/sys`, or `runtime/cgo`, and no file references `os.StartProcess`, `os.FindProcess`, `os.Process`, `exec.Command`, `syscall.Exec`, `syscall.ForkExec`, or `syscall.StartProcess`. |

## Pinned versions

| Dependency | Version | Why this version |
| --- | --- | --- |
| `golang.org/x/crypto` | v0.57.0 | Current release at the time of the spike; requires Go 1.26, and the pinned image has 1.27.1. |
| `github.com/charmbracelet/x/ansi` | v0.11.8 | Current release; the chosen output-side primitive. |
| `github.com/charmbracelet/ultraviolet` | v0.0.0-20261001125412-878653296cfd | Pseudo-version: the project publishes no semver tag. Only used by the evaluation probe. |
| Go toolchain | 1.27.1, base image digest from `containers/versions.env` | Same pin as the development container. |
| OpenSSH client | 9.2p1 Debian-2+deb12u10 (OpenSSL 3.0.20) | Installed by the spike `Containerfile`; see the correction below. |

## Commands run

All commands ran from the checkout root
`D:\code\vibeshell\.worktrees\a03-ssh-terminal`.

```powershell
# container
pwsh -File scripts/dev.ps1 start

# formatting, vetting, tests, and the input decoder's fuzzer
pwsh -File scripts/dev.ps1 exec -Command go -C experiments/ssh-terminal vet ./...
pwsh -File scripts/dev.ps1 exec -Command go -C experiments/ssh-terminal test -race -count=1 -timeout 300s ./...
pwsh -File scripts/dev.ps1 exec -Command go -C experiments/ssh-terminal test -fuzz FuzzDecoderStaysInsideItsBounds -fuzztime 30s ./internal/termdecode/

# the real-OpenSSH integration run
pwsh -File experiments/ssh-terminal/run-spike.ps1
```

`run-spike.ps1` builds `experiments/ssh-terminal/Containerfile` from the pinned
Go base image, starts one container, and inside it runs `go test -race ./...`,
drives the spike server with the real `ssh` client, then runs the two probe
programs. It exits non-zero if any check fails.

### Correction to the task card

The task card said the development image has no `ssh` client and that the spike
therefore needs its own image. In fact the pinned `golang` image already ships
`OpenSSH_9.2p1`, `python3`, and `script`. The spike `Containerfile` still exists
and still installs `openssh-client` explicitly, so the image states its own
client version at build time and stays correct if a future base pin drops the
package. The alternative of two containers on a Podman network was not needed:
the server and the client run in one container and talk over loopback.

## Receipts

| File | Contents |
| --- | --- |
| `experiments/ssh-terminal/receipts/real-openssh.txt` | Every `ssh` command, its output, its exit code, every check verdict, and the server's own structured log for both authentication modes. |
| `experiments/ssh-terminal/receipts/go-test-race.txt` | `go test -race -count=1 -timeout 300s ./...` as run inside the spike image. |
| `experiments/ssh-terminal/receipts/probes.txt` | The terminal-primitive evaluation and the transport measurements. |

## The SSH channel contract, answered precisely

### Refusal behaviour

| Client request | Behaviour | Evidence |
| --- | --- | --- |
| `exec` (`ssh host ls -la`) | Refused. The reason is written to the session's stderr, the request is answered with failure, and the channel is closed. The client prints `exec request failed on channel 0` and exits 255. | receipt: `exec is refused`, and the server log line `type=exec reason="exec is not supported: vibeshell serves interactive shell sessions only"` |
| `subsystem` (`ssh -s host sftp`) | Refused the same way. `ssh -s` prints `subsystem request failed on channel 0` and exits 255; it does not print the server's stderr, so the reason is asserted from the server log. | receipt: `a requested subsystem (ssh -s) is refused` and the matching server log line |
| `direct-tcpip` | The channel open is rejected with `ssh.Prohibited` and a reason naming the channel type. `ssh -L` opens its local listener happily, so the refusal only surfaces when the forwarded port is actually used: the client's local connection is then closed with no data. | receipt: `a used local forward is refused at the direct-tcpip channel` plus `type=direct-tcpip reason="vibeshell opens interactive session channels only; direct-tcpip is not supported"` |
| `forwarded-tcpip`, `x11`, `auth-agent@openssh.com` | Rejected at channel open, same reason. | Go tests `TestChannelTypesThatAreRefused` |
| `x11-req`, `auth-agent-req@openssh.com` (channel requests) | Refused **without** closing the channel. This is a change from the first implementation, and it matters: a real `ssh -A` client carries on without the capability, so closing the channel would end a session that a user would expect to keep. Verified with a real client: the agent refusal is logged, then the client's own `exec` request is refused in turn. | receipt: `agent forwarding is refused but the session continues` |
| `tcpip-forward`, `cancel-tcpip-forward` (connection requests) | Refused with a reason. | Go test `TestConnectionLevelRequestsAreRefused` |
| Any unknown request type | Refused with `channel request "<type>" is not supported`. | Go test `TestChannelRequestsThatAreRefused` |
| `pubkey`, `publickey certificate`, `keyboard-interactive` | Never advertised in password mode, so every such client is refused. | Go tests `TestPasswordModeOffersNoOtherMethod`, and the receipt's password-mode refusals |

Two facts worth recording for the service:

- **A refused request does not always reach the user's terminal.** `exec`
  refusals do (the client relays the session's stderr). `subsystem` refusals do
  not, and neither do channel-open refusals, which travel back in the channel
  open failure. The service should therefore log refusals, not rely on the
  client surfacing them.
- **`x11-req` and `auth-agent-req` are optional capabilities.** Refusing them is
  correct; closing the session channel is not.

### Public-key authentication

Not registered in either mode. `ServerConfig` never gets a `PublicKeyCallback`,
so `golang.org/x/crypto/ssh` answers `SSH_MSG_USERAUTH_REQUEST` for `publickey`
with a failure that names the methods that are available. Go test
`TestPasswordModeOffersNoOtherMethod` asserts that a client offering a public
key or a keyboard-interactive challenge cannot connect. Host authentication is
unaffected: the service still presents a persistent host key, and
`LoadOrCreateHostKey` keeps the same ed25519 key across restarts (test
`TestLoadOrCreateHostKeyIsStableAcrossCalls`).

### Authentication modes

**Public mode** sets `NoClientAuth`, which makes the SSH `none` method succeed,
and leaves `PasswordCallback` nil, so the password method stays refused. The
client is therefore never prompted. Verified two ways with the real client:
`ssh -o BatchMode=yes` (which disables all prompting and fails if a prompt is
needed) succeeds, and the receipt shows no password prompt in any output.

**Password mode** registers only `PasswordCallback`. Refusals are uniform for an
unknown user, a wrong password, and a disabled account: all three produce
`authentication refused`, and the client sees only `Permission denied
(password).` An oversized password is refused before any comparison. The
`PasswordFile` type here is a spike stand-in; Argon2id hashing and the versioned
password file belong to the authentication task.

### Resize behaviour

`window-change` is answered without a reply, as RFC 4254 requires, and updates
the session's terminal size. The size is bounded to 1..4096 in each direction.

One correction to the contract, found with the real client: **OpenSSH reports a
pty dimension of zero when it has no size to send.** `ssh -tt host` with stdin
from a pipe sends `cols=0 rows=0`. Refusing that would break ordinary non
interactive use of `ssh -tt`, which is exactly how a scripted client drives a
server. A zero therefore means "unknown" and falls back to the documented default
of 80x24; any other out-of-range value is a malformed request and is refused.
Tests: `TestPTYRequestWithoutUsableDimensionsUsesTheDocumentedDefault`,
`TestPTYRequestOutOfRangeDimensionsAreRefused`.

Resize delivery from a real terminal needed one more finding. A local resize only
becomes an SSH `window-change` if the client's `SIGWINCH` handler runs, which
requires the pty to be the client's controlling terminal. A first driver that
just opened a pty and piped it to `ssh` never sent a resize at all, silently. The
driver now calls `setsid` plus `TIOCSCTTY` before exec, exactly as a terminal
emulator does, and the real client then sends `window-change`; the receipt shows
`resize 132x40` reaching the server and the server logging
`msg="window change" terminal="xterm-256color 132x40"`.

Sizes coalesce: while a size is undelivered, a newer size replaces it, so a
client that resizes faster than the session consumes events cannot grow the
event queue. The pending size lives in its own slot rather than being searched
for in the event channel, because a resize must never displace a queued key
press. Tests: `TestResizeCoalescesToTheNewestSize`,
`TestResizeKeepsQueuedKeysInOrder`, `TestResizeDeliveredImmediatelyWhenTheQueueHasRoom`.

### Ctrl-C delivery

**Ctrl-C arrives in band as the byte `0x03`, not as an SSH signal request.** With
a pty session, OpenSSH puts the local terminal in raw mode and forwards the byte;
it does not translate it into a `signal` request. The real-client receipt shows
the server reporting `key ctrl+c` and `interrupt ctrl-c` after the driver wrote
`03` to the pty master.

`signal` requests are supported separately for `INT`, `TERM`, `HUP`, and `QUIT`,
and any other signal name is refused. That path is exercised by Go tests only
(`TestSignalRequestsAreDeliveredAndBounded`), because `ssh` sends a `signal`
request only when it has no pty, and the spike's `-T` mode takes the plain
stdin path instead.

### EOF and exit status

EOF is the client closing the session's write side, not a disconnect: the
session reports it separately through `InputClosed`, flushes any bytes the input
produced before the end, and can still exit with a chosen status. With a real
client, `printf 'ls\r' | ssh -tt` shows `eof err=<nil>` and exit 0, and
`printf 'exit 3\r' | ssh -tt -o BatchMode=yes` shows exit status 3 reaching the
client.

Exit status is sent once. A second call is an error rather than a second request
(`TestExitStatusIsSentOnceAndReportedToTheClient`).

### Latency and cancellation

Measured with `cmd/transport-probe` against an in-process server over loopback,
with the Go SSH client, on this host. These are loopback figures for a spike,
not service targets.

| Measurement | n | min | p50 | p95 | p99 | max |
| --- | --- | --- | --- | --- | --- | --- |
| Keypress round trip (write, decode, echo) | 200 | 19.9 µs | 146 µs | 205 µs | 229 µs | 245 µs |
| Session start (channel open, pty-req, shell) | 50 | 230 µs | 426 µs | 690 µs | 838 µs | 1.22 ms |
| Disconnect to cancelled session context | 20 | 40.5 µs | 92.9 µs | 135 µs | 135 µs | 136 µs |

Cancellation is therefore bounded by the transport's own read latency rather
than by any timer in the session: the worst observed case was 136 µs, and the
`TestDisconnectCancelsTheSessionContextPromptly` test asserts a 2 s ceiling so a
regression fails loudly rather than slowly.

Back pressure, with a 64 KiB output queue: a client that stops reading caused
8 388 608 bytes of writes to complete in 914 µs with 8 224 768 bytes dropped,
and a second connection's session ran normally throughout
(`TestSlowClientDoesNotBlockAnotherSession`). The queue discards the **oldest**
bytes, so what a client finally receives is the tail of recent output, which for
a terminal is the current screen rather than a fragment of an earlier frame.

The one real concurrency bug this found is worth recording, because it was
invisible to any test that did not use a real client. The queue's `waitEmpty`
checked only the queued bytes, not bytes a writer had taken from the queue and
not yet handed to the SSH channel. A session could therefore announce its exit
status and close the channel ahead of output the client still had to read, and
the user saw a truncated session. The queue now tracks in-flight bytes and
`waitEmpty` waits for both (`TestOutputQueueWaitEmptySeesInFlightBytes`,
`TestOutputQueueWaitEmptyStopsOnTheDoneChannel`).

### Session independence and shutdown

Two channels on one connection are two sessions: distinct terminal sizes,
distinct event streams, and input that never crosses between them
(`TestChannelsOnOneConnectionKeepIndependentState`). A session starts when the
client asks for a shell, not when the channel opens, so a channel that is never
used for a shell never creates an application session, and a `pty-req` that
precedes the `shell` request is already applied.

Every session's context is a child of the connection's, and the connection's
context is cancelled when the transport reports closure, so a disconnect
cancels every session of that connection promptly. `Serve` closes its listener
and every open connection when its own context is cancelled, so a cancelled
context is a shutdown.

The session lifecycle is driven by the handler, not by the channel closing. A
client keeps its session channel open until it receives the exit status, so an
implementation that waits for the channel to close before sending that status
deadlocks. The spike hit exactly that and the test suite hung for two minutes
before the timeout dump showed the cause.

## Terminal primitives

### Recommendation

Use `github.com/charmbracelet/x/ansi` **as a pure byte-stream library**, for
output-side work only, together with a small owned input decoder. Do not use
`ultraviolet`'s `Terminal`.

- **Why not `Terminal`:** it owns a process terminal. `NewTerminal(nil, nil)`
  builds a console over `os.Stdin` and `os.Stdout`; `Start` calls
  `Console.MakeRaw`, reads `TERM` from the process environment, builds its key
  table from terminfo, and installs a `SIGWINCH` handler. In the container the
  probe's call failed with `failed to set terminal to raw mode: inappropriate
  ioctl for device`, which is the honest answer: a service has no terminal to
  own.
- **Why `x/ansi` is usable:** it opens no file, reads no environment variable,
  starts no goroutine, and takes an `io.Writer` or a byte slice.
  `DecodeSequence` walks a byte stream and reports each item with its cell
  width; `StringWidth` and `Strip` measure and clean text. The probe
  (`cmd/terminal-primitives`, section 6 of `receipts/probes.txt`) compiles and
  runs this use: a frame containing `OSC 52` (a clipboard write) and `OSC 0` (a
  window title) comes out with both sequences removed and the SGR styling and
  cursor positioning intact, and `Width("\x1b[31mred\x1b[0m")` is 3.
- **What `x/ansi` is used for in the spike:** `internal/termrender` validates a
  rendered frame against an allowlist of cursor, erase, and SGR sequences, and
  measures display width. That is exactly the PLAN 6.1 requirement that text
  displayed as data must not activate clipboard, hyperlink, title-change, or
  other unapproved controls. Clipboard (`OSC 52`), hyperlink (`OSC 8`), title
  (`OSC 0`), DCS, APC, PM, and SOS are all removed; `CSI ?1049h` and other
  private sequences are refused unless a policy opts in.

### The input decoder

`internal/termdecode` is a small byte-stream state machine with no terminal, no
timer, and no goroutine. Its properties, all covered by tests:

- UTF-8 runes, including a rune split across two writes.
- Control keys: Ctrl-A/B/C/D/E/F/K/L/U/W, Enter, Tab, Backspace, arrows, Home,
  End, Delete, Insert, Page Up/Down, shift-Tab, and modifiers in the xterm
  `ESC [ 1 ; m X` form.
- Escape sequences split across writes at any byte boundary, including one byte
  per write, and the SS3 (`ESC O x`) forms.
- Bracketed paste, including a paste end marker split across writes. A paste is
  bounded at 64 KiB by default: a longer paste yields one `Paste` with
  `Truncated` set preceded by a `KeyPasteOverflow`, and the remainder is
  discarded. The truncation point is walked back to a character boundary, so the
  text that is kept is valid UTF-8.
- An unterminated escape sequence is bounded at 256 bytes by default. Beyond the
  bound the decoder emits `KeyMalformed` and resumes at the next byte, which is
  what a real terminal does when its escape timeout expires.
- xterm's in-band resize notification `CSI 8 ; rows ; cols t` decodes to the same
  `Resize` event that an SSH `window-change` produces, so one consumer handles
  both.
- `Flush` emits whatever remained buffered, so a trailing partial sequence
  becomes an observable event instead of vanishing at end of input.

A fuzzer ran the decoder over 7 821 129 generated inputs in 30 s without a
failure, checking that it never panics and never produces more events than input
bytes.

### Ultraviolet, evaluated

`ultraviolet` is not rejected wholesale; the probe separates its parts.

| Part | Usable over an SSH byte stream? | Evidence |
| --- | --- | --- |
| `TerminalReader` (`NewTerminalReader(io.Reader, termType)`) | Yes. It decodes a fragmented stream correctly: the probe fed the same bytes one byte at a time and got `h`, `i`, `up`, `ctrl+c`, `ctrl+d`, a 14-byte paste, and `shift+left`, matching the local decoder exactly. | `receipts/probes.txt` section 1 |
| | But it resolves an unfinished escape sequence on a **timer** (`EscTimeout`, 50 ms by default) rather than by the end of the stream, and its paste buffer is unbounded: a 1 MiB paste arrived as a 1 048 576-byte `PasteEvent` with no overflow event. | `receipts/probes.txt` sections 1 and 3 |
| `TerminalScreen` / `TerminalRenderer` (`NewTerminalScreen(io.Writer, Environ)`, `NewTerminalRenderer(io.Writer, []string)`) | Yes. Writing to an in-memory buffer produced `"hello 世界\r\x1b[J"` with no terminal involved. | `receipts/probes.txt` section 4 |
| `Terminal` (`NewTerminal(Console, *Options)`) | No. It owns a process terminal. | `receipts/probes.txt` section 5 |

So the timer and the unbounded paste buffer, not a tty assumption, are what
disqualify `TerminalReader` for this service. A VibeShell session needs a decode
that is a pure function of the bytes received, with every buffer bounded by
policy (PLAN 6.1), and the local decoder provides that in about 700 lines of event
types and parsing. If
that judgement is revisited, `TerminalReader` is the piece to re-measure, and
its bounds would have to be enforced above it.

Note also that `ultraviolet` pulls in eleven transitive modules, including a
terminfo database reader, for the parts the service would not use.

## What the spike does not establish

- **No load numbers.** Latency figures are loopback on one host with a Go
  client. The roughly-100-concurrent-user target in PLAN section 13 is not
  addressed here; that needs a separate load exercise.
- **No host key lifecycle.** `LoadOrCreateHostKey` creates an ed25519 key at
  mode 0600 and reuses it. Rotation, and the interaction with a persistent
  service volume, are untested.
- **No host key distribution.** Clients here use `StrictHostKeyChecking=no`
  with `UserKnownHostsFile=/dev/null`. How an administrator publishes a host key
  is out of scope.
- **No keyboard-interactive, certificate, or public-key client.** Those methods
  are refused by construction; a client that offered them was not run.
- **`exit-status` after a refusal.** When `exec` is refused, no exit status is
  sent; the client reports 255. Whether the service should send one for a
  refused request is a contract question, not a transport one.
- **Signal requests are untested with a real client.** See the Ctrl-C section:
  `ssh` does not send them for a pty session.
- **Multiplexed sessions under load.** Two channels on one connection are tested;
  many channels, many connections, and CPU saturation are not.
- **The `x11-req` refusal path is tested only from Go.** No real client was made
  to request X11; the `x11-req` payload was not validated against a real client's
  encoding.
- **TLS, ciphers, and algorithms were not tuned.** The defaults were used.
- **`eow@openssh.com` is refused** as an unknown request. A real client did not
  send it in any scenario tested here, so whether refusing it is correct for
  every client version is unconfirmed.

## Handoff notes

- The nested module keeps `x/crypto`, `x/ansi`, and `ultraviolet` out of the
  root `go.mod`. When the service adopts any of them, the root manifest needs the
  pinned versions recorded in the table above.
- `internal/termrender` is the only place `x/ansi` is used. If the terminal
  implementation grows a real screen model, that package is where the frame
  validation and width measurement belong.
- `experiments/ssh-terminal/cmd/spike-server` is an observation session, not a
  shell. The application session handler replaces it entirely.
- The container and image names derive from the checkout path, exactly as
  `scripts/dev.ps1` does, so a spike run never collides with the development
  container or with another agent's resources.
