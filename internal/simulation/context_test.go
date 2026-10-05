package simulation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// newTestAssembler wires a context assembler over fresh fakes.
func newTestAssembler() (*ContextAssembler, *fakeEvents, *fakeRetrieval, *fakeSummarizer) {
	events := newFakeEvents()
	retrieval := newFakeRetrieval()
	summarizer := &fakeSummarizer{text: "older history summary"}
	assembler := NewContextAssembler(retrieval, summarizer, newFakeRedactor())
	return assembler, events, retrieval, summarizer
}

// seedSessionEvent appends a session event and registers it for scoped
// retrieval. A nil retrieval skips the registration (summary-only fixtures).
func seedSessionEvent(t *testing.T, events *fakeEvents, retrieval *fakeRetrieval, seq uint64, kind domain.EventKind, payload any) domain.EventRecord {
	t.Helper()
	record := eventWithID(t, testSession, seq, kind, payload)
	appended, err := events.Append(context.Background(), record.Envelope)
	if err != nil {
		t.Fatalf("append event: %v", err)
	}
	if retrieval != nil {
		retrieval.add(appended, domain.ScopeSession, testUser)
	}
	return appended
}

// TestContextBudget verifies token budgeting: recent events are included
// raw, older events are summarized, and the total stays within the context
// limit minus the reservation.
func TestContextBudget(t *testing.T) {
	assembler, events, retrieval, _ := newTestAssembler()
	call := testCallContext()

	// Each payload is ~300 chars: ~78 payload tokens plus the 8-token
	// envelope overhead, so roughly 86 tokens per event.
	payload := map[string]string{"text": strings.Repeat("x", 300)}
	for i := uint64(1); i <= 10; i++ {
		seedSessionEvent(t, events, retrieval, i, domain.EventKindInputAccepted, payload)
	}

	req := ContextRequest{
		SessionID:     testSession,
		UserID:        testUser,
		CWD:           domain.MustParsePath("/home/alice"),
		Policy:        call.Policy,
		PromptVersion: "prompt-v1",
		ModelRoute:    mustParseRouteID(),
		ContextLimit:  400,
		ReserveTokens: 100,
		RecentLimit:   50,
		NowUnixMilli:  1_700_000_000_000,
	}
	assembled, err := assembler.Assemble(context.Background(), req)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}

	// Available budget is 300 tokens; each event costs ~86, so three fit
	// raw and the rest are summarized.
	if !assembled.Truncated {
		t.Error("expected truncation when history exceeds the budget")
	}
	if len(assembled.Events) != 3 {
		t.Errorf("included %d raw events, want 3", len(assembled.Events))
	}
	if len(assembled.Summaries) != 1 {
		t.Fatalf("included %d summaries, want 1", len(assembled.Summaries))
	}
	if assembled.TotalTokens > req.ContextLimit {
		t.Errorf("total tokens = %d, exceed limit %d", assembled.TotalTokens, req.ContextLimit)
	}
	if assembled.ReservedTokens != req.ReserveTokens {
		t.Errorf("reserved = %d, want %d", assembled.ReservedTokens, req.ReserveTokens)
	}
	// Newest events are included first.
	if assembled.Events[0].Envelope.Sequence != 10 {
		t.Errorf("newest included event = seq %d, want 10", assembled.Events[0].Envelope.Sequence)
	}
	if assembled.Events[2].Envelope.Sequence != 8 {
		t.Errorf("oldest included event = seq %d, want 8", assembled.Events[2].Envelope.Sequence)
	}
}

