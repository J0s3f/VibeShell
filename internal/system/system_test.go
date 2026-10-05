package system

import (
	"bytes"
	"testing"
	"time"
)

// TestClockNowUnixMilliTracksWallClock verifies the real clock reports a
// plausible UTC wall-clock value near the current time.
func TestClockNowUnixMilliTracksWallClock(t *testing.T) {
	clock := NewClock()
	before := time.Now().UnixMilli()
	got := clock.NowUnixMilli()
	after := time.Now().UnixMilli()
	if got < before || got > after {
		t.Fatalf("NowUnixMilli = %d, want between %d and %d", got, before, after)
	}
}

// TestClockMonotonicNanosIsNonNegativeAndMonotonic verifies monotonic values
// never go backwards within one clock.
func TestClockMonotonicNanosIsNonNegativeAndMonotonic(t *testing.T) {
	clock := NewClock()
	first := clock.MonotonicNanos()
	if first < 0 {
		t.Fatalf("MonotonicNanos = %d, want non-negative", first)
	}
	second := clock.MonotonicNanos()
	if second < first {
		t.Fatalf("MonotonicNanos went backwards: %d then %d", first, second)
	}
}

// TestRandomBytesLengthAndVariety verifies the random source returns the
// requested length and does not trivially repeat.
func TestRandomBytesLengthAndVariety(t *testing.T) {
	r := NewRandom()
	first, err := r.Bytes(32)
	if err != nil {
		t.Fatalf("Bytes(32) error: %v", err)
	}
	if len(first) != 32 {
		t.Fatalf("Bytes(32) length = %d, want 32", len(first))
	}
	second, err := r.Bytes(32)
	if err != nil {
		t.Fatalf("Bytes(32) second error: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("two 32-byte draws were identical")
	}
}

// TestRandomBytesRejectsNonPositive verifies a nonsensical request is an
// error rather than a silent empty slice.
func TestRandomBytesRejectsNonPositive(t *testing.T) {
	r := NewRandom()
	if _, err := r.Bytes(0); err == nil {
		t.Fatal("Bytes(0) = nil error, want error")
	}
}

// TestRandomIntnBounds verifies Intn stays in range and rejects a
// non-positive bound safely.
func TestRandomIntnBounds(t *testing.T) {
	r := NewRandom()
	for i := 0; i < 1000; i++ {
		if got := r.Intn(5); got < 0 || got >= 5 {
			t.Fatalf("Intn(5) = %d, out of range", got)
		}
	}
	if got := r.Intn(0); got != 0 {
		t.Fatalf("Intn(0) = %d, want 0", got)
	}
}
