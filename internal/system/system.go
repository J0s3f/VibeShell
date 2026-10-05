// Package system provides the real, process-wide implementations of the
// Clock and Random outbound ports declared in internal/ports.
//
// These are the only places that read the operating system clock or a real
// entropy source. Domain and application policies take explicit int64
// timestamps and injected ports instead, so their tests never depend on real
// time or randomness (AGENTS.md "Inject clocks, randomness, and external
// boundaries"). The composition root constructs these adapters and injects
// them where a port is required.
package system

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"time"

	"j0s.at/vibeshell/internal/ports"
)

// Clock is the real wall-clock and monotonic time source.
type Clock struct {
	// started is the process's monotonic reference point. MonotonicNanos is
	// measured from here so the value is stable for the process lifetime
	// rather than depending on the host's arbitrary monotonic origin.
	started time.Time
}

// NewClock returns a clock anchored at the current instant. Its monotonic
// values are nanoseconds since construction, which is what PLAN 10.1 needs
// for ordering events inside one recording.
func NewClock() *Clock {
	return &Clock{started: time.Now()}
}

// NowUnixMilli returns wall-clock UTC milliseconds.
func (c *Clock) NowUnixMilli() int64 { return time.Now().UnixMilli() }

// MonotonicNanos returns nanoseconds since this clock was constructed.
func (c *Clock) MonotonicNanos() int64 { return int64(time.Since(c.started)) }

// Compile-time proof that the adapter satisfies the outbound port.
var _ ports.Clock = (*Clock)(nil)

// Random is the real cryptographic randomness source.
type Random struct{}

// NewRandom returns a random source backed by crypto/rand.
func NewRandom() *Random { return &Random{} }

// Bytes returns n bytes from crypto/rand. It rejects a non-positive request
// rather than returning an empty slice that callers could mistake for entropy.
func (r *Random) Bytes(n int) ([]byte, error) {
	if n <= 0 {
		return nil, fmt.Errorf("system: random byte count must be positive, got %d", n)
	}
	out := make([]byte, n)
	if _, err := rand.Read(out); err != nil {
		return nil, fmt.Errorf("system: read random bytes: %w", err)
	}
	return out, nil
}

// Intn returns a uniform value in [0, n) using crypto/rand, so selection
// policy (PLAN 8.4 route choice) is not biased by a modulo reduction.
func (r *Random) Intn(n int) int {
	if n <= 0 {
		return 0
	}
	value, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		// crypto/rand does not fail in practice; a failure here must not
		// silently collapse every choice onto one value, so fall back to a
		// time-derived index that is still within range.
		return int(time.Now().UnixNano() % int64(n))
	}
	return int(value.Int64())
}

// Compile-time proof that the adapter satisfies the outbound port.
var _ ports.Random = (*Random)(nil)
