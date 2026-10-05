# Plan: generated interactive artifacts (intermediate step)

Status: proposed (2026-10-05). Implements a stepping stone toward
[`interactive-terminal-apps.md`](interactive-terminal-apps.md).

## Goal

Let a generated application declare an **interactive screen** — a validated
interaction `Spec` plus `State` — so the host can decode it, build the pure
`interactions.Machine`, and render its `Frame` to the terminal. This proves the
riskiest part of the full terminal-UI plan (can the model produce a valid
interaction, and can the trusted renderer draw it?) before the live input loop
is built.

## Scope and compatibility

This is the **generation-and-rendering half** of the full plan:

| Full plan phase | This step |
| --- | --- |
| P0 contracts / ABI | **in scope** |
| P2 interaction renderer | **in scope** |
| P4 generation ABI | **in scope** |
| P1 host input loop (bound keys, unbound → handler, exit) | deferred |
| P3 SSH transport (line mode ↔ app mode) | deferred |
| P5 persistence / lifecycle | deferred |
| P6 end-to-end / docs | deferred |

Everything here is additive and uses the same contracts the full plan freezes,
so P1/P3/P5/P6 build on it without rework. After this step a generated
interactive app **displays its screen** over SSH; driving it with keys is the
next step.

## Current state

- `internal/interactions.Machine` is complete and pure (ADR 0010) but unused:
  `New(spec, state, screen, limits)`, `View() (domain.AppView)`,
  `Frame() (Frame)`, `Apply`, `Replace`. `Frame{Rows, Cursor, Spans, Status,
  View}` is the renderer projection (rows + semantic `SpanStyle` runs, no escape
  bytes).
- `domain.AppManifest` / `domain.AppResult` carry no interaction.
- `cmd/vibeshell/generation.go` `runApp` returns only `Output.Text` from
  `appText`; it never emits a view or frame.
- `renderer.RenderView(AppView)` renders text-shaped views; there is no renderer
  for `interactions.Frame` (spans, table columns, gutters).
- The generation prompt forces `view.mode: "text"`.

## Contracts (frozen before parallel work)

### `internal/domain` (Task S1)

```go
// AppManifest gains an optional serialized interaction.Spec.
InteractionSpec json.RawMessage `json:"interaction_spec,omitempty"`

// AppResult gains an optional serialized {spec?, state}.
Interaction json.RawMessage `json:"interaction,omitempty"`
```

`ValidateResult` additionally requires that `Interaction` and `InteractionSpec`,
when present, are valid JSON objects (shape only; the interactions package owns
the semantic rules). `schemas/app-abi.schema.json` and `tests/contract` are
updated to match.

### `internal/interactions` (Task S2)

```go
// DecodeResult decodes an artifact's interaction JSON into a validated Spec and
// State. manifestSpec is the artifact's declared spec (may be empty); resultJSON
// is a run's {"spec"?, "state"} (may be empty). When the result carries a spec it
// replaces the manifest's; otherwise the manifest spec stands.
func DecodeResult(manifestSpec, resultJSON json.RawMessage) (Spec, State, error)

// BuildMachine decodes a result and builds a validated Machine.
func BuildMachine(manifestSpec, resultJSON json.RawMessage, screen ScreenMetrics, limits Limits) (*Machine, error)
```

Decoding uses `DisallowUnknownFields`; a missing spec and missing state are
errors; `Machine.New` re-validates the combination. A result with no interaction
is the caller's concern (it is optional), so `DecodeResult` returns a typed
validation error rather than a zero value.

### `internal/terminal/renderer` (Task S3)

```go
// RenderInteraction maps an interaction frame to exact terminal bytes: each
// SpanStyle to an SGR attribute, the cursor to an absolute position, the status
// row, and the alternate screen for full-screen modes. It stores the frame and
// returns its content reference and size, like RenderView.
func (r *Renderer) RenderInteraction(frame interactions.Frame, size screen.Size) (domain.ContentRef, int64, error)
```

The renderer imports `internal/interactions` (host-side, no cycle). Alternative
considered: pass rows/spans as renderer-local primitives — rejected as a
duplicate of the runtime's semantic types.

## Tasks

| Task | Owner scope | Deliverable |
| --- | --- | --- |
| S1 | `internal/domain/**`, `schemas/app-abi.schema.json`, `tests/contract/**` | ABI fields + shape validation + schema + contract tests |
| S2 | `internal/interactions/**` | `DecodeResult`, `BuildMachine`, tests |
| S3 | `internal/terminal/renderer/**` | `RenderInteraction` + tests |
| Main | `cmd/vibeshell/**`, `prompts/**` | Generation prompt, staging validation, engine builds the machine and emits the frame; integration; live test; docs |

S1, S2, S3 are independent (S2 operates on raw JSON and needs no S1 fields; S3
needs only the existing `Frame`). Main depends on all three.

## Generation (main agent)

- Extend `generationPrompt` to describe the interactive shape (manifest
  `interaction_spec`; result `interaction` = `{"spec"?, "state"}`), keeping the
  existing text shape and the artifact ABI. The model chooses text for a
  one-shot command and an interaction for a navigable/editable screen.
- Staging validation: when an artifact declares `interaction_spec`, decode and
  `Machine.New` it; an invalid interaction fails staging validation.
- `runApp`: when the result carries an interaction, `BuildMachine`, render
  `Frame` via `RenderInteraction`, and return a candidate whose
  `Output.Content` is the frame ref (delivered as `OutputContent`, already
  wired). Otherwise keep the text path.

## Acceptance

- `gofmt` clean; `go build ./...`; `go vet`; `go test ./...` green.
- Unit: `DecodeResult`/`BuildMachine` reject malformed/unknown-field specs and
  missing state; `RenderInteraction` maps each `SpanStyle`, the cursor, the
  status row, and the alternate screen, and never emits a control sequence that
  data could have supplied.
- Live: a generated interactive app over SSH presents its rendered screen (a
  receipt records the artifact's interaction mode and the emitted frame size).
- Compatibility: the full plan's P1/P3/P5/P6 can proceed against these
  contracts without changes.
