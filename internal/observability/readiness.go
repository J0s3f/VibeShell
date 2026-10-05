// Package observability provides operational logging, metrics, and status reporting
// for VibeShell.
package observability

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Subsystem identifies a major subsystem for readiness reporting.
// These are the ONLY subsystems that can be reported; this prevents
// leaking provider/account details in status output.
type Subsystem string

const (
	SubsystemConfig   Subsystem = "config"
	SubsystemStorage  Subsystem = "storage"
	SubsystemProvider Subsystem = "provider"
	SubsystemSandbox  Subsystem = "sandbox"
	SubsystemSSH      Subsystem = "ssh"
	SubsystemTerminal Subsystem = "terminal"
	SubsystemRouting  Subsystem = "routing"
	SubsystemHealth   Subsystem = "health"
)

// ReadinessStatus represents the health of a subsystem.
type ReadinessStatus int

const (
	StatusHealthy ReadinessStatus = iota
	StatusDegraded
	StatusUnhealthy
	StatusUnknown
)

// String returns a human-readable status.
func (s ReadinessStatus) String() string {
	switch s {
	case StatusHealthy:
		return "healthy"
	case StatusDegraded:
		return "degraded"
	case StatusUnhealthy:
		return "unhealthy"
	default:
		return "unknown"
	}
}

// SubsystemStatus holds the status of one subsystem.
type SubsystemStatus struct {
	Name        Subsystem
	Status      ReadinessStatus
	Message     string // Human-readable detail (NO secrets, NO provider/account IDs)
	LastCheck   time.Time
	LastHealthy time.Time // When this subsystem was last healthy
}

// ReadinessReporter tracks and reports subsystem readiness.
// It is designed for operator-facing status (admin CLI, internal health endpoint).
// It does NOT expose model routes, account details, or credentials.
type ReadinessReporter struct {
	mu       sync.RWMutex
	statuses map[Subsystem]*SubsystemStatus
	// overall is derived; cached for quick access
	overall    ReadinessStatus
	overallMsg string
	startTime  time.Time
	readyTime  time.Time // When overall first became healthy
}

// NewReadinessReporter creates a new readiness reporter with all subsystems unknown.
func NewReadinessReporter() *ReadinessReporter {
	r := &ReadinessReporter{
		statuses:  make(map[Subsystem]*SubsystemStatus),
		overall:   StatusUnknown,
		startTime: time.Now(),
	}
	// Initialize all known subsystems to unknown
	for _, s := range []Subsystem{
		SubsystemConfig, SubsystemStorage, SubsystemProvider,
		SubsystemSandbox, SubsystemSSH, SubsystemTerminal,
		SubsystemRouting, SubsystemHealth,
	} {
		r.statuses[s] = &SubsystemStatus{
			Name:      s,
			Status:    StatusUnknown,
			Message:   "not yet checked",
			LastCheck: time.Now(),
		}
	}
	return r
}

// SetStatus updates the status of a subsystem.
// The message must not contain secrets, provider names, account IDs, or credentials.
// Use generic messages like "database connection failed" not "postgres://user:pass@host/db failed".
func (r *ReadinessReporter) SetStatus(name Subsystem, status ReadinessStatus, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	st, ok := r.statuses[name]
	if !ok {
		// Unknown subsystem - ignore silently (could log internally)
		return
	}

	st.Status = status
	st.Message = message
	st.LastCheck = time.Now()
	if status == StatusHealthy {
		st.LastHealthy = time.Now()
	}

	r.recomputeOverallLocked()
}

// SetHealthy is a convenience for setting healthy status.
func (r *ReadinessReporter) SetHealthy(name Subsystem, message string) {
	r.SetStatus(name, StatusHealthy, message)
}

// SetDegraded is a convenience for setting degraded status.
func (r *ReadinessReporter) SetDegraded(name Subsystem, message string) {
	r.SetStatus(name, StatusDegraded, message)
}

// SetUnhealthy is a convenience for setting unhealthy status.
func (r *ReadinessReporter) SetUnhealthy(name Subsystem, message string) {
	r.SetStatus(name, StatusUnhealthy, message)
}

