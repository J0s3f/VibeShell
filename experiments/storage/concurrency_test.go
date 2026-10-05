package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// latencyRecorder collects durations for a quantile report. Samples are appended
// under a mutex, which is cheap next to a query that has to reach a disk.
type latencyRecorder struct {
	mu        sync.Mutex
	samples   []time.Duration
	overMilli int64
}

func (r *latencyRecorder) add(d time.Duration) {
	r.mu.Lock()
	r.samples = append(r.samples, d)
	r.mu.Unlock()
}

// quantile returns the nearest-rank quantile of the recorded samples. A caller
// that recorded no samples gets zero rather than a panic, because a gate that
// failed to run should say so rather than crash while reporting.
func (r *latencyRecorder) quantile(q float64) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.samples) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), r.samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(math.Ceil(q*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func (r *latencyRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.samples)
}

func (r *latencyRecorder) addTo(receipt *report, label string) {
	receipt.add(label+" samples", r.count())
	receipt.add(label+" p50", r.quantile(0.50))
	receipt.add(label+" p95", r.quantile(0.95))
	receipt.add(label+" p99", r.quantile(0.99))
	receipt.add(label+" max", r.quantile(1.0))
}

// TestSQLiteAdmitsOnlyOneWriter qualifies the single-writer half of gate 4: a
// second connection cannot write while a transaction holds the write lock, and
// the refusal is a distinguishable driver error rather than a generic string.
func TestSQLiteAdmitsOnlyOneWriter(t *testing.T) {
	path := NewDatabasePath(t, "single-writer")

	// busy_timeout=0 makes the refusal immediate, so the test observes SQLite's
	// own answer instead of the wait policy.
	holder, err := OpenWriter(path, NoWait())
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	defer holder.Close()
	contender, err := OpenWriter(path, NoWait())
	if err != nil {
		t.Fatalf("open contender: %v", err)
	}
	defer contender.Close()

	if _, err := holder.Exec(Schema); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	receipt := newReport("Gate 4a: one writer at a time")
	receipt.add("captured (UTC)", time.Now().UTC().Format(time.RFC3339))
	receipt.add("journal_mode", queryString(t, holder, `PRAGMA journal_mode`))

	tx, err := holder.Begin()
	if err != nil {
		t.Fatalf("begin holder transaction: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO namespaces (id, name) VALUES ('held', 'held')`); err != nil {
		t.Fatalf("holder write: %v", err)
	}

	_, err = contender.Exec(`INSERT INTO namespaces (id, name) VALUES ('blocked', 'blocked')`)
	if err == nil {
		t.Fatal("a second writer succeeded while the write lock was held")
	}
	receipt.add("contender refused", describeErr(err))

	// The refusal must be identifiable as SQLITE_BUSY, because storage failure
	// is a first-class condition that the service reports and reacts to.
	var driverErr *sqlite.Error
	if !errors.As(err, &driverErr) {
		t.Fatalf("contender error is %T, want *sqlite.Error", err)
	}
	code := driverErr.Code()
	receipt.add("driver error type", fmt.Sprintf("%T", err))
	receipt.add("driver error code", code)
	receipt.add("SQLITE_BUSY", sqlite3.SQLITE_BUSY)
	receipt.add("SQLITE_LOCKED", sqlite3.SQLITE_LOCKED)
	if code != sqlite3.SQLITE_BUSY && code != sqlite3.SQLITE_LOCKED {
		t.Errorf("contender error code is %d, want SQLITE_BUSY or SQLITE_LOCKED", code)
	}

	// After the holder releases the lock the contender succeeds.
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback holder: %v", err)
	}
	if _, err := contender.Exec(`INSERT INTO namespaces (id, name) VALUES ('after', 'after')`); err != nil {
		t.Fatalf("contender write after the lock was released: %v", err)
	}
	receipt.add("contender accepted after release", true)

	receipt.write(t, "gate4-single-writer-"+RunLabel())
}

// TestLongWriteTransactionStarvesWriters is the measurement behind the rule that
// database transactions stay short: a transaction that holds the write lock for
// a fixed delay delays every other writer by at least that long.
func TestLongWriteTransactionStarvesWriters(t *testing.T) {
	const holdDelay = 250 * time.Millisecond

	path := NewDatabasePath(t, "long-writer")
	holder, err := OpenWriter(path, NoWait())
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	defer holder.Close()
	// A patient contender: it waits rather than failing, which is what shows the
	// cost landing on the caller.
	patient, err := OpenWriter(path, PatientWait(5000))
	if err != nil {
		t.Fatalf("open patient contender: %v", err)
	}
	defer patient.Close()

	if _, err := holder.Exec(Schema); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	seedWorld(t, holder, "ns-long", "root-long")

	receipt := newReport("Gate 4b: cost of holding the write lock")
	receipt.add("held for", holdDelay)

	tx, err := holder.Begin()
	if err != nil {
		t.Fatalf("begin holder transaction: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO namespaces (id, name) VALUES ('a', 'a')`); err != nil {
		t.Fatalf("holder write: %v", err)
	}

	blocked := make(chan time.Duration, 1)
	go func() {
		started := time.Now()
		_, err := patient.Exec(`INSERT INTO namespaces (id, name) VALUES ('b', 'b')`)
		elapsed := time.Since(started)
		if err != nil {
			t.Errorf("patient write failed: %v", err)
		}
		blocked <- elapsed
	}()

	time.Sleep(20 * time.Millisecond)
	time.Sleep(holdDelay)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit holder: %v", err)
	}

	select {
	case elapsed := <-blocked:
		receipt.add("patient write waited", elapsed)
		if elapsed < holdDelay {
			t.Errorf("the waiting writer waited %v, expected at least the %v the lock was held", elapsed, holdDelay)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting writer never completed")
	}

	receipt.add("conclusion", "every second of lock holding is a second of delay for all other writers, so a turn must not hold a transaction across model work")
	receipt.write(t, "gate4-long-writer-"+RunLabel())
}

// TestConcurrentReadersBesideOneWriter measures the shape the plan calls for:
// many readers beside one writer, all under the race detector.
func TestConcurrentReadersBesideOneWriter(t *testing.T) {
	const (
		readerCount = 8
		commits     = 120
		writers     = 4
		// seededNodeCount is the rows seedNodes writes under dir-home, on top of
		// the namespace root and the home directory.
		seededNodeCount = 40
	)

	path := NewDatabasePath(t, "concurrency")
	writerDB, err := OpenWriter(path, Options{})
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer writerDB.Close()
	readerDB, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer readerDB.Close()

	if _, err := writerDB.Exec(Schema); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	seedWorld(t, writerDB, "ns-conc", "root-conc")
	insertDirectory(t, writerDB, "dir-home", "root-conc", "home")
	seedNodes(t, writerDB, "dir-home", seededNodeCount)

	queue := NewWriterQueue(writerDB, 8)
	defer queue.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Readers run for as long as the writer does, observing that the committed
	// revision only ever moves forward.
	stopReaders := make(chan struct{})
	var readersDone sync.WaitGroup
	var readerLatency latencyRecorder
	var readerFailures atomic.Int64
	var highestRevision atomic.Int64
	highestRevision.Store(1)

	for r := 0; r < readerCount; r++ {
		readersDone.Add(1)
		go func() {
			defer readersDone.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
				}
				started := time.Now()
				var revision int64
				err := readerDB.QueryRowContext(ctx, `SELECT revision FROM nodes WHERE id = 'dir-home'`).Scan(&revision)
				readerLatency.add(time.Since(started))
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					readerFailures.Add(1)
					t.Errorf("reader query: %v", err)
					return
				}
				for {
					seen := highestRevision.Load()
					if revision <= seen || highestRevision.CompareAndSwap(seen, revision) {
						break
					}
				}
			}
		}()
	}

	// A bounded number of goroutines submit commits through the queue, so the
	// measured throughput is the queue's, not the number of submitters'.
	perWriter := commits / writers
	var writersDone sync.WaitGroup
	var writerLatency latencyRecorder
	var submitted, refused atomic.Int64
	started := time.Now()
	for w := 0; w < writers; w++ {
		writersDone.Add(1)
		go func(worker int) {
			defer writersDone.Done()
			for i := 0; i < perWriter; i++ {
				seq := worker*perWriter + i
				name := fmt.Sprintf("node-%04d", seq)
				submitStarted := time.Now()
				err := queue.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx,
						`INSERT INTO nodes (id, namespace_id, parent_id, name, kind, revision)
						 SELECT ?, namespace_id, id, ?, 'file', 1 FROM nodes WHERE id = ?`,
						name, name, "dir-home")
					return err
				})
				writerLatency.add(time.Since(submitStarted))
				switch {
				case err == nil:
					submitted.Add(1)
				case errors.Is(err, ErrBackpressure):
					refused.Add(1)
				default:
					t.Errorf("submit %d: %v", seq, err)
					return
				}
			}
		}(w)
	}
	writersDone.Wait()
	elapsed := time.Since(started)
	close(stopReaders)
	readersDone.Wait()

	// Drain the queue so that the row count can be compared with the submissions.
	queue.Close()

	stats := queue.Stats()
	receipt := newReport(fmt.Sprintf("Gate 4c: readers beside one writer (run %s)", RunLabel()))
	receipt.add("captured (UTC)", time.Now().UTC().Format(time.RFC3339))
	receipt.add("readers", readerCount)
	receipt.add("submitters", writers)
	receipt.add("queue capacity", queue.Capacity())
	receipt.add("commits submitted", submitted.Load())
	receipt.add("commits refused (backpressure)", refused.Load())
	receipt.add("commits completed", stats.Completed)
	receipt.add("commit failures", stats.Failures)
	receipt.add("queue max depth", stats.MaxDepth)
	receipt.add("elapsed", elapsed)
	receipt.addf("writer throughput %.1f commits/sec", float64(stats.Completed)/elapsed.Seconds())
	receipt.add("longest commit", stats.MaxCommit)
	receipt.add("mean commit", time.Duration(stats.CommitNanos/max64(stats.Completed, 1)))
	writerLatency.addTo(receipt, "submit-to-accepted")
	readerLatency.addTo(receipt, "reader query")
	receipt.add("reader failures", readerFailures.Load())
	receipt.add("highest revision seen", highestRevision.Load())
	receipt.add("rows written", countRows(t, readerDB, `nodes`))
	receipt.add("queue error", queue.LastError())

	if readerFailures.Load() != 0 {
		t.Errorf("%d readers failed", readerFailures.Load())
	}
	if stats.Failures != 0 {
		t.Errorf("%d commits failed, last error: %v", stats.Failures, queue.LastError())
	}
	if stats.Completed != submitted.Load() {
		t.Errorf("completed %d commits but %d were accepted", stats.Completed, submitted.Load())
	}
	if want := int64(seededNodeCount) + 2 + int64(perWriter*writers); countRows(t, readerDB, `nodes`) != want {
		t.Errorf("nodes holds %d rows, want %d", countRows(t, readerDB, `nodes`), want)
	}
	if stats.MaxDepth > int64(queue.Capacity()) {
		t.Errorf("queue reached depth %d, above its capacity %d", stats.MaxDepth, queue.Capacity())
	}
	if highestRevision.Load() != 1 {
		t.Errorf("a reader observed revision %d, want 1: the inserts should not move this node's revision", highestRevision.Load())
	}

	receipt.write(t, "gate4-concurrency-"+RunLabel())
}

