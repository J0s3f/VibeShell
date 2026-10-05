// Package observability provides operational logging, metrics, and status reporting
// for VibeShell.
package observability

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// MetricName is a fixed metric name from the allowed set.
// All metrics must be declared here to enforce low cardinality.
type MetricName string

// Allowed metric names. These are the ONLY metrics that can be created.
// Adding a new metric requires updating this list and the corresponding test.
const (
	MetricSessionsConnected    MetricName = "sessions.connected"
	MetricSessionsActive       MetricName = "sessions.active"
	MetricModelRequestsTotal   MetricName = "model.requests.total"
	MetricModelRequestsActive  MetricName = "model.requests.active"
	MetricModelRequestDuration MetricName = "model.request.duration_ms"
	MetricModelRequestErrors   MetricName = "model.request.errors"
	MetricQueueDepth           MetricName = "queue.depth"
	MetricQueueLatency         MetricName = "queue.latency_ms"
	MetricSandboxInstances     MetricName = "sandbox.instances"
	MetricSandboxMemory        MetricName = "sandbox.memory_bytes"
	MetricSandboxEvictions     MetricName = "sandbox.evictions"
	MetricEventLatency         MetricName = "event.latency_ms"
	MetricRouteFailures        MetricName = "route.failures"
	MetricRouteProbes          MetricName = "route.probes"
	MetricWriterBackpressure   MetricName = "writer.backpressure"
	MetricWorldConflicts       MetricName = "world.conflicts"
	MetricExportProgressBytes  MetricName = "export.progress.bytes"
	MetricExportProgressItems  MetricName = "export.progress.items"
	MetricDiskFree             MetricName = "disk.free_bytes"
	MetricMemoryUsed           MetricName = "memory.used_bytes"
	MetricConfigReloads        MetricName = "config.reloads"
	MetricHealthChecks         MetricName = "health.checks"
	MetricHealthCheckFailures  MetricName = "health.check.failures"
)

// MetricType distinguishes counter and gauge metrics.
type MetricType int

const (
	MetricTypeCounter MetricType = iota
	MetricTypeGauge
)

// LabelName is a fixed label name from the allowed set.
// Labels must have bounded values (e.g., "success"/"error", not raw input).
type LabelName string

// Allowed label names for metrics.
const (
	LabelResult        LabelName = "result"         // "success", "error", "timeout", "cancelled"
	LabelComponent     LabelName = "component"      // "ssh", "sandbox", "routing", "storage", "config"
	LabelOperation     LabelName = "operation"      // high-level operation name
	LabelTier          LabelName = "tier"           // routing tier as string "1", "2", etc.
	LabelProtocol      LabelName = "protocol"       // protocol family
	LabelHealthStatus  LabelName = "health_status"  // "healthy", "cooling", "probing", "quarantined"
	LabelErrorType     LabelName = "error_type"     // categorized error type
	LabelProbeType     LabelName = "probe_type"     // health probe type
	LabelShutdownPhase LabelName = "shutdown_phase" // shutdown phase name
)

// AllowedLabelValues defines the bounded value set for each label.
// Any value not in this set will be rejected by the registry.
var AllowedLabelValues = map[LabelName]map[string]struct{}{
	LabelResult: {
		"success": {}, "error": {}, "timeout": {}, "cancelled": {}, "retry": {},
	},
	LabelComponent: {
		"ssh": {}, "sandbox": {}, "routing": {}, "storage": {}, "config": {},
		"terminal": {}, "model": {}, "world": {}, "export": {}, "health": {},
	},
	LabelOperation: {
		"session_start": {}, "session_end": {}, "model_request": {}, "tool_call": {},
		"world_commit": {}, "world_read": {}, "app_activate": {}, "app_generate": {},
		"config_reload": {}, "health_check": {}, "probe": {}, "export": {}, "backup": {},
		"shutdown": {}, "startup": {}, "sandbox_run": {}, "sandbox_evict": {},
	},
	LabelTier: {
		"1": {}, "2": {}, "3": {}, "4": {}, "5": {},
	},
	LabelProtocol: {
		"chat": {}, "responses": {}, "messages": {}, "gemini": {},
	},
	LabelHealthStatus: {
		"healthy": {}, "cooling": {}, "probing": {}, "quarantined": {},
	},
	LabelErrorType: {
		"quota": {}, "rate_limit": {}, "invalid_credential": {}, "model_removed": {},
		"provider_outage": {}, "network_timeout": {}, "context_too_long": {},
		"invalid_response": {}, "content_rejection": {}, "unknown": {},
	},
	LabelProbeType: {
		"catalogue": {}, "inference": {}, "capability": {},
	},
	LabelShutdownPhase: {
		"stop_accepting": {}, "cancel_work": {}, "flush_events": {},
		"close_sessions": {}, "checkpoint": {}, "complete": {},
	},
}

