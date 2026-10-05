package load

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"j0s.at/vibeshell/internal/application"
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/routing"
	"j0s.at/vibeshell/internal/system"
)

// modelScenarioConfig sizes the admitted model-request scenario. The pool is
// deliberately small: PLAN 13 requires proving backpressure against a
// deterministic delayed double rather than making a large paid load test.
type modelScenarioConfig struct {
	// Sessions is how many concurrent sessions submit model-backed turns.
	Sessions int
	// TurnsPerSession is how many turns each session submits.
	TurnsPerSession int
	// Concurrency is the admitted global pool size.
	Concurrency int
	// MaxAccountConcurrency bounds one account inside that pool.
	MaxAccountConcurrency int
	// QueueDepth bounds waiters before the gate rejects.
	QueueDepth int
	// Delay is the injected provider service time.
	Delay time.Duration
	// Accounts is how many accounts the policy spreads requests over.
	Accounts int
	// TurnTimeout bounds one submitted turn.
	TurnTimeout time.Duration
}

// runModel submits concurrent turns through the real routing policy into the
// harness admission gate and the delayed provider double, and reports admission
// evidence separately from provider time.
//
// Turns are submitted at the coordinator boundary rather than over SSH: the
// transport is already exercised by the idle and mix scenarios, and admission
// control sits above it. The coordinator, routing policy, world store, and event
// store behind it are the shipped implementations.
func runModel(ctx context.Context, sampler *Sampler, cfg modelScenarioConfig) ScenarioResult {
	result := ScenarioResult{
		Name: "admitted-model-requests",
		Description: fmt.Sprintf(
			"%d sessions submit %d turns each through internal/routing into a harness-owned admission gate of %d "+
				"concurrent requests (max %d per account) against a deterministic provider double delayed by %s; "+
				"no live provider call is made and no credential is used.",
			cfg.Sessions, cfg.TurnsPerSession, cfg.Concurrency, cfg.MaxAccountConcurrency, cfg.Delay),
		Config: map[string]any{
			"sessions":                cfg.Sessions,
			"turns_per_session":       cfg.TurnsPerSession,
			"admitted_concurrency":    cfg.Concurrency,
			"max_account_concurrency": cfg.MaxAccountConcurrency,
			"queue_depth":             cfg.QueueDepth,
			"provider_delay":          cfg.Delay.String(),
			"accounts":                cfg.Accounts,
			"provider":                "deterministic delayed double (no live calls)",
			"admission_gate":          "harness-owned; see findings",
		},
		Errors: map[string]int{},
		Passed: true,
	}
	started := time.Now()
	result.Samples = append(result.Samples, sampler.Sample("model:start"))

	provider := &DelayedProvider{Delay: cfg.Delay}
	gate := NewAdmittedGateway(provider, AdmittedGatewayOptions{
		Concurrency:           cfg.Concurrency,
		QueueDepth:            cfg.QueueDepth,
		MaxAccountConcurrency: cfg.MaxAccountConcurrency,
	})
	engine := NewRoutedEngine(newHarnessRouter(gate, cfg.Accounts))

	dir, err := harnessTempDir("model")
	if err != nil {
		result.Passed = false
		result.Notes = append(result.Notes, err.Error())
		return result
	}
	stack, err := NewStack(ctx, StackOptions{Dir: dir, Engine: engine, SharingEnabled: true, WriterQueueDepth: 512})
	if err != nil {
		result.Passed = false
		result.Notes = append(result.Notes, err.Error())
		return result
	}
	defer cleanupStack(ctx, stack)

	var (
		mu      sync.Mutex
		turnLat Latencies
	)
	submitted := atomic.Int64{}
	var wg sync.WaitGroup
	for i := 0; i < cfg.Sessions; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			username := fmt.Sprintf("modeluser%02d", index)
			shell, openErr := stack.Coordinator.Open(ctx, harnessOpenRequest(username))
			if openErr != nil {
				mu.Lock()
				result.Errors["session_open"]++
				mu.Unlock()
				return
			}
			pump := newSessionPump(shell, cfg.TurnsPerSession)

			var sequence uint64
			for turn := 0; turn < cfg.TurnsPerSession && ctx.Err() == nil; turn++ {
				sequence++
				at := time.Now()
				_, acceptErr := shell.Accept(ctx, application.SessionInput{
					Kind:     application.InputCommand,
					Command:  fmt.Sprintf("mix-model turn %d", turn),
					Sequence: sequence,
				})
				if acceptErr != nil {
					mu.Lock()
					result.Errors["accept"]++
					mu.Unlock()
					continue
				}
				submitted.Add(1)
				// This submission's outcome is the completion signal. The
				// series below therefore holds routing, admission, and
				// coordination time; provider time is measured separately by the
				// double and is never folded into it.
				outcome, ok := pump.awaitOutcome(cfg.TurnTimeout)
				mu.Lock()
				if !ok {
					result.Errors["turn_timeout"]++
					mu.Unlock()
					continue
				}
				mu.Unlock()
				if outcome.Kind == application.OutcomeCompleted {
					turnLat.Record(time.Since(at))
				} else {
					mu.Lock()
					result.Errors["outcome_"+string(outcome.Kind)]++
					mu.Unlock()
				}
			}
			_ = shell.End(ctx, application.EndExit)
			<-pump.done
		}(i)
	}
	wg.Wait()

	// Give the event writer a moment to drain so the final sample is honest.
	time.Sleep(500 * time.Millisecond)
	result.DurationMs = time.Since(started).Milliseconds()
	result.Samples = append(result.Samples, sampler.Sample("model:end"))

	providerStats := provider.Stats()
	admission := gate.Report()
	local := turnLat.Summary()
	localP95 := local.P95Ms
	providerLatency := providerStats.Latency
	result.Latency = &local
	result.LatencyLabel = "local turn (routing, admission, coordination)"
	result.LocalP95Ms = &localP95
	result.ProviderMs = &providerLatency

	stats := stack.Events.Stats()
	result.Metrics = map[string]any{
		"turns_submitted":             submitted.Load(),
		"turns_completed":             turnLat.Count(),
		"local_turn_p95_ms":           local.P95Ms,
		"provider_time_p95_ms":        providerStats.Latency.P95Ms,
		"provider_time_is_injected":   true,
		"provider_requests":           providerStats.Requests,
		"provider_cancelled":          providerStats.Cancelled,
		"provider_deadline_exceeded":  providerStats.DeadlineExceeded,
		"provider_max_in_flight":      providerStats.MaxConcurrentInFlight,
		"admitted_concurrency_bound":  admission.Concurrency,
		"admitted_requests":           admission.Admitted,
		"admission_rejections":        admission.Rejected,
		"max_concurrent_in_flight":    admission.MaxInFlight,
		"max_waiting":                 admission.MaxWaiting,
		"max_account_in_flight":       admission.MaxAccountInFlight,
		"admission_queue_wait_p95_ms": admission.QueueWait.P95Ms,
		"event_writer_max_depth":      stats.MaxDepth,
		"event_writer_submitted":      stats.Submitted,
		"event_writer_rejected":       stats.Rejected,
		"database_bytes":              databaseBytes(stack.DBPath),
	}

	// The admission bound is the whole point of the scenario: exceeding it would
	// mean the gate bounds nothing.
	if admission.MaxInFlight > admission.Concurrency {
		result.Passed = false
		result.Notes = append(result.Notes,
			fmt.Sprintf("observed %d concurrent requests above the admitted bound of %d",
				admission.MaxInFlight, admission.Concurrency))
	}
	if cfg.MaxAccountConcurrency > 0 && admission.MaxAccountInFlight > cfg.MaxAccountConcurrency {
		result.Passed = false
		result.Notes = append(result.Notes,
			fmt.Sprintf("observed %d concurrent requests on one account above the per-account bound of %d",
				admission.MaxAccountInFlight, cfg.MaxAccountConcurrency))
	}
	if admission.Admitted == 0 {
		result.Passed = false
		result.Notes = append(result.Notes, "no request was admitted: backpressure cannot be claimed")
	}
	if providerStats.Requests == 0 {
		result.Passed = false
		result.Notes = append(result.Notes, "the provider double was never called")
	}
	return result
}

