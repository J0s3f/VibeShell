// Package terminal groups VibeShell's owned terminal core: the byte-stream
// decoder, the local command-line editor, the screen model, and the ANSI
// renderer behind ports.TerminalRenderer. Nothing here owns an operating
// system terminal, a process, or a file descriptor; the core consumes byte
// streams and terminal metadata delivered by a transport (PLAN 6.1).
//
// # Packages
//
//   - input decodes a client's byte stream into ordered session input events:
//     UTF-8 text, control keys, escape sequences that arrive fragmented across
//     reads, and bracketed pastes. Resize and EOF do not arrive as bytes, so
//     the decoder accepts them as separate calls and keeps them in order.
//   - screen owns the cell grid: cells with style, cursor, viewport,
//     scrollback policy, and alternate-screen state. It interprets data text
//     with cursor-motion controls only and never interprets an escape
//     sequence, so a cell can never hold terminal control bytes.
//   - editor owns line-mode command editing: grapheme-aware backspace and
//     delete, arrows, Home/End, the common Emacs keys, history navigation,
//     bracketed multiline paste, completion selection, and the prompt-region
//     redraw the local layer needs.
//   - renderer implements ports.TerminalRenderer. It validates a declarative
//     view with domain.ValidateView, renders it through the screen model, and
//   - stores the resulting ANSI frame as a content reference.
//
// # Primitive decision
//
// The decoder, screen model, editor, and renderer are written against the Go
// standard library alone (unicode, utf8, bytes, strings). PLAN 3.2 allows
// Charm ANSI/Ultraviolet primitives "where useful" and explicitly permits a
// small local stream adapter instead of Ultraviolet when the library's runtime
// assumes ownership of a process terminal; Ultraviolet does, and x/term is a
// local-process-pty utility with no escape-sequence decoder at all. The only
// candidate primitive with real value was x/text's East Asian width table,
// and it reports combining marks as narrow, so a cell model would still need
// its own zero-width table. Avoiding the dependency keeps the whole terminal
// core auditable for the one invariant that matters here (no data text can
// produce a control sequence) and keeps the module manifest under the main
// agent's ownership.
//
// # Wiring
//
// A session owns one decoder, one screen, and one editor, and hands every
// event from input.Decoder to the editor, then paints the editor's prompt
// region with editor.Painter. Full-screen interaction instead hands each
// validated domain.AppView to the renderer. Keeping that composition in the
// session coordinator (C01) is what lets one decoder serve line mode, a pager,
// and a generated application without the core knowing which is active.
package terminal
