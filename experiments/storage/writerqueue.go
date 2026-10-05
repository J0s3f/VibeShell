package storage

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrBackpressure reports that a commit request was refused because the bounded
// writer queue stayed full for as long as the caller's context allowed. The
// change was not applied, so the caller may refresh and retry instead of
// believing an unacknowledged world change was committed.
var ErrBackpressure = errors.New("storage: writer queue full")

// ErrWriterStopped reports that the queue is shut down and accepts no work.
var ErrWriterStopped = errors.New("storage: writer queue stopped")

// CommitFunc runs inside the queue's single transaction. Every world change and
// every accepted-event row of one turn belongs in one CommitFunc, which is what
// PLAN 10.3 requires of a commit: the write lock is held only for this function.
type CommitFunc func(ctx context.Context, tx *sql.Tx) error

// QueueStats is the observable backpressure signal PLAN 10.2 asks for. A caller
// that watches Depth approach Capacity, or that receives ErrBackpressure, knows
// the queue is the bottleneck rather than inferring it from latency.
type QueueStats struct {
	Capacity    int
	Depth       int64
	Submitted   int64
	Completed   int64
	Rejected    int64
	Failures    int64
	MaxDepth    int64
	CommitNanos int64
	MaxCommit   time.Duration
}

// WriterQueue serializes commits onto SQLite's single write lock with a fixed
// capacity, instead of letting concurrent callers race for it and discover the
// limit as a mid-transaction SQLITE_BUSY. Each accepted request runs in one
// BEGIN IMMEDIATE transaction on a single-connection pool, so the lock is never
// held across model work.
type WriterQueue struct {
	db     *sql.DB
	queue  chan CommitFunc
	closed chan struct{}

	mu           sync.Mutex
	shuttingDown bool
	submitters   sync.WaitGroup
	workers      sync.WaitGroup

	depth       atomic.Int64
	submitted   atomic.Int64
	completed   atomic.Int64
	rejected    atomic.Int64
	failures    atomic.Int64
	maxDepth    atomic.Int64
	commitNanos atomic.Int64
	maxCommit   atomic.Int64

	lastFailure atomic.Pointer[error]

	closeOnce sync.Once
}

// NewWriterQueue starts a queue with the given capacity over db and returns it.
// The caller must Close it to release the worker.
func NewWriterQueue(db *sql.DB, capacity int) *WriterQueue {
	q := &WriterQueue{
		db:     db,
		queue:  make(chan CommitFunc, capacity),
		closed: make(chan struct{}),
	}
	q.workers.Add(1)
	go q.work()
	return q
}

// Capacity reports the configured queue depth bound.
func (q *WriterQueue) Capacity() int { return cap(q.queue) }

// Submit enqueues one commit, waiting for room while ctx allows. It returns
// ErrBackpressure when the caller's budget expires first and ErrWriterStopped
// after Close.
//
// This prototype reports admission only. The outcome of a commit is observed
// through Stats and LastError once the queue has drained; a production queue
// needs one result channel per request, which is recorded as an open question in
// the spike report rather than built here.
func (q *WriterQueue) Submit(ctx context.Context, commit CommitFunc) error {
	q.mu.Lock()
	if q.shuttingDown {
		q.mu.Unlock()
		q.rejected.Add(1)
		return ErrWriterStopped
	}
	q.submitters.Add(1)
	q.mu.Unlock()
	defer q.submitters.Done()

	q.submitted.Add(1)
	select {
	case q.queue <- commit:
		q.raiseDepth(int64(len(q.queue)))
		return nil
	case <-ctx.Done():
		q.rejected.Add(1)
		return ErrBackpressure
	case <-q.closed:
		q.rejected.Add(1)
		return ErrWriterStopped
	}
}

// Stats samples the queue. Counters are cumulative for the queue's lifetime.
func (q *WriterQueue) Stats() QueueStats {
	return QueueStats{
		Capacity:    cap(q.queue),
		Depth:       int64(len(q.queue)),
		Submitted:   q.submitted.Load(),
		Completed:   q.completed.Load(),
		Rejected:    q.rejected.Load(),
		Failures:    q.failures.Load(),
		MaxDepth:    q.maxDepth.Load(),
		CommitNanos: q.commitNanos.Load(),
		MaxCommit:   time.Duration(q.maxCommit.Load()),
	}
}

// LastError returns the most recent commit failure, or nil when every accepted
// commit succeeded.
func (q *WriterQueue) LastError() error {
	if p := q.lastFailure.Load(); p != nil {
		return *p
	}
	return nil
}

// Close refuses further submissions, wakes the submitters already waiting for
// room, and finishes the commits already accepted. An acknowledged commit is
// therefore never dropped by a graceful shutdown.
func (q *WriterQueue) Close() {
	q.closeOnce.Do(func() {
		q.mu.Lock()
		q.shuttingDown = true
		q.mu.Unlock()
		close(q.closed)
	})
	// Every Submit has now either enqueued or failed, so the channel can no
	// longer grow and draining it to empty loses nothing.
	q.submitters.Wait()
	q.workers.Wait()
}

func (q *WriterQueue) work() {
	defer q.workers.Done()
	for {
		select {
		case commit := <-q.queue:
			q.execute(commit)
		case <-q.closed:
			q.submitters.Wait()
			for {
				select {
				case commit := <-q.queue:
					q.execute(commit)
				default:
					return
				}
			}
		}
	}
}

func (q *WriterQueue) execute(commit CommitFunc) {
	started := time.Now()
	err := q.run(commit)
	elapsed := time.Since(started)

	q.completed.Add(1)
	q.commitNanos.Add(int64(elapsed))
	q.raiseMaxCommit(elapsed)
	if err != nil {
		// A failed commit is a first-class condition for the caller, so it is
		// recorded rather than swallowed; the world change was rolled back and
		// must not be reported as applied.
		q.failures.Add(1)
		q.lastFailure.Store(&err)
	}
}

func (q *WriterQueue) run(commit CommitFunc) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := commit(ctx, tx); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

func (q *WriterQueue) raiseDepth(depth int64) {
	for {
		current := q.maxDepth.Load()
		if depth <= current || q.maxDepth.CompareAndSwap(current, depth) {
			return
		}
	}
}

func (q *WriterQueue) raiseMaxCommit(elapsed time.Duration) {
	for {
		current := q.maxCommit.Load()
		if int64(elapsed) <= current || q.maxCommit.CompareAndSwap(current, int64(elapsed)) {
			return
		}
	}
}
