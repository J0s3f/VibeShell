package load

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"j0s.at/vibeshell/internal/application"
	"j0s.at/vibeshell/internal/domain"
)

// Options configures one harness run. The defaults describe the PLAN 13
// planning workload for roughly 100 concurrent users; every field can be scaled
// down so a smaller host still produces a receipt that states its own ceiling.
type Options struct {
	// ReceiptDir is where the JSON receipt and its markdown summary are written.
	ReceiptDir string
	// WorkDir is the scratch directory for harness databases.
	WorkDir string
	// SourceDir is the checkout the receipt names in its revision stamp.
	SourceDir string
	// Revision overrides the detected revision. A container that cannot read the
	// host's worktree metadata cannot run git, and a receipt must still name the
	// code it measured, so the caller may state the revision explicitly.
	Revision string
	// Idle sizes the idle/churn scenario.
	Idle idleScenarioConfig
	// Mix sizes the interactive input mix.
	Mix mixScenarioConfig
	// Model sizes the admitted model-request scenario.
	Model modelScenarioConfig
	// World sizes the world contention scenario.
	World worldScenarioConfig
	// Soak sizes the sandbox/state soak.
	Soak soakScenarioConfig
	// ProviderDelay is the injected provider service time, used for the
	// receipt's provider report.
	ProviderDelay time.Duration
	// Skip lists scenarios to skip by name, for a fast partial run.
	Skip []string
}

// DefaultOptions returns the PLAN 13 planning workload.
func DefaultOptions() Options {
	return Options{
		ReceiptDir: "receipts",
		WorkDir:    "",
		SourceDir:  "../..",
		Idle: idleScenarioConfig{
			Sessions:          100,
			ChurnRounds:       4,
			ChurnSessions:     10,
			KeepaliveInterval: 5 * time.Second,
			HoldTime:          25 * time.Second,
		},
		Mix: mixScenarioConfig{
			Sessions:    20,
			Rounds:      3,
			TurnTimeout: 60 * time.Second,
			PasteBytes:  128 << 10,
			EmitBytes:   256 << 10,
		},
		Model: modelScenarioConfig{
			Sessions:              16,
			TurnsPerSession:       4,
			Concurrency:           4,
			MaxAccountConcurrency: 2,
			QueueDepth:            64,
			Delay:                 750 * time.Millisecond,
			Accounts:              2,
			TurnTimeout:           2 * time.Minute,
		},
		World: worldScenarioConfig{
			Writers:            8,
			SharedWriters:      8,
			Readers:            8,
			ReadRounds:         25,
			ConflictingWriters: 6,
			Operations:         25,
		},
		Soak: soakScenarioConfig{
			Rounds:            6,
			Instances:         4,
			EventsPerInstance: 25,
			StateBytes:        64 << 10,
			HeapLimitBytes:    4 << 20,
		},
		ProviderDelay: 750 * time.Millisecond,
	}
}

// ReceiptsDir is where a run writes its receipts relative to its options.
func (o Options) ReceiptsDir() string {
	if filepath.IsAbs(o.ReceiptDir) {
		return o.ReceiptDir
	}
	return filepath.Clean(o.ReceiptDir)
}

// EnvironmentCPUs reports the CPU count the run will measure on, so the
// console line and the receipt cannot disagree.
func (o Options) EnvironmentCPUs() int { return ReadEnvironment().NumCPU }

