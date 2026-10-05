// Package observability provides operational logging, metrics, and status reporting
// for VibeShell.
package observability

import (
	"testing"
)

func TestRegistry_Counter_Basic(t *testing.T) {
	r := NewRegistry()
	c, err := r.Counter(MetricModelRequestsTotal, Labels(
		MetricLabel{LabelResult, "success"},
		MetricLabel{LabelComponent, "routing"},
	))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	c.Inc()
	c.Add(5)

	if c.Value() != 6 {
		t.Errorf("expected 6, got %d", c.Value())
	}
}

func TestRegistry_Gauge_Basic(t *testing.T) {
	r := NewRegistry()
	g, err := r.Gauge(MetricSessionsActive, Labels(
		MetricLabel{LabelComponent, "ssh"},
	))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	g.Set(10)
	g.Add(5)
	g.Sub(3)

	if g.Value() != 12 {
		t.Errorf("expected 12, got %d", g.Value())
	}
}

func TestRegistry_RejectsUnknownMetricName(t *testing.T) {
	r := NewRegistry()
	_, err := r.Counter("unknown_metric", nil)
	if err == nil {
		t.Error("expected error for unknown metric name")
	}
}

func TestRegistry_RejectsUnknownLabelName(t *testing.T) {
	r := NewRegistry()
	_, err := r.Counter(MetricModelRequestsTotal, Labels(
		MetricLabel{"unknown_label", "value"},
	))
	if err == nil {
		t.Error("expected error for unknown label name")
	}
}

func TestRegistry_RejectsUnboundedLabelValue(t *testing.T) {
	r := NewRegistry()

	// These should all be rejected because the values are not in the allowed set
	testCases := []struct {
		name   string
		labels map[LabelName]string
	}{
		{"raw_command", Labels(MetricLabel{LabelOperation, "rm -rf /"})},
		{"username", Labels(MetricLabel{LabelResult, "alice"})},
		{"api_key", Labels(MetricLabel{LabelErrorType, "sk-abc123"})},
		{"unbounded_tier", Labels(MetricLabel{LabelTier, "99"})},
		{"custom_protocol", Labels(MetricLabel{LabelProtocol, "custom"})},
		{"raw_component", Labels(MetricLabel{LabelComponent, "my-custom-component"})},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.Counter(MetricModelRequestsTotal, tc.labels)
			if err == nil {
				t.Errorf("expected error for unbounded label value %v", tc.labels)
			}
		})
	}
}

func TestRegistry_AllowsBoundedLabelValues(t *testing.T) {
	r := NewRegistry()

	// All these should succeed
	validLabels := []map[LabelName]string{
		Labels(MetricLabel{LabelResult, "success"}),
		Labels(MetricLabel{LabelResult, "error"}),
		Labels(MetricLabel{LabelResult, "timeout"}),
		Labels(MetricLabel{LabelResult, "cancelled"}),
		Labels(MetricLabel{LabelResult, "retry"}),

		Labels(MetricLabel{LabelComponent, "ssh"}),
		Labels(MetricLabel{LabelComponent, "sandbox"}),
		Labels(MetricLabel{LabelComponent, "routing"}),
		Labels(MetricLabel{LabelComponent, "storage"}),
		Labels(MetricLabel{LabelComponent, "config"}),
		Labels(MetricLabel{LabelComponent, "terminal"}),
		Labels(MetricLabel{LabelComponent, "model"}),
		Labels(MetricLabel{LabelComponent, "world"}),
		Labels(MetricLabel{LabelComponent, "export"}),
		Labels(MetricLabel{LabelComponent, "health"}),

		Labels(MetricLabel{LabelTier, "1"}),
		Labels(MetricLabel{LabelTier, "2"}),
		Labels(MetricLabel{LabelTier, "3"}),
		Labels(MetricLabel{LabelTier, "4"}),
		Labels(MetricLabel{LabelTier, "5"}),

		Labels(MetricLabel{LabelProtocol, "chat"}),
		Labels(MetricLabel{LabelProtocol, "responses"}),
		Labels(MetricLabel{LabelProtocol, "messages"}),
		Labels(MetricLabel{LabelProtocol, "gemini"}),

		Labels(MetricLabel{LabelHealthStatus, "healthy"}),
		Labels(MetricLabel{LabelHealthStatus, "cooling"}),
		Labels(MetricLabel{LabelHealthStatus, "probing"}),
		Labels(MetricLabel{LabelHealthStatus, "quarantined"}),

		Labels(MetricLabel{LabelErrorType, "quota"}),
		Labels(MetricLabel{LabelErrorType, "rate_limit"}),
		Labels(MetricLabel{LabelErrorType, "invalid_credential"}),
		Labels(MetricLabel{LabelErrorType, "model_removed"}),
		Labels(MetricLabel{LabelErrorType, "provider_outage"}),
		Labels(MetricLabel{LabelErrorType, "network_timeout"}),
		Labels(MetricLabel{LabelErrorType, "context_too_long"}),
		Labels(MetricLabel{LabelErrorType, "invalid_response"}),
		Labels(MetricLabel{LabelErrorType, "content_rejection"}),
		Labels(MetricLabel{LabelErrorType, "unknown"}),

		Labels(MetricLabel{LabelProbeType, "catalogue"}),
		Labels(MetricLabel{LabelProbeType, "inference"}),
		Labels(MetricLabel{LabelProbeType, "capability"}),

		Labels(MetricLabel{LabelShutdownPhase, "stop_accepting"}),
		Labels(MetricLabel{LabelShutdownPhase, "cancel_work"}),
		Labels(MetricLabel{LabelShutdownPhase, "flush_events"}),
		Labels(MetricLabel{LabelShutdownPhase, "close_sessions"}),
		Labels(MetricLabel{LabelShutdownPhase, "checkpoint"}),
		Labels(MetricLabel{LabelShutdownPhase, "complete"}),

		Labels(
			MetricLabel{LabelResult, "success"},
			MetricLabel{LabelComponent, "routing"},
			MetricLabel{LabelTier, "1"},
		),
	}

	for i, labels := range validLabels {
		t.Run("valid_"+string(rune(i)), func(t *testing.T) {
			_, err := r.Counter(MetricModelRequestsTotal, labels)
			if err != nil {
				t.Errorf("unexpected error for valid labels %v: %v", labels, err)
			}
		})
	}
}

