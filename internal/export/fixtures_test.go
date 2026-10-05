package export

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	"j0s.at/vibeshell/internal/domain"
)

// ---------------------------------------------------------------------------
// Test doubles for the three stores plus clock and randomness. They implement
// the ports only; the export package never imports a concrete adapter.
// ---------------------------------------------------------------------------

type memoryEvents struct {
	mu     sync.Mutex
	bySess map[domain.SessionID][]domain.EventRecord
}

func newMemoryEvents() *memoryEvents {
	return &memoryEvents{bySess: map[domain.SessionID][]domain.EventRecord{}}
}

func (m *memoryEvents) add(sid domain.SessionID, recs ...domain.EventRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bySess[sid] = append(m.bySess[sid], recs...)
}

func (m *memoryEvents) Append(context.Context, domain.EventEnvelope) (domain.EventRecord, error) {
	return domain.EventRecord{}, fmt.Errorf("memoryEvents.Append not implemented")
}

func (m *memoryEvents) GetByID(_ context.Context, id domain.EventID) (domain.EventRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, recs := range m.bySess {
		for _, r := range recs {
			if r.Envelope.EventID == id {
				return r, nil
			}
		}
	}
	return domain.EventRecord{}, domain.NewNotFoundError(domain.CodeEventNotFound, "event not found", nil)
}

func (m *memoryEvents) List(_ context.Context, session domain.SessionID, minSeq uint64, limit int) ([]domain.EventRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.EventRecord
	for _, r := range m.bySess[session] {
		if r.Envelope.Sequence < minSeq {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, r)
	}
	return out, nil
}

type retrievalRecord struct {
	rec  domain.EventRecord
	user domain.UserID
}

type memoryRetrieval struct {
	records []retrievalRecord
}

func (m *memoryRetrieval) add(user domain.UserID, recs ...domain.EventRecord) {
	for _, r := range recs {
		m.records = append(m.records, retrievalRecord{rec: r, user: user})
	}
}

func (m *memoryRetrieval) Query(_ context.Context, q domain.RetrievalQuery, _ domain.ScopePolicy) (domain.RetrievalResult, error) {
	sessions := map[domain.SessionID]bool{}
	for _, s := range q.Scope.SessionIDs {
		sessions[s] = true
	}
	users := map[domain.UserID]bool{}
	for _, u := range q.Scope.UserIDs {
		users[u] = true
	}
	f := q.Filter
	var filtered []retrievalRecord
	for _, entry := range m.records {
		if len(sessions) > 0 && !sessions[entry.rec.Envelope.SessionID] {
			continue
		}
		if len(users) > 0 && !users[entry.user] {
			continue
		}
		if len(f.Kinds) > 0 && !containsKind(f.Kinds, entry.rec.Envelope.Kind) {
			continue
		}
		if f.FromTime != 0 && entry.rec.Envelope.Timestamp < f.FromTime {
			continue
		}
		if f.ToTime != 0 && entry.rec.Envelope.Timestamp >= f.ToTime {
			continue
		}
		filtered = append(filtered, entry)
	}
	start := 0
	if q.Pagination.Cursor != "" {
		parsed, err := strconv.Atoi(q.Pagination.Cursor)
		if err != nil {
			return domain.RetrievalResult{}, err
		}
		start = parsed
	}
	if start > len(filtered) {
		start = len(filtered)
	}
	limit := q.Pagination.Limit
	if limit <= 0 {
		limit = 100
	}
	res := domain.RetrievalResult{Scope: q.Scope, Filter: q.Filter, QueriedAt: 1}
	end := start + limit
	if end < len(filtered) {
		res.Truncated = true
		res.NextCursor = strconv.Itoa(end)
	} else {
		end = len(filtered)
	}
	for _, entry := range filtered[start:end] {
		res.Events = append(res.Events, entry.rec)
	}
	return res, nil
}

func (m *memoryRetrieval) Surrounding(context.Context, domain.EventReference, int, int, domain.ScopePolicy) ([]domain.EventRecord, error) {
	return nil, fmt.Errorf("memoryRetrieval.Surrounding not implemented")
}

type memoryContent struct {
	mu    sync.Mutex
	blobs map[string][]byte
}

func newMemoryContent() *memoryContent {
	return &memoryContent{blobs: map[string][]byte{}}
}

func (m *memoryContent) put(id string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blobs[id] = append([]byte(nil), data...)
}

