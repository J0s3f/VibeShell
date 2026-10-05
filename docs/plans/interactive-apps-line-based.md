# Plan: line-based interactive generated apps

Status: implemented (2026-10-05). Supersedes the intermediate scope of
[`interactive-artifacts-intermediate.md`](interactive-artifacts-intermediate.md);
stays compatible with [`interactive-terminal-apps.md`](interactive-terminal-apps.md).

## Goal

Let a generated application run as a **line-based interactive session**: the user
types a line, the app responds, it keeps state across lines, and it exits back to
the shell. Everything stays line-based — the existing SSH line editor submits a
complete line and the app's reply is plain text. No raw keystrokes, no alternate
screen, no terminal UI.

The generated-app API gains the ability to (a) receive a line of user input and
(b) tell the shell it is still running and wants the next line.

## Why line-based first

The line editor, turn/commit machinery, per-session app state, and text output
already exist. Line-based interactivity reuses all of them, so it de-risks the
product with far less new machinery than the terminal-UI plan, and it shares the
foreground-app contracts that the terminal-UI plan later extends.

## Compatibility with the terminal-UI plan

| Concern | Line-based (this step) | Terminal-UI (later) |
| --- | --- | --- |
| Foreground transition | `SessionPatch.Foreground{app,version}` | same |
| App run loop | one turn per line, `input` event | one turn per key event |
| Input kind | `InputCommand` (a line) | `InputKey`/`InputResize` |
| Rendering | plain text lines | `interactions.Frame` → `RenderInteraction` |
| State | `apps.Service` session state | same, plus the interaction machine |

The terminal-UI plan adds `InputKey` routing, the `Machine`, and frame rendering;
the line-based `InputCommand` path remains for text apps. Nothing here is
reversed by it.

## Current state

- The SSH line editor submits `application.SessionInput{Kind: InputCommand,
  Command: <line>}`; each command starts a turn.
- `cmd/vibeshell/generation.go` resolves an app by command name and runs it once
  with `event.args`; it never stays in the app.
- `application.ForegroundState{Kind: shell|app, AppID, AppVersionID}` exists and
  is carried on `SessionPatch.Foreground` / `SessionContext.Foreground`; nothing
  sets it to `app`.
- `apps.Service.ResolveSession` / `SaveSessionState` persist per-session app
  state and pin the version.
- `AppRunner` (`internal/simulation`) fulfils world reads and runs the sandbox.
- `PromptState{CWD, ExitCode}` drives the shell prompt; there is no app prompt.

## Contracts (frozen before parallel work)

### `internal/domain` (Task L1)

```go
type AppResult struct {
    // … existing fields …
    // AwaitingInput asks the shell to keep this app foreground and deliver the
    // next line as another input event.
    AwaitingInput bool `json:"awaiting_input,omitempty"`
    // Prompt is the continuation prompt shown while the app is foreground. It
    // is data, sanitized by the terminal layer; bounded to 64 bytes.
    Prompt string `json:"prompt,omitempty"`
}
```

`ValidateResult` rejects `AwaitingInput && Exited` and an over-long `Prompt`.
The line input event payload is `{"line": <string>, "args": [<string>…]}`; the
generation prompt documents it. `schemas/app-abi.schema.json` and
`tests/contract` are updated.

### `internal/application` (Task L2)

```go
// PromptState gains the app prompt shown while an app is foreground.
type PromptState struct {
    CWD      domain.ValidPath
    ExitCode int
    // App is set while a generated application is foreground.
    App *AppPrompt
}
type AppPrompt struct {
    AppID  domain.AppID
    Name   string
    Prompt string
}
```

- `handleInput`: when `Context.Foreground.Kind == app` and the input is a
  command, the turn is an **app turn** (the engine resolves the app from
  `Foreground`, not the command name). `InputEOF` while an app is foreground
  exits the app (foreground back to `shell`) instead of ending the session.
- The candidate's `SessionPatch.Foreground` is set from the app result:
  `AwaitingInput` → `{app, version}`; `Exited` → `shell`.
- The session records the app prompt so the emitted `OutputPrompt` carries it.

### `cmd/vibeshell` (Task L3)

- `generationEngine.Generate`: when `req.Context.Foreground.Kind == app`, resolve
  the foreground app and run it with an `input` event carrying `{"line", "args"}`;
  return a candidate with `Output.Text`, `ExitStatus`, `SessionPatch.Foreground`
  and the app prompt. Otherwise unchanged (command-name resolution).
- `promptString` renders the app prompt while an app is foreground.
- The generation prompt documents the line-based interactive shape.

