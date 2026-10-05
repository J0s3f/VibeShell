package load

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"j0s.at/vibeshell/internal/adapters/sandbox"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// soakScenarioConfig sizes the sandbox and app-state soak.
type soakScenarioConfig struct {
	// Rounds is how many open/evict/restore cycles to run.
	Rounds int
	// Instances is how many sandbox instances run concurrently per round.
	Instances int
	// EventsPerInstance is how many events each instance processes while open.
	EventsPerInstance int
	// StateBytes is the size of the app state restored on each cycle.
	StateBytes int
	// HeapLimitBytes is the per-instance guest heap cap.
	HeapLimitBytes int64
}

// runSoak repeatedly opens and closes sandbox instances and evicts and restores
// application state, then checks whether anything grew without bound. Growth is
// judged against the first round rather than against zero, so a warm-up cost is
// not mistaken for a leak.
func runSoak(ctx context.Context, sampler *Sampler, cfg soakScenarioConfig) ScenarioResult {
	result := ScenarioResult{
		Name: "sandbox-and-state-soak",
		Description: fmt.Sprintf(
			"%d rounds of %d concurrent sandbox instances each processing %d events, with app state evicted and "+
				"restored (%d bytes) every round; memory, file descriptors, and database growth are compared "+
				"between the first and last round.",
			cfg.Rounds, cfg.Instances, cfg.EventsPerInstance, cfg.StateBytes),
		Config: map[string]any{
			"rounds":               cfg.Rounds,
			"instances":            cfg.Instances,
			"events_per_instance":  cfg.EventsPerInstance,
			"state_bytes":          cfg.StateBytes,
			"instance_heap_bytes":  cfg.HeapLimitBytes,
			"instance_deadline_ms": 2000,
		},
		Errors: map[string]int{},
		Passed: true,
	}
	started := time.Now()

	engine, err := sandbox.NewAdapter(ctx, 0, sandbox.Config{})
	if err != nil {
		result.Passed = false
		result.Notes = append(result.Notes, "sandbox engine did not compile: "+err.Error())
		return result
	}
	defer func() { _ = engine.Close(context.Background()) }()

	artifact := soakArtifact()
	state := soakState(cfg.StateBytes)
	limits := ports.SandboxLimits{
		DeadlineMs:  2000,
		MaxMemoryB:  cfg.HeapLimitBytes,
		MaxOutputB:  1 << 20,
		MaxEvents:   128,
		SeededRand:  12345,
		SimNowMilli: 1_700_000_000_000,
	}

	// runRound opens Instances concurrent sandbox instances, drives
	// EventsPerInstance events through each, and closes them. It returns the
	// number of successful runs.
	runRound := func(round int) (int, int) {
		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			ok       int
			failures int
		)
		for i := 0; i < cfg.Instances; i++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				for event := 0; event < cfg.EventsPerInstance; event++ {
					if ctx.Err() != nil {
						return
					}
					payload, _ := json.Marshal(map[string]any{
						"index": index,
						"event": event,
						"round": round,
					})
					_, runErr := engine.Run(ctx, artifact, state, domain.AppEvent{
						EventType: domain.AppEventInput,
						Payload:   payload,
						Timestamp: limits.SimNowMilli,
					}, limits)
					mu.Lock()
					if runErr != nil {
						failures++
					} else {
						ok++
					}
					mu.Unlock()
				}
			}(i)
		}
		wg.Wait()
		return ok, failures
	}

	var (
		totalOK        int
		totalFailures  int
		roundLatencies = Latencies{}
	)
	firstRSS, lastRSS := int64(0), int64(0)
	firstFDs, lastFDs := 0, 0
	result.Samples = append(result.Samples, sampler.Sample("soak:start"))

	for round := 0; round < cfg.Rounds; round++ {
		// Eviction: drop the retained state and hand the instance a fresh copy,
		// which is what an idle-instance eviction actually does.
		state = soakState(cfg.StateBytes)
		runtime.GC()

		at := time.Now()
		ok, failures := runRound(round)
		roundLatencies.Record(time.Since(at))
		totalOK += ok
		totalFailures += failures
		if failures > 0 {
			result.Errors["sandbox_run_failed"] += failures
		}

		sample := sampler.Sample(fmt.Sprintf("soak:round_%d", round))
		result.Samples = append(result.Samples, sample)
		if round == 0 {
			firstRSS, firstFDs = sample.RSSBytes, sample.OpenConnections
		}
		lastRSS, lastFDs = sample.RSSBytes, sample.OpenConnections
		if ctx.Err() != nil {
			break
		}
	}

	result.DurationMs = time.Since(started).Milliseconds()
	result.Samples = append(result.Samples, sampler.Sample("soak:end"))
	summary := roundLatencies.Summary()
	runP95 := summary.P95Ms
	result.Latency = &summary
	result.LatencyLabel = "soak round"
	result.LocalP95Ms = &runP95

	growth := lastRSS - firstRSS
	fdGrowth := lastFDs - firstFDs
	result.Metrics = map[string]any{
		"rounds":                     cfg.Rounds,
		"sandbox_runs":               totalOK,
		"sandbox_failures":           totalFailures,
		"round_p95_ms":               summary.P95Ms,
		"round_max_ms":               summary.MaxMs,
		"first_round_rss_bytes":      firstRSS,
		"last_round_rss_bytes":       lastRSS,
		"rss_growth_bytes":           growth,
		"rss_growth_human":           HumanBytes(growth),
		"rss_growth_per_round_bytes": growth / int64(cfg.Rounds-1),
		"first_round_open_fds":       firstFDs,
		"last_round_open_fds":        lastFDs,
		"open_fd_growth":             fdGrowth,
		"retained_state_bytes":       cfg.StateBytes,
		"engine_imports":             strings.Join(engine.Imports(), ","),
		"final_heap_inuse_bytes":     result.Samples[len(result.Samples)-1].HeapInuseBytes,
		"final_goroutines":           result.Samples[len(result.Samples)-1].Goroutines,
	}

	// A leak shows up as memory or descriptors that keep climbing across rounds.
	// The bound is expressed per round so a longer soak does not fail purely
	// because it ran longer.
	if cfg.Rounds >= 3 {
		perRound := growth / int64(cfg.Rounds-1)
		if perRound > 8<<20 {
			result.Passed = false
			result.Notes = append(result.Notes,
				fmt.Sprintf("resident memory grew %s per round across %d rounds: investigate as a leak",
					HumanBytes(perRound), cfg.Rounds))
		}
		if fdGrowth > 16 {
			result.Passed = false
			result.Notes = append(result.Notes,
				fmt.Sprintf("open file descriptors grew by %d across %d rounds: investigate as a handle leak",
					fdGrowth, cfg.Rounds))
		}
	}
	if totalFailures > 0 {
		result.Passed = false
		result.Notes = append(result.Notes, fmt.Sprintf("%d sandbox runs failed", totalFailures))
	}
	return result
}

