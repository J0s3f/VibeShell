package auth

import (
	"errors"
	"sync"
	"time"
)

// ConnectionConfig bounds failed authentication attempts on one connection.
type ConnectionConfig struct {
	// MaxAttempts is the number of failed attempts a connection may make
	// before every further attempt is rejected with ErrAttemptsExceeded.
	// Successful authentication resets the budget.
	MaxAttempts int
}

// DefaultConnectionConfig matches sshd's default MaxAuthTries of six failed
// attempts per connection.
func DefaultConnectionConfig() ConnectionConfig {
	return ConnectionConfig{MaxAttempts: 6}
}

func (c ConnectionConfig) validate() error {
	if c.MaxAttempts < 1 {
		return errors.New("connection: MaxAttempts must be at least 1")
	}
	return nil
}

// AbuseConfig bounds the authentication failure rate of one source across its
// connections.
type AbuseConfig struct {
	// Window is the failure-rate measurement window.
	Window time.Duration
	// Threshold failures inside one window trigger the penalty.
	Threshold int
	// Penalty is how long a penalized source is rejected with ErrAbusive
	// without doing any Argon2id work.
	Penalty time.Duration
	// MaxTrackedSources bounds the guard's bookkeeping. Expired entries are
	// pruned first; if the table is still full the least recently touched
	// source is evicted, so memory stays bounded.
	MaxTrackedSources int
}

// DefaultAbuseConfig rejects a source for five minutes after ten failures in
// one minute, tracking at most 1024 sources.
func DefaultAbuseConfig() AbuseConfig {
	return AbuseConfig{
		Window:            time.Minute,
		Threshold:         10,
		Penalty:           5 * time.Minute,
		MaxTrackedSources: 1024,
	}
}

func (c AbuseConfig) validate() error {
	if c.Window <= 0 {
		return errors.New("abuse: Window must be positive")
	}
	if c.Threshold < 1 {
		return errors.New("abuse: Threshold must be at least 1")
	}
	if c.Penalty <= 0 {
		return errors.New("abuse: Penalty must be positive")
	}
	if c.MaxTrackedSources < 1 {
		return errors.New("abuse: MaxTrackedSources must be at least 1")
	}
	return nil
}

// ConnectionLimiter tracks the failed-attempt budget of a single connection.
// The transport creates one per connection; it is safe for concurrent use.
type ConnectionLimiter struct {
	cfg ConnectionConfig
	now func() time.Time

	mu       sync.Mutex
	failures int
}

// NewConnectionLimiter validates cfg and creates a limiter using now.
func NewConnectionLimiter(cfg ConnectionConfig, now func() time.Time) (*ConnectionLimiter, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &ConnectionLimiter{cfg: cfg, now: now}, nil
}

// Allow reports whether this connection may start another authentication
// attempt.
func (l *ConnectionLimiter) Allow() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failures >= l.cfg.MaxAttempts {
		return ErrAttemptsExceeded
	}
	return nil
}

// RecordFailure counts one failed attempt against the connection budget.
func (l *ConnectionLimiter) RecordFailure() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures++
}

// RecordSuccess clears the budget after an authenticated session start.
func (l *ConnectionLimiter) RecordSuccess() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures = 0
}

// sourceState is one tracked source's failure window and penalty.
type sourceState struct {
	failures    int
	windowStart time.Time
	abuseUntil  time.Time
	updated     time.Time
}

// AbuseGuard rejects sources whose authentication failures arrive at an
// abusive rate, independently of any single connection. Safe for concurrent
// use.
type AbuseGuard struct {
	cfg AbuseConfig
	now func() time.Time

	mu      sync.Mutex
	sources map[string]*sourceState
}

// NewAbuseGuard validates cfg and creates a guard using now.
func NewAbuseGuard(cfg AbuseConfig, now func() time.Time) (*AbuseGuard, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &AbuseGuard{cfg: cfg, now: now, sources: make(map[string]*sourceState)}, nil
}

// Allow reports whether source may start another authentication attempt
// without entering any Argon2id work.
func (g *AbuseGuard) Allow(source string) error {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	state := g.sources[source]
	if state == nil {
		return nil
	}
	state.updated = now
	if now.Before(state.abuseUntil) {
		return ErrAbusive
	}
	if now.Sub(state.windowStart) >= g.cfg.Window {
		state.failures = 0
		state.windowStart = now
	}
	return nil
}

// RecordFailure counts one failed attempt for source and starts the penalty
// once the threshold is reached inside one window.
func (g *AbuseGuard) RecordFailure(source string) {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()

	state := g.sources[source]
	if state == nil {
		g.makeRoom(now)
		state = &sourceState{windowStart: now}
		g.sources[source] = state
	}
	if now.Sub(state.windowStart) >= g.cfg.Window {
		state.failures = 0
		state.windowStart = now
	}
	state.failures++
	state.updated = now
	if state.failures >= g.cfg.Threshold && !now.Before(state.abuseUntil) {
		state.abuseUntil = now.Add(g.cfg.Penalty)
	}
}

// RecordSuccess forgets source after an authenticated session start.
func (g *AbuseGuard) RecordSuccess(source string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.sources, source)
}

// makeRoom prunes expired state and, if the table is still at its bound,
// evicts the least recently touched source. Callers hold g.mu.
func (g *AbuseGuard) makeRoom(now time.Time) {
	if len(g.sources) < g.cfg.MaxTrackedSources {
		return
	}
	for key, state := range g.sources {
		expired := now.Sub(state.updated) >= g.cfg.Window && !now.Before(state.abuseUntil)
		if expired {
			delete(g.sources, key)
		}
	}
	for len(g.sources) >= g.cfg.MaxTrackedSources {
		oldestKey := ""
		var oldest time.Time
		for key, state := range g.sources {
			if oldestKey == "" || state.updated.Before(oldest) {
				oldestKey, oldest = key, state.updated
			}
		}
		delete(g.sources, oldestKey)
	}
}
