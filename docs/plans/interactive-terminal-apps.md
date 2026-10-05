# Plan: interactive vibed apps with a terminal UI

Status: proposed (2026-10-04). No implementation started.

## Goal

Let a generated application own the terminal the way a real TUI does: it
declares a screen (editor, pager, table, form, status), binds keys to primitives,
and reacts to events, with the trusted host rendering it and never executing
generated code outside the sandbox. The pieces already exist — the
`internal/interactions` runtime (ADR 0010) is complete and tested but has no
production caller (audit A2). This plan wires it end to end and extends the
generation ABI so the model can produce such apps.

## Non-goals

- No real `vi`/`less`/`top` process, no arbitrary widget toolkit, no guest-side
  event loop (ADR 0010 rejected these).
- No terminal mouse protocols, job control, or registers/macros (PLAN 6.3).
- No change to the simulated-world or sandbox security model.

## Current state (facts)

**The runtime exists and is pure.** `internal/interactions`:
`Machine.New(spec, state, screen, limits)`, `Apply(Input) (Outcome, error)`,
`Replace(spec, state)`, `SetScreen`, `View() (domain.AppView, error)`,
`Frame() (Frame, error)`, `InputEvent(...)`, `ExtensionRequest(...)`. It
implements the eight primitive actions, six view modes, modes/bindings, undo,
bounded search, viewport, and status placeholders. `Frame{Rows, Cursor, Spans,
Status, View}` is the renderer projection (no escape bytes); `domain.AppView` is
the cross-boundary description. `Outcome.Kind` is `applied|pending|unknown|
exited`; `unknown` is the extension seam.

**The domain contracts exist.** `domain.AppView{Mode, BufferRef, Cursor,
Viewport, StatusLine, KeyBindings, Metadata}`; modes `text|form|table|editor|
pager|status`; `AppResult{NewState, View, Effects, WorldReads, AIRequest,
Exited, ExitCode}`; `AppEvent` types `input|timer|resize|ai_request|world_change`.
`ValidateView` currently checks only the mode and action names.

**Foreground state exists.** `application.ForegroundState{Kind: shell|app,
AppID, AppVersionID}`, carried on `SessionPatch.Foreground` and
`SessionContext.Foreground`. Nothing sets it to `app` today.

**Input contracts exist but are unused.** `application.SessionInput` has
`InputKey` ("one control key ... inside a foreground interaction") and
`InputResize`. But `handleInput` only special-cases `InputResize`/`InputCancel`/
`InputEOF`; **`InputKey` falls into `default` and starts a turn** (`beginTurn`),
which is wrong. Nothing submits `InputKey` yet.

**Frame delivery works.** `cmd/vibeshell/sshhandler.go` `writeOutput` now
fetches `OutputFrame`/`OutputContent` bytes from the content store and writes
them; `renderer.RenderView` renders an `AppView` to exact bytes. But
`runApp` (`cmd/vibeshell/generation.go`) returns only `Output.Text` (from
`appText`); it never sets `Output.View`, so no frame is produced for an app.

**Generation is text-only.** The generation prompt forces `view.mode: "text"`
and `view.metadata.output`. `internal/adapters/sandbox` runs
`handle(state, event)` and returns `domain.AppResult`.

**The SSH transport is line-mode.** `sshhandler.readLoop` drives
`editor.Editor` + `input.Decoder`, submits `InputCommand` on Enter, cancels on
Ctrl-C, EOFs on Ctrl-D, and repaints on resize locally. It never submits
`InputKey`/`InputResize` and never leaves the shell prompt.

**The renderer is text-shaped.** `renderer.RenderView` builds a full-screen
repaint from the `AppView`'s `BufferRef` content + status line + cursor; it does
not render `interactions.Span` runs, table columns, form fields, gutters, or key
hints, because those live in the interaction `Frame`/`Metadata`.

## The gap

1. **ABI**: an artifact cannot return an interaction (`Spec`+`State`) that the
   host turns into a `Machine`.
2. **Host loop**: nothing holds a `Machine` for a session, applies keys, and
   renders `Frame`.
3. **Input plumbing**: `InputKey`/`InputResize` are not routed to a foreground
   app, and `handleInput` mishandles `InputKey`.
4. **Rendering**: no renderer maps `interactions.Frame` (rows + spans + cursor)
   to bytes, and `runApp` never emits `Output.View`.
5. **Generation**: the model is told to produce text only.
6. **Lifecycle/persistence**: entering/leaving the foreground app, alt-screen
   transitions, resize, and restoring the interaction on reconnect are unbuilt.

## Design decisions

### D1 — The interaction is app-owned data, decoded host-side (recommended)
The artifact carries the interaction as opaque JSON; `domain` never imports
`interactions`. Recommended shape:

- `domain.AppManifest` gains `InteractionSpec json.RawMessage` (optional): the
  static `interactions.Spec`, validated at candidate registration.