// TestBoundedWriterQueueAppliesBackpressure shows that the queue refuses work
// instead of growing without bound, and that everything it accepts is applied
// exactly once.
func TestBoundedWriterQueueAppliesBackpressure(t *testing.T) {
	const capacity = 4

	path := NewDatabasePath(t, "backpressure")
	db, err := OpenWriter(path, Options{})
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(Schema); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	seedWorld(t, db, "ns-bp", "root-bp")

	queue := NewWriterQueue(db, capacity)

	// Occupy the single worker so that the queue is the only thing that can move.
	release := make(chan struct{})
	occupied := make(chan struct{})
	if err := queue.Submit(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		close(occupied)
		<-release
		_, err := tx.ExecContext(ctx,
			`INSERT INTO namespaces (id, name) VALUES ('occupied', 'occupied')`)
		return err
	}); err != nil {
		t.Fatalf("occupy the worker: %v", err)
	}
	<-occupied

	var accepted, refused atomic.Int64
	var lastOrder atomic.Int64
	var outOfOrder atomic.Int64

	// Each submitter gets a short budget of its own. The worker is held, so a
	// refusal here can only come from the queue being full, and the short budget
	// keeps the gate quick.
	for i := 0; i < capacity*3; i++ {
		seq := int64(i)
		submitCtx, cancelSubmit := context.WithTimeout(context.Background(), 100*time.Millisecond)
		// Each commit records its own submission order; the single worker must
		// apply them in that order.
		err := queue.Submit(submitCtx, func(ctx context.Context, tx *sql.Tx) error {
			for {
				previous := lastOrder.Load()
				if seq <= previous || lastOrder.CompareAndSwap(previous, seq) {
					break
				}
			}
			if lastOrder.Load() != seq {
				outOfOrder.Add(1)
			}
			_, err := tx.ExecContext(ctx,
				`INSERT INTO namespaces (id, name) VALUES (?, ?)`,
				fmt.Sprintf("row-%04d", seq), fmt.Sprintf("row-%04d", seq))
			return err
		})
		cancelSubmit()
		switch {
		case err == nil:
			accepted.Add(1)
		case errors.Is(err, ErrBackpressure):
			refused.Add(1)
		default:
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	// A submitter that arrives at a full queue with no budget left is refused
	// rather than queued indefinitely.
	shortCtx, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShort()
	shortErr := queue.Submit(shortCtx, func(ctx context.Context, tx *sql.Tx) error { return nil })
	if !errors.Is(shortErr, ErrBackpressure) {
		t.Fatalf("a submit with no budget returned %v, want ErrBackpressure", shortErr)
	}
	refused.Add(1)

	depthWhileFull := queue.Stats()
	close(release)
	queue.Close()

	stats := queue.Stats()
	receipt := newReport("Gate 4d: bounded writer queue backpressure")
	receipt.add("capacity", capacity)
	receipt.add("accepted", accepted.Load())
	receipt.add("refused", refused.Load())
	receipt.add("depth while full", depthWhileFull.Depth)
	receipt.add("max depth observed", stats.MaxDepth)
	receipt.add("completed", stats.Completed)
	receipt.add("failures", stats.Failures)
	receipt.add("out-of-order applications", outOfOrder.Load())
	receipt.add("rows present", countRows(t, db, `namespaces`))

	if accepted.Load() > int64(capacity)+1 {
		t.Errorf("the queue accepted %d requests with capacity %d and one busy worker", accepted.Load(), capacity)
	}
	if refused.Load() == 0 {
		t.Error("no request was refused, so no backpressure was observed")
	}
	if stats.MaxDepth > int64(capacity) {
		t.Errorf("queue depth reached %d, above the capacity %d", stats.MaxDepth, capacity)
	}
	if outOfOrder.Load() != 0 {
		t.Errorf("%d commits ran out of submission order", outOfOrder.Load())
	}
	if stats.Failures != 0 {
		t.Errorf("%d commits failed, last error: %v", stats.Failures, queue.LastError())
	}
	// Everything accepted must be applied: the occupying commit, everything the
	// queue took, and nothing that was refused.
	if want := accepted.Load() + 2; countRows(t, db, `namespaces`) != want {
		t.Errorf("namespaces holds %d rows, want %d (seed plus every accepted commit)", countRows(t, db, `namespaces`), want)
	}

	receipt.write(t, "gate4-backpressure-"+RunLabel())
}

// TestSubmitAfterCloseRefusesWork checks the shutdown contract.
func TestSubmitAfterCloseRefusesWork(t *testing.T) {
	path := NewDatabasePath(t, "shutdown")
	db, err := OpenWriter(path, Options{})
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(Schema); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	seedWorld(t, db, "ns-shutdown", "root-shutdown")

	queue := NewWriterQueue(db, 2)
	committed := make(chan struct{})
	if err := queue.Submit(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO namespaces (id, name) VALUES ('kept', 'kept')`)
		if err != nil {
			return err
		}
		close(committed)
		return nil
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	<-committed
	queue.Close()

	err = queue.Submit(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		_, execErr := tx.ExecContext(ctx, `INSERT INTO namespaces (id, name) VALUES ('late', 'late')`)
		return execErr
	})
	if !errors.Is(err, ErrWriterStopped) {
		t.Fatalf("submit after close returned %v, want ErrWriterStopped", err)
	}
	if count := queryInt(t, db, `SELECT COUNT(*) FROM namespaces WHERE id = 'kept'`); count != 1 {
		t.Error("an accepted commit was lost during shutdown")
	}
	if count := queryInt(t, db, `SELECT COUNT(*) FROM namespaces WHERE id = 'late'`); count != 0 {
		t.Error("a refused commit was applied")
	}
}

// seedNodes inserts count regular files under parent, all at revision 1.
func seedNodes(t *testing.T, db *sql.DB, parent string, count int) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin seed nodes: %v", err)
	}
	defer tx.Rollback()

	for i := 0; i < count; i++ {
		name := fmt.Sprintf("seed-%04d", i)
		if _, err := tx.Exec(
			`INSERT INTO nodes (id, namespace_id, parent_id, name, kind, revision)
			 SELECT ?, namespace_id, id, ?, 'file', 1 FROM nodes WHERE id = ?`,
			name, name, parent,
		); err != nil {
			t.Fatalf("insert node %s: %v", name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed nodes: %v", err)
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
