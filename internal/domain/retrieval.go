package domain

import (
	"encoding/json"
	"errors"
	"fmt"
)

// RetrievalScope defines the scope for event retrieval.
type RetrievalScope struct {
	Scopes        []Scope     `json:"scopes"`                // session, user, shared, baseline
	UserIDs       []UserID    `json:"user_ids,omitempty"`    // specific users (for shared scope)
	SessionIDs    []SessionID `json:"session_ids,omitempty"` // specific sessions
	IncludeShared bool        `json:"include_shared"`        // include shared scope if sharing enabled
}

// RetrievalFilter filters events for retrieval. Optional references are
// pointers so absent filters are omitted from the encoding.
type RetrievalFilter struct {
	Kinds            []EventKind   `json:"kinds,omitempty"`             // event kinds to include
	TurnID           *TurnID       `json:"turn_id,omitempty"`           // specific turn
	AttemptID        *AttemptID    `json:"attempt_id,omitempty"`        // specific attempt
	AppVersionID     *AppVersionID `json:"app_version_id,omitempty"`    // specific app version
	FromTime         int64         `json:"from_time,omitempty"`         // unix milliseconds (inclusive)
	ToTime           int64         `json:"to_time,omitempty"`           // unix milliseconds (exclusive)
	MinSequence      uint64        `json:"min_sequence,omitempty"`      // per-session minimum sequence
	MaxSequence      uint64        `json:"max_sequence,omitempty"`      // per-session maximum sequence
	ProvenanceSource string        `json:"provenance_source,omitempty"` // filter by provenance source
}

// RetrievalQuery combines scope, filter, and pagination.
type RetrievalQuery struct {
	Scope      RetrievalScope  `json:"scope"`
	Filter     RetrievalFilter `json:"filter"`
	Pagination Pagination      `json:"pagination"`
}

// Pagination controls result paging.
type Pagination struct {
	Limit      int    `json:"limit"`            // max results (1-1000)
	Cursor     string `json:"cursor,omitempty"` // opaque cursor for next page
	Descending bool   `json:"descending"`       // newest first
}

// DefaultPagination returns sensible defaults.
func DefaultPagination() Pagination {
	return Pagination{
		Limit:      100,
		Descending: true,
	}
}

// Validate validates the pagination parameters.
func (p Pagination) Validate() error {
	if p.Limit <= 0 {
		return errors.New("limit must be positive")
	}
	if p.Limit > 1000 {
		return errors.New("limit cannot exceed 1000")
	}
	return nil
}

// RetrievalResult contains the retrieved events and pagination info.
type RetrievalResult struct {
	Events     []EventRecord   `json:"events"`
	NextCursor string          `json:"next_cursor,omitempty"`
	Truncated  bool            `json:"truncated"`             // true if more results exist
	TotalCount int64           `json:"total_count,omitempty"` // if requested
	Scope      RetrievalScope  `json:"scope"`
	Filter     RetrievalFilter `json:"filter"`
	QueriedAt  int64           `json:"queried_at"` // unix milliseconds
}

// EventRecord is a retrieved event with its envelope and optional payload.
type EventRecord struct {
	Envelope   EventEnvelope   `json:"envelope"`
	Payload    json.RawMessage `json:"payload,omitempty"` // omitted if not requested or too large
	Provenance Provenance      `json:"provenance"`        // copied from envelope for convenience
}

// SurroundingContext retrieves events around a target event.
type SurroundingContext struct {
	Before int `json:"before"` // events before target
	After  int `json:"after"`  // events after target
}

// EventReference is a stable reference to an event.
type EventReference struct {
	EventID   EventID   `json:"event_id"`
	SessionID SessionID `json:"session_id"`
	Sequence  uint64    `json:"sequence"`
	Kind      EventKind `json:"kind"`
	Timestamp int64     `json:"timestamp"`
}

// RetrievalError represents a retrieval error.
type RetrievalError struct {
	Code    string         `json:"code"` // "invalid_scope", "invalid_filter", "denied", "limit_exceeded"
	Message string         `json:"message"`
	Query   RetrievalQuery `json:"query,omitempty"`
}

func (e RetrievalError) Error() string {
	return fmt.Sprintf("retrieval error [%s]: %s", e.Code, e.Message)
}

// ErrRetrievalDenied is returned when scope policy denies the retrieval.
var ErrRetrievalDenied = errors.New("retrieval denied by scope policy")

// ErrInvalidCursor is returned for an invalid pagination cursor.
var ErrInvalidCursor = errors.New("invalid pagination cursor")

// ErrLimitExceeded is returned when the result limit is exceeded.
var ErrLimitExceeded = errors.New("result limit exceeded")

// ContextWindow represents a bounded context window for model input.
type ContextWindow struct {
	MaxTokens      int           `json:"max_tokens"`
	ReservedTokens int           `json:"reserved_tokens"` // for response/tools
	Events         []EventRecord `json:"events"`
	Summaries      []SummaryRef  `json:"summaries"`
	TotalTokens    int           `json:"total_tokens"`
	Truncated      bool          `json:"truncated"`
}

// SummaryRef references a derived summary.
type SummaryRef struct {
	SummaryID    string    `json:"summary_id"`
	Scope        Scope     `json:"scope"`
	SourceEvents []EventID `json:"source_events"`
	TokenCount   int       `json:"token_count"`
	ModelRoute   RouteID   `json:"model_route"`
	CreatedAt    int64     `json:"created_at"`
}

// ExportQuery defines an export operation.
type ExportQuery struct {
	Scope      RetrievalScope  `json:"scope"`
	Filter     RetrievalFilter `json:"filter"`
	Format     string          `json:"format"` // "jsonl", "transcript", "asciicast"
	OutputPath string          `json:"output_path"`
	Redact     RedactionPolicy `json:"redact"`
}

// RedactionPolicy controls export redaction.
type RedactionPolicy struct {
	RedactSecrets   bool   `json:"redact_secrets"`    // API keys, passwords
	RedactPayloads  bool   `json:"redact_payloads"`   // model request/response bodies
	RedactUserInput bool   `json:"redact_user_input"` // user typed input
	ReplacementText string `json:"replacement_text"`  // e.g., "[REDACTED]"
}

// DefaultRedactionPolicy returns a safe default.
func DefaultRedactionPolicy() RedactionPolicy {
	return RedactionPolicy{
		RedactSecrets:   true,
		RedactPayloads:  false,
		RedactUserInput: false,
		ReplacementText: "[REDACTED]",
	}
}

// ExportResult contains export metadata.
type ExportResult struct {
	ExportID    string `json:"export_id"`
	EventCount  int64  `json:"event_count"`
	SizeBytes   int64  `json:"size_bytes"`
	Checksum    string `json:"checksum"` // SHA256
	StartedAt   int64  `json:"started_at"`
	CompletedAt int64  `json:"completed_at"`
	Format      string `json:"format"`
}
