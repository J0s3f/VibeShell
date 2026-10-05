package apps

import (
	"errors"
	"sync/atomic"
)

// FixedClock is a Clock double that returns an
// explicit, settable time so artifact and event
// timestamps are deterministic under test.
type FixedClock struct {
	now int64
}

// NewFixedClock builds a clock at the given Unix
// millisecond time.
func NewFixedClock(nowUnixMilli int64) *FixedClock {
	return &FixedClock{now: nowUnixMilli}
}

// NowUnixMilli returns the fixed wall-clock time.
func (c *FixedClock) NowUnixMilli() int64 { return c.now }

// MonotonicNanos returns a fixed monotonic offset.
func (c *FixedClock) MonotonicNanos() int64 { return 0 }

// Advance moves the clock forward by delta
// milliseconds.
func (c *FixedClock) Advance(delta int64) { c.now += delta }

// SequenceRandom is a Random double that mints
// deterministic, unique byte sequences. IDs derive
// from it reproducibly while staying unique within
// a test.
type SequenceRandom struct {
	counter atomic.Int64
}

// Bytes returns n bytes derived from the sequence
// counter: a 7-byte big-endian counter prefix
// followed by a byte pattern, repeated to n bytes.
func (r *SequenceRandom) Bytes(n int) ([]byte, error) {
	if n <= 0 {
		return nil, errors.New("random byte count must be positive")
	}
	sequence := r.counter.Add(1)
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		// Spread the counter across the bytes so
		// consecutive 16-byte identities differ in
		// every position, not only the tail.
		shift := uint((i * 8) % 56)
		out[i] = byte((sequence >> shift) & 0xff)
	}
	return out, nil
}

// Intn returns a deterministic value in [0, n).
func (r *SequenceRandom) Intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.counter.Add(1) % int64(n))
}