func (m *memoryContent) Put(_ context.Context, data []byte, _ string) (domain.ContentRef, error) {
	id, err := domain.ParseContentID("cnt_00000000000000000000000000")
	if err != nil {
		return domain.ContentRef{}, err
	}
	m.put(id.String(), data)
	return domain.ContentRef{Hash: id, Size: int64(len(data)), MediaType: "application/octet-stream"}, nil
}

func (m *memoryContent) Get(_ context.Context, ref domain.ContentRef, offset, length int64) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.blobs[ref.Hash.String()]
	if !ok {
		return nil, domain.NewNotFoundError(domain.CodeContentNotFound, "content not found", map[string]string{"hash": ref.Hash.String()})
	}
	if offset < 0 || offset > int64(len(b)) {
		return nil, domain.NewValidationError(domain.CodeInvalidInput, "content offset out of range", nil)
	}
	end := int64(len(b))
	if length >= 0 && offset+length < end {
		end = offset + length
	}
	out := make([]byte, end-offset)
	copy(out, b[offset:end])
	return out, nil
}

type fixedClock struct{ now int64 }

func (c *fixedClock) NowUnixMilli() int64   { return c.now }
func (c *fixedClock) MonotonicNanos() int64 { return 0 }
func (c *fixedClock) advance(deltaMS int64) { c.now += deltaMS }

type sequenceRandom struct{ counter int }

func (r *sequenceRandom) Bytes(n int) ([]byte, error) {
	if n <= 0 {
		return nil, fmt.Errorf("random byte count must be positive")
	}
	r.counter++
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(r.counter + i)
	}
	return out, nil
}

func (r *sequenceRandom) Intn(n int) int {
	if n <= 0 {
		return 0
	}
	return 0
}

// ---------------------------------------------------------------------------
// Canonical fixture stream
// ---------------------------------------------------------------------------

type fixture struct {
	session   domain.SessionID
	user      domain.UserID
	events    *memoryEvents
	retrieval *memoryRetrieval
	content   *memoryContent
	clock     *fixedClock
	random    *sequenceRandom
	records   []domain.EventRecord
	frames    [][]byte
	complete  bool
}

func mustSession(t interface{ Fatalf(string, ...any) }, s string) domain.SessionID {
	id, err := domain.ParseSessionID(s)
	if err != nil {
		t.Fatalf("parse session: %v", err)
	}
	return id
}

func mustUser(t interface{ Fatalf(string, ...any) }, s string) domain.UserID {
	id, err := domain.ParseUserID(s)
	if err != nil {
		t.Fatalf("parse user: %v", err)
	}
	return id
}

func mustContentID(t interface{ Fatalf(string, ...any) }, s string) domain.ContentID {
	id, err := domain.ParseContentID(s)
	if err != nil {
		t.Fatalf("parse content id: %v", err)
	}
	return id
}

// envelope builds a valid offline event envelope with an inline payload.
func envelope(t interface{ Fatalf(string, ...any) }, sid domain.SessionID, seq uint64, kind domain.EventKind, payload domain.EventPayload, ts int64) domain.EventEnvelope {
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	env := domain.NewEventEnvelope(sid, seq, kind, domain.Provenance{
		Source:           "test",
		ConfigVersion:    "cfg_1",
		PromptVersion:    "prm_1",
		CatalogueVersion: "cat_1",
	}, ts, int64(seq)*1_000_000)
	id, err := domain.ParseEventID(fmt.Sprintf("evt_%026X", seq))
	if err != nil {
		t.Fatalf("parse event id: %v", err)
	}
	env.EventID = id
	env.Payload = domain.PayloadRef{Inline: raw}
	return env
}

func recordOf(env domain.EventEnvelope) domain.EventRecord {
	return domain.EventRecord{Envelope: env, Payload: env.Payload.Inline, Provenance: env.Provenance}
}

