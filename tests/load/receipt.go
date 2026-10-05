package load

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ReceiptSchema identifies the receipt document format. A reader can tell an
// older receipt from a newer one without parsing prose.
const ReceiptSchema = "vibeshell.load.receipt/1"

// Receipt is the complete record of one harness run.
type Receipt struct {
	Schema      string           `json:"schema"`
	GeneratedAt time.Time        `json:"generated_at"`
	Harness     HarnessInfo      `json:"harness"`
	Environment Environment      `json:"environment"`
	Provider    ProviderReport   `json:"provider"`
	Config      map[string]any   `json:"config"`
	Scenarios   []ScenarioResult `json:"scenarios"`
	Findings    []string         `json:"findings"`
	Verdict     string           `json:"verdict"`
}

// HarnessInfo identifies the code that produced the receipt.
type HarnessInfo struct {
	Package  string `json:"package"`
	Revision string `json:"revision"`
	Dirty    bool   `json:"dirty"`
}

// ProviderReport states, in the receipt itself, whether model time was real or
// doubled. PLAN 13 forbids presenting an upstream delay as a local benchmark,
// so this is the first thing a reader should find.
type ProviderReport struct {
	Kind        string  `json:"kind"`
	LiveCalls   bool    `json:"live_calls"`
	DelayMillis float64 `json:"delay_ms"`
	Detail      string  `json:"detail"`
}

// ScenarioResult is one scenario's measurements and verdict.
type ScenarioResult struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	DurationMs  int64           `json:"duration_ms"`
	Config      map[string]any  `json:"config,omitempty"`
	Metrics     map[string]any  `json:"metrics,omitempty"`
	Latency     *LatencySummary `json:"latency,omitempty"`
	// LatencyLabel names what Latency and LocalP95Ms measure, so a round time or
	// a read latency is never misread as a local event latency.
	LatencyLabel string           `json:"latency_label,omitempty"`
	LocalP95Ms   *float64         `json:"local_event_p95_ms,omitempty"`
	ProviderMs   *LatencySummary  `json:"provider_time,omitempty"`
	Samples      []ResourceSample `json:"resource_samples,omitempty"`
	Errors       map[string]int   `json:"errors,omitempty"`
	Passed       bool             `json:"passed"`
	Notes        []string         `json:"notes,omitempty"`
}

// WriteReceipt writes receipt as indented JSON and returns the file path.
func WriteReceipt(dir string, receipt Receipt) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create receipt directory: %w", err)
	}
	name := fmt.Sprintf("load-%s.json", receipt.GeneratedAt.UTC().Format("20060102T150405Z"))
	path := filepath.Join(dir, name)
	encoded, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode receipt: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		return "", fmt.Errorf("write receipt %s: %w", path, err)
	}
	return path, nil
}

// RenderSummary writes the human-readable companion to the JSON receipt. It is
// derived entirely from the same Receipt value, so the two can never disagree.
func RenderSummary(dir string, receipt Receipt) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create receipt directory: %w", err)
	}
	path := filepath.Join(dir, "summary.md")
	var b strings.Builder
	b.WriteString("# VibeShell capacity and soak receipt\n\n")
	fmt.Fprintf(&b, "- schema: `%s`\n", receipt.Schema)
	fmt.Fprintf(&b, "- generated: %s\n", receipt.GeneratedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "- harness: `%s` at `%s`%s\n", receipt.Harness.Package, receipt.Harness.Revision, dirtySuffix(receipt.Harness.Dirty))
	fmt.Fprintf(&b, "- provider time: %s (live calls: %v)\n", receipt.Provider.Kind, receipt.Provider.LiveCalls)
	fmt.Fprintf(&b, "- go: %s, CPUs: %d, GOMAXPROCS: %d, memory: %s\n",
		receipt.Environment.GoVersion, receipt.Environment.NumCPU,
		receipt.Environment.GOMAXPROCS, HumanBytes(receipt.Environment.MemTotalBytes))
	if receipt.Environment.CgroupCPUMax != "" {
		fmt.Fprintf(&b, "- cgroup cpu.max: `%s`, memory.max: `%s`\n", receipt.Environment.CgroupCPUMax, receipt.Environment.CgroupMemMax)
	}
	fmt.Fprintf(&b, "- verdict: **%s**\n\n", receipt.Verdict)

	b.WriteString("## Scenarios\n\n")
	for _, s := range receipt.Scenarios {
		mark := "FAIL"
		if s.Passed {
			mark = "pass"
		}
		fmt.Fprintf(&b, "### %s (%s, %d ms)\n\n%s\n\n", s.Name, mark, s.DurationMs, s.Description)
		if s.LocalP95Ms != nil {
			label := s.LatencyLabel
			if label == "" {
				label = "local event"
			}
			fmt.Fprintf(&b, "- %s p95: %.2f ms\n", label, *s.LocalP95Ms)
		}
		if s.ProviderMs != nil {
			fmt.Fprintf(&b, "- provider time (injected double): p50 %.2f ms, p95 %.2f ms over %d requests\n",
				s.ProviderMs.P50Ms, s.ProviderMs.P95Ms, s.ProviderMs.Count)
		}
		if len(s.Metrics) > 0 {
			b.WriteString("- metrics:\n")
			for _, line := range renderMetrics(s.Metrics, "  ") {
				b.WriteString(line)
			}
		}
		if len(s.Errors) > 0 {
			fmt.Fprintf(&b, "- errors: %s\n", renderCounts(s.Errors))
		}
		for _, note := range s.Notes {
			fmt.Fprintf(&b, "- note: %s\n", note)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Findings\n\n")
	if len(receipt.Findings) == 0 {
		b.WriteString("- none recorded\n")
	}
	for _, f := range receipt.Findings {
		fmt.Fprintf(&b, "- %s\n", f)
	}
	b.WriteString("\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("write summary %s: %w", path, err)
	}
	return path, nil
}

// renderMetrics prints a metric map in a stable key order so a diff between two
// receipts shows changed numbers, not reshuffled lines.
func renderMetrics(metrics map[string]any, indent string) []string {
	keys := make([]string, 0, len(metrics))
	for k := range metrics {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, fmt.Sprintf("%s- %s: %v\n", indent, k, metrics[k]))
	}
	return lines
}

func renderCounts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}

func dirtySuffix(dirty bool) string {
	if dirty {
		return " (dirty working tree)"
	}
	return ""
}

// GitRevision reports the short revision and dirty flag of the checkout. A
// receipt that cannot name its own revision is not a reproducible receipt, so
// the failure is reported rather than hidden.
func GitRevision(dir string) (string, bool, error) {
	describe, err := runGit(dir, "describe", "--always", "--dirty")
	if err != nil {
		return "unknown", false, err
	}
	return describe, strings.HasSuffix(describe, "-dirty"), nil
}
