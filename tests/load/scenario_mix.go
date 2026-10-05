package load

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// mixScenarioConfig sizes the interactive mix scenario.
type mixScenarioConfig struct {
	// Sessions is how many concurrent interactive clients run the mix.
	Sessions int
	// Rounds is how many mixed interactions each client performs.
	Rounds int
	// TurnTimeout bounds one interaction so a stall is reported, not hung on.
	TurnTimeout time.Duration
	// PasteBytes is the size of the large bracketed paste.
	PasteBytes int
	// EmitBytes is the size of the large command output.
	EmitBytes int
}

// inputMix is the workload mix from PLAN 13: command submission, editor
// keystrokes, pager navigation, top-like refreshes, and a large paste/output
// operation. Counts are per round so the receipt states the exact mix that
// produced its latencies.
type inputMix struct {
	commands    int
	keystrokes  int
	pagerKeys   int
	topRefresh  int
	largeOutput int
	largePaste  int
	resizes     int
}

// String renders the mix for the receipt.
func (m inputMix) String() string {
	return fmt.Sprintf("commands=%d keystroke_batches=%d pager_keys=%d top_refreshes=%d large_output=%d large_paste=%d resizes=%d",
		m.commands, m.keystrokes, m.pagerKeys, m.topRefresh, m.largeOutput, m.largePaste, m.resizes)
}

// defaultMix is the per-round workload for one interactive client.
func defaultMix() inputMix {
	return inputMix{
		commands:    8,
		keystrokes:  3,
		pagerKeys:   3,
		topRefresh:  2,
		largeOutput: 1,
		largePaste:  1,
		resizes:     1,
	}
}

// slowLocalThreshold is the PLAN 13 goal for p95 local editing and navigation.
// A measurement above it is recorded rather than hidden, so a host that cannot
// meet the goal shows the shortfall instead of quietly passing.
const slowLocalThreshold = 50 * time.Millisecond

// pagerKeySequences are the navigation keys a pager-style view receives.
var pagerKeySequences = []string{"\x1b[5~", "\x1b[B", "\x1b[6~", "\x1b[A", "\x1b[5~"}