- `domain.AppResult` gains `Interaction json.RawMessage` (optional): a serialized
  `{spec?, state}` the host decodes; `spec` is present only when the handler
  replaces the interaction (new columns, mode switch), otherwise the manifest
  spec stands and only `state` is applied.

Alternatives:
- Put `Spec`+`State` in `AppView.Metadata`: rejected — `Metadata` already carries
  the machine's own mode metadata, and overloading it hides the ABI in a
  free-form field.
- Have the host reconstruct a `Spec` from `AppView`+`Metadata`: rejected — modes,
  targets, and the default mode are not in `AppView`, so the reconstruction is
  lossy and the binding params would be unvalidated.
- A new `domain` type mirroring `Spec`: rejected — `interactions` already owns
  that schema and re-validates it; duplicating it invites drift.

### D2 — Two layers of key handling, as ADR 0010 intends
- **Bound keys** run in the host `Machine` with no model call and no sandbox run
  (`Apply` → `applied`/`pending`), and re-render.
- **Unbound keys** (`unknown`) become an `AppEventInput` (`InputEvent`) sent to
  the sandboxed handler, which returns a new `AppResult`; the host `Replace`s the
  spec/state and re-renders. When the handler cannot act, `ExtensionRequest`
  describes the key to the model-driven extension path (bounded 800 tokens/30 s).
This is the split that keeps routine interaction deterministic and offline.

### D3 — The `Machine` lives in the session, rebuilt from persisted state
The session (coordinator side) holds the foreground `Machine` in memory for the
lifetime of the attachment; the app's `State` is persisted through
`apps.Service.SaveSessionState` after each handler run (not on every local edit,
which would be a write per keystroke). On reconnect or a new session, the app is
re-pinned (`ResolveSession`) and its persisted `State` rebuilds the `Machine`.
Alternative: rebuild the machine from persisted state on every key — rejected as
a needless write per key.

### D4 — A dedicated renderer for `interactions.Frame`
`renderer` gains `RenderInteraction(frame interactions.Frame, size screen.Size)
([]byte, int64, error)` mapping rows + `Span` runs to SGR attributes, positioning
the cursor absolutely, reserving the status row, and entering the alternate
screen for full-screen modes. `interactions` → `renderer` is a host-side
dependency with no cycle. The existing `RenderView(AppView)` stays for the
text-mode path. Alternative: extend `RenderView` to accept spans — rejected; the
`Frame` is already the exact renderer projection and mixing the two shapes
muddies the boundary ADR 0010 drew.

### D5 — Key transport contract
The transport submits `SessionInput{Kind: InputKey, Key: <canonical>}` where
`<canonical>` is the `interactions.Chord` spelling (`"a"`, `"ctrl-x"`, `"up"`,
`"enter"`, `"page-down"`), plus `Text` for a text run delivered with a key. A new
`input.Event → interactions.Input` mapping (chord + text) is the single place the
decoder's events become the runtime's chords. `InputResize` carries the size for
`Machine.SetScreen`.

### D6 — Bound keys are recorded but are not turns
Every key is appended as `input.accepted` (research record). A bound key applies
locally and emits an `OutputFrame`; an unbound key starts a turn so the handler
runs. This keeps "only application judgement needs the model" while preserving
the record. Alternative: every key is a turn — rejected; it spends the model and
the turn budget on cursor movement.

### D7 — Foreground transition is a session patch
When an app's result carries an interaction, the candidate sets
`SessionPatch.Foreground = {app, version}`; leaving the app (an `exited` result
or a quit binding) sets it back to `shell`. The transport reads
`SessionContext.Foreground` to decide whether the readLoop is in line mode or
app mode.

### D8 — Generation picks text or interactive
The generation prompt is extended to describe both shapes and when to choose
one: interactive for a program with a navigable/editable screen (editor, pager,
table, form, monitor), text for a one-shot command. The artifact's interaction
`Spec` is validated at candidate registration exactly like the current manifest;
an invalid `Spec` fails staging validation rather than reaching the terminal.

## Contracts (freeze before implementation)

```go
// internal/domain/apps.go
type AppManifest struct { /* … */ InteractionSpec json.RawMessage `json:"interaction_spec,omitempty"` }
type AppResult   struct { /* … */ Interaction json.RawMessage `json:"interaction,omitempty"` }

// internal/interactions (new)
// DecodeResult decodes an artifact's interaction JSON into a Spec and State.
func DecodeResult(manifestSpec, resultJSON json.RawMessage) (Spec, State, error)

// internal/terminal/renderer (new)
func (r *Renderer) RenderInteraction(frame interactions.Frame, size screen.Size) (domain.ContentRef, int64, error)

// internal/application (new)
// InputKey now routes to the foreground app; ForegroundState drives it.
// A new InputKey path in handleInput: apply the machine or start an app turn.

// cmd/vibeshell (new)
type foregroundApp struct { machine *interactions.Machine; app domain.AppID; version domain.AppVersionID; size screen.Size }
```