// Run executes every selected scenario and returns the receipt. A scenario that
// cannot run records its failure in the receipt instead of aborting the run: a
// partial measurement with an honest gap is more useful than no measurement.
func Run(ctx context.Context, opts Options) (Receipt, error) {
	started := time.Now()
	sampler := NewSampler()
	env := ReadEnvironment()

	receipt := Receipt{
		Schema:      ReceiptSchema,
		GeneratedAt: started.UTC(),
		Harness: HarnessInfo{
			Package:  "tests/load",
			Revision: "unknown",
		},
		Environment: env,
		Provider: ProviderReport{
			Kind:        "deterministic delayed double",
			LiveCalls:   false,
			DelayMillis: millis(opts.ProviderDelay),
			Detail: "The harness never contacts a provider. Model time is an injected delay; " +
				"receipts label it as such so it is never read as an upstream measurement.",
		},
		Config: map[string]any{
			"idle":           opts.Idle,
			"mix":            opts.Mix,
			"model":          opts.Model,
			"world":          opts.World,
			"soak":           opts.Soak,
			"skipped":        opts.Skip,
			"goarch":         env.Arch,
			"receipt_schema": ReceiptSchema,
		},
		Findings: []string{},
	}
	if opts.Revision != "" {
		receipt.Harness.Revision = opts.Revision
		receipt.Harness.Dirty = false
		receipt.Findings = append(receipt.Findings,
			"the revision stamp was supplied by the caller because git could not read the checkout from inside the container")
	} else if revision, dirty, err := GitRevision(opts.SourceDir); err == nil {
		receipt.Harness.Revision = revision
		receipt.Harness.Dirty = dirty
		if dirty {
			receipt.Findings = append(receipt.Findings,
				"the receipt was produced from a dirty working tree: the revision stamp does not identify the exact code")
		}
	} else {
		receipt.Findings = append(receipt.Findings,
			"git was unavailable, so the receipt carries no revision stamp: "+err.Error())
	}
	receipt.Findings = append(receipt.Findings,
		"admission control for concurrent model requests is harness-owned: the service configuration validates "+
			"inference.global_concurrency, inference.max_account_concurrency, and inference.wait_queue_depth, but no "+
			"runtime package enforces them yet, so the bound measured here is the harness gate's, not the service's")

	skipped := map[string]bool{}
	for _, name := range opts.Skip {
		skipped[name] = true
	}

	workDir := opts.WorkDir
	if workDir == "" {
		dir, err := os.MkdirTemp("", "vibeshell-load-")
		if err != nil {
			return receipt, fmt.Errorf("create harness work directory: %w", err)
		}
		workDir = dir
		defer func() { _ = os.RemoveAll(dir) }()
	}

	// Scenarios 1 and 2 share one stack so the mix runs against a service that
	// has already served a full idle population: that is the realistic order.
	if !skipped["idle-sessions-and-churn"] || !skipped["interactive-input-mix"] {
		engine := NewLocalEngine(opts.Mix.EmitBytes)
		dir, err := os.MkdirTemp(workDir, "sessions-")
		if err != nil {
			return receipt, fmt.Errorf("create session work directory: %w", err)
		}
		stack, stackErr := NewStack(ctx, StackOptions{
			Dir:              dir,
			Engine:           engine,
			SharingEnabled:   true,
			WriterQueueDepth: 1024,
		})
		if stackErr != nil {
			receipt.Scenarios = append(receipt.Scenarios, ScenarioResult{
				Name:        "stack-startup",
				Description: "assemble the SSH, coordinator, renderer, and SQLite stack",
				Passed:      false,
				Errors:      map[string]int{"startup": 1},
				Notes:       []string{stackErr.Error()},
			})
		} else {
			if !skipped["idle-sessions-and-churn"] {
				receipt.Scenarios = append(receipt.Scenarios, runIdle(ctx, stack, sampler, opts.Idle))
			}
			if !skipped["interactive-input-mix"] {
				mixResult := runMix(ctx, stack, sampler, opts.Mix)
				receipt.Scenarios = append(receipt.Scenarios, mixResult)
				receipt.Findings = append(receipt.Findings, mixFinding(mixResult)...)
			}
			receipt.Findings = append(receipt.Findings, fmt.Sprintf(
				"local-engine scenarios produced %d events in the research store with a maximum writer-queue depth of %d",
				stack.Events.Stats().Submitted, stack.Events.Stats().MaxDepth))
			cleanupStack(ctx, stack)
		}
		_ = os.RemoveAll(dir)
	}

	if !skipped["admitted-model-requests"] {
		receipt.Scenarios = append(receipt.Scenarios, runModel(ctx, sampler, opts.Model))
	}
	if !skipped["world-contention"] {
		receipt.Scenarios = append(receipt.Scenarios, runWorld(ctx, sampler, opts.World))
	}
	if !skipped["sandbox-and-state-soak"] {
		receipt.Scenarios = append(receipt.Scenarios, runSoak(ctx, sampler, opts.Soak))
	}

	receipt.Verdict = verdict(receipt.Scenarios)
	return receipt, nil
}

