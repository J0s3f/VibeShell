// Package ssh internal queue tests. These run in-package to exercise the
// bounded output queue directly; the protocol-level consequence (a stalled
// client sees drops but never blocks the handler) is covered in
// refusal_test.go.
//
// Every test below honors the pump protocol: bytes returned by pop are in
// flight until reported via markWritten, and waitEmpty completes only after
// delivery, not dequeueing.
package ssh

import (
	"testing"
)

// drain simulates one pump step: dequeue everything and report it written.
func drain(q *outputQueue) {
	buf := make([]byte, 64<<10)
	for {
		chunk := q.pop(buf)
		if len(chunk) == 0 {
			return
		}
		q.markWritten(len(chunk))
	}
}

func TestOutputQueueRoundTrip(t *testing.T) {
	q := newOutputQueue(64)
	q.push([]byte("hello "))
	q.push([]byte("world"))
	buf := make([]byte, 64)
	if got := string(q.pop(buf)); got != "hello world" {
		t.Errorf("pop = %q, want %q", got, "hello world")
	}
	if got := q.pop(buf); len(got) != 0 {
		t.Errorf("pop on empty = %q, want empty", got)
	}
	// Dequeued but undelivered bytes still hold the drain waiter.
	done := make(chan struct{})
	close(done)
	if q.waitEmpty(done) {
		t.Error("waitEmpty after pop without markWritten = true, want false")
	}
	q.markWritten(len("hello world"))
	if !q.waitEmpty(done) {
		t.Error("waitEmpty after delivery = false, want true")
	}
}

func TestOutputQueueDropsOldestBytes(t *testing.T) {
	q := newOutputQueue(8)
	q.push([]byte("12345678"))
	q.push([]byte("AB"))
	buf := make([]byte, 16)
	if got := string(q.pop(buf)); got != "345678AB" {
		t.Errorf("pop = %q, want the newest %q", got, "345678AB")
	}
	if got := q.droppedCount(); got != 2 {
		t.Errorf("dropped = %d, want 2", got)
	}
	if got := q.queued(); got != 0 {
		t.Errorf("queued after pop = %d, want 0", got)
	}
	q.markWritten(len("345678AB"))
	done := make(chan struct{})
	close(done)
	if !q.waitEmpty(done) {
		t.Error("waitEmpty after delivery = false, want true")
	}
}

func TestOutputQueueBoundsASingleOversizedWrite(t *testing.T) {
	q := newOutputQueue(4)
	q.push([]byte("0123456789"))
	buf := make([]byte, 16)
	if got := string(q.pop(buf)); got != "6789" {
		t.Errorf("pop = %q, want the newest %q", got, "6789")
	}
	if got := q.droppedCount(); got != 6 {
		t.Errorf("dropped = %d, want 6", got)
	}
}

func TestOutputQueueWaitEmpty(t *testing.T) {
	q := newOutputQueue(8)
	q.push([]byte("stalled"))

	// A closed done channel unblocks the wait with false while bytes are
	// neither dequeued nor delivered.
	cancelled := make(chan struct{})
	close(cancelled)
	if q.waitEmpty(cancelled) {
		t.Fatal("waitEmpty with done closed = true, want false while bytes remain")
	}

	// Full delivery (dequeue plus markWritten) unblocks the wait with true.
	release := make(chan struct{})
	go func() {
		defer close(release)
		drain(q)
	}()
	if !q.waitEmpty(release) {
		t.Error("waitEmpty after delivery = false, want true")
	}

	// An already drained queue reports immediately.
	if !q.waitEmpty(cancelled) {
		t.Error("waitEmpty on drained queue = false, want true")
	}
}
