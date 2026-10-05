# ADR 0010: Declarative interactive primitives bound to eight approved actions

Status: accepted.

Date: 2026-10-03.

## Context

PLAN 6.2 requires a generated application to return a bounded **declarative**
interaction description — display mode, buffer/data references, status text, key
bindings to approved primitive actions, and events handled either by sandboxed
application logic or by another model decision — while the view stays validated
data consumed by the trusted terminal renderer. PLAN 6.2 also enumerates the
allowed primitives (move the cursor, edit an in-memory text buffer, select a
range, search a buffer, scroll a viewport, update a status line, switch a
declared mode, emit a semantic event), requires resource limits and forbids I/O
in them, and routes saving, shared-fact mutation, and leaving an application
through the application/world commit path.

PLAN 6.3 tightens this: the runtime needs reusable text-editor, pager, and
table/status-screen primitives plus general cell/text/form views and sandboxed
event handlers; it must **not** implement a real vi/less/top process; and the
runtime API must be general enough that adding a new application requires no Go
code change.

Three project rules bear directly on the design. AGENTS.md forbids passing a
user command or generated source to a host shell or unrestricted runtime.
PLAN 6.1 assigns terminal control sequences to the owned screen model and
requires that text displayed as data cannot activate clipboard, hyperlink,
title-change, or other unapproved terminal controls. PLAN 6.3 requires that an
unknown interaction enter the model-driven generation/extension path and must
never fall through to a real executable.

The unresolved question this record settles is *where a screen lives*: whether a
generated artifact describes its screen as data that the trusted host renders,
or whether it owns a drawing surface.

## Decision

**`internal/interactions` is the whole interaction runtime, and it is pure.**
It contains no I/O, no clock, no randomness, no terminal, and no sandbox. Time,
randomness, and session identity are injected by the caller, so a complete key
flow is reproducible in a test.

**The supported universe is actions, not program names.** A binding maps one key
sequence to one of the eight actions the domain package already declares
(`domain.IsValidPrimitiveAction`): `move_cursor`, `edit_buffer`, `select_range`,
`search_buffer`, `scroll`, `update_status`, `mode_switch`, `emit_event`. Nothing
in the runtime package knows any program name. This is enforced hermetically:
`TestNoProgramNameUniverse` parses the runtime package's AST and fails on any
string literal naming `vi`, `vim`, `less`, `more`, `top`, `htop`, `nano`, or
`emacs`, because such a table has no other legitimate reason to exist. Adding an
application is therefore a new `Spec`, handler, and initial `State`.

**A `Spec` is data, validated twice.** `Spec{View, Mode, DefaultMode, Modes}`
is decoded with `DisallowUnknownFields` — a typo in a generated artifact fails
loudly instead of silently disabling a binding — and then validated
structurally. `ViewSpec` is `Mode` plus the parts that belong to that mode:
`StatusLine`, `Gutter` (editor), `Columns`/`Footer` (table, status), `Fields`
(form), `CursorVisible` (text, pager). `ViewSpec.validate` rejects fields that do
not belong to the mode, so a table cannot smuggle in form fields and the renderer
never has to guess which parts of the description are live.

**Keys are chords and sequences, not bytes.** `Chord` is a literal character or
a `NamedKey` plus `Ctrl`/`Alt`; `Sequence` is a space-separated chord list and is
the unit a binding matches, which is how a spec expresses `"d d"` or `"3 G"`
without the runtime knowing the command's meaning. Named keys are a deliberately
small set (`enter`, `escape`, `tab`, `backspace`, `delete`, `insert`, `up`,
`down`, `left`, `right`, `home`, `end`, `page-up`, `page-down`, `space`) plus
`text`, which is an **input class rather than a key**: it matches any printable
chord or any delivery carrying a text run, so one binding serves text entry in
every mode that wants it instead of a binding per character. Because a single
space separates chords, the space bar is named rather than literal. `ctrl-`
requires a lowercase letter, because Ctrl chords address control codes and only
letters have them; named keys take no modifiers.

**Modes declare their own bindings and their own cursor convention.**
`ModeDef{Name, Column, Inherit, Targets, Bindings}`. A mode inherits the default
mode's bindings and overrides what it declares; `inherit: false` makes it stand
alone. `Targets` is the statically-enforced list of modes a mode may switch to, so
an undeclared or forbidden switch is a validation error rather than a runtime
one. `ColumnPolicy` is `within_line` (default: the cursor must rest on a
character, which is command-mode editing) or `allow_end` (the insertion position
past the last character, which is text entry). This is a declared policy rather
than a rule keyed on program names, so a new application picks its own
convention. A mode that claims `text` for entry must set `inherit: false`,
otherwise the inherited letter keys swallow the text instead of reaching the
application — a sharp edge that is documented on the field and covered by the
editor fixture.

