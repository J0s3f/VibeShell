package inference

import (
	"time"

	"j0s.at/vibeshell/internal/domain"
)

// waiter is one caller parked in the bounded wait queue. granted is set under
// the gate's mutex exactly once, which is what lets a slot handoff and the
// caller's own cancellation race safely: whoever gets there first decides
// whether the waiter owns a slot.
type waiter struct {
	account domain.AccountID
	started time.Time
	ready   chan struct{}
	granted bool

	prev *waiter
	next *waiter
}

// waiters is the FIFO wait queue, kept in arrival order so a wait is fair. It is
// an intrusive list so a caller that gives up can unlink itself in constant
// time without searching or reordering the queue behind it.
type waiters struct {
	head *waiter
	tail *waiter
}

func (q *waiters) push(w *waiter) {
	w.prev, w.next = q.tail, nil
	if q.tail != nil {
		q.tail.next = w
	}
	q.tail = w
	if q.head == nil {
		q.head = w
	}
}

// remove unlinks w. Every call site removes a waiter that is still queued, so
// this needs no membership test of its own.
func (q *waiters) remove(w *waiter) {
	if w.prev != nil {
		w.prev.next = w.next
	} else {
		q.head = w.next
	}
	if w.next != nil {
		w.next.prev = w.prev
	} else {
		q.tail = w.prev
	}
	w.prev, w.next = nil, nil
}
