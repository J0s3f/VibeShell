package ssh

import "sync"

// outputQueue is a bounded byte queue in front of a client that may not be
// reading. It exists so a slow client can neither make a session grow without
// limit nor block the application goroutine that produces output.
//
// When a push would exceed the limit the oldest queued bytes are discarded,
// not the newest, so what a client finally receives is the tail of the recent
// output: for a terminal that is the current screen state rather than a
// fragment of one earlier frame. DroppedBytes reports the loss; a non-zero
// value means that session's screen may be incomplete and never affects
// another session.
//
// Delivery accounting closes a subtle race: the pump dequeues bytes before
// writing them to the channel, so "queue empty" does not mean "bytes on the
// wire". waitEmpty therefore waits until every admitted byte has been handed
// to the channel (accepted == written), which is what lets a session send its
// exit status and close only after the client can receive the preceding
// output in order.
type outputQueue struct {
	limit int
	wake  chan struct{}

	mu       sync.Mutex
	buf      []byte
	dropped  int64
	accepted uint64 // bytes admitted to the queue (dropped bytes excluded)
	written  uint64 // bytes the pump handed to the channel
}

func newOutputQueue(limit int) *outputQueue {
	if limit < 1 {
		limit = 1
	}
	return &outputQueue{
		limit: limit,
		wake:  make(chan struct{}, 1),
		buf:   make([]byte, 0, min(limit, 8<<10)),
	}
}

// push appends p, discarding the oldest bytes when the queue is full. Push
// never blocks.
func (q *outputQueue) push(p []byte) {
	if len(p) == 0 {
		return
	}
	q.mu.Lock()
	var admitted int
	if len(p) >= q.limit {
		// The whole previous queue is discarded for the tail of p.
		droppedOld := len(q.buf)
		q.dropped += int64(droppedOld + len(p) - q.limit)
		q.buf = append(q.buf[:0], p[len(p)-q.limit:]...)
		admitted = q.limit - droppedOld
	} else {
		admitted = len(p)
		if overflow := len(q.buf) + len(p) - q.limit; overflow > 0 {
			q.dropped += int64(overflow)
			n := copy(q.buf, q.buf[overflow:])
			q.buf = q.buf[:n]
			admitted -= overflow
		}
		q.buf = append(q.buf, p...)
	}
	// Accepted counts bytes owed delivery: newly admitted bytes minus
	// previously queued bytes discarded to make room for them.
	q.accepted += uint64(admitted)
	q.mu.Unlock()

	// A buffered wake-up keeps the pump goroutine from being signalled once
	// per byte while it is already awake.
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// pop copies the queued bytes into dst and returns the filled prefix. It
// returns an empty slice when the queue is empty. Popped bytes are in flight
// until the pump reports them via markWritten.
func (q *outputQueue) pop(dst []byte) []byte {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := copy(dst, q.buf)
	q.buf = q.buf[:copy(q.buf, q.buf[n:])]
	return dst[:n]
}

// markWritten records that n dequeued bytes reached the channel and wakes any
// drain waiter.
func (q *outputQueue) markWritten(n int) {
	if n <= 0 {
		return
	}
	q.mu.Lock()
	q.written += uint64(n)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// waitEmpty blocks until every admitted byte has been handed to the channel
// or done closes, and reports which happened. A session uses it to flush its
// output before it announces the exit status, without waiting forever for a
// client that stopped reading. When the channel breaks, the pump stops
// without completing delivery and the done timeout bounds the wait.
func (q *outputQueue) waitEmpty(done <-chan struct{}) bool {
	for {
		q.mu.Lock()
		pending := q.accepted - q.written
		q.mu.Unlock()
		if pending == 0 {
			return true
		}
		select {
		case <-q.wake:
		case <-done:
			return false
		}
	}
}

// droppedCount reports how many bytes were discarded for a slow client.
func (q *outputQueue) droppedCount() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

// queued reports the currently held byte count, which never exceeds the limit.
func (q *outputQueue) queued() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.buf)
}