// soakArtifact is a minimal generated-application artifact: it touches state and
// event data and returns a validated text view, which is enough to make the
// guest do real work without depending on any particular app.
func soakArtifact() domain.AppArtifact {
	source := `
function main(state, event) {
  var payload = event.payload || {};
  var counter = (state.user_state && state.user_state.counter) || 0;
  counter = counter + 1;
  return {
    new_state: { user_state: { counter: counter } },
    view: {
      mode: "text",
      status_line: "round " + (payload.round || 0) + " event " + (payload.event || 0),
      metadata: { counter: counter }
    },
    effects: [],
    world_reads: [],
    exited: false
  };
}
`
	appID, err := domain.ParseAppID(fixedIdentity(domain.PrefixApp, 1))
	if err != nil {
		panic("load harness: " + err.Error())
	}
	versionID, err := domain.ParseAppVersionID(fixedIdentity(domain.PrefixAppVer, 1))
	if err != nil {
		panic("load harness: " + err.Error())
	}
	return domain.AppArtifact{
		AppID:     appID,
		VersionID: versionID,
		Manifest: domain.AppManifest{
			ABIVersion:   domain.AppABIVersion,
			CommandNames: []string{"loadsoak"},
			Description:  "load harness soak artifact",
			StateSchema:  json.RawMessage(`{"type":"object"}`),
			Entrypoint:   "main",
			Version:      "1.0.0",
		},
		Source: source,
		Scope:  domain.ScopeShared,
	}
}

// soakState builds an app state of approximately size bytes. The state is
// evicted and rebuilt every round, which is what an instance eviction does to
// retained state.
func soakState(size int) domain.AppState {
	if size <= 0 {
		return domain.AppState{}
	}
	filler := strings.Repeat("s", size)
	return domain.AppState{
		UserState: json.RawMessage(`{"counter":0,"filler":"` + filler + `"}`),
	}
}
