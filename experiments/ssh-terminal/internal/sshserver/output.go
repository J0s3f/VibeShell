package sshserver

import "sync"

// outputQueue is a bounded byte queue in front of a client that may not be
// reading. It exists so a slow client can never make a session grow without
// limit and can never block the session's own writer.
//
// When a push would exceed the limit the oldest queued bytes are discarded, not
// the newest, so what a client finally receives is the tail of the recent
// output: for a terminal that is the current screen state rather than a
// fragment of one earlier frame. DroppedBytes reports the loss.
type outputQueue struct {
	limit   int
	wake    chan struct{}
	empty   chan struct{}
	mu      sync.Mutex
	buf     []byte
	dropped int64
	// inflight counts bytes taken out of buf by a writer that has not finished
	// handing them to the SSH channel yet. Without it the queue looks empty
	// while a write is still pending, and a session could announce its exit
	// status and close the channel ahead of output the client still had to
	// read.
	inflight int
}

func newOutputQueue(limit int) *outputQueue {
	if limit < 1 {
		limit = 1
	}
	return &outputQueue{
		limit: limit,
		wake:  make(chan struct{}, 1),
		empty: make(chan struct{}),
		buf:   make([]byte, 0, min(limit, 8<<10)),
	}
}

// signalEmpty tells waiters that the queue has nothing left. The caller holds
// the lock.
func (q *outputQueue) signalEmpty() {
	select {
	case <-q.empty:
	default:
		close(q.empty)
	}
}

// push appends p, discarding the oldest bytes if the queue is full.
func (q *outputQueue) push(p []byte) {
	if len(p) == 0 {
		return
	}
	q.mu.Lock()
	if len(p) >= q.limit {
		q.dropped += int64(len(q.buf) + len(p) - q.limit)
		q.buf = append(q.buf[:0], p[len(p)-q.limit:]...)
	} else {
		if overflow := len(q.buf) + len(p) - q.limit; overflow > 0 {
			q.dropped += int64(overflow)
			n := copy(q.buf, q.buf[overflow:])
			q.buf = q.buf[:n]
		}
		q.buf = append(q.buf, p...)
	}
	q.mu.Unlock()

	// A buffered wake-up keeps the pump goroutine from being signalled once per
	// byte while it is already awake.
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// pop copies the queued bytes into dst and returns the filled prefix. It
// returns an empty slice when the queue is empty. The caller must report the
// written count with done so that waitEmpty sees the bytes leave the queue only
// after the channel accepted them.
func (q *outputQueue) pop(dst []byte) []byte {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := copy(dst, q.buf)
	q.buf = q.buf[:copy(q.buf, q.buf[n:])]
	q.inflight += n
	q.signalIfDrained()
	return dst[:n]
}

// done reports that a writer handed written bytes to the SSH channel.
func (q *outputQueue) done(written int) {
	q.mu.Lock()
	q.inflight -= written
	q.signalIfDrained()
	q.mu.Unlock()
}

// signalIfDrained releases waiters when nothing is left to deliver. The caller
// holds the lock.
func (q *outputQueue) signalIfDrained() {
	if len(q.buf) == 0 && q.inflight == 0 {
		q.signalEmpty()
	}
}

// waitEmpty blocks until every queued and in-flight byte has been handed to the
// SSH channel, or until done is closed, and reports which happened. A session
// uses it to flush its output before it announces the exit status.
func (q *outputQueue) waitEmpty(done <-chan struct{}) bool {
	for {
		q.mu.Lock()
		pending := q.pendingLocked()
		wait := q.empty
		q.mu.Unlock()
		if pending == 0 {
			return true
		}
		select {
		case <-wait:
		case <-done:
			return false
		}
	}
}

// dropped reports how many bytes the queue had to discard.
func (q *outputQueue) droppedCount() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

// len reports the currently queued byte count, which never exceeds the limit.
func (q *outputQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.lenLocked()
}

// lenLocked reports the queued byte count; the caller holds the lock.
func (q *outputQueue) lenLocked() int { return len(q.buf) }

// pendingLocked reports the bytes still owed to the client, queued or being
// written; the caller holds the lock.
func (q *outputQueue) pendingLocked() int { return len(q.buf) + q.inflight }
