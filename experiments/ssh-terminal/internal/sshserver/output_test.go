package sshserver

import (
	"bytes"
	"testing"
	"time"
)

func TestOutputQueueKeepsTheNewestBytesAndDropsTheOldest(t *testing.T) {
	const limit = 16
	queue := newOutputQueue(limit)

	queue.push(bytes.Repeat([]byte("a"), limit))
	if queue.len() != limit {
		t.Fatalf("queued %d bytes, want %d", queue.len(), limit)
	}
	if queue.droppedCount() != 0 {
		t.Fatalf("dropped %d bytes before overflowing", queue.droppedCount())
	}

	// Pushing half as much again must evict the oldest bytes, not the newest.
	queue.push(bytes.Repeat([]byte("b"), limit/2))
	if got := queue.len(); got != limit {
		t.Fatalf("queued %d bytes after overflow, want the %d byte limit", got, limit)
	}
	if got, want := queue.droppedCount(), int64(limit/2); got != want {
		t.Fatalf("dropped %d bytes, want %d", got, want)
	}

	got := drain(queue)
	if want := string(bytes.Repeat([]byte("a"), limit/2)) + string(bytes.Repeat([]byte("b"), limit/2)); string(got) != want {
		t.Fatalf("queue holds %q, want %q", got, want)
	}
}

func TestOutputQueueBoundsASingleOversizedWrite(t *testing.T) {
	const limit = 8
	queue := newOutputQueue(limit)
	queue.push([]byte("0123456789abcdef"))
	if got := queue.len(); got != limit {
		t.Fatalf("queued %d bytes, want %d", got, limit)
	}
	if got, want := queue.droppedCount(), int64(8); got != want {
		t.Fatalf("dropped %d bytes, want %d", got, want)
	}
	if got, want := string(drain(queue)), "89abcdef"; got != want {
		t.Fatalf("queue holds %q, want the newest %q", got, want)
	}
}

func TestOutputQueueWaitEmptySeesInFlightBytes(t *testing.T) {
	// A writer that has taken bytes out of the queue but has not yet handed them
	// to the channel still owes the client those bytes. waitEmpty must not
	// report the queue as empty, or a session would announce its exit status and
	// close the channel ahead of output the client still had to read.
	queue := newOutputQueue(64)
	done := make(chan struct{})
	defer close(done)

	queue.push([]byte("queued output"))
	chunk := queue.pop(make([]byte, 64))
	if string(chunk) != "queued output" {
		t.Fatalf("pop returned %q", chunk)
	}

	waited := make(chan bool, 1)
	go func() { waited <- queue.waitEmpty(done) }()
	select {
	case <-waited:
		t.Fatal("waitEmpty returned while bytes were still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	queue.done(len(chunk))
	select {
	case ok := <-waited:
		if !ok {
			t.Fatal("waitEmpty reported the done channel, not a drained queue")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitEmpty did not return after the write completed")
	}
}

func TestOutputQueueWaitEmptyStopsOnTheDoneChannel(t *testing.T) {
	queue := newOutputQueue(64)
	queue.push([]byte("never delivered"))
	queue.pop(make([]byte, 64)) // in flight and never reported

	done := make(chan struct{})
	close(done)
	if queue.waitEmpty(done) {
		t.Fatal("waitEmpty reported a drained queue while a write was still pending")
	}
}

func TestOutputQueuePopIsChunked(t *testing.T) {
	queue := newOutputQueue(64)
	queue.push([]byte("abcdefghij"))
	dst := make([]byte, 4)
	if got := string(queue.pop(dst)); got != "abcd" {
		t.Fatalf("first chunk = %q, want abcd", got)
	}
	if got := string(queue.pop(dst)); got != "efgh" {
		t.Fatalf("second chunk = %q, want efgh", got)
	}
	if got := string(queue.pop(dst)); got != "ij" {
		t.Fatalf("third chunk = %q, want ij", got)
	}
	if got := string(queue.pop(dst)); got != "" {
		t.Fatalf("pop from an empty queue returned %q", got)
	}
}

func drain(queue *outputQueue) []byte {
	var out []byte
	buf := make([]byte, 64)
	for {
		chunk := queue.pop(buf)
		if len(chunk) == 0 {
			return out
		}
		out = append(out, chunk...)
	}
}