func TestRegistry_Snapshot(t *testing.T) {
	r := NewRegistry()
	c, _ := r.Counter(MetricModelRequestsTotal, Labels(
		MetricLabel{LabelResult, "success"},
	))
	g, _ := r.Gauge(MetricSessionsActive, Labels(
		MetricLabel{LabelComponent, "ssh"},
	))

	c.Inc()
	c.Inc()
	g.Set(5)

	snap := r.Snapshot()

	if len(snap) != 2 {
		t.Errorf("expected 2 metrics, got %d", len(snap))
	}

	// Find our counter
	var foundCounter uint64
	var foundGauge int64
	for _, entry := range snap {
		if entry.Name == MetricModelRequestsTotal && entry.Type == MetricTypeCounter {
			foundCounter = entry.Value.(uint64)
		}
		if entry.Name == MetricSessionsActive && entry.Type == MetricTypeGauge {
			foundGauge = entry.Value.(int64)
		}
	}
	if foundCounter != 2 {
		t.Errorf("expected counter value 2, got %d", foundCounter)
	}
	if foundGauge != 5 {
		t.Errorf("expected gauge value 5, got %d", foundGauge)
	}
}

func TestRegistry_ConcurrentAccess(t *testing.T) {
	r := NewRegistry()
	c, _ := r.Counter(MetricModelRequestsTotal, Labels(
		MetricLabel{LabelResult, "success"},
	))

	done := make(chan struct{})
	for i := 0; i < 100; i++ {
		go func() {
			for {
				select {
				case <-done:
					return
				default:
					c.Inc()
				}
			}
		}()
	}

	// Let it run for a bit
	for i := 0; i < 1000; i++ {
		c.Inc()
	}

	close(done)
	// Just verify no panic and value is reasonable
	if c.Value() < 1000 {
		t.Errorf("counter value too low: %d", c.Value())
	}
}

func TestMetrics_AllowedLabelValuesCoverage(t *testing.T) {
	// Ensure every allowed label name has at least one allowed value
	for name, values := range AllowedLabelValues {
		if len(values) == 0 {
			t.Errorf("label %q has no allowed values", name)
		}
	}
}

func TestMetrics_AllMetricNamesHaveTests(t *testing.T) {
	// This test documents all metric names that should be tested.
	// If you add a new metric, add a test case for it.
	metricNames := []MetricName{
		MetricSessionsConnected,
		MetricSessionsActive,
		MetricModelRequestsTotal,
		MetricModelRequestsActive,
		MetricModelRequestDuration,
		MetricModelRequestErrors,
		MetricQueueDepth,
		MetricQueueLatency,
		MetricSandboxInstances,
		MetricSandboxMemory,
		MetricSandboxEvictions,
		MetricEventLatency,
		MetricRouteFailures,
		MetricRouteProbes,
		MetricWriterBackpressure,
		MetricWorldConflicts,
		MetricExportProgressBytes,
		MetricExportProgressItems,
		MetricDiskFree,
		MetricMemoryUsed,
		MetricConfigReloads,
		MetricHealthChecks,
		MetricHealthCheckFailures,
	}

	for _, name := range metricNames {
		t.Run(string(name), func(t *testing.T) {
			r := NewRegistry()
			_, err := r.Counter(name, Labels(MetricLabel{LabelResult, "success"}))
			if err != nil {
				t.Errorf("metric %q should be valid: %v", name, err)
			}
		})
	}
}