// recomputeOverallLocked recalculates overall status.
// Must be called with mu held.
func (r *ReadinessReporter) recomputeOverallLocked() {
	hasUnhealthy := false
	hasDegraded := false
	hasUnknown := false
	var unhealthyNames []Subsystem
	var degradedNames []Subsystem

	for _, st := range r.statuses {
		switch st.Status {
		case StatusUnhealthy:
			hasUnhealthy = true
			unhealthyNames = append(unhealthyNames, st.Name)
		case StatusDegraded:
			hasDegraded = true
			degradedNames = append(degradedNames, st.Name)
		case StatusUnknown:
			hasUnknown = true
		}
	}

	switch {
	case hasUnhealthy:
		r.overall = StatusUnhealthy
		r.overallMsg = fmt.Sprintf("unhealthy subsystems: %v", unhealthyNames)
	case hasDegraded:
		r.overall = StatusDegraded
		r.overallMsg = fmt.Sprintf("degraded subsystems: %v", degradedNames)
	case hasUnknown:
		r.overall = StatusUnknown
		r.overallMsg = "some subsystems not yet checked"
	default:
		r.overall = StatusHealthy
		if r.readyTime.IsZero() {
			r.readyTime = time.Now()
		}
		r.overallMsg = "all subsystems healthy"
	}
}

// Overall returns the aggregate readiness status and message.
func (r *ReadinessReporter) Overall() (ReadinessStatus, string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.overall, r.overallMsg
}

// IsReady returns true if overall status is healthy.
func (r *ReadinessReporter) IsReady() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.overall == StatusHealthy
}

// Status returns a copy of all subsystem statuses.
func (r *ReadinessReporter) Status() map[Subsystem]SubsystemStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[Subsystem]SubsystemStatus, len(r.statuses))
	for k, v := range r.statuses {
		out[k] = *v
	}
	return out
}

// FailingSubsystems returns the names of unhealthy/degraded subsystems.
// This is the primary operator-facing signal: which subsystem is failing.
func (r *ReadinessReporter) FailingSubsystems() []Subsystem {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var failing []Subsystem
	for _, st := range r.statuses {
		if st.Status == StatusUnhealthy || st.Status == StatusDegraded {
			failing = append(failing, st.Name)
		}
	}
	return failing
}

// Uptime returns the time since the reporter was created.
func (r *ReadinessReporter) Uptime() time.Duration {
	return time.Since(r.startTime)
}

// ReadyDuration returns the time since overall became healthy, or 0 if not yet ready.
func (r *ReadinessReporter) ReadyDuration() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.readyTime.IsZero() {
		return 0
	}
	return time.Since(r.readyTime)
}

// CheckFunc is a function that checks a subsystem and reports its status.
// It should perform a quick, bounded check and call SetStatus on the reporter.
type CheckFunc func(ctx context.Context, r *ReadinessReporter)

// RunChecks executes a set of check functions concurrently with a timeout.
// This is useful for startup readiness probes.
func (r *ReadinessReporter) RunChecks(ctx context.Context, timeout time.Duration, checks ...CheckFunc) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var wg sync.WaitGroup
	for _, check := range checks {
		wg.Add(1)
		go func(c CheckFunc) {
			defer wg.Done()
			c(ctx, r)
		}(check)
	}
	wg.Wait()
}

// DefaultReadinessReporter is a package-level reporter for convenience.
var DefaultReadinessReporter = NewReadinessReporter()

// SetStatus is a convenience function using the default reporter.
func SetStatus(name Subsystem, status ReadinessStatus, message string) {
	DefaultReadinessReporter.SetStatus(name, status, message)
}

// Overall is a convenience function using the default reporter.
func Overall() (ReadinessStatus, string) {
	return DefaultReadinessReporter.Overall()
}

// IsReady is a convenience function using the default reporter.
func IsReady() bool {
	return DefaultReadinessReporter.IsReady()
}

// FailingSubsystems is a convenience function using the default reporter.
func FailingSubsystems() []Subsystem {
	return DefaultReadinessReporter.FailingSubsystems()
}