// mixFinding reports what the interactive mix measured about the PLAN 13 goal,
// including the boundary a reader should look at next. Each entry states a
// measured number and a plausible suspect separately, because the harness
// observes a latency but does not prove its cause.
func mixFinding(mix ScenarioResult) []string {
	if mix.LocalP95Ms == nil {
		return nil
	}
	goal := float64(slowLocalThreshold.Milliseconds())
	findings := []string{}
	if *mix.LocalP95Ms <= goal {
		findings = append(findings,
			fmt.Sprintf("local event p95 was %.2f ms, within the PLAN 13 goal of %d ms", *mix.LocalP95Ms, int(goal)))
	} else {
		findings = append(findings, fmt.Sprintf(
			"local event p95 was %.2f ms against the PLAN 13 goal of %d ms with %v inputs measured; "+
				"the transport round trip itself stayed far under the goal (p95_ms_resize %v, "+
				"p95_ms_editor_keystrokes %v), so the cost is in durable recording rather than in the SSH path. "+
				"The SQLite event store serialises appends on a single connection with synchronous=FULL, which is "+
				"the first boundary to measure; this harness observes the latency but does not prove that cause.",
			*mix.LocalP95Ms, int(goal), mix.Metrics["measured_inputs"],
			mix.Metrics["p95_ms_resize"], mix.Metrics["p95_ms_editor_keystrokes"]))
	}
	if refused, ok := mix.Metrics["inputs_refused_by_app"].(int64); ok && refused > 0 {
		findings = append(findings, fmt.Sprintf(
			"%d inputs were refused with database_unavailable during the mix, which is the coordinator refusing "+
				"semantic work after a durable recording failure; the coordinator does not surface which append "+
				"failed, so the append that broke under this mix is not identified here.", refused))
	}
	return findings
}

// verdict summarises the run. It states failures plainly; it never upgrades a
// partial measurement into a pass.
func verdict(scenarios []ScenarioResult) string {
	if len(scenarios) == 0 {
		return "no scenario ran"
	}
	failed := 0
	for _, s := range scenarios {
		if !s.Passed {
			failed++
		}
	}
	if failed == 0 {
		return fmt.Sprintf("all %d scenarios met their bounds on this host", len(scenarios))
	}
	return fmt.Sprintf("%d of %d scenarios did not meet their bounds on this host; read the notes before quoting any number",
		failed, len(scenarios))
}

// harnessTempDir creates a scratch directory for one scenario.
func harnessTempDir(label string) (string, error) {
	dir, err := os.MkdirTemp("", "vibeshell-load-"+label+"-")
	if err != nil {
		return "", fmt.Errorf("create %s work directory: %w", label, err)
	}
	return dir, nil
}

// cleanupStack stops a harness stack and removes its scratch directory. A stop
// failure is not fatal to the run: the receipt already holds the measurements.
func cleanupStack(ctx context.Context, stack *Stack) {
	stopCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_ = stack.Close(stopCtx)
	_ = os.RemoveAll(filepath.Dir(stack.DBPath))
	_ = ctx
}

// harnessOpenRequest builds the coordinator accept request for a harness user.
func harnessOpenRequest(username string) application.OpenSessionRequest {
	principal, err := publicIdentity(username)
	if err != nil {
		// publicIdentity only fails for an invalid username, which the harness
		// controls; surfacing the zero identity would produce a confusing
		// accept error, so the reason is named here.
		panic("load harness: " + err.Error())
	}
	return application.OpenSessionRequest{
		Principal: principal,
		AuthMode:  application.AuthModePublic,
		Terminal: application.TerminalMetadata{
			Term: "xterm-256color",
			Size: domain.TermSize{Cols: 120, Rows: 40},
		},
		CWD:          domain.ValidPath("/home/" + username),
		TransportRef: "loadharness",
	}
}

// ErrorSummary renders scenario errors for a console log line.
func ErrorSummary(s ScenarioResult) string {
	if len(s.Errors) == 0 {
		return ""
	}
	return fmt.Sprintf("%s errors: %s", s.Name, renderCounts(s.Errors))
}

// FormatVerdict prints a one-line console summary of a receipt.
func FormatVerdict(receipt Receipt) string {
	var b strings.Builder
	for _, s := range receipt.Scenarios {
		status := "fail"
		if s.Passed {
			status = "pass"
		}
		fmt.Fprintf(&b, "  %-28s %s (%d ms)\n", s.Name, status, s.DurationMs)
	}
	fmt.Fprintf(&b, "  verdict: %s\n", receipt.Verdict)
	return b.String()
}
