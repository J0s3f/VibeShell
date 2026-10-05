// Package integration holds VibeShell's failure and research acceptance suite
// (PLAN 15.5 D05). It exercises the real routing, admin, export, apps, and
// SQLite storage code against deterministic provider doubles; no test makes a
// live model call.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"j0s.at/vibeshell/internal/adapters/sqlite"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/routing"
)

// ---------------------------------------------------------------------------
// Identity fixtures
// ---------------------------------------------------------------------------

// crockford builds a regex-valid identity value for a small integer so route,
// account, session, and turn fixtures stay readable and deterministic.
func crockford(n int) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	v := make([]byte, 26)
	for i := range v {
		v[i] = alphabet[0]
	}
	for i := 25; i >= 0 && n > 0; i-- {
		v[i] = alphabet[n%32]
		n /= 32
	}
	return string(v)
}

func mustParse[T any](t *testing.T, parse func(string) (T, error), s string) T {
	t.Helper()
	v, err := parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

func route(t *testing.T, n int) domain.RouteID {
	return mustParse(t, domain.ParseRouteID, domain.PrefixRoute+"_"+crockford(n))
}

func account(t *testing.T, n int) domain.AccountID {
	return mustParse(t, domain.ParseAccountID, domain.PrefixAccount+"_"+crockford(n))
}

func sessionID(t *testing.T, n int) domain.SessionID {
	return mustParse(t, domain.ParseSessionID, domain.PrefixSession+"_"+crockford(n))
}

func turnID(t *testing.T, n int) domain.TurnID {
	return mustParse(t, domain.ParseTurnID, domain.PrefixTurn+"_"+crockford(n))
}

func userID(t *testing.T, n int) domain.UserID {
	return mustParse(t, domain.ParseUserID, domain.PrefixUser+"_"+crockford(n))
}

// ---------------------------------------------------------------------------
// Deterministic ports
// ---------------------------------------------------------------------------

// testClock is an injected wall clock; policies read explicit timestamps so a
// failure test never sleeps.
type testClock struct {
	mu  sync.Mutex
	now int64
}

func newTestClock(now int64) *testClock { return &testClock{now: now} }

func (c *testClock) NowUnixMilli() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) MonotonicNanos() int64 { return c.NowUnixMilli() * 1_000_000 }

func (c *testClock) advance(ms int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now += ms
}

// seededRandom is a deterministic ports.Random so route/account selection is
// reproducible across runs.
type seededRandom struct{ r *rand.Rand }

func newSeededRandom(seed int64) *seededRandom {
	return &seededRandom{r: rand.New(rand.NewSource(seed))}
}

func (s *seededRandom) Bytes(n int) ([]byte, error) {
	if n <= 0 {
		return nil, fmt.Errorf("random byte count must be positive")
	}
	b := make([]byte, n)
	_, err := s.r.Read(b)
	return b, err
}

func (s *seededRandom) Intn(n int) int {
	if n <= 0 {
		return 0
	}
	return s.r.Intn(n)
}

// ---------------------------------------------------------------------------
// Model gateway double
// ---------------------------------------------------------------------------

// fakeGateway is the ports.ModelGateway double. The Handler decides the
// outcome of each request; every request is recorded for later assertions. No
// network call is ever made.
type fakeGateway struct {
	mu      sync.Mutex
	handler func(ctx context.Context, req domain.ModelRequest) (domain.ModelResponse, error)
	calls   []domain.ModelRequest
}

func (g *fakeGateway) Request(ctx context.Context, req domain.ModelRequest) (domain.ModelResponse, error) {
	g.mu.Lock()
	g.calls = append(g.calls, req)
	handler := g.handler
	g.mu.Unlock()
	if handler == nil {
		return successResponse(req), nil
	}
	return handler(ctx, req)
}

func (g *fakeGateway) requests() []domain.ModelRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]domain.ModelRequest(nil), g.calls...)
}

func (g *fakeGateway) callCount() int { return len(g.requests()) }

func successResponse(req domain.ModelRequest) domain.ModelResponse {
	return domain.ModelResponse{
		RequestID:    req.RequestID,
		RouteID:      req.RouteID,
		AccountID:    req.AccountID,
		Message:      domain.Message{Role: domain.RoleAssistant, Content: "ok"},
		FinishReason: domain.FinishReasonStop,
		Usage:        domain.Usage{TotalTokens: 3},
	}
}

// failure returns an ErrorEnvelope as an error, the shape a real gateway
// adapter produces for a classified provider failure.
func failure(class domain.FailureClass, req domain.ModelRequest, retryAfter *int64) error {
	env := domain.NewErrorEnvelope(class, string(class), req.RouteID, req.AccountID, time.Now().UnixMilli())
	env.RetryAfter = retryAfter
	return env
}

// ---------------------------------------------------------------------------
// Routing health store double
// ---------------------------------------------------------------------------

