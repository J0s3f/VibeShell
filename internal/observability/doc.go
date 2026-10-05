// Package observability provides operational logging, metrics, and status reporting
// for VibeShell. It is strictly separated from the permanent research-event stream.
//
// Operational logs vs. research events
//
// Operational logs (this package):
// - Structured, low-cardinality, redacted logs for operators and debugging.
// - Emitted during normal service operation via slog.
// - Never contain raw commands, usernames, file contents, or credentials.
// - Metrics have bounded label values (no raw user input).
// - Readiness/status names failing subsystems without leaking provider/account details.
//
// Research events (internal/adapters/sqlite/events, internal/application):
//   - Append-only, permanent, high-fidelity record of every session, turn, model
//     request/response, tool call, world change, and terminal frame.
//   - May contain user-supplied content, commands, and file contents (as data).
//   - Redacted only for known secret shapes at write time; no promise of perfect
//     redaction for arbitrary pasted text.
//   - Exported via admin CLI as JSONL bundles, transcripts, and terminal replays.
//   - The agent's scoped retrieval port sees only policy-filtered projections.
//
// This package MUST NOT import internal/adapters/sqlite, internal/application,
// or internal/simulation. It provides operational observability only. The
// research-event stream is a separate concern with its own storage, retrieval,
// and export pipeline.
package observability
