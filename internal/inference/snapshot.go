package inference

import "j0s.at/vibeshell/internal/domain"

// Snapshot is the observable backpressure of the gate: the limits in force, the
// capacity currently held, and what the gate did under those limits. It is a
// point-in-time copy, safe to read while requests are running.
type Snapshot struct {
	GlobalConcurrency     int
	MaxAccountConcurrency int   // zero when the per-account bound is not configured
	WaitQueueDepth        int   // zero when the gate never waits
	MaxWaitMs             int64 // zero when only the caller's context bounds the wait

	InFlight int
	Waiting  int
	// AccountsInFlight is the per-account concurrency currently in use. It makes
	// one account's budget visible independently of any other account's.
	AccountsInFlight map[domain.AccountID]int

	// Admitted counts every slot grant, QueueFullRejections and WaitExpired
	// count refusals by the two local bounds, and Cancelled counts waiters whose
	// own context ended first. MaxInFlight, MaxWaiting, and TotalWaitMs are the
	// observed peaks and accumulated queueing cost of backpressure.
	Admitted            int64
	QueueFullRejections int64
	WaitExpired         int64
	Cancelled           int64
	MaxInFlight         int
	MaxWaiting          int
	TotalWaitMs         int64
}

// Snapshot copies the gate's current limits and counters.
func (g *Gate) Snapshot() Snapshot {
	g.mu.Lock()
	defer g.mu.Unlock()

	accounts := make(map[domain.AccountID]int, len(g.perAccount))
	for account, n := range g.perAccount {
		accounts[account] = n
	}
	return Snapshot{
		GlobalConcurrency:     g.cfg.GlobalConcurrency,
		MaxAccountConcurrency: g.cfg.MaxAccountConcurrency,
		WaitQueueDepth:        g.cfg.WaitQueueDepth,
		MaxWaitMs:             g.cfg.MaxWait.Milliseconds(),
		InFlight:              g.inFlight,
		Waiting:               g.waiting,
		AccountsInFlight:      accounts,
		Admitted:              g.counters.admitted,
		QueueFullRejections:   g.counters.queueFull,
		WaitExpired:           g.counters.waitExpired,
		Cancelled:             g.counters.cancelled,
		MaxInFlight:           g.counters.maxInFlight,
		MaxWaiting:            g.counters.maxWaiting,
		TotalWaitMs:           g.counters.waitedMs,
	}
}