**Action parameters are a typed union, not raw JSON.** `Params` has eight
pointer members and exactly one may be populated, and it must be the member
matching the action. Each action then has its own argument rules (move targets,
edit ops, anchor sources, query sources, scroll units, status text, mode target,
event name and snapshot source), and defaulted members are normalized during
validation. This is what lets validation reject a binding that would hand a
primitive an argument it cannot honour, instead of failing later at the point of
use.

**The view mode fixes the focus space, and impossible bindings are rejected
statically.** Text/editor/pager address buffer runes, table/status address cells,
form addresses fields (whose values are single-line documents). `Machine.New` —
and `Replace`, because handler output is untrusted input to this package too —
runs `checkBindingForView`, rejecting `edit_buffer` on a cell grid,
`search_buffer` outside a buffer, and field moves outside a form. Catching these
at build time means the host never has to interpret a runtime failure as a
missing key.

**`Apply` reports one of four outcomes.** `applied`, `pending` (the chords so far
are only a prefix of a declared multi-key command), `unknown` (no binding claimed
the key, reported together with the canonical chord spelling), and `exited`.
`unknown` is the extension seam: `InputEvent` builds the sandboxed handler's input
event, and when the handler cannot act, `ExtensionRequest` builds a bounded
`domain.AIRequest` carrying only sanitized context (view mode, keys pressed,
binding count, status) capped at `MaxExtensionText`, with a fixed 800-token /
30-second budget so one key press can never become a long call. There is no path
from a binding to a real executable.

**`Frame` and `domain.AppView` are two projections with two different jobs.**

- `Frame` is the **rendering projection inside the trusted boundary**: the exact
  text of each row, one cursor position, and semantic `SpanStyle` runs
  (default, selection, cursor line, selected row, header, gutter) addressed in
  rune columns. It contains no escape bytes at all. `Frame.Validate` re-checks
  UTF-8, per-row data safety, row count, and that no span points outside its row.
- `domain.AppView` is the **validated cross-boundary description**: mode,
  `BufferRef`, cursor, viewport, status line, the full effective binding table for
  key hints, and mode-specific `Metadata`. It is what an adapter publishes, and
  `Machine.view` re-validates it with `domain.ValidateView` before it leaves the
  package.

Both are produced from the same state and both are plain data; the difference is
the audience and the contract each one is validated against. `Frame` is what the
renderer draws, `AppView` is what crosses the port, and the schema contract for
`AppView` is kept aligned with the domain type (commit `76ff780`).

**Data can never become a control sequence.** `SanitizeData` replaces every C0
rune except TAB, DEL, and every C1 rune — where eight-bit escape sequences live
— with a visible `U+FFFD`, so a user can tell content was sanitized rather than
silently losing it. It is applied to buffer content, cell text, status text,
labels, and event text. Status-line placeholders are a closed set of nine
(`mode`, `status`, `dirty`, `line`, `lines`, `column`, `total`, `percent`,
`sort`) substituted from state, with unknown placeholders rejected at validation;
declaring the dirty marker `[+]` here is what lets a view show unsaved changes
without every application writing its own status update.

**Twenty bounds, enforced at three points.** `Limits` carries
`MaxBufferBytes` (1 MiB), `MaxLines` (100 000), `MaxLineBytes` (64 KiB),
`MaxBindings` (256), `MaxChordLength` (4), `MaxUndoDepth` (64),
`MaxSearchSteps` (200 000), `MaxCount` (1 000 000), `MaxStatusBytes` (512),
`MaxMetadataBytes` (64 KiB), `MaxColumns` (64), `MaxRows` (100 000),
`MaxCellBytes` (8 KiB), `MaxFields` (64), `MaxFieldBytes` (8 KiB),
`MaxEventsPerKey` (8), `MaxEventBytes` (1 MiB), `MaxFrameRows` (512),
`MaxFrameColumns` (4096), and `MaxExtensionText` (512). A zero field is replaced
by the default so a sparse struct literal cannot silently remove a bound, and
negatives are rejected. They are checked when a machine is built (declarations
and state), after every applied action, and when a frame is built — so a later
key press cannot exceed a limit that was already known to be too large. A breach
is a domain limit error carrying `limit`/`actual`/`allowed`, and edits are applied
to a copy that is stored only once every bound holds, so a refused edit leaves the
document exactly as it was. Search is a literal substring scan bounded by
`MaxSearchSteps`: no regular expressions, so a pattern cannot cost more than the
bound and cannot be mistaken for code.

