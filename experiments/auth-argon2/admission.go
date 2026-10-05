package auth

import (
	"context"
	"errors"
	"sync/atomic"
)

var (
	// ErrBusy reports that the authentication queue is full; the attempt is
	// rejected before any Argon2id work starts.
	ErrBusy = errors.New("authentication capacity exhausted")
	// ErrAttemptsExceeded reports that a connection has used its failed
	// attempt budget.
	ErrAttemptsExceeded = errors.New("connection attempt limit reached")
	// ErrAbusive reports that a source is inside an abuse penalty window.
	ErrAbusive = errors.New("source inside abuse penalty window")
	// ErrAuthFailed is the single failure returned to the transport for an
	// unknown user, a disabled user, or a wrong password, so the client
	// cannot distinguish them.
	ErrAuthFailed = errors.New("authentication failed")
)

// AdmissionConfig bounds simultaneous authentication work (PLAN 4.2).
type AdmissionConfig struct {
	// MaxConcurrent is the number of Argon2id computations allowed to run
	// at once; each uses at most its hash's memory parameter.
	MaxConcurrent int
	// MaxWaiting is how many attempts may wait for a slot before being
	// rejected with ErrBusy.
	MaxWaiting int
}

// DefaultAdmissionConfig bounds simultaneous Argon2id work. Each hash runs
// its own p lanes, so extra slots oversubscribe the CPU quickly: a burst
// measurement on the 4-CPU reference shape showed four slots did not raise
// throughput over two slots, but tripled per-hash latency and doubled peak
// heap (receipts/burst.log). Peak verification memory is MaxConcurrent times
// the recommended hash memory (2 x 128 MiB = 256 MiB), well inside the 8 GiB
// reference host.
func DefaultAdmissionConfig() AdmissionConfig {
	return AdmissionConfig{MaxConcurrent: 2, MaxWaiting: 16}
}

func (c AdmissionConfig) validate() error {
	if c.MaxConcurrent < 1 {
		return errors.New("admission: MaxConcurrent must be at least 1")
	}
	if c.MaxWaiting < 0 {
		return errors.New("admission: MaxWaiting must not be negative")
	}
	return nil
}

// Admission admits a bounded number of concurrent authentication attempts and
// bounds how many may queue behind them.
type Admission struct {
	cfg     AdmissionConfig
	slots   chan struct{}
	waiting atomic.Int64
	inWork  atomic.Int64
}

// NewAdmission validates cfg and creates the admission controller.
func NewAdmission(cfg AdmissionConfig) (*Admission, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Admission{cfg: cfg, slots: make(chan struct{}, cfg.MaxConcurrent)}, nil
}

// acquire reserves a verification slot, waiting at most until ctx is done or
// the waiting budget is exhausted. The returned release function must be
// called exactly once.
func (a *Admission) acquire(ctx context.Context) (release func(), err error) {
	select {
	case a.slots <- struct{}{}:
		a.inWork.Add(1)
		return a.release, nil
	default:
	}
	if a.waiting.Add(1) > int64(a.cfg.MaxWaiting) {
		a.waiting.Add(-1)
		return nil, ErrBusy
	}
	defer a.waiting.Add(-1)

	select {
	case a.slots <- struct{}{}:
		a.inWork.Add(1)
		return a.release, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (a *Admission) release() {
	// Decrement before freeing the slot so observers never see more work in
	// flight than the configured bound.
	a.inWork.Add(-1)
	<-a.slots
}