// TestSummarizationProvenance verifies that a derived summary carries its
// source event range, scope, and model/prompt version, and that the raw
// older events are referenced but not duplicated into the context.
func TestSummarizationProvenance(t *testing.T) {
	assembler, events, retrieval, summarizer := newTestAssembler()
	call := testCallContext()

	// ~200-char payloads: ~53 payload tokens + 8 overhead = ~61 tokens each.
	payload := map[string]string{"text": strings.Repeat("y", 200)}
	var all []domain.EventRecord
	for i := uint64(1); i <= 6; i++ {
		all = append(all, seedSessionEvent(t, events, retrieval, i, domain.EventKindInputAccepted, payload))
	}

	req := ContextRequest{
		SessionID:     testSession,
		UserID:        testUser,
		Policy:        call.Policy,
		PromptVersion: "prompt-v9",
		ModelRoute:    mustParseRouteID(),
		ContextLimit:  200,
		ReserveTokens: 50,
		RecentLimit:   50,
		NowUnixMilli:  1_700_000_000_000,
	}
	assembled, err := assembler.Assemble(context.Background(), req)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(summarizer.requests) != 1 {
		t.Fatalf("summarizer got %d requests, want 1", len(summarizer.requests))
	}
	summaryReq := summarizer.requests[0]
	if summaryReq.Scope != domain.ScopeSession {
		t.Errorf("summary scope = %v, want session", summaryReq.Scope)
	}
	if summaryReq.PromptVersion != "prompt-v9" {
		t.Errorf("summary prompt version = %q, want prompt-v9", summaryReq.PromptVersion)
	}
	if summaryReq.ModelRoute != req.ModelRoute {
		t.Errorf("summary model route = %v, want %v", summaryReq.ModelRoute, req.ModelRoute)
	}
	// The summary covers exactly the events that did not fit: seq 1-4.
	if len(summaryReq.Events) != 4 {
		t.Fatalf("summary covers %d events, want 4", len(summaryReq.Events))
	}
	wantIDs := []domain.EventID{all[0].Envelope.EventID, all[1].Envelope.EventID, all[2].Envelope.EventID, all[3].Envelope.EventID}
	for i, id := range wantIDs {
		if summaryReq.Events[i].Envelope.EventID != id {
			t.Errorf("summary source event %d = %v, want %v", i, summaryReq.Events[i].Envelope.EventID, id)
		}
	}
	// The summary reference in the context carries the same provenance.
	if len(assembled.Summaries) != 1 {
		t.Fatalf("got %d summaries, want 1", len(assembled.Summaries))
	}
	ref := assembled.Summaries[0].Ref
	if ref.Scope != domain.ScopeSession {
		t.Errorf("ref scope = %v, want session", ref.Scope)
	}
	if len(ref.SourceEvents) != 4 {
		t.Errorf("ref source events = %d, want 4", len(ref.SourceEvents))
	}
	if ref.ModelRoute != req.ModelRoute {
		t.Errorf("ref model route = %v, want %v", ref.ModelRoute, req.ModelRoute)
	}
	if assembled.Summaries[0].Text != "older history summary" {
		t.Errorf("summary text = %q", assembled.Summaries[0].Text)
	}
	// Raw older events are not duplicated into the context.
	for _, ev := range assembled.Events {
		if ev.Envelope.Sequence < 5 {
			t.Errorf("older event seq %d included raw, want summarized", ev.Envelope.Sequence)
		}
	}
}

// TestContextSharingOffExcludesSharedSummaries verifies that a disabled
// sharing policy keeps the shared bucket empty: no shared event and no
// shared summary can enter the context.
func TestContextSharingOffExcludesSharedSummaries(t *testing.T) {
	assembler, events, retrieval, _ := newTestAssembler()
	call := testCallContext()
	call.Policy = domain.RestrictedScopePolicy()

	payload := map[string]string{"text": strings.Repeat("z", 300)}
	for i := uint64(1); i <= 3; i++ {
		seedSessionEvent(t, events, retrieval, i, domain.EventKindInputAccepted, payload)
	}
	// A shared-world event recorded in another user's session that must not
	// leak into this caller's context.
	shared := eventWithID(t, testSession2, 100, domain.EventKindWorldRead, domain.WorldReadPayload{
		Scope: domain.ScopeShared,
		Paths: []domain.ValidPath{domain.MustParsePath("/etc/motd")},
	})
	if _, err := events.Append(context.Background(), shared.Envelope); err != nil {
		t.Fatalf("append shared event: %v", err)
	}
	retrieval.add(shared, domain.ScopeShared, testUser2)

	req := ContextRequest{
		SessionID:     testSession,
		UserID:        testUser,
		Policy:        call.Policy,
		PromptVersion: "prompt-v1",
		ModelRoute:    mustParseRouteID(),
		ContextLimit:  1000,
		ReserveTokens: 100,
		RecentLimit:   50,
		NowUnixMilli:  1_700_000_000_000,
	}
	assembled, err := assembler.Assemble(context.Background(), req)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	for _, ev := range assembled.Events {
		if ev.Envelope.SessionID == testSession2 {
			t.Error("shared event leaked into context while sharing is disabled")
		}
	}
	for _, sum := range assembled.Summaries {
		if sum.Ref.Scope == domain.ScopeShared {
			t.Error("shared summary entered context while sharing is disabled")
		}
	}
}