// newHarnessRouter builds a routing policy with the given number of accounts in
// one enabled tier. The policy is the shipped domain policy, so tier expansion,
// session affinity, and health transitions run for real rather than being
// approximated by the harness. No account carries a key reference: the double
// needs no credential, and a load harness must hold no secret.
func newHarnessRouter(gateway *AdmittedGateway, accounts int) *routing.Router {
	if accounts < 1 {
		accounts = 1
	}
	const product = "opencode"
	routeID := domain.MustParseRouteID(fixedIdentity(domain.PrefixRoute, 1))
	accountList := make([]domain.Account, 0, accounts)
	for i := 0; i < accounts; i++ {
		accountList = append(accountList, domain.Account{
			ID:                domain.MustParseAccountID(fixedIdentity(domain.PrefixAccount, i+1)),
			QuotaGroup:        fmt.Sprintf("loadgroup%02d", i),
			PermittedProducts: []string{product},
			Enabled:           true,
		})
	}
	policy := domain.RoutePolicy{
		Tiers: []domain.TierConfig{{
			Name:     "load-double",
			RouteIDs: []domain.RouteID{routeID},
			Enabled:  true,
		}},
		MaxAttempts:    2,
		TurnDeadlineMs: 60_000,
		ProbeBudget:    domain.ProbeBudget{MaxConcurrentProbes: 1, MaxProbesPerHour: 2},
	}
	return routing.NewRouter(gateway, NewMemoryHealthStore(), nil, system.NewClock(), system.NewRandom(), routing.Config{
		Policy:   policy,
		Routes:   []routing.RouteSpec{{ID: routeID, Product: product, Paid: false}},
		Accounts: accountList,
	})
}

// sessionPump drains one session's outputs and outcomes so the session never
// blocks on its own writer, and republishes outcomes for the submitting
// goroutine to await.
type sessionPump struct {
	outcomes chan application.SessionOutcome
	done     chan struct{}
}

// newSessionPump starts the drain for one session. The outcome channel is
// buffered for the session's whole turn budget so the drain never stalls.
func newSessionPump(shell application.Shell, turns int) *sessionPump {
	capacity := turns*2 + 8
	pump := &sessionPump{outcomes: make(chan application.SessionOutcome, capacity), done: make(chan struct{})}
	go func() {
		defer close(pump.done)
		outputs := shell.Outputs()
		outcomeCh := shell.Outcomes()
		for outputs != nil || outcomeCh != nil {
			select {
			case <-shell.Done():
				return
			case _, ok := <-outputs:
				if !ok {
					outputs = nil
				}
			case outcome, ok := <-outcomeCh:
				if !ok {
					outcomeCh = nil
					continue
				}
				select {
				case pump.outcomes <- outcome:
				default:
				}
			}
		}
	}()
	return pump
}

// awaitOutcome returns the session's next outcome, or false on timeout.
func (p *sessionPump) awaitOutcome(timeout time.Duration) (application.SessionOutcome, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case outcome, ok := <-p.outcomes:
		return outcome, ok
	case <-timer.C:
		return application.SessionOutcome{}, false
	}
}