// runMix drives a realistic interactive mix across concurrent SSH clients and
// reports local event latency per input class, separately from any provider
// time. The engine behind this scenario is the model-free local double, so the
// provider column is genuinely zero rather than an estimate.
func runMix(ctx context.Context, stack *Stack, sampler *Sampler, cfg mixScenarioConfig) ScenarioResult {
	mix := defaultMix()
	result := ScenarioResult{
		Name: "interactive-input-mix",
		Description: fmt.Sprintf(
			"%d concurrent SSH clients running %d rounds of a realistic mix (%s); "+
				"local event latency is measured from the client's final input byte to the prompt that follows the turn, "+
				"with the model-free engine so provider time is exactly zero.",
			cfg.Sessions, cfg.Rounds, mix),
		Config: map[string]any{
			"sessions":     cfg.Sessions,
			"rounds":       cfg.Rounds,
			"mix":          mix.String(),
			"paste_bytes":  cfg.PasteBytes,
			"emit_bytes":   cfg.EmitBytes,
			"turn_timeout": cfg.TurnTimeout.String(),
			"engine":       "load-harness local engine (no provider)",
			"goal_p95_ms":  slowLocalThreshold.Milliseconds(),
		},
		Errors: map[string]int{},
		Passed: true,
	}
	started := time.Now()
	result.Samples = append(result.Samples, sampler.Sample("mix:start"))

	var (
		mu             sync.Mutex
		perClass       = map[string]*Latencies{}
		latency        Latencies
		slowResponses  int
		allErrs        = map[string]int{}
		recordFailure  = func(kind string) { allErrs[kind]++ }
		classForSeries = func(name string) *Latencies {
			mu.Lock()
			defer mu.Unlock()
			if perClass[name] == nil {
				perClass[name] = &Latencies{}
			}
			return perClass[name]
		}
	)

	pastePayload := buildPastePayload(cfg.PasteBytes)
	classes := map[string]*Latencies{
		"command":           classForSeries("command"),
		"editor_keystrokes": classForSeries("editor_keystrokes"),
		"pager_navigation":  classForSeries("pager_navigation"),
		"top_refresh":       classForSeries("top_refresh"),
		"large_output":      classForSeries("large_output"),
		"large_paste":       classForSeries("large_paste"),
		"resize":            classForSeries("resize"),
	}

	var wg sync.WaitGroup
	for i := 0; i < cfg.Sessions; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			client, err := Dial(stack.Address, fmt.Sprintf("mixuser%02d", index), 200, 60)
			if err != nil {
				mu.Lock()
				recordFailure("dial")
				mu.Unlock()
				return
			}
			defer func() { _ = client.Close() }()

			run := mixRun{
				client:   client,
				cfg:      cfg,
				mix:      mix,
				mu:       &mu,
				lat:      &latency,
				slow:     &slowResponses,
				failures: &allErrs,
				classes:  classes,
				paste:    pastePayload,
			}
			for round := 0; round < cfg.Rounds && ctx.Err() == nil; round++ {
				run.round(ctx)
			}
		}(i)
	}
	wg.Wait()

	result.DurationMs = time.Since(started).Milliseconds()
	result.Samples = append(result.Samples, sampler.Sample("mix:end"))

	summary := latency.Summary()
	p95 := summary.P95Ms
	result.Latency = &summary
	result.LatencyLabel = "local event"
	result.LocalP95Ms = &p95
	if p95 > float64(slowLocalThreshold.Milliseconds()) {
		// PLAN 13's goal is p95 local editing and navigation under 50 ms. A
		// measurement above it is a finding about this host and configuration,
		// so it is stated rather than averaged into a pass.
		result.Notes = append(result.Notes,
			fmt.Sprintf("local event p95 was %.2f ms, above the PLAN 13 goal of %d ms; %d of %d measured inputs exceeded it",
				p95, slowLocalThreshold.Milliseconds(), slowResponses, latency.Count()))
	}
	// Provider time is zero for this scenario and is stated as zero rather than
	// omitted, so a reader cannot mistake silence for an unmeasured value.
	zero := LatencySummary{}
	result.ProviderMs = &zero
	result.Errors = allErrs

	stats := stack.Events.Stats()
	refused, refusalReasons := stack.Handler.Refusals()
	result.Metrics = map[string]any{
		"local_event_p95_ms":      summary.P95Ms,
		"local_event_p50_ms":      summary.P50Ms,
		"local_event_p99_ms":      summary.P99Ms,
		"local_event_max_ms":      summary.MaxMs,
		"measured_inputs":         latency.Count(),
		"inputs_over_50ms":        slowResponses,
		"provider_time_ms":        0,
		"inputs_refused_by_app":   refused,
		"coordinator_turn_queue":  stack.Coordinator.Limits().MaxQueuedTurns,
		"handler_inputs_by_kind":  stack.Handler.InputCounts(),
		"prompts_written":         stack.Handler.Prompts(),
		"transport_dropped_bytes": stack.Handler.DroppedBytes(),
		"event_writer_max_depth":  stats.MaxDepth,
		"event_writer_submitted":  stats.Submitted,
		"event_writer_rejected":   stats.Rejected,
		"database_bytes":          databaseBytes(stack.DBPath),
		"cpu_ms_per_input":        cpuPerConnection(sampler.CPUMillis(), int64(latency.Count())),
	}
	for name, series := range perClass {
		classSummary := series.Summary()
		result.Metrics["p95_ms_"+name] = classSummary.P95Ms
		result.Metrics["count_"+name] = classSummary.Count
	}
	if summary.Count == 0 {
		result.Passed = false
		result.Notes = append(result.Notes, "no input completed: the mix produced no measurement")
	}
	if refused > 0 {
		// A refusal is the coordinator's bounded turn queue doing its job, not a
		// failure, but it is a capacity signal and must be visible.
		result.Notes = append(result.Notes,
			fmt.Sprintf("the application refused %d inputs because a session's turn queue of %d was full; "+
				"refusals are bounded and reported to the client as a prompt, never as a hang. Example reasons: %s",
				refused, stack.Coordinator.Limits().MaxQueuedTurns, strings.Join(refusalReasons, " | ")))
	}
	return result
}