// TestContextHeader verifies the header carries the trusted identity and
// environment facts with bounded env.
func TestContextHeader(t *testing.T) {
	assembler, events, retrieval, _ := newTestAssembler()
	call := testCallContext()

	seedSessionEvent(t, events, retrieval, 1, domain.EventKindSessionStart, domain.SessionStartPayload{
		UserID: testUser, AuthMode: "public",
	})

	req := ContextRequest{
		SessionID:     testSession,
		UserID:        testUser,
		CWD:           domain.MustParsePath("/home/alice"),
		Env:           map[string]string{"HOME": "/home/alice", "SHELL": "/bin/vibeshell"},
		Policy:        call.Policy,
		PromptVersion: "prompt-v1",
		ModelRoute:    mustParseRouteID(),
		ContextLimit:  1000,
		ReserveTokens: 100,
		RecentLimit:   50,
		NowUnixMilli:  1_700_000_000_000,
	}
	assembled, err := assembler.Assemble(context.Background(), req)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	header := assembled.Header
	if header.SessionID != testSession {
		t.Errorf("header session = %v, want %v", header.SessionID, testSession)
	}
	if header.UserID != testUser {
		t.Errorf("header user = %v, want %v", header.UserID, testUser)
	}
	if header.PromptVersion != "prompt-v1" {
		t.Errorf("header prompt version = %q", header.PromptVersion)
	}
	if header.CWD != domain.MustParsePath("/home/alice") {
		t.Errorf("header cwd = %v", header.CWD)
	}
	if header.Env["HOME"] != "/home/alice" {
		t.Errorf("header env HOME = %q", header.Env["HOME"])
	}
	if header.PolicyRevision != call.Policy.PolicyRevision {
		t.Errorf("header policy revision = %d, want %d", header.PolicyRevision, call.Policy.PolicyRevision)
	}
	if header.AssembledAt != req.NowUnixMilli {
		t.Errorf("header assembled at = %d, want %d", header.AssembledAt, req.NowUnixMilli)
	}
}

// TestContextInvalidBudget verifies budget validation.
func TestContextInvalidBudget(t *testing.T) {
	assembler, _, _, _ := newTestAssembler()
	call := testCallContext()

	req := ContextRequest{
		SessionID:     testSession,
		UserID:        testUser,
		Policy:        call.Policy,
		ContextLimit:  100,
		ReserveTokens: 100,
		NowUnixMilli:  1_700_000_000_000,
	}
	_, err := assembler.Assemble(context.Background(), req)
	if !domain.IsValidationError(err) {
		t.Fatalf("reserve == limit: error = %v, want validation", err)
	}

	req.ReserveTokens = -1
	_, err = assembler.Assemble(context.Background(), req)
	if !domain.IsValidationError(err) {
		t.Fatalf("negative reserve: error = %v, want validation", err)
	}
}

