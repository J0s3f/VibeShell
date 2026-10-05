package load

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// idleScenarioConfig sizes the idle and churn scenario. The target comes from
// PLAN 13 (roughly 100 concurrent users); the harness reports the ceiling it
// actually reached instead of assuming the target was met.
type idleScenarioConfig struct {
	// Sessions is how many concurrent idle sessions to hold open.
	Sessions int
	// ChurnRounds is how many connect/disconnect cycles to run on top of them.
	ChurnRounds int
	// ChurnSessions is how many clients participate per churn round.
	ChurnSessions int
	// KeepaliveInterval is the idle keepalive period.
	KeepaliveInterval time.Duration
	// HoldTime is how long the idle sessions stay connected.
	HoldTime time.Duration
}

// runIdle holds a pool of idle sessions with keepalives while a separate pool
// churns connections: the PLAN 13 workload "100 idle SSH users with keepalives
// and repeated connect/disconnect cycles".
func runIdle(ctx context.Context, stack *Stack, sampler *Sampler, cfg idleScenarioConfig) ScenarioResult {
	result := ScenarioResult{
		Name: "idle-sessions-and-churn",
		Description: fmt.Sprintf(
			"%d concurrent idle sessions with keepalives held open while %d clients run %d connect/disconnect rounds; "+
				"achieved connections, memory, and CPU are reported rather than assumed.",
			cfg.Sessions, cfg.ChurnSessions, cfg.ChurnRounds),
		Config: map[string]any{
			"target_sessions":    cfg.Sessions,
			"churn_rounds":       cfg.ChurnRounds,
			"churn_sessions":     cfg.ChurnSessions,
			"keepalive_interval": cfg.KeepaliveInterval.String(),
			"hold_time":          cfg.HoldTime.String(),
		},
		Errors: map[string]int{},
		Passed: true,
	}
	started := time.Now()
	// The churn pool samples from its own goroutine, so every sample goes
	// through one locked collector rather than being appended from two places.
	sink := newSampleSink(sampler)
	sink.Add("idle:start")

	var (
		idleOpened   atomic.Int64
		idleFailed   atomic.Int64
		churnOpened  atomic.Int64
		churnFailed  atomic.Int64
		keepaliveOK  atomic.Int64
		keepaliveErr atomic.Int64
		liveCount    atomic.Int64
		peakLive     atomic.Int64
	)

	// recordLive updates the peak simultaneous-connection count.
	recordLive := func(delta int64) {
		current := liveCount.Add(delta)
		if delta > 0 {
			for {
				observed := peakLive.Load()
				if current <= observed || peakLive.CompareAndSwap(observed, current) {
					break
				}
			}
		}
	}

	idleClients := make(chan *Client, cfg.Sessions)
	stopKeepalive := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < cfg.Sessions; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			client, err := Dial(stack.Address, fmt.Sprintf("loaduser%03d", index), 120, 40)
			if err != nil {
				idleFailed.Add(1)
				result.Errors["idle_dial"]++
				return
			}
			defer func() { _ = client.Close() }()
			idleOpened.Add(1)
			recordLive(1)
			idleClients <- client

			ticker := time.NewTicker(cfg.KeepaliveInterval)
			defer ticker.Stop()
			for {
				select {
				case <-stopKeepalive:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := client.Keepalive(15 * time.Second); err != nil {
						keepaliveErr.Add(1)
						continue
					}
					keepaliveOK.Add(1)
				}
			}
		}(i)
	}

	// Churn pool: repeatedly connect and disconnect while the idle pool is held.
	churnDone := make(chan struct{})
	go func() {
		defer close(churnDone)
		for round := 0; round < cfg.ChurnRounds; round++ {
			for i := 0; i < cfg.ChurnSessions; i++ {
				if ctx.Err() != nil {
					return
				}
				client, err := Dial(stack.Address, fmt.Sprintf("churn%02d", i), 100, 30)
				if err != nil {
					churnFailed.Add(1)
					result.Errors["churn_dial"]++
					continue
				}
				churnOpened.Add(1)
				recordLive(1)
				// A short exchange proves the connection is interactive before
				// it is dropped, so this is session churn, not TCP churn.
				if _, err := client.SubmitLine("pwd", 60*time.Second); err != nil {
					result.Errors["churn_exchange"]++
				}
				recordLive(-1)
				_ = client.Close()
			}
			sink.Add(fmt.Sprintf("idle:churn_round_%d", round))
		}
	}()

	// Hold the idle pool for the configured time, sampling as we go.
	holdTick := cfg.HoldTime / 4
	if holdTick <= 0 {
		holdTick = time.Second
	}
	ticker := time.NewTicker(holdTick)
	deadline := time.Now().Add(cfg.HoldTime)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
		case <-ticker.C:
			sink.Add("idle:holding")
		}
		if ctx.Err() != nil {
			break
		}
	}
	ticker.Stop()
	close(stopKeepalive)
	<-churnDone
	wg.Wait()
	close(idleClients)
	// Drain the idle pool, then wait for the server side to notice: closing a
	// client is asynchronous, so the coordinator's session count may only be
	// read once it has had time to fall to zero. How long that takes is itself a
	// result, and any residual afterwards is a leak rather than a timing
	// artefact.
	drainStart := time.Now()
	for client := range idleClients {
		_ = client.Close()
	}
	residual := waitForSessionsToDrain(ctx, stack, 30*time.Second)
	drainTime := time.Since(drainStart)

	result.DurationMs = time.Since(started).Milliseconds()
	sink.Add("idle:end")
	result.Samples = sink.Snapshot()
	stats := stack.Events.Stats()

	peakRSS := int64(0)
	for _, s := range result.Samples {
		if s.RSSBytes > peakRSS {
			peakRSS = s.RSSBytes
		}
	}
	final := result.Samples[len(result.Samples)-1]
	achieved := idleOpened.Load()
	result.Metrics = map[string]any{
		"idle_sessions_requested":            cfg.Sessions,
		"idle_sessions_achieved":             achieved,
		"idle_sessions_failed":               idleFailed.Load(),
		"peak_client_connections":            peakLive.Load(),
		"churn_connections_opened":           churnOpened.Load(),
		"churn_connections_failed":           churnFailed.Load(),
		"keepalives_sent":                    keepaliveOK.Load(),
		"keepalives_failed":                  keepaliveErr.Load(),
		"handler_sessions_opened":            stack.Handler.SessionCount(),
		"coordinator_sessions_active_at_end": stack.Coordinator.ActiveSessions(),
		"event_writer_max_depth":             stats.MaxDepth,
		"event_writer_submitted":             stats.Submitted,
		"event_writer_rejected":              stats.Rejected,
		"peak_rss_bytes":                     peakRSS,
		"peak_rss_human":                     HumanBytes(peakRSS),
		"end_rss_bytes":                      final.RSSBytes,
		"end_heap_inuse_bytes":               final.HeapInuseBytes,
		"end_goroutines":                     final.Goroutines,
		"end_open_fds":                       final.OpenConnections,
		"cpu_ms_per_idle_session":            cpuPerConnection(sampler.CPUMillis(), achieved),
		"rss_bytes_per_idle_session":         bytesPer(peakRSS, achieved),
		"database_bytes":                     databaseBytes(stack.DBPath),
		"rss_bytes_per_idle_session_human":   HumanBytes(int64(bytesPer(peakRSS, achieved))),
	}
	if stack.Coordinator.ActiveSessions() != 0 {
		result.Passed = false
		result.Notes = append(result.Notes,
			fmt.Sprintf("%d sessions were still registered 30s after the last client closed: a leak",
				residual))
	}
	result.Metrics["session_drain_ms"] = drainTime.Milliseconds()
	result.Metrics["sessions_residual_after_drain"] = residual
	if achieved < int64(cfg.Sessions) {
		result.Passed = false
		result.Notes = append(result.Notes,
			fmt.Sprintf("only %d of %d requested idle sessions were established: report this ceiling, do not assume 100",
				achieved, cfg.Sessions))
	}
	return result
}

// waitForSessionsToDrain polls until the coordinator has no session left or the
// timeout expires, and returns the residual count.
func waitForSessionsToDrain(ctx context.Context, stack *Stack, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	for {
		residual := stack.Coordinator.ActiveSessions()
		if residual == 0 || time.Now().After(deadline) || ctx.Err() != nil {
			return residual
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// cpuPerConnection reports CPU milliseconds spent per established connection.
func cpuPerConnection(cpuMillis, connections int64) float64 {
	if connections == 0 {
		return 0
	}
	return float64(cpuMillis) / float64(connections)
}

// bytesPer reports how many bytes one unit accounts for.
func bytesPer(total int64, units int64) float64 {
	if units == 0 {
		return 0
	}
	return float64(total) / float64(units)
}

// databaseBytes reports the on-disk size of the harness database and its
// write-ahead log, which is the durable-growth figure PLAN 13 asks for.
func databaseBytes(path string) int64 {
	var total int64
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if info, err := os.Stat(candidate); err == nil {
			total += info.Size()
		}
	}
	return total
}
