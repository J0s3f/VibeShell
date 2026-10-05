package export

import "j0s.at/vibeshell/internal/domain"

// Format identifiers accepted by Service.Export and domain.ExportQuery.Format.
const (
	FormatJSONL      = "jsonl"
	FormatTranscript = "transcript"
	FormatAsciicast  = "asciicast"
)

// Versioned layout identifiers. They are written into each projection so a
// later reader can interpret it without guessing.
const (
	// BundleFormatVersion is the JSONL research-bundle layout version.
	BundleFormatVersion = 1
	// TranscriptFormatVersion is the readable-transcript layout version.
	TranscriptFormatVersion = 1
	// AsciicastFormatVersion is the only replay version emitted or parsed.
	AsciicastFormatVersion = 2
)

// Session statuses recorded in the bundle manifest. A session is complete
// only when its session.end event was part of the exported stream.
const (
	statusComplete             = "complete"
	statusIncompleteDisconnect = "incomplete_disconnected"
)

// record is one canonical event plus its resolved payload JSON and the exact
// bytes of every content reference the payload carries. It is the single
// source every projection reads.
type record struct {
	env     domain.EventEnvelope
	payload []byte
	blobs   map[string][]byte // content id -> exact bytes
}

// sessionStatus is the explicit outcome recorded for one exported session.
// Complete stays false for a disconnected or truncated session: the export
// never invents an ending that was not recorded.
type sessionStatus struct {
	SessionID  domain.SessionID
	Complete   bool
	EndReason  string
	EventCount int
}

// streamSummary carries the stream facts a projection needs at close: the
// per-session outcome, the event count, the first event timestamp, and the
// distinct config/prompt/catalogue snapshots referenced by the exported
// events.
type streamSummary struct {
	sessions         []sessionStatus
	count            int
	startedAt        int64
	configVersion    string
	promptVersion    string
	catalogueVersion string
}
