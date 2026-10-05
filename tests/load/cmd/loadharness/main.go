// Command loadharness runs the VibeShell capacity and soak harness (PLAN 13,
// task D06) and writes receipts under tests/load/receipts.
//
// It performs no live provider call and needs no credential. Every run records
// the host it measured, so a receipt can be read against the machine that
// produced it.
//
// Usage inside the development container:
//
//	go run ./tests/load/cmd/loadharness
//	go run ./tests/load/cmd/loadharness -sessions 40 -hold 15s
//	go run ./tests/load/cmd/loadharness -skip idle-sessions-and-churn
//
// The process exits non-zero when an executed scenario missed one of its
// bounds, so a capacity regression is visible in CI instead of being read as a
// pass. A scenario whose target the host cannot sustain fails on purpose: the
// honest ceiling is the result.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"j0s.at/vibeshell/tests/load"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "loadharness: "+err.Error())
		os.Exit(1)
	}
}

// run parses flags, executes the harness, writes the receipts, and reports the
// verdict on the console.
func run() error {
	var (
		receipts  = flag.String("receipts", "tests/load/receipts", "directory the receipt and summary are written to")
		source    = flag.String("source", ".", "checkout directory the receipt names in its revision stamp")
		work      = flag.String("work", "", "scratch directory for harness databases (a temporary directory when empty)")
		revision  = flag.String("revision", "", "revision stamp for the receipt (used when git cannot read the checkout)")
		sessions  = flag.Int("sessions", 0, "override the concurrent session target (0 keeps the PLAN 13 value)")
		hold      = flag.Duration("hold", 0, "override how long idle sessions are held open")
		rounds    = flag.Int("rounds", 0, "override interactive rounds per mix client")
		providerD = flag.Duration("provider-delay", 0, "override the injected provider delay")
		soak      = flag.Int("soak-rounds", 0, "override the soak round count")
		skipList  = flag.String("skip", "", "comma-separated scenario names to skip")
		timeout   = flag.Duration("timeout", 60*time.Minute, "overall harness timeout")
	)
	flag.Parse()

	opts := load.DefaultOptions()
	opts.ReceiptDir = *receipts
	opts.SourceDir = *source
	opts.Revision = *revision
	opts.WorkDir = *work
	if *sessions > 0 {
		opts.Idle.Sessions = *sessions
		opts.Mix.Sessions = *sessions / 5
		if opts.Mix.Sessions < 1 {
			opts.Mix.Sessions = 1
		}
	}
	if *hold > 0 {
		opts.Idle.HoldTime = *hold
	}
	if *rounds > 0 {
		opts.Mix.Rounds = *rounds
	}
	if *providerD > 0 {
		opts.Model.Delay = *providerD
		opts.ProviderDelay = *providerD
	}
	if *soak > 0 {
		opts.Soak.Rounds = *soak
	}
	if *skipList != "" {
		opts.Skip = splitList(*skipList)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	fmt.Printf("loadharness: starting on %d CPUs; receipts -> %s\n",
		opts.EnvironmentCPUs(), opts.ReceiptsDir())
	receipt, err := load.Run(ctx, opts)
	if err != nil {
		return err
	}
	receiptPath, err := load.WriteReceipt(opts.ReceiptsDir(), receipt)
	if err != nil {
		return err
	}
	summaryPath, err := load.RenderSummary(opts.ReceiptsDir(), receipt)
	if err != nil {
		return err
	}

	fmt.Print(load.FormatVerdict(receipt))
	fmt.Printf("  receipt: %s\n  summary: %s\n", receiptPath, summaryPath)

	failed := 0
	for _, scenario := range receipt.Scenarios {
		if !scenario.Passed {
			failed++
			for _, note := range scenario.Notes {
				fmt.Printf("  ! %s: %s\n", scenario.Name, note)
			}
			if extra := load.ErrorSummary(scenario); extra != "" {
				fmt.Printf("  ! %s\n", extra)
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d scenarios missed their bounds", failed, len(receipt.Scenarios))
	}
	return nil
}

// splitList parses a comma-separated flag value, dropping empty entries.
func splitList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