## Phased implementation

Each phase is independently reviewable; phases P1–P3 can run in parallel
(different files), P4–P6 depend on them.

### P0 — Contracts and ABI (main agent, small)
- Add `AppManifest.InteractionSpec`, `AppResult.Interaction`, and
  `domain.AppResult.Interaction` to `ValidateResult` (shape-only).
- Add `interactions.DecodeResult` + tests.
- Update the `app-abi` schema and the contract test (`tests/contract`).
- **Acceptance**: `go test ./internal/domain ./internal/interactions
  ./tests/contract`.

### P1 — Host machine loop (coordinator)
- Fix `handleInput` so `InputKey` never starts a plain command turn; when
  `Foreground.Kind == app`, route it to the foreground app path.
- Add the foreground app path: apply the machine; on `applied`/`pending` record
  `input.accepted`, emit `OutputFrame`; on `unknown` start a turn whose engine
  runs the handler with `InputEvent`; on `exited` patch the foreground back to
  shell.
- Hold the `foregroundApp` (D3) on the session; persist `State` after handler
  runs; rebuild on pin.
- **Acceptance**: new `internal/application` tests drive a fake interactive app
  through bind/unbind/exit with no model; `go test -race ./internal/application`.

### P2 — Interaction renderer
- Add `renderer.RenderInteraction` mapping `Frame` rows/spans/cursor/status to
  bytes (SGR per `SpanStyle`, absolute positioning, alt screen by mode), bounded
  by the existing `Limits`.
- Add golden/fuzz tests; assert `TestFrameNeverEmitsControlSequences`'s property
  holds through the renderer.
- **Acceptance**: `go test -race ./internal/terminal/renderer`.

### P3 — SSH transport (line mode ↔ app mode)
- In `readLoop`, when the session's `Foreground` is an app, stop feeding the
  editor and instead decode keys into `SessionInput{Kind: InputKey}` (+ `Text`)
  and submit them; submit `InputResize`; render `OutputFrame` (already wired).
- On entering/leaving the app, reset the editor and prompt region; leave the
  alternate screen on exit.
- **Acceptance**: `cmd/vibeshell` handler tests with a fake shell; a manual SSH
  smoke test.

### P4 — Generation ABI (interactive artifacts)
- Extend the generation prompt to describe the interactive shape and when to use
  it; keep the text shape for one-shot commands.
- Validate `InteractionSpec`/`Interaction` at candidate registration and staging.
- **Acceptance**: a live generation of an interactive app passes staging
  validation; a recorded receipt.

### P5 — Persistence and lifecycle
- Persist the interaction `State` in the app's session state; restore it on
  reconnect; handle app rollback while foregrounded; clean exit back to the
  shell.
- **Acceptance**: `internal/apps` + `cmd/vibeshell` durable tests.

### P6 — End-to-end and docs
- A fixture interactive app (`tests/fixtures/interactions/{editor,pager,monitor}`)
  driven over real SSH: open, navigate, edit, save (semantic event → world
  commit), quit.
- ADR update (0010 status), FEATURES, `docs/guides/`, and a live receipt.
- **Acceptance**: the e2e flow above, plus the full `go test ./...`.

## Parallelization (AGENTS)

- **P1**, **P2**, **P3** are independent packages and can be one subagent each
  (dedicated worktree + container), with frozen contracts from P0.
- **P4** is `cmd/vibeshell/generation.go` (main agent) to avoid composition
  conflicts.
- **P5/P6** are main-agent integration after P1–P4 merge.
- The main agent wires `cmd/vibeshell` and verifies each handoff is reachable
  (constructor called, a test exercising the path) before merging — the same
  gate used for wave 1.

## Risks and open questions

- **Renderer fidelity**: wide characters, combining marks, tabs, and resize are
  the trusted renderer's job (ADR 0010 limitation) and are unproven for the
  `Frame` projection.
- **Sandbox ABI**: the fixture handlers are rule tables, not a real JS engine
  (ADR 0010). P4 must prove a sandboxed guest can return a valid `Spec`/`State`.
- **Per-key cost**: a bound key must not write to storage; D3/D6 keep writes to
  handler runs and explicit saves.
- **Alt-screen and the shell prompt**: entering/leaving must not corrupt the
  line editor's prompt region or the recorded transcript.
- **Reconnect**: restoring a foreground app must re-pin the same version and
  state.
- Open: whether `MaxChordLength` (4) and the nine status placeholders suffice;
  whether a new view mode can be added without a Go change (ADR 0010 leaves this
  open).

## Acceptance (overall)

- A generated interactive app (editor/pager/table/monitor) opens over SSH,
  renders through the trusted renderer, responds to bound keys with no model
  call, routes unbound keys to the sandboxed handler, saves through the
  world-commit path on a semantic event, and quits back to the shell.
- No generated byte reaches the terminal as a control sequence.
- Full suite green; audit A2 marked resolved; ADR 0010 updated with the
  integration evidence.
