package sshserver

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"j0s.at/vibeshell/experiments/ssh-terminal/internal/termdecode"
)

// newResizeTestSession returns a session with only the state the resize path
// uses, so the coalescing rule can be checked without a network round trip and
// without timing.
func newResizeTestSession(depth int) *Session {
	return &Session{
		events:     make(chan termdecode.Event, depth),
		limits:     DefaultLimits,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		resizeWake: make(chan struct{}, 1),
	}
}

// pendingResize reports the size waiting in the coalescing slot.
func (s *Session) pendingResize() (Terminal, bool) {
	s.resizeMu.Lock()
	defer s.resizeMu.Unlock()
	if s.resizePending == nil {
		return Terminal{}, false
	}
	return *s.resizePending, true
}

func TestResizeCoalescesToTheNewestSize(t *testing.T) {
	const (
		depth        = 1
		resizeCount  = 40
		newestColumn = 80 + resizeCount
	)
	session := newResizeTestSession(depth)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go session.publishResizes(ctx)

	for i := 1; i <= resizeCount; i++ {
		session.pushResize(Terminal{Cols: 80 + i, Rows: 24})
	}
	// The publisher can hold one size in the stream and one in the coalescing
	// slot; the slot must hold the newest.
	deadline := time.Now().Add(5 * time.Second)
	var pending Terminal
	var ok bool
	for {
		pending, ok = session.pendingResize()
		if ok || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !ok {
		t.Fatal("no size stayed in the coalescing slot")
	}
	if pending.Cols != newestColumn {
		t.Fatalf("coalescing slot holds column %d, want the newest %d", pending.Cols, newestColumn)
	}
	if got := len(session.events); got > depth {
		t.Fatalf("queued %d events, want at most the stream depth %d", got, depth)
	}
}

func TestResizeKeepsQueuedKeysInOrder(t *testing.T) {
	// A resize may wait, but a key press may never be dropped or reordered to
	// make room for one.
	session := newResizeTestSession(2)
	session.events <- termdecode.Key{Name: termdecode.KeyRune, Rune: 'a'}
	session.events <- termdecode.Key{Name: termdecode.KeyRune, Rune: 'b'}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go session.publishResizes(ctx)
	session.pushResize(Terminal{Cols: 100, Rows: 30})

	// Draining the first key frees room, and the resize follows the second.
	for _, want := range []rune{'a', 'b'} {
		key, ok := (<-session.events).(termdecode.Key)
		if !ok {
			t.Fatalf("event is %T, want a key", key)
		}
		if key.Rune != want {
			t.Fatalf("key rune = %q, want %q", key.Rune, want)
		}
	}
	select {
	case event := <-session.events:
		resize, ok := event.(termdecode.Resize)
		if !ok {
			t.Fatalf("event is %s, want the queued resize", describeTest(event))
		}
		if resize.Cols != 100 || resize.Rows != 30 {
			t.Fatalf("resize = %dx%d, want 100x30", resize.Cols, resize.Rows)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the resize never reached the event stream")
	}
}

func TestResizeDeliveredImmediatelyWhenTheQueueHasRoom(t *testing.T) {
	session := newResizeTestSession(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go session.publishResizes(ctx)

	session.pushResize(Terminal{Cols: 132, Rows: 50})
	select {
	case event := <-session.events:
		resize, ok := event.(termdecode.Resize)
		if !ok {
			t.Fatalf("event is %s, want a resize", describeTest(event))
		}
		if resize.Cols != 132 || resize.Rows != 50 {
			t.Fatalf("resize = %dx%d, want 132x50", resize.Cols, resize.Rows)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the resize never reached the event stream")
	}
}

func describeTest(event termdecode.Event) string {
	switch typed := event.(type) {
	case termdecode.Key:
		return "key " + typed.String()
	case termdecode.Resize:
		return "resize"
	case termdecode.Paste:
		return "paste"
	default:
		return "unknown event"
	}
}