**Sorting is handler logic.** `Table.Sort` (`SortState{Column, Order}`) records
the ordering the handler *reported* so the view can label it through the `{sort}`
placeholder, and `Column.Sortable`/`SortKey` declares that a column can be sorted
and which key the handler receives in the emitted semantic event. The runtime
renders rows in the order it was given and never reorders them, because ordering
simulated process data is a claim about simulated truth that only the application
may make. Giving the runtime a `sort_by` primitive would put that judgement in
the one component that has no idea what the data means.

**Prompt state is handler logic.** `State.PromptText` is text the application
collected in *its own* prompt or command line. `QueryPrompt` reads it (so a spec
searches for a pattern typed after `/`), and `SnapshotPromptText` puts it in an
emitted event's `Command` field. Keeping text entry in the handler puts the
application's prompt line where the application owns it, instead of the runtime
inventing a generic command line that every application would then have to fight.

**The primitives own only generic notes.** "nothing to undo", "nothing to redo",
"pattern not found", "no search pattern". Everything specific to an application
is a semantic event worded by the handler, which is the same split that keeps
simulation truth in the application.

**Acceptance is demonstrated through fixtures.** `tests/fixtures/interactions/`
holds an `editor`, a `pager`, and a `monitor` example, each an `app.json` (the
artifact description plus the handler rules a sandbox double replays) and a
`handler.js`. `TestFixturesAreReachableFromTheTestSuite` fails when a checked-in
example is not loaded by a test, so the fixtures cannot rot.

## Alternatives considered

- **The generated application emits raw ANSI.** Rejected. PLAN 6.1 gives control
  sequences to the owned screen model, and application-authored bytes are exactly
  how clipboard, hyperlink, and title-change sequences get activated by text
  shown as data. A screen described as bytes also cannot be validated for width,
  cannot be diffed, and cannot be asserted on in a test. The C1-aware sanitizer
  and `TestFrameNeverEmitsControlSequences` exist because this boundary is data,
  not bytes.
- **Embedding a process TUI framework** (a bubbletea/tcell-style widget set and
  event loop, in the guest or the host). Rejected. AGENTS.md forbids handing a
  user command or generated source to a host runtime; a guest-side widget loop
  would give generated code its own event loop, its own unbounded state, and no
  single place where limits, sanitization, and the renderer boundary are
  enforced. It also collapses the required "unsupported binding → handler →
  extension path" behaviour into "the framework swallowed the key". The accepted
  cost is real: there is no arbitrary widget, so a genuinely novel presentation
  must be composed from the declared view modes or extended deliberately.
- **Letting the artifact name programs** (a `vi`-mode flag, a `less` special
  case, a per-program binding table). Rejected by PLAN 6.3's "no Go code change"
  requirement; it also makes the runtime the place where simulated-truth
  decisions accumulate. The AST guard test enforces this hermetically rather than
  by review.
- **A runtime JSON-Schema validator for the spec**, as ADR 0009 chose against for
  configuration. Not chosen for the same reason plus one more: JSON Schema is
  value-based and cannot express the action↔parameter correspondence, the
  view-mode field restrictions, or the permitted mode-target graph, all of which
  are Go-side rules here.
- **Raw `json.RawMessage` action parameters.** Rejected. It would defer the
  "does this argument mean anything for this primitive" question to run time,
  which is the failure mode the eight-member union exists to remove.
- **Regex search.** Rejected. Literal matching with a bounded scan is the only
  form whose cost is bounded by construction and whose pattern cannot be mistaken
  for code.
- **A sort primitive, or a runtime prompt line.** Rejected for the same reason as
  the decision: both are application judgement about simulated truth, and both
  would let a generated artifact assert ordering or command semantics the runtime
  cannot check.

## Consequences

- Adding an application is data plus a handler. No Go change in this package, and
  the AST guard test makes a regression fail immediately.
- Routine interaction stays deterministic and offline-testable; only application
  judgement needs the model, and only on the `unknown` path.
- The action and view-mode vocabulary lives in `internal/domain`, so the two
  packages cannot drift: `interactions` imports the domain's mode and action
  constants and its error types rather than redeclaring them.