// TestModelSummarizerProvenance verifies the production summarizer tags
// the record with the request provenance and the source range.
func TestModelSummarizerProvenance(t *testing.T) {
	gateway := &fakeGateway{
		response: domain.ModelResponse{
			Message: domain.Message{Role: domain.RoleAssistant, Content: "condensed history"},
			Usage:   domain.Usage{PromptTokens: 10, CompletionTokens: 5},
		},
	}
	summarizer := &ModelSummarizer{Gateway: gateway, RouteID: mustParseRouteID()}
	events := newFakeEvents()
	var records []domain.EventRecord
	for i := uint64(1); i <= 3; i++ {
		records = append(records, seedSessionEvent(t, events, nil, i, domain.EventKindInputAccepted, domain.InputAcceptedPayload{
			Action: "command", Command: "ls",
		}))
	}
	req := SummaryRequest{
		Events:        records,
		Scope:         domain.ScopeUser,
		ModelRoute:    mustParseRouteID(),
		PromptVersion: "prompt-v3",
		NowUnixMilli:  1_700_000_000_000,
	}
	record, err := summarizer.Summarize(context.Background(), req)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if record.Scope != domain.ScopeUser {
		t.Errorf("scope = %v, want user", record.Scope)
	}
	if record.PromptVersion != "prompt-v3" {
		t.Errorf("prompt version = %q, want prompt-v3", record.PromptVersion)
	}
	if record.ModelRoute != req.ModelRoute {
		t.Errorf("model route = %v, want %v", record.ModelRoute, req.ModelRoute)
	}
	if record.SourceRange == nil || record.SourceRange.FromSeq != 1 || record.SourceRange.ToSeq != 3 {
		t.Errorf("source range = %+v, want seq 1-3", record.SourceRange)
	}
	if len(record.SourceEvents) != 3 {
		t.Errorf("source events = %d, want 3", len(record.SourceEvents))
	}
	if record.Text != "condensed history" {
		t.Errorf("text = %q", record.Text)
	}
	if record.TokenCount <= 0 {
		t.Errorf("token count = %d, want positive", record.TokenCount)
	}
	// The gateway received a bounded request.
	if gateway.request.MaxTokens != SummaryMaxTokens {
		t.Errorf("gateway max tokens = %d, want %d", gateway.request.MaxTokens, SummaryMaxTokens)
	}
	if gateway.request.DeadlineMs != SummaryDeadlineMs {
		t.Errorf("gateway deadline = %d, want %d", gateway.request.DeadlineMs, SummaryDeadlineMs)
	}
}

// TestModelSummarizerEmptyRejected verifies an empty summary request is a
// validation error, not a model call.
func TestModelSummarizerEmptyRejected(t *testing.T) {
	gateway := &fakeGateway{}
	summarizer := &ModelSummarizer{Gateway: gateway, RouteID: mustParseRouteID()}
	_, err := summarizer.Summarize(context.Background(), SummaryRequest{})
	if !domain.IsValidationError(err) {
		t.Fatalf("empty request: error = %v, want validation", err)
	}
	if gateway.calls != 0 {
		t.Errorf("gateway called %d times, want 0", gateway.calls)
	}
}

// TestDefaultRedactor verifies the production redactor replaces secret
// shapes and drops records that are only secret material.
func TestDefaultRedactor(t *testing.T) {
	redactor := DefaultRedactor{}

	out, drop := redactor.RedactJSON(json.RawMessage(`{"api_key":"sk-abcdefghijklmnopqrstuvwxyz123456","user":"alice"}`))
	if drop {
		t.Error("record with non-secret fields should not be dropped")
	}
	if !strings.Contains(string(out), "[REDACTED]") {
		t.Errorf("redacted payload = %s, want redaction marker", out)
	}
	if strings.Contains(string(out), "sk-abcdefghijklmnopqrstuvwxyz123456") {
		t.Errorf("secret survived redaction: %s", out)
	}

	_, drop = redactor.RedactJSON(json.RawMessage(`"sk-abcdefghijklmnopqrstuvwxyz123456"`))
	if !drop {
		t.Error("record consisting solely of a secret should be dropped")
	}

	if redacted := redactor.RedactText("token=abcdefghijklmnopqrstuvwxyz123456"); redacted != "token=abcdefghijklmnopqrstuvwxyz123456" {
		t.Errorf("RedactText left the value intact: %q", redacted)
	}
}

// fakeGateway is a canned ModelGateway for summarizer tests.
type fakeGateway struct {
	response domain.ModelResponse
	err      error
	request  domain.ModelRequest
	calls    int
}

func (f *fakeGateway) Request(_ context.Context, req domain.ModelRequest) (domain.ModelResponse, error) {
	f.calls++
	f.request = req
	return f.response, f.err
}

func mustParseRouteID() domain.RouteID {
	id, err := domain.ParseRouteID("rte_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err != nil {
		panic(err)
	}
	return id
}
