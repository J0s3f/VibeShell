package sqlite

import (
	"context"
	"errors"
	"sync"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// testEventsBatched returns an event store with a small batch bound so tests
// exercise the group-commit path rather than the idle single-event path.
func testEventsBatched(t *testing.T, maxBatch int) (*DB, *Events) {
	t.Helper()
	db := testDB(t, DefaultOptions())
	evts, err := NewEvents(db.SQL(), EventsOptions{MaxBatch: maxBatch})
	if err != nil {
		t.Fatalf("NewEvents: %v", err)
	}
	t.Cleanup(evts.Close)
	return db, evts
}

// TestGroupCommitPreservesOrderingAndTiming drives many concurrent appends for
// one session through a batch bound larger than the burst. Every event must
// receive a distinct, contiguous sequence in arrival order and keep the
// timestamp its caller set; batching must not collapse or renumber them.
func TestGroupCommitPreservesOrderingAndTiming(t *testing.T) {
	_, evts := testEventsBatched(t, 64)
	ctx := context.Background()
	session := testSessionID(t)
	payload := inlineJSON(t, domain.ShutdownPayload{})

	const n = 200
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		bySeq = make(map[uint64]int64)
	)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ts := int64(1000 + i)
			rec, err := evts.Append(ctx, makeEnvelope(session, domain.EventKindShutdown, payload, ts))
			if err != nil {
				errs <- err
				return
			}
			mu.Lock()
			bySeq[rec.Envelope.Sequence] = rec.Envelope.Timestamp
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(bySeq) != n {
		t.Fatalf("distinct sequences = %d, want %d", len(bySeq), n)
	}
	timestamps := make(map[int64]bool, n)
	for seq := uint64(1); seq <= n; seq++ {
		ts, ok := bySeq[seq]
		if !ok {
			t.Fatalf("missing sequence %d: group commit skipped or renumbered events", seq)
		}
		if timestamps[ts] {
			t.Fatalf("timestamp %d appeared on more than one sequence", ts)
		}
		timestamps[ts] = true
	}
	// List returns the durable events ordered by sequence, not timestamp.
	list, err := evts.List(ctx, session, 1, n)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != n {
		t.Fatalf("listed %d events, want %d", len(list), n)
	}
	for i, rec := range list {
		if rec.Envelope.Sequence != uint64(i+1) {
			t.Fatalf("list[%d] sequence = %d, want %d", i, rec.Envelope.Sequence, i+1)
		}
	}
}

// TestGroupCommitAcknowledgedEventIsDurableAcrossReopen appends a burst that
// the writer groups into batches, then reopens the database and checks every
// acknowledged event was recorded exactly once with its payload. Group commit
// must not trade away the durability of an acknowledged append.
func TestGroupCommitAcknowledgedEventIsDurableAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/batched.db"
	db, err := Open(path, DefaultOptions())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	evts, err := NewEvents(db.SQL(), EventsOptions{MaxBatch: 8})
	if err != nil {
		t.Fatal(err)
	}
	session := testSessionID(t)
	payload := inlineJSON(t, domain.ShutdownPayload{Reason: "restart", Graceful: true})

	const n = 40
	ids := make([]domain.EventID, n)
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
			ids[i] = rec.Envelope.EventID
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
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
	evts2, err := NewEvents(reopened.SQL(), EventsOptions{MaxBatch: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer evts2.Close()

	seen := make(map[domain.EventID]bool, n)
	for _, id := range ids {
		if id.IsZero() {
			t.Fatal("an acknowledged append returned a zero event id")
		}
		got, err := evts2.GetByID(ctx, id)
		if err != nil {
			t.Fatalf("GetByID after reopen: %v", err)
		}
		if got.Envelope.Sequence == 0 || got.Envelope.Kind != domain.EventKindShutdown {
			t.Fatalf("reopened record = seq %d kind %q", got.Envelope.Sequence, got.Envelope.Kind)
		}
		if seen[got.Envelope.EventID] {
			t.Fatalf("event %s recorded twice", got.Envelope.EventID)
		}
		seen[got.Envelope.EventID] = true
	}
	if len(seen) != n {
		t.Fatalf("durable events = %d, want %d", len(seen), n)
	}
}

// TestGroupCommitBatchErrorIsIsolated proves one bad append cannot fail the
// others sharing its transaction. A caller-declared sequence gap is rejected by
// the store; the unrelated appends grouped with it must still commit.
func TestGroupCommitBatchErrorIsIsolated(t *testing.T) {
	_, evts := testEventsBatched(t, 64)
	ctx := context.Background()
	session := testSessionID(t)
	payload := inlineJSON(t, domain.ShutdownPayload{})

	// A caller-provided sequence that cannot match the next one fails
	// appendRecord. Force it into the same batch as good appends by submitting
	// all of them while the writer is busy: the bound is 64 and only one event
	// is bad.
	bad := makeEnvelope(session, domain.EventKindShutdown, payload, 1)
	bad.Sequence = 99

	outcomes := make(chan error, 3)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := evts.Append(ctx, bad)
		outcomes <- err
	}()
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := evts.Append(ctx, makeEnvelope(session, domain.EventKindShutdown, payload, int64(10+i))); err != nil {
				outcomes <- err
				return
			}
			outcomes <- nil
		}(i)
	}
	wg.Wait()
	close(outcomes)
	var sawConflict bool
	var good int
	for err := range outcomes {
		if err == nil {
			good++
			continue
		}
		var de *domain.DomainError
		if errors.As(err, &de) && de.Category == domain.CategoryConflict {
			sawConflict = true
			continue
		}
		t.Fatalf("unexpected error: %v", err)
	}
	if !sawConflict {
		t.Fatal("the sequence-gap append did not report a conflict")
	}
	if good != 2 {
		t.Fatalf("good appends committed = %d, want 2; one bad append failed its batch", good)
	}
	list, err := evts.List(ctx, session, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("durable events = %d, want 2 (the bad append must not persist)", len(list))
	}
}

// TestGroupCommitQueueStaysBounded confirms group commit does not change the
// backpressure contract: a saturated queue with an exhausted caller budget is
// still rejected rather than waiting without bound.
func TestGroupCommitQueueStaysBounded(t *testing.T) {
	_, evts := testEventsBatched(t, 64)
	if got := evts.Stats().Capacity; got != 256 {
		t.Fatalf("default queue capacity = %d, want 256", got)
	}
	w := &eventsWriter{db: evts.sql, queue: make(chan appendJob, 2), closed: make(chan struct{}), maxBatch: 64}
	defer close(w.closed)
	for i := 0; i < 2; i++ {
		if err := w.submit(context.Background(), appendJob{}); err != nil {
			t.Fatalf("fill queue: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.submit(ctx, appendJob{}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("saturated submit err = %v, want ErrQueueFull", err)
	}
	stats := w.stats()
	if stats.Rejected != 1 || stats.Depth != 2 || stats.MaxDepth != 2 {
		t.Fatalf("stats = %+v, want one rejection at bounded depth 2", stats)
	}
}
