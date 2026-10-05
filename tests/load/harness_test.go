// Package load_test runs the capacity and soak harness from `go test`.
//
// The harness is deliberately excluded from the default test run: a capacity
// measurement takes minutes, binds every core the machine has, and writes files
// under tests/load/receipts. It therefore runs only when explicitly requested,
// either through the loadharness command or through VIBESHELL_LOAD=1 here.
package load_test

import (
	"context"
	"os"
	"testing"
	"time"

	"j0s.at/vibeshell/tests/load"
)

// runEnvVar opts a test run into executing the harness. A test run without it
// skips, so `go test -race ./...` stays fast and hermetic.
const runEnvVar = "VIBESHELL_LOAD"

// TestCapacityHarness runs every scenario and writes receipts.
//
// It is skipped unless VIBESHELL_LOAD=1 is set. The skip message names the
// command and the environment variable so a reader is never left guessing how
// to produce a receipt.
func TestCapacityHarness(t *testing.T) {
	if os.Getenv(runEnvVar) == "" {
		t.Skipf("capacity harness skipped: set %s=1 to run it, or use "+
			"go run ./tests/load/cmd/loadharness", runEnvVar)
	}

	opts := load.DefaultOptions()
	opts.SourceDir = "../.."
	opts.WorkDir = t.TempDir()
	if dir := os.Getenv("VIBESHELL_LOAD_RECEIPTS"); dir != "" {
		opts.ReceiptDir = dir
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	receipt, err := load.Run(ctx, opts)
	if err != nil {
		t.Fatalf("load harness: %v", err)
	}
	receiptPath, err := load.WriteReceipt(opts.ReceiptsDir(), receipt)
	if err != nil {
		t.Fatalf("write receipt: %v", err)
	}
	if _, err := load.RenderSummary(opts.ReceiptsDir(), receipt); err != nil {
		t.Fatalf("write summary: %v", err)
	}
	t.Logf("receipt written to %s\n%s", receiptPath, load.FormatVerdict(receipt))

	// A harness run that cannot meet a bound is a real result, not a broken
	// test, so it is reported rather than failed here: the receipt is the
	// deliverable and the verdict is part of it. The command exits non-zero
	// instead, which is what a pipeline should gate on.
	for _, scenario := range receipt.Scenarios {
		if !scenario.Passed {
			t.Logf("scenario %s missed its bounds: %v", scenario.Name, scenario.Notes)
		}
	}
}
