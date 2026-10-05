# Interaction fixtures

Each directory holds one declarative acceptance example for the primitives in
`internal/interactions`:

- `app.json` — the artifact's static description: the app manifest, the
  interaction spec (view mode, declared modes, key bindings to approved
  primitive actions), the initial interaction state, the screen size the flows
  are recorded at, and the handler rules the sandbox test double replays.
- `handler.js` — the sandboxed event handler that belongs to the artifact. It is
  the artifact's `source`; the fixtures load it, hash it, and record it in
  `domain.AppArtifact`. These tests do not execute it (this task has no
  JavaScript engine); they exercise the same ABI through a deterministic double.

`app.json` is the whole contract a generated artifact needs. Nothing here names
a program the runtime knows: the supported universe is the eight approved
primitive actions, and each example is a different arrangement of them.

## Shape of `app.json`

| Field | Meaning |
| --- | --- |
| `example` | Human-readable name, for test output only. |
| `artifact` | App ID, version ID, manifest, and the handler file name. |
| `interaction` | `interactions.Spec`: view mode, modes, bindings, mode targets. |
| `state` | `interactions.State` the handler returns before the first key. |
| `screen` | Rows and columns the recorded flows run at. |
| `handler.rules` | Ordered rules the double matches an event against. |
| `handler.text_rules` | Ordered rules the double matches raw key text against. |

A rule matches the first time its `event` (or `text`) matches. A rule's
`result` may carry `spec` (a partial spec overlay), `state` (a partial state
overlay), `effects` (proposed `domain.AppEffect` values), `exited`, and
`exit_code`. A `null` rule means "the handler cannot handle this", which is the
case that must route to the model-driven extension path.

## Why the handler is a rule table

The real handler is JavaScript running in the sandbox adapter (B07). The rules
express the same decisions as data so the key flows are deterministic and
offline here: the primitive dispatch, the view, and the frame are the real
implementations, and only the application's own decisions (interpreting `w`,
reordering simulated process rows, refusing to quit a dirty buffer) come from
the table. That split is what the acceptance examples need to show: routine
interaction stays deterministic, while application judgement stays in the
application.