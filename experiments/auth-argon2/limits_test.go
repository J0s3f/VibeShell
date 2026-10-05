package auth

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClock is the injected time source for policy tests; nothing sleeps.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestConnectionLimiterEnforcesAttemptBudget(t *testing.T) {
	clock := newFakeClock()
	limiter, err := NewConnectionLimiter(ConnectionConfig{MaxAttempts: 3}, clock.now)
	if err != nil {
		t.Fatalf("NewConnectionLimiter: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := limiter.Allow(); err != nil {
			t.Fatalf("attempt %d rejected before budget used: %v", i+1, err)
		}
		limiter.RecordFailure()
	}
	if err := limiter.Allow(); !errors.Is(err, ErrAttemptsExceeded) {
		t.Fatalf("Allow after budget error = %v, want ErrAttemptsExceeded", err)
	}

	limiter.RecordSuccess()
	if err := limiter.Allow(); err != nil {
		t.Fatalf("Allow after success error = %v, want nil", err)
	}
}

func TestConnectionLimiterRejectsInvalidConfig(t *testing.T) {
	if _, err := NewConnectionLimiter(ConnectionConfig{MaxAttempts: 0}, time.Now); err == nil {
		t.Error("MaxAttempts 0 accepted")
	}
}

func TestAbuseGuardPenalizesAbusiveSource(t *testing.T) {
	clock := newFakeClock()
	guard, err := NewAbuseGuard(AbuseConfig{
		Window:            time.Minute,
		Threshold:         3,
		Penalty:           5 * time.Minute,
		MaxTrackedSources: 4,
	}, clock.now)
	if err != nil {
		t.Fatalf("NewAbuseGuard: %v", err)
	}

	const source = "198.51.100.7:54321"
	for i := 0; i < 2; i++ {
		if err := guard.Allow(source); err != nil {
			t.Fatalf("Allow before threshold: %v", err)
		}
		guard.RecordFailure(source)
	}
	guard.RecordFailure(source) // third failure reaches the threshold

	if err := guard.Allow(source); !errors.Is(err, ErrAbusive) {
		t.Fatalf("Allow in penalty error = %v, want ErrAbusive", err)
	}
	if err := guard.Allow("203.0.113.9:1234"); err != nil {
		t.Fatalf("unrelated source rejected: %v", err)
	}

	clock.advance(5 * time.Minute) // penalty over
	if err := guard.Allow(source); err != nil {
		t.Fatalf("Allow after penalty error = %v, want nil", err)
	}
}

func TestAbuseGuardWindowExpiresFailures(t *testing.T) {
	clock := newFakeClock()
	guard, err := NewAbuseGuard(AbuseConfig{
		Window:            time.Minute,
		Threshold:         3,
		Penalty:           5 * time.Minute,
		MaxTrackedSources: 4,
	}, clock.now)
	if err != nil {
		t.Fatalf("NewAbuseGuard: %v", err)
	}

	const source = "198.51.100.8:1111"
	guard.RecordFailure(source)
	guard.RecordFailure(source)
	clock.advance(2 * time.Minute) // outside the window
	guard.RecordFailure(source)    // starts a fresh window at 1 of 3

	if err := guard.Allow(source); err != nil {
		t.Fatalf("Allow after window expiry error = %v, want nil", err)
	}
}

func TestAbuseGuardSuccessForgetsSource(t *testing.T) {
	clock := newFakeClock()
	guard, err := NewAbuseGuard(AbuseConfig{
		Window:            time.Minute,
		Threshold:         2,
		Penalty:           5 * time.Minute,
		MaxTrackedSources: 4,
	}, clock.now)
	if err != nil {
		t.Fatalf("NewAbuseGuard: %v", err)
	}

	const source = "198.51.100.9:2222"
	guard.RecordFailure(source)
	guard.RecordSuccess(source)
	if _, tracked := guard.sources[source]; tracked {
		t.Fatal("source still tracked after success")
	}
	guard.RecordFailure(source)
	if err := guard.Allow(source); err != nil {
		t.Fatalf("Allow after reset error = %v, want nil", err)
	}
}

func TestAbuseGuardBoundsTrackedSources(t *testing.T) {
	clock := newFakeClock()
	const maxTracked = 2
	guard, err := NewAbuseGuard(AbuseConfig{
		Window:            time.Minute,
		Threshold:         3,
		Penalty:           5 * time.Minute,
		MaxTrackedSources: maxTracked,
	}, clock.now)
	if err != nil {
		t.Fatalf("NewAbuseGuard: %v", err)
	}

	guard.RecordFailure("source-a")
	clock.advance(time.Second)
	guard.RecordFailure("source-b")
	clock.advance(time.Second)
	guard.RecordFailure("source-c") // table at capacity: evicts source-a

	if got := len(guard.sources); got > maxTracked {
		t.Fatalf("tracked sources = %d, want <= %d", got, maxTracked)
	}
	if _, tracked := guard.sources["source-a"]; tracked {
		t.Fatal("least recently touched source was not evicted")
	}
}

func TestAbuseGuardRejectsInvalidConfig(t *testing.T) {
	valid := AbuseConfig{Window: time.Minute, Threshold: 1, Penalty: time.Minute, MaxTrackedSources: 1}
	if _, err := NewAbuseGuard(valid, time.Now); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	cases := []AbuseConfig{
		{Window: 0, Threshold: 1, Penalty: time.Minute, MaxTrackedSources: 1},
		{Window: time.Minute, Threshold: 0, Penalty: time.Minute, MaxTrackedSources: 1},
		{Window: time.Minute, Threshold: 1, Penalty: 0, MaxTrackedSources: 1},
		{Window: time.Minute, Threshold: 1, Penalty: time.Minute, MaxTrackedSources: 0},
	}
	for i, cfg := range cases {
		if _, err := NewAbuseGuard(cfg, time.Now); err == nil {
			t.Errorf("invalid config %d accepted", i)
		}
	}
}