// metricKeyString creates a string key from name and labels for map storage.
// Labels are sorted for consistent keys.
func metricKeyString(name MetricName, labels map[LabelName]string) string {
	if len(labels) == 0 {
		return string(name)
	}
	// Sort label names for consistent key
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, string(k))
	}
	// Simple bubble sort for small number of labels
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[i] > keys[j] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	key := string(name)
	for _, k := range keys {
		key += "|" + k + "=" + labels[LabelName(k)]
	}
	return key
}

// Registry is a low-cardinality metrics registry.
// It enforces:
//   - Only declared metric names can be used
//   - Only declared label names can be used
//   - Label values must be from the allowed bounded sets
//   - No raw commands, usernames, keys, or unbounded strings as labels
type Registry struct {
	mu       sync.RWMutex
	counters map[string]*counterMetric
	gauges   map[string]*gaugeMetric
}

// counterMetric holds a counter value.
type counterMetric struct {
	value atomic.Uint64
}

// gaugeMetric holds a gauge value.
type gaugeMetric struct {
	value atomic.Int64
}

// NewRegistry creates a new metrics registry.
func NewRegistry() *Registry {
	return &Registry{
		counters: make(map[string]*counterMetric),
		gauges:   make(map[string]*gaugeMetric),
	}
}

// validateName checks that a metric name is allowed.
func validateName(name MetricName) error {
	switch name {
	case MetricSessionsConnected, MetricSessionsActive,
		MetricModelRequestsTotal, MetricModelRequestsActive,
		MetricModelRequestDuration, MetricModelRequestErrors,
		MetricQueueDepth, MetricQueueLatency,
		MetricSandboxInstances, MetricSandboxMemory, MetricSandboxEvictions,
		MetricEventLatency,
		MetricRouteFailures, MetricRouteProbes,
		MetricWriterBackpressure,
		MetricWorldConflicts,
		MetricExportProgressBytes, MetricExportProgressItems,
		MetricDiskFree, MetricMemoryUsed,
		MetricConfigReloads,
		MetricHealthChecks, MetricHealthCheckFailures:
		return nil
	default:
		return fmt.Errorf("metric name %q is not in the allowlist", name)
	}
}

// validateLabels checks that all label names and values are allowed.
func validateLabels(labels map[LabelName]string) error {
	for name, value := range labels {
		allowed, ok := AllowedLabelValues[name]
		if !ok {
			return fmt.Errorf("label name %q is not in the allowlist", name)
		}
		if _, ok := allowed[value]; !ok {
			return fmt.Errorf("label value %q for label %q is not in the allowed set", value, name)
		}
	}
	return nil
}

// Counter returns a counter metric, creating it if needed.
// The name and labels must be from the allowlists.
func (r *Registry) Counter(name MetricName, labels map[LabelName]string) (*Counter, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	if err := validateLabels(labels); err != nil {
		return nil, err
	}
	key := metricKeyString(name, labels)

	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.counters[key]; ok {
		return &Counter{m: m}, nil
	}
	m := &counterMetric{}
	r.counters[key] = m
	return &Counter{m: m}, nil
}

// Gauge returns a gauge metric, creating it if needed.
// The name and labels must be from the allowlists.
func (r *Registry) Gauge(name MetricName, labels map[LabelName]string) (*Gauge, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	if err := validateLabels(labels); err != nil {
		return nil, err
	}
	key := metricKeyString(name, labels)

	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.gauges[key]; ok {
		return &Gauge{m: m}, nil
	}
	m := &gaugeMetric{}
	r.gauges[key] = m
	return &Gauge{m: m}, nil
}

// Counter is a handle to a counter metric.
type Counter struct {
	m *counterMetric
}

// Inc increments the counter by 1.
func (c *Counter) Inc() {
	c.m.value.Add(1)
}

// Add adds n to the counter.
func (c *Counter) Add(n uint64) {
	c.m.value.Add(n)
}

// Value returns the current counter value.
func (c *Counter) Value() uint64 {
	return c.m.value.Load()
}