// memoryHealth is the routing.HealthStore double. It mirrors the adapter's
// Get/Save/List contract so health transitions are asserted directly.
type memoryHealth struct {
	mu sync.Mutex
	m  map[string]domain.HealthRecord
}

func newMemoryHealth() *memoryHealth { return &memoryHealth{m: map[string]domain.HealthRecord{}} }

func healthKey(rec domain.HealthRecord) domain.HealthKey {
	key := domain.HealthKey{RouteID: rec.RouteID, Scope: rec.Scope}
	if rec.AccountID != nil {
		key.AccountID = *rec.AccountID
	}
	return key
}

func (s *memoryHealth) Get(_ context.Context, key domain.HealthKey) (domain.HealthRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.m[key.String()]
	return rec, ok, nil
}

func (s *memoryHealth) Save(_ context.Context, rec domain.HealthRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[healthKey(rec).String()] = rec
	return nil
}

func (s *memoryHealth) List(_ context.Context) ([]domain.HealthRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.HealthRecord, 0, len(s.m))
	for _, rec := range s.m {
		out = append(out, rec)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Spending ledger double
// ---------------------------------------------------------------------------

type memoryLedger struct {
	mu        sync.Mutex
	reserved  []float64
	settled   []float64
	released  int
	reserveOK bool
}

func (l *memoryLedger) Reserve(_ context.Context, _ routing.SpendingRef, amount float64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.reserveOK {
		return domain.NewLimitError(domain.CodeQuotaExceeded, "spending limit exceeded", nil)
	}
	l.reserved = append(l.reserved, amount)
	return nil
}

func (l *memoryLedger) Reconcile(_ context.Context, _ routing.SpendingRef, actual float64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.settled = append(l.settled, actual)
	return nil
}

func (l *memoryLedger) Release(_ context.Context, _ routing.SpendingRef) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.released++
	return nil
}

// ---------------------------------------------------------------------------
// Real SQLite storage fixtures
// ---------------------------------------------------------------------------

// testStore bundles the real SQLite adapter behind its ports plus the
// research/retrieval/recovery/admin-facing services. It is the durable half of
// the integration suite; providers are always doubles.
type testStore struct {
	db       *sqlite.DB
	events   *sqlite.Events
	recorder *sqlite.Recovery
	dir      string
}

// openStore creates a file-backed SQLite database (WAL, synchronous=FULL) in a
// fresh per-test directory and wires the event and recovery adapters.
func openStore(t *testing.T) *testStore {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlite.Open(dir+"/vibeshell.db", sqlite.DefaultOptions())
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	events, err := sqlite.NewEvents(db.SQL(), sqlite.EventsOptions{})
	if err != nil {
		t.Fatalf("sqlite.NewEvents: %v", err)
	}
	t.Cleanup(events.Close)
	return &testStore{
		db:       db,
		events:   events,
		recorder: sqlite.NewRecovery(db, events, newTestClock(1_700_000_000_000)),
		dir:      dir,
	}
}

// namespace ensures a user's personal namespace exists.
func (s *testStore) namespace(t *testing.T, user domain.UserID) domain.Namespace {
	t.Helper()
	ns, err := s.db.EnsureNamespace(context.Background(), domain.NamespaceForUser(user, "user:test"))
	if err != nil {
		t.Fatalf("EnsureNamespace: %v", err)
	}
	return ns
}

// putContent stores exact bytes and returns their content reference.
func (s *testStore) putContent(t *testing.T, data []byte, media string) domain.ContentRef {
	t.Helper()
	ref, err := s.db.Put(context.Background(), data, media)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return ref
}

// appendEvent appends one inline event through the real writer.
func (s *testStore) appendEvent(t *testing.T, session domain.SessionID, kind domain.EventKind, payload any, ts int64) domain.EventRecord {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	env := domain.NewEventEnvelope(session, 0, kind, domain.Provenance{Source: "test"}, ts, ts*1_000_000)
	env.Payload = domain.PayloadRef{Inline: raw}
	rec, err := s.events.Append(context.Background(), env)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return rec
}

// commitFile creates one regular file at path in ns with exact content.
func (s *testStore) commitFile(t *testing.T, ns domain.NamespaceID, p domain.ValidPath, ref domain.ContentRef) domain.Node {
	t.Helper()
	ctx := context.Background()
	meta := domain.NewNodeMetadata(0o644, 1000, 1000, 1_700_000_000_000)
	cs := domain.EmptyChangeSet(turnID(t, 900), mustParse(t, domain.ParseAttemptID, domain.PrefixAttempt+"_"+crockford(900)), 1_700_000_000_000)
	cs.AddCreate(ns, p, domain.NodeKindFile, meta, ref)
	if _, err := s.db.Commit(ctx, cs, domain.DefaultScopePolicy()); err != nil {
		t.Fatalf("Commit(%s): %v", p, err)
	}
	node, err := s.db.LookupPath(ctx, ns, p)
	if err != nil {
		t.Fatalf("LookupPath(%s): %v", p, err)
	}
	return node
}