// mixRun carries one client's round state. Grouping the accumulators keeps the
// per-round body readable instead of threading nine parameters through it.
type mixRun struct {
	client   *Client
	cfg      mixScenarioConfig
	mix      inputMix
	mu       *sync.Mutex
	lat      *Latencies
	slow     *int
	failures *map[string]int
	classes  map[string]*Latencies
	paste    string
}

// record files one measurement, or one failure when the step did not complete.
// The comparison against the goal threshold is recorded rather than enforced, so
// a shortfall is visible in the receipt instead of hiding inside an average.
func (r mixRun) record(class, label string, d time.Duration, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		(*r.failures)[label]++
		return
	}
	r.classes[class].Record(d)
	r.lat.Record(d)
	if d > slowLocalThreshold {
		*r.slow++
	}
}

// round performs one round of the mix for a single client. Every step is timed
// the same way, so the classes are comparable.
func (r mixRun) round(ctx context.Context) {
	// Ordinary command submission.
	for i := 0; i < r.mix.commands; i++ {
		d, err := r.client.SubmitLine("echo load-harness-mix", r.cfg.TurnTimeout)
		r.record("command", "command_failed", d, err)
		if ctx.Err() != nil {
			return
		}
	}

	// Editor keystrokes: raw printable input that the line editor consumes and
	// echoes. This is the PLAN 13 "editor keystroke" class without a turn.
	typed := strings.Repeat("vibeshell mix keystroke ", 4)
	erase := strings.Repeat("\x7f", len(typed))
	for i := 0; i < r.mix.keystrokes; i++ {
		d, err := r.client.TypeKeys(typed, r.cfg.TurnTimeout)
		r.record("editor_keystrokes", "keystrokes_failed", d, err)
		// Clear the draft so the next round starts from a known state.
		_, _ = r.client.TypeKeys(erase, r.cfg.TurnTimeout)
		if ctx.Err() != nil {
			return
		}
	}

	// Pager navigation: page and arrow keys inside a full-screen view.
	for i := 0; i < r.mix.pagerKeys && i < len(pagerKeySequences); i++ {
		d, err := r.client.TypeKeys(pagerKeySequences[i], r.cfg.TurnTimeout)
		r.record("pager_navigation", "pager_failed", d, err)
		if ctx.Err() != nil {
			return
		}
	}

	// Top-like refreshes: repeated full-screen refresh commands.
	for i := 0; i < r.mix.topRefresh; i++ {
		d, err := r.client.SubmitLine("top", r.cfg.TurnTimeout)
		r.record("top_refresh", "top_failed", d, err)
		if ctx.Err() != nil {
			return
		}
	}

	// Large output: one command whose body is many times the per-session output
	// queue bound, so the transport's bounded queue is exercised.
	for i := 0; i < r.mix.largeOutput; i++ {
		d, err := r.client.SubmitLine("emit", r.cfg.TurnTimeout)
		r.record("large_output", "large_output_failed", d, err)
		if ctx.Err() != nil {
			return
		}
	}

	// Large paste: one bracketed paste inside the application paste bound.
	for i := 0; i < r.mix.largePaste; i++ {
		d, err := r.client.Paste(r.paste, r.cfg.TurnTimeout)
		r.record("large_paste", "large_paste_failed", d, err)
		if ctx.Err() != nil {
			return
		}
	}

	// Real window changes through the SSH transport.
	for i := 0; i < r.mix.resizes; i++ {
		d, err := r.client.Resize(160+i*10, 48+i*4)
		r.record("resize", "resize_failed", d, err)
		if ctx.Err() != nil {
			return
		}
	}
}

// buildPastePayload returns deterministic paste content of the requested size.
func buildPastePayload(size int) string {
	if size <= 0 {
		return ""
	}
	const line = "the quick brown fox jumps over the lazy dog 0123456789\n"
	repeats := size/len(line) + 1
	return strings.Repeat(line, repeats)[:size]
}