- The renderer's screen model still owns cell width. Row clipping here is
  rune-based and only guarantees that no row exceeds the declared column count in
  runes, so a row of wide characters or combining marks can still be wider on
  screen than the column count. Wide-character, combining-mark, tab, CR/LF, and
  resize handling are explicitly the trusted renderer's responsibility.
- Undo depth, buffer size, search steps, and emitted-event size are bounded, so an
  artifact cannot offer unbounded undo or an unbounded data snapshot. Oversized
  asks fail with a domain limit error the handler can act on.
- The primitives cannot save, mutate the world, or end an application. `:w` is a
  semantic event, so the editor's save path runs through the application/world
  commit path and gets revision checks and conflict presentation for free.
- Every input is delivered through `Apply`, so a resize re-clamps viewport and
  cursor without touching the rest of the state, and an exited interaction
  rejects further input rather than resurrecting itself.
- The fixture handler ABI is exercised through a deterministic rule table, not a
  real JavaScript engine. The ABI is therefore proven against a double, which is
  a weaker guarantee than an executed guest.

## Evidence

Read for this record (documentation change only; no build or test run):

- `internal/interactions/`: `doc.go`, `spec.go`, `binding.go`, `keys.go`,
  `frame.go`, `machine.go`, `state.go`, `limits.go`, `status.go`, `search.go`,
  `errors.go`, and the function inventory of `buffer.go`.
- `internal/domain/apps.go` — `AppView`, `AppViewMode*`, `Action*`,
  `IsValidPrimitiveAction`, `ValidateView`, `AppResult`.
- Fixtures: `tests/fixtures/interactions/README.md` and the `editor`, `pager`,
  `monitor` directories (`app.json`, `handler.js`).
- Tests present in the tree (names read, **not executed** for this
  documentation task): `primitives_test.go` (19 tests, including
  `TestFrameNeverEmitsControlSequences`,
  `TestSanitizeDataKeepsTabsAndNeutralizesControls`,
  `TestResourceLimitsRefuseOversizedInput`,
  `TestSearchStepBoundIsEnforced`, `TestMultiKeyCommandWaitsForTheRest`,
  `TestUnboundKeyIsReportedForTheHandlerAndExtensionPath`,
  `TestTextBindingInsertsTypedTextWithoutPerCharacterBindings`,
  `TestResizeReprojectsWithoutLosingState`,
  `TestMachineRejectsBindingsThatCannotApplyToTheView`);
  `guard_test.go` (three hermetic source checks);
  `examples_test.go` (12 acceptance flows driven only through the public API).
- Commits: `18bf812` "Add generic interactive primitives and generated-app
  examples", merged as `0424fb9` "Merge C05 interactive primitives"; `76ff780`
  "fix(contracts): Align app-abi schema appView with domain.AppView".

## Unverified items and limitations

- **No JavaScript engine ran.** The fixture handlers are rule tables replayed by a
  test double, per the fixtures' own README. A real guest adapter is still needed
  for PLAN 6.3's AI participation, and this record does not prove that a sandboxed
  engine can meet the ABI these fixtures describe.
- **No renderer or SSH integration is in scope here.** Whether the trusted
  renderer maps `SpanStyle` spans and cursor positions onto correct sequences —
  including for wide characters, combining marks, tabs, and resize — is unverified
  by this package's tests.
- **Periodic refresh for top-like screens is not implemented.** PLAN 6.2's
  "refresh only while visible, coalesce, bound the frequency and token budget"
  rules belong to the turn/generation layer; `AppEventTimer` and refresh cadence
  are not modelled in this package.
- **Guest-side resource limits are out of scope.** These bounds cover the
  primitives' own memory, scanning, and frame work. CPU and wall-clock limits for
  the sandboxed handler are a sandbox concern and are not enforced here.
- **Large text runs and bracketed paste are consumed, not sized.** `Input.Text`
  attaches to the first chord of a delivery, so how the terminal adapter splits
  and bounds a very large paste before it reaches `Apply` is unverified.
- **PLAN 6.3's shell composition** — pipes, redirections, quoting, exact virtual
  content — is not part of this decision and is unaffected by it.
- Explicitly out of scope per PLAN 6.3 and not modelled: counts beyond a single
  chord prefix, registers, macros, advanced ex commands, terminal mouse protocols,
  and full job control.
- Open questions: whether `MaxChordLength` of 4 is generous enough for plausible
  generated artifacts (it is a bound, not a demonstrated limit); whether the nine
  status placeholders suffice for a form or table application that wants a
  progress or unit field; and whether a new view mode can ever be added without a
  Go change in this package, which this decision does not guarantee.