## Tasks

| Task | Owner scope | Deliverable |
| --- | --- | --- |
| L1 | `internal/domain/**`, `schemas/app-abi.schema.json`, `tests/contract/**` | ABI fields + validation + schema + contract tests |
| L2 | `internal/application/**` | Foreground line routing, `PromptState.App`, exit-on-EOF, tests |
| L3 | `cmd/vibeshell/**` | Engine app-turn handling, app prompt rendering, generation prompt |
| Main | integration, live test, docs | wire, verify reachability, live SSH run, FEATURES/ADR |

L1 → L2 → L3 (each builds on the previous contract). They can start in parallel
against the frozen contracts; integration is main-agent.

## Implementation status (2026-10-05)

Merged: L1 (`48b7f7a`), L2 (`c584c38`), L3a (`829ada5`), and the main-agent
wiring — the prompt-bound collapse (`c3cd2a5`), the `line` input field and
sanitized app-prompt rendering (`56d9a5e`), the engine app-turn path
(`e8338fe`), the reader staying alive when an app exits on end of input
(`34be7d8`), and running the pinned version (`68e883f`). `go test ./...` and the
race tests for the affected packages are green.

L3a was finished by the main agent after its subagent hit a free-model rate
limit (recorded in `docs/ops/model-rate-limits.md`); the partial work was
verified in its worktree before integration.

Live-verified 2026-10-05 over real SSH: `todo --interactive` generated a
line-based app that kept its state across lines (`add` then `list` showed the
item) and returned to the shell on `quit`; receipt
`experiments/opencode-live-probe/receipts/2026-10-05T114338-line-based-interactive.json`.
The live runs also surfaced and fixed three defects: the generation token budget
and the SSE event bound (ADR 0012), the shell's own exit words ending the session
while an app was foreground, and the generated app's state scope keys.

## Generation: the model chooses one-shot or interactive

At first creation the model is told **all** the shapes an app may take and picks
one. This is the same decision the terminal-UI plan later extends; the choice is
the model's, made once per app.

- **One-shot** (today's behaviour, still the default for simple commands):
  `handle` returns its text output and finishes. The shell prompt returns
  immediately.
- **Interactive, line-based** (this step): `handle` returns `awaiting_input:
  true` and a `prompt`; the shell keeps the app foreground and delivers each
  subsequent line as an `input` event; the app returns `exited: true` to finish.
- **Interactive, full-screen TUI** (later): reserved wording in the prompt so a
  future revision can add a full-screen shape without changing how the one-shot
  vs interactive choice is made.

The prompt states the criteria (a REPL, game, monitor, or wizard is interactive;
a filter, lister, or formatter is one-shot) and that either is valid. An app that
declares neither `awaiting_input` nor `exited` is one-shot. Existing generated
apps keep working unchanged.

## App state contract

A generated app's `new_state` must use the scope keys `session_state`,
`user_state`, and `shared_state` (each an object). The shell decodes exactly
those keys into `domain.AppState` and passes them back on the next call as
`state.session_state`, `state.user_state`, and `state.shared_state`; any other
key is dropped. The generation ABI and the operator brief state this explicitly,
guarded by `TestGenerationABIDocumentsStateScopes`. A live app that used
`session`/`user`/`shared` lost its state between lines
(`add` reported success, `list` was empty); the corrected prompt is verified in
receipt `2026-10-05T114338-line-based-interactive.json`.

## Behaviour

```
alice@vibeshell.live:~$ moon-orchard            # unknown command -> generated app
Moon Orchard 0.3.7. You are at the edge of a grove.
> look                                          # app prompt; the line goes to the app
Silver trees; a low bright moon.
> tend                                          # state persists across lines
You tend the grove. Moon phase: waxing.
> quit
alice@vibeshell.live:~$                          # app exited; shell prompt returns
```

- While an app is foreground, every submitted line is an app turn; the shell does
  not interpret it.
- Ctrl-D (EOF) while an app is foreground exits the app, not the session.
- The app's `new_state` is saved after every line, so a reconnect resumes it
  (subject to the existing session-state persistence).

## Acceptance

- `gofmt` clean; `go build ./...`; `go vet`; `go test ./...` green.
- Unit: an app result with `AwaitingInput` keeps the app foreground and the next
  line reaches the app; `Exited` returns to the shell; `AwaitingInput && Exited`
  and an over-long prompt are rejected; EOF in app mode exits the app.
- Live: a generated line-based interactive app over SSH: invoke it, type several
  lines (including one that changes state), see the state persist, and `quit`
  back to the shell; a redacted receipt records the flow.
