// Package export implements the three research projections defined in
// PLAN 10.5 from one canonical event stream: a versioned JSONL research
// bundle, a readable UTF-8 transcript, and an asciicast v2 terminal
// recording.
//
// All three projections consume the same ordered records assembled by
// recordSource. The source pages through the EventStore or RetrievalStore in
// bounded batches and resolves content references through the ContentStore;
// each writer streams to disk through a bufio buffer instead of holding the
// whole export in memory. Secrets are redacted through
// internal/observability's Redactor according to the query's
// domain.RedactionPolicy, and an export of a session that never recorded an
// end is explicitly marked incomplete rather than given a fabricated
// successful ending.
//
// The package depends only on internal/domain, internal/ports, and
// internal/observability; it never imports a concrete adapter, so a test can
// substitute doubles for the three stores and the clock and randomness ports.
package export
