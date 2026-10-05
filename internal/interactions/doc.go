// Package interactions implements the reusable declarative interaction
// primitives that stand between a generated application's sandboxed event
// handler and the trusted terminal renderer (PLAN 6.2, 6.3).
//
// # Division of labour
//
// A generated application never paints a screen and never receives a key it
// cannot describe. Its sandboxed JavaScript handler owns application logic and
// data; it describes the screen as a Spec (a view mode plus declared modes and
// key bindings) and holds application state. The primitives in this package own
// everything a screen can do without knowing what application is running:
//
//   - a text buffer with a cursor, selection, and bounded undo/redo,
//   - literal buffer search with bounded scanning,
//   - viewport scrolling and screen layout for text, form, table, and status
//     views,
//   - mode switching with per-mode cursor conventions,
//   - status-line updates, and
//   - semantic events handed back to the sandboxed handler.
//
// # The supported universe is actions, not program names
//
// A binding maps a key chord to one of the eight approved primitive actions
// declared in the domain package (move_cursor, edit_buffer, select_range,
// search_buffer, scroll, update_status, mode_switch, emit_event). Nothing here
// knows any program names: adding an application means writing a new Spec and
// handler, never editing this package. Keys that no declared binding claims are
// reported as OutcomeUnknown; the caller routes them to the artifact's handler
// first and to the model-driven generation/extension path when the handler
// cannot handle them either (KeyUnknownRouting). A binding never falls through
// to a real executable.
//
// # Side-effect boundary
//
// Nothing in this package performs I/O. Buffers live in memory, content
// references are resolved by the host through the ContentStore port, and
// saving, world mutation, or leaving an application happens through the
// application's effect-proposal path. Time, randomness, and session identity
// are injected by the caller, so every function here is deterministic and
// testable without a sandbox, a terminal, or a model.
//
// # Renderer boundary
//
// Frame is a plain-data projection: exact text per row, a cursor position, and
// semantic style spans. It deliberately contains no escape bytes, and
// SanitizeData neutralizes any control byte that arrives as application data,
// so text shown as data cannot activate clipboard, hyperlink, title-change, or
// other unapproved terminal controls (PLAN 6.1). The trusted renderer maps
// semantic spans onto the terminal sequences it owns.
package interactions
