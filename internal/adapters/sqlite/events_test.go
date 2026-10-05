package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

func testSessionID(t *testing.T) domain.SessionID {
	t.Helper()
	raw, err := newTestPrefixID(domain.PrefixSession)
	if err != nil {
		t.Fatal(err)
	}
	id, err := domain.ParseSessionID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func inlineJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func makeEnvelope(session domain.SessionID, kind domain.EventKind, payload json.RawMessage, ts int64) domain.EventEnvelope {
	return domain.EventEnvelope{
		SchemaVersion: domain.EventSchemaVersion,
		SessionID:     session,
		Timestamp:     ts,
		Kind:          kind,
		Payload:       domain.PayloadRef{Inline: payload},
		Provenance:    domain.Provenance{Source: "test"},
	}
}

func testEvents(t *testing.T) (*DB, *Events) {
	t.Helper()
	db := testDB(t, DefaultOptions())
	evts, err := NewEvents(db.SQL(), EventsOptions{})
	if err != nil {
		t.Fatalf("NewEvents: %v", err)
	}
	t.Cleanup(evts.Close)
	return db, evts
}

func TestAppendAssignsSequenceAndID(t *testing.T) {
	_, evts := testEvents(t)
	ctx := context.Background()
	session := testSessionID(t)
	payload := inlineJSON(t, domain.ShutdownPayload{Reason: "signal", Graceful: true})

	for wantSeq := uint64(1); wantSeq <= 3; wantSeq++ {
		rec, err := evts.Append(ctx, makeEnvelope(session, domain.EventKindShutdown, payload, 1000+int64(wantSeq)))
		if err != nil {
			t.Fatalf("Append %d: %v", wantSeq, err)
		}
		if rec.Envelope.Sequence != wantSeq {
			t.Errorf("sequence = %d, want %d", rec.Envelope.Sequence, wantSeq)
		}
		if rec.Envelope.EventID.IsZero() {
			t.Error("EventID not assigned")
		}
		got, err := evts.GetByID(ctx, rec.Envelope.EventID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if got.Envelope.Kind != domain.EventKindShutdown {
			t.Errorf("kind = %q", got.Envelope.Kind)
		}
		if string(got.Payload) == "" {
			t.Error("payload lost")
		}
	}
}

func TestAppendSequencesAreIndependentPerSession(t *testing.T) {
	_, evts := testEvents(t)
	ctx := context.Background()
	payload := inlineJSON(t, domain.ShutdownPayload{})
	for _, session := range []domain.SessionID{testSessionID(t), testSessionID(t)} {
		rec, err := evts.Append(ctx, makeEnvelope(session, domain.EventKindShutdown, payload, 1))
		if err != nil {
			t.Fatal(err)
		}
		if rec.Envelope.Sequence != 1 {
			t.Errorf("first sequence for session = %d, want 1", rec.Envelope.Sequence)
		}
	}
}

func TestAppendRejectsSequenceGap(t *testing.T) {
	_, evts := testEvents(t)
	ctx := context.Background()
	session := testSessionID(t)
	payload := inlineJSON(t, domain.ShutdownPayload{})
	env := makeEnvelope(session, domain.EventKindShutdown, payload, 1)
	env.Sequence = 7
	_, err := evts.Append(ctx, env)
	var de *domain.DomainError
	if !errors.As(err, &de) || de.Category != domain.CategoryConflict {
		t.Fatalf("err = %v, want conflict DomainError", err)
	}
}

func TestAppendValidation(t *testing.T) {
	_, evts := testEvents(t)
	ctx := context.Background()
	session := testSessionID(t)
	payload := inlineJSON(t, domain.ShutdownPayload{})

	noSession := makeEnvelope(domain.SessionID{}, domain.EventKindShutdown, payload, 1)
	if _, err := evts.Append(ctx, noSession); err == nil {
		t.Error("expected validation error for missing session")
	}
	badKind := makeEnvelope(session, "not.a.kind", payload, 1)
	if _, err := evts.Append(ctx, badKind); err == nil {
		t.Error("expected validation error for unknown kind")
	}
	empty := makeEnvelope(session, domain.EventKindShutdown, nil, 1)
	if _, err := evts.Append(ctx, empty); err == nil {
		t.Error("expected validation error for empty payload")
	}
	big := makeEnvelope(session, domain.EventKindShutdown, json.RawMessage(`"`+string(make([]byte, 2000))+`"`), 1)
	if _, err := evts.Append(ctx, big); err == nil {
		t.Error("expected limit error for oversized inline payload")
	}
}

func TestContentRefPayloadRoundTrip(t *testing.T) {
	_, evts := testEvents(t)
	ctx := context.Background()
	session := testSessionID(t)
	raw, err := newTestPrefixID(domain.PrefixContent)
	if err != nil {
		t.Fatal(err)
	}
	contentID, err := domain.ParseContentID(raw)
	if err != nil {
		t.Fatal(err)
	}
	env := makeEnvelope(session, domain.EventKindTerminalFrame, nil, 1)
	env.Payload = domain.PayloadRef{ContentID: &contentID}
	rec, err := evts.Append(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	got, err := evts.GetByID(ctx, rec.Envelope.EventID)
	if err != nil {
		t.Fatal(err)
	}
	var ref map[string]string
	if err := json.Unmarshal(got.Payload, &ref); err != nil {
		t.Fatalf("payload ref JSON: %v", err)
	}
	if ref["content_id"] != contentID.String() {
		t.Errorf("content_id = %q", ref["content_id"])
	}
}

func TestListOrdersBySequenceNotTimestamp(t *testing.T) {
	_, evts := testEvents(t)
	ctx := context.Background()
	session := testSessionID(t)
	payload := inlineJSON(t, domain.ShutdownPayload{})
	// Timestamps arrive out of order; sequence is the ordering authority.
	for _, ts := range []int64{3000, 1000, 2000} {
		if _, err := evts.Append(ctx, makeEnvelope(session, domain.EventKindShutdown, payload, ts)); err != nil {
			t.Fatal(err)
		}
	}
	list, err := evts.List(ctx, session, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	for i, rec := range list {
		if rec.Envelope.Sequence != uint64(i+1) {
			t.Fatalf("list order by timestamp would break: got seq %d at index %d", rec.Envelope.Sequence, i)
		}
	}
	tail, err := evts.List(ctx, session, 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 1 || tail[0].Envelope.Sequence != 3 {
		t.Fatalf("tail = %+v", tail)
	}
}

func TestAppendAfterCloseIsRejected(t *testing.T) {
	db := testDB(t, DefaultOptions())
	evts, err := NewEvents(db.SQL(), EventsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	evts.Close()
	session := testSessionID(t)
	payload := inlineJSON(t, domain.ShutdownPayload{})
	_, err = evts.Append(context.Background(), makeEnvelope(session, domain.EventKindShutdown, payload, 1))
	if !errors.Is(err, ErrWriterStopped) {
		t.Fatalf("err = %v, want ErrWriterStopped", err)
	}
}

func TestWriterQueueBackpressureRejectsSaturatedAppend(t *testing.T) {
	_, evts := testEvents(t)
	if got := evts.Stats().Capacity; got != 256 {
		t.Fatalf("default queue capacity = %d, want 256", got)
	}
	// A saturated queue with an exhausted caller budget must reject rather
	// than block forever or grow without bound; the event is not persisted.
	w := &eventsWriter{db: evts.sql, queue: make(chan appendJob, 1), closed: make(chan struct{}), maxBatch: 64}
	defer close(w.closed)
	if err := w.submit(context.Background(), appendJob{}); err != nil {
		t.Fatalf("fill queue: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.submit(ctx, appendJob{}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("saturated submit err = %v, want ErrQueueFull", err)
	}
	stats := w.stats()
	if stats.Rejected != 1 || stats.Depth != 1 || stats.MaxDepth != 1 {
		t.Fatalf("stats = %+v, want one rejection at bounded depth 1", stats)
	}
}

func TestAppendIsDurableAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/events.db"
	ctx := context.Background()
	db, err := Open(path, DefaultOptions())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	evts, err := NewEvents(db.SQL(), EventsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	session := testSessionID(t)
	payload := inlineJSON(t, domain.ShutdownPayload{Reason: "restart", Graceful: true})
	rec, err := evts.Append(ctx, makeEnvelope(session, domain.EventKindShutdown, payload, 1000))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	evts.Close()
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(path, DefaultOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	// Migrations must be idempotent across reopen.
	if pending, _, err := NeedsMigration(ctx, reopened.SQL()); err != nil || pending {
		t.Fatalf("NeedsMigration after reopen = (%v, %v), want (false, nil)", pending, err)
	}
	evts2, err := NewEvents(reopened.SQL(), EventsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer evts2.Close()
	got, err := evts2.GetByID(ctx, rec.Envelope.EventID)
	if err != nil {
		t.Fatalf("GetByID after reopen: %v", err)
	}
	if got.Envelope.Sequence != 1 || got.Envelope.Kind != domain.EventKindShutdown {
		t.Fatalf("reopened record = seq %d kind %q", got.Envelope.Sequence, got.Envelope.Kind)
	}
	list, err := evts2.List(ctx, session, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Envelope.EventID != rec.Envelope.EventID {
		t.Fatalf("list after reopen = %+v", list)
	}
}

func TestConcurrentAppendsGetUniqueSequences(t *testing.T) {
	_, evts := testEvents(t)
	ctx := context.Background()
	session := testSessionID(t)
	payload := inlineJSON(t, domain.ShutdownPayload{})
	const n = 32
	seen := make(map[uint64]bool)
	var mu sync.Mutex
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec, err := evts.Append(ctx, makeEnvelope(session, domain.EventKindShutdown, payload, int64(1000+i)))
			if err != nil {
				errs <- err
				return
			}
			mu.Lock()
			if seen[rec.Envelope.Sequence] {
				errs <- fmt.Errorf("duplicate sequence %d", rec.Envelope.Sequence)
			}
			seen[rec.Envelope.Sequence] = true
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(seen) != n {
		t.Fatalf("distinct sequences = %d, want %d", len(seen), n)
	}
	stats := evts.Stats()
	if stats.Completed != n || stats.Failures != 0 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestMigrationRegisteredAndApplied(t *testing.T) {
	db := testDB(t, DefaultOptions())
	ctx := context.Background()
	pending, latest, err := NeedsMigration(ctx, db.SQL())
	if err != nil {
		t.Fatal(err)
	}
	if pending || latest != CurrentSchemaVersion() {
		t.Errorf("pending=%v latest=%d want none, %d", pending, latest, CurrentSchemaVersion())
	}
	if CurrentSchemaVersion() < 2 {
		t.Fatalf("events migration not registered: %d", CurrentSchemaVersion())
	}
}