// canonicalFixture builds one accepted session with user input, terminal
// output, a full-screen transition, a model failure, an interruption, and a
// resize. When complete is false no session.end is recorded, modelling a
// disconnected session.
func canonicalFixture(t interface{ Fatalf(string, ...any) }, complete bool, secret string) *fixture {
	session := mustSession(t, "ses_00000000000000000000000001")
	user := mustUser(t, "usr_00000000000000000000000001")
	route, _ := domain.ParseRouteID("rte_00000000000000000000000001")
	account, _ := domain.ParseAccountID("acc_00000000000000000000000001")

	frame1 := []byte("drwxr-xr-x 2 user user 4096 Oct  3 00:00 .\r\n-rw-r--r-- 1 user user   42 Oct  3 00:00 README.md\r\n")
	frame2 := []byte("\x1b[H\x1b[2J[top] rows=24 cols=80 (alt screen)\r\n")
	if secret != "" {
		// A secret echoed into terminal output exercises blob redaction.
		frame1 = append(frame1, []byte("api_key="+secret+"\r\n")...)
	}
	f1 := mustContentID(t, "cnt_00000000000000000000000001")
	f2 := mustContentID(t, "cnt_00000000000000000000000002")

	content := newMemoryContent()
	content.put(f1.String(), frame1)
	content.put(f2.String(), frame2)

	command := "ls -la"
	if secret != "" {
		command = "export TOKEN=" + secret
	}

	var envs []domain.EventEnvelope
	ts := int64(1_000_000)
	step := func() int64 { ts += 100; return ts }
	appendEnv := func(kind domain.EventKind, payload domain.EventPayload) {
		env := envelope(t, session, uint64(len(envs)+1), kind, payload, step())
		envs = append(envs, env)
	}

	appendEnv(domain.EventKindSessionStart, domain.SessionStartPayload{
		UserID: user, AuthMode: "secure", TerminalType: "xterm-256color",
		TerminalSize: domain.TermSize{Cols: 80, Rows: 24}, ClientAddr: "10.0.0.7",
	})
	appendEnv(domain.EventKindInputAccepted, domain.InputAcceptedPayload{Action: "command", Command: command})
	appendEnv(domain.EventKindTerminalFrame, domain.TerminalFramePayload{
		FrameID: "f_0001", ContentRef: domain.ContentRef{Hash: f1, Size: int64(len(frame1)), MediaType: "application/octet-stream"},
	})
	appendEnv(domain.EventKindTerminalPrompt, domain.TerminalPromptPayload{Prompt: "user@vibeos:~$ ", CWD: "/home/user", ExitCode: 0})
	appendEnv(domain.EventKindTerminalMode, terminalModePayload{From: "line", To: "app"})
	appendEnv(domain.EventKindTerminalFrame, domain.TerminalFramePayload{
		FrameID: "f_0002", Mode: "alt", ContentRef: domain.ContentRef{Hash: f2, Size: int64(len(frame2)), MediaType: "application/octet-stream"},
	})
	appendEnv(domain.EventKindTerminalMode, terminalModePayload{From: "app", To: "line"})
	appendEnv(domain.EventKindModelRequest, domain.ModelRequestPayload{
		RouteID: route, AccountID: account, Messages: json.RawMessage(`[]`), Tools: json.RawMessage(`[]`),
		MaxTokens: 512, DeadlineMs: 30000, ContextBytes: 10,
	})
	if secret != "" {
		appendEnv(domain.EventKindModelResponse, domain.ModelResponsePayload{
			RouteID: route, AccountID: account, FinishReason: "stop",
			Text: "Authorization: Bearer " + secret + "0123456789",
		})
	}
	appendEnv(domain.EventKindModelError, domain.ModelErrorPayload{
		RouteID: route, AccountID: account, ErrorClass: domain.FailureNetworkTimeout,
		ErrorMessage: "upstream timeout after 30s", Retryable: true, StatusCode: 504,
	})
	appendEnv(domain.EventKindInputAccepted, domain.InputAcceptedPayload{Action: "cancel"})
	appendEnv(domain.EventKindInputAccepted, domain.InputAcceptedPayload{Action: "resize", Resize: &domain.TermSize{Cols: 120, Rows: 40}})
	if complete {
		appendEnv(domain.EventKindSessionEnd, domain.SessionEndPayload{Reason: "exit", TurnsCount: 3, DurationMs: 1234})
	}

	events := newMemoryEvents()
	retrieval := &memoryRetrieval{}
	var records []domain.EventRecord
	for _, env := range envs {
		rec := recordOf(env)
		records = append(records, rec)
		events.add(session, rec)
		retrieval.add(user, rec)
	}

	return &fixture{
		session:   session,
		user:      user,
		events:    events,
		retrieval: retrieval,
		content:   content,
		clock:     &fixedClock{now: 1_700_000_000_000},
		random:    &sequenceRandom{},
		records:   records,
		frames:    [][]byte{frame1, frame2},
		complete:  complete,
	}
}

func (f *fixture) service(t interface {
	Fatalf(string, ...any)
}, opts Options) *Service {
	svc, err := NewService(f.events, f.retrieval, f.content, f.clock, f.random, opts)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}