// Gauge is a handle to a gauge metric.
type Gauge struct {
	m *gaugeMetric
}

// Set sets the gauge value.
func (g *Gauge) Set(v int64) {
	g.m.value.Store(v)
}

// Add adds n to the gauge value.
func (g *Gauge) Add(n int64) {
	g.m.value.Add(n)
}

// Sub subtracts n from the gauge value.
func (g *Gauge) Sub(n int64) {
	g.m.value.Add(-n)
}

// Value returns the current gauge value.
func (g *Gauge) Value() int64 {
	return g.m.value.Load()
}

// MetricSnapshotEntry represents a single metric in a snapshot.
type MetricSnapshotEntry struct {
	Name   MetricName
	Labels map[LabelName]string
	Value  any // uint64 for counters, int64 for gauges
	Type   MetricType
}

// Snapshot returns a copy of all current metric values.
// This is safe to call concurrently with metric updates.
func (r *Registry) Snapshot() []MetricSnapshotEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entries := make([]MetricSnapshotEntry, 0, len(r.counters)+len(r.gauges))

	for key, m := range r.counters {
		name, labels := parseMetricKey(key)
		entries = append(entries, MetricSnapshotEntry{
			Name:   name,
			Labels: labels,
			Value:  m.value.Load(),
			Type:   MetricTypeCounter,
		})
	}
	for key, m := range r.gauges {
		name, labels := parseMetricKey(key)
		entries = append(entries, MetricSnapshotEntry{
			Name:   name,
			Labels: labels,
			Value:  m.value.Load(),
			Type:   MetricTypeGauge,
		})
	}
	return entries
}

// parseMetricKey reverses metricKeyString for snapshot display.
func parseMetricKey(key string) (MetricName, map[LabelName]string) {
	// Find first | separator
	idx := 0
	for i := 0; i < len(key); i++ {
		if key[i] == '|' {
			idx = i
			break
		}
	}
	if idx == 0 {
		return MetricName(key), nil
	}
	name := MetricName(key[:idx])
	labels := make(map[LabelName]string)
	// Parse label pairs
	rest := key[idx+1:]
	for len(rest) > 0 {
		eqIdx := -1
		for i := 0; i < len(rest); i++ {
			if rest[i] == '=' {
				eqIdx = i
				break
			}
		}
		if eqIdx == -1 {
			break
		}
		labelName := LabelName(rest[:eqIdx])
		rest = rest[eqIdx+1:]
		valEnd := len(rest)
		for i := 0; i < len(rest); i++ {
			if rest[i] == '|' {
				valEnd = i
				break
			}
		}
		labels[labelName] = rest[:valEnd]
		if valEnd < len(rest) {
			rest = rest[valEnd+1:]
		} else {
			rest = ""
		}
	}
	return name, labels
}

// MetricLabel is a key-value pair for building label maps.
// Use this to construct label maps in a type-safe way.
type MetricLabel struct {
	Name  LabelName
	Value string
}

// Labels converts a slice of MetricLabel to the map format used by the registry.
func Labels(pairs ...MetricLabel) map[LabelName]string {
	m := make(map[LabelName]string, len(pairs))
	for _, p := range pairs {
		m[p.Name] = p.Value
	}
	return m
}

// DefaultRegistry is a package-level registry for convenience.
var DefaultRegistry = NewRegistry()

// GetCounter is a convenience function using the default registry.
func GetCounter(name MetricName, labels map[LabelName]string) (*Counter, error) {
	return DefaultRegistry.Counter(name, labels)
}

// GetGauge is a convenience function using the default registry.
func GetGauge(name MetricName, labels map[LabelName]string) (*Gauge, error) {
	return DefaultRegistry.Gauge(name, labels)
}

// GetSnapshot is a convenience function using the default registry.
func GetSnapshot() []MetricSnapshotEntry {
	return DefaultRegistry.Snapshot()
}

// WithContext returns a context with the registry attached.
// Use RegistryFromContext to retrieve it.
func WithContext(ctx context.Context, r *Registry) context.Context {
	return context.WithValue(ctx, registryKey{}, r)
}

// RegistryFromContext retrieves the registry from context, or returns the default.
func RegistryFromContext(ctx context.Context) *Registry {
	if r, ok := ctx.Value(registryKey{}).(*Registry); ok {
		return r
	}
	return DefaultRegistry
}

type registryKey struct{}
