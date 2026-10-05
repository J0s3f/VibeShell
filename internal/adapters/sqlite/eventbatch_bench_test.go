package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// BenchmarkAppendDurable measures the full acknowledged-append cost through the
// writer queue against a file-backed database, which is where synchronous=FULL
// actually pays for a commit. A ":memory:" handle would skip the fsync and
// measure the wrong thing.
func BenchmarkAppendDurable(b *testing.B) {
	evts := benchEvents(b)
	ctx := context.Background()
	session := benchSessionID(b)
	payload, _ := json.Marshal(domain.ShutdownPayload{})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := evts.Append(ctx, makeEnvelope(session, domain.EventKindShutdown, payload, int64(i))); err != nil {
			b.Fatalf("Append: %v", err)
		}
	}
}

// BenchmarkAppendConcurrent drives concurrent acknowledged appends, the shape
// the load harness measures: many sessions arriving at the single writer at
// once. ns/op is the per-event acknowledged latency, so a group commit that
// amortizes one fsync across a batch lowers it.
func BenchmarkAppendConcurrent(b *testing.B) {
	evts := benchEvents(b)
	ctx := context.Background()
	session := benchSessionID(b)
	payload, _ := json.Marshal(domain.ShutdownPayload{})

	const workers = 20
	b.ResetTimer()
	var wg sync.WaitGroup
	per := b.N / workers
	if per == 0 {
		per = 1
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if _, err := evts.Append(ctx, makeEnvelope(session, domain.EventKindShutdown, payload, int64(worker*per+i))); err != nil {
					b.Errorf("Append: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
}

func benchEvents(b *testing.B) *Events {
	b.Helper()
	db, err := Open(filepath.Join(b.TempDir(), "bench.db"), DefaultOptions())
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })
	evts, err := NewEvents(db.SQL(), EventsOptions{})
	if err != nil {
		b.Fatalf("NewEvents: %v", err)
	}
	b.Cleanup(evts.Close)
	return evts
}

func benchSessionID(b *testing.B) domain.SessionID {
	b.Helper()
	raw, err := newTestPrefixID(domain.PrefixSession)
	if err != nil {
		b.Fatal(err)
	}
	id, err := domain.ParseSessionID(raw)
	if err != nil {
		b.Fatal(err)
	}
	return id
}
