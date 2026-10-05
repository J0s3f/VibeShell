package simulation

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// Summarizer bounds one model call for history summarization.
const (
	// SummaryMaxTokens caps the summary response.
	SummaryMaxTokens = 512
	// SummaryDeadlineMs caps one summarization request.
	SummaryDeadlineMs int64 = 30000
	// SummaryPromptMaxChars bounds the event text embedded in the summary prompt.
	SummaryPromptMaxChars = 32 << 10
)

// Summarizer turns older history into a derived summary record. The record
// carries its provenance — source event range, model/prompt version, and
// scope — so a summary is never mistaken for raw history (PLAN 7.3).
type Summarizer interface {
	Summarize(ctx context.Context, req SummaryRequest) (SummaryRecord, error)
}

// SummaryRequest is one summarization job: a scope's older events, oldest
// first, plus the provenance context the record must carry.
type SummaryRequest struct {
	Events        []domain.EventRecord // oldest first
	Scope         domain.Scope
	ModelRoute    domain.RouteID
	PromptVersion string
	NowUnixMilli  int64
}

// EventRange identifies the per-session sequence span a summary covers.
type EventRange struct {
	SessionID domain.SessionID `json:"session_id"`
	FromSeq   uint64           `json:"from_seq"`
	ToSeq     uint64           `json:"to_seq"`
}

// SummaryRecord is the derived summary with its provenance.
type SummaryRecord struct {
	SummaryID     string           `json:"summary_id"`
	Scope         domain.Scope     `json:"scope"`
	SourceEvents  []domain.EventID `json:"source_events"`
	SourceRange   *EventRange      `json:"source_range,omitempty"`
	TokenCount    int              `json:"token_count"`
	ModelRoute    domain.RouteID   `json:"model_route"`
	PromptVersion string           `json:"prompt_version"`
	CreatedAt     int64            `json:"created_at"`
	Text          string           `json:"text"`
}

// ModelSummarizer is the production Summarizer: it asks the selected model
// to compress older history and tags the result with the request's
// provenance. The summary text is model output; the source range, scope,
// and version provenance are injected here and never taken from the model.
type ModelSummarizer struct {
	Gateway ports.ModelGateway
	RouteID domain.RouteID
}

// Summarize implements Summarizer.
func (m *ModelSummarizer) Summarize(ctx context.Context, req SummaryRequest) (SummaryRecord, error) {
	if len(req.Events) == 0 {
		return SummaryRecord{}, domain.NewValidationError(
			CodeContextInvalid, "summary request carries no events", nil)
	}
	prompt := buildSummaryPrompt(req)
	resp, err := m.Gateway.Request(ctx, domain.ModelRequest{
		RouteID: m.RouteID,
		Messages: []domain.Message{
			{Role: domain.RoleSystem, Content: summarySystemPrompt},
			{Role: domain.RoleUser, Content: prompt},
		},
		MaxTokens:  SummaryMaxTokens,
		DeadlineMs: SummaryDeadlineMs,
	})
	if err != nil {
		return SummaryRecord{}, err
	}
	return SummaryRecord{
		SummaryID:     deriveSummaryID(req.Events),
		Scope:         req.Scope,
		SourceEvents:  eventIDs(req.Events),
		SourceRange:   eventRange(req.Events),
		TokenCount:    estimateTokens(resp.Message.Content),
		ModelRoute:    req.ModelRoute,
		PromptVersion: req.PromptVersion,
		CreatedAt:     req.NowUnixMilli,
		Text:          resp.Message.Content,
	}, nil
}

const summarySystemPrompt = "You compress shell session history into a concise summary for continuing the session. " +
	"Summarize the commands run, files changed, errors seen, and current intent. " +
	"Return only the summary text; no commentary."

// buildSummaryPrompt renders the events to summarize as bounded lines,
// oldest first, truncating with an explicit note when the event text would
// exceed the prompt budget.
func buildSummaryPrompt(req SummaryRequest) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Scope: %s\n", req.Scope))
	remaining := SummaryPromptMaxChars
	truncated := false
	for i := len(req.Events) - 1; i >= 0; i-- {
		ev := req.Events[i].Envelope
		line := fmt.Sprintf("#%d %s", ev.Sequence, ev.Kind)
		if ev.TurnID != nil {
			line += " turn=" + ev.TurnID.String()
		}
		line += "\n"
		if len(line) > remaining {
			truncated = true
			break
		}
		b.WriteString(line)
		remaining -= len(line)
	}
	if truncated {
		b.WriteString("... (older events truncated)\n")
	}
	return b.String()
}

// ContentGenerator infers plausible content for an unexplored path from the
// path, the node kind, and the surrounding command hint. The composition
// root wires a model-backed generator; tests wire a deterministic fake.
type ContentGenerator interface {
	GenerateContent(ctx context.Context, path domain.ValidPath, kind domain.NodeKind, hint string) ([]byte, error)
}

// SecretRedactor removes secret material from records before they cross
// into model context. RedactJSON reports whether a record consists solely
// of secret material and must be dropped entirely.
type SecretRedactor interface {
	RedactJSON(payload json.RawMessage) (json.RawMessage, bool)
	RedactText(s string) string
}

// DefaultRedactor is the production SecretRedactor: it replaces common
// secret shapes (API-key-like tokens, credential JSON values) with a
// redaction marker.
type DefaultRedactor struct{}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`(?i)"(api_key|apikey|password|passwd|secret|token|private_key)"\s*:\s*"[^"]*"`),
}

// RedactJSON implements SecretRedactor.
func (DefaultRedactor) RedactJSON(payload json.RawMessage) (json.RawMessage, bool) {
	s := string(payload)
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	// A record is undisclosable when nothing but the redaction markers and
	// JSON punctuation remains after secrets are removed.
	stripped := strings.ReplaceAll(s, "[REDACTED]", "")
	stripped = strings.Trim(stripped, " \t\r\n\"'{}[]:,=")
	if stripped == "" {
		return nil, true
	}
	return json.RawMessage(s), false
}

// RedactText implements SecretRedactor.
func (DefaultRedactor) RedactText(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}
