package load

import (
	"sort"
	"sync"
	"time"
)

// LatencySummary is the reported distribution of one measured latency series.
// It is deliberately coarse: a harness that printed every sample would bury the
// two numbers PLAN 13 asks for (local handling and provider time) in noise.
type LatencySummary struct {
	Count  int     `json:"count"`
	MinMs  float64 `json:"min_ms"`
	P50Ms  float64 `json:"p50_ms"`
	P95Ms  float64 `json:"p95_ms"`
	P99Ms  float64 `json:"p99_ms"`
	MaxMs  float64 `json:"max_ms"`
	MeanMs float64 `json:"mean_ms"`
}

// Latencies accumulates durations and reports their distribution. It is safe
// for concurrent use because scenarios time many clients at once.
type Latencies struct {
	mu      sync.Mutex
	samples []time.Duration
}

// Record adds one observation.
func (l *Latencies) Record(d time.Duration) {
	l.mu.Lock()
	l.samples = append(l.samples, d)
	l.mu.Unlock()
}

// Count returns how many observations were recorded.
func (l *Latencies) Count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.samples)
}

// Summary computes the distribution. Percentiles use the nearest-rank method on
// a sorted copy, which never invents an interpolation between samples that were
// not observed.
func (l *Latencies) Summary() LatencySummary {
	l.mu.Lock()
	sorted := append([]time.Duration(nil), l.samples...)
	l.mu.Unlock()
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	summary := LatencySummary{Count: len(sorted)}
	if len(sorted) == 0 {
		return summary
	}
	var total time.Duration
	for _, d := range sorted {
		total += d
	}
	summary.MinMs = millis(sorted[0])
	summary.MaxMs = millis(sorted[len(sorted)-1])
	summary.MeanMs = millis(total / time.Duration(len(sorted)))
	summary.P50Ms = millis(percentile(sorted, 0.50))
	summary.P95Ms = millis(percentile(sorted, 0.95))
	summary.P99Ms = millis(percentile(sorted, 0.99))
	return summary
}

// percentile returns the nearest-rank value at q over a sorted slice.
func percentile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(float64(len(sorted))*q + 0.999999) // ceil(len*q)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// millis converts a duration to fractional milliseconds for reporting.
func millis(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / float64(time.Millisecond)
}
