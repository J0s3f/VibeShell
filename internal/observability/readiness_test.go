// Package observability provides operational logging, metrics, and status reporting
// for VibeShell.
package observability

import (
	"context"
	"testing"
	"time"
)

func TestReadinessReporter_InitialState(t *testing.T) {
	r := NewReadinessReporter()

	status, _ := r.Overall()
	if status != StatusUnknown {
		t.Errorf("expected unknown, got %s", status)
	}
	if !r.IsReady() {
		// Not ready is correct initially
	}

	failing := r.FailingSubsystems()
	if len(failing) != 0 {
		t.Errorf("expected no failing subsystems initially, got %v", failing)
	}
}

func TestReadinessReporter_SetHealthy(t *testing.T) {
	r := NewReadinessReporter()

	// All 8 subsystems must be healthy for overall to be healthy
	r.SetHealthy(SubsystemConfig, "config loaded")
	r.SetHealthy(SubsystemStorage, "database connected")
	r.SetHealthy(SubsystemProvider, "provider available")
	r.SetHealthy(SubsystemSandbox, "sandbox ready")
	r.SetHealthy(SubsystemSSH, "ssh listening")
	r.SetHealthy(SubsystemTerminal, "terminal ready")
	r.SetHealthy(SubsystemRouting, "routing configured")
	r.SetHealthy(SubsystemHealth, "health checks passing")

	status, msg := r.Overall()
	if status != StatusHealthy {
		t.Errorf("expected healthy, got %s: %s", status, msg)
	}
	if !r.IsReady() {
		t.Error("should be ready when all subsystems healthy")
	}
}

func TestReadinessReporter_SetDegraded(t *testing.T) {
	r := NewReadinessReporter()

	r.SetHealthy(SubsystemConfig, "config loaded")
	r.SetDegraded(SubsystemProvider, "high latency")

	status, msg := r.Overall()
	if status != StatusDegraded {
		t.Errorf("expected degraded, got %s: %s", status, msg)
	}
	if r.IsReady() {
		t.Error("should not be ready when degraded")
	}

	failing := r.FailingSubsystems()
	if len(failing) != 1 || failing[0] != SubsystemProvider {
		t.Errorf("expected provider failing, got %v", failing)
	}
}

func TestReadinessReporter_SetUnhealthy(t *testing.T) {
	r := NewReadinessReporter()

	r.SetHealthy(SubsystemConfig, "config loaded")
	r.SetUnhealthy(SubsystemStorage, "connection failed")

	status, msg := r.Overall()
	if status != StatusUnhealthy {
		t.Errorf("expected unhealthy, got %s: %s", status, msg)
	}
	if r.IsReady() {
		t.Error("should not be ready when unhealthy")
	}

	failing := r.FailingSubsystems()
	if len(failing) != 1 || failing[0] != SubsystemStorage {
		t.Errorf("expected storage failing, got %v", failing)
	}
}

func TestReadinessReporter_MultipleFailing(t *testing.T) {
	r := NewReadinessReporter()

	r.SetDegraded(SubsystemProvider, "high latency")
	r.SetUnhealthy(SubsystemStorage, "connection failed")

	status, _ := r.Overall()
	if status != StatusUnhealthy {
		t.Errorf("expected unhealthy (worst wins), got %s", status)
	}

	failing := r.FailingSubsystems()
	if len(failing) != 2 {
		t.Errorf("expected 2 failing, got %v", failing)
	}
}

func TestReadinessReporter_UnknownSubsystems(t *testing.T) {
	r := NewReadinessReporter()

	r.SetHealthy(SubsystemConfig, "config loaded")
	// storage remains unknown

	status, msg := r.Overall()
	if status != StatusUnknown {
		t.Errorf("expected unknown (not all checked), got %s: %s", status, msg)
	}
	if r.IsReady() {
		t.Error("should not be ready when some unknown")
	}
}

func TestReadinessReporter_StatusMap(t *testing.T) {
	r := NewReadinessReporter()

	r.SetHealthy(SubsystemConfig, "config loaded")
	r.SetDegraded(SubsystemProvider, "high latency")

	statusMap := r.Status()
	if len(statusMap) != 8 { // all 8 subsystems
		t.Errorf("expected 8 subsystems, got %d", len(statusMap))
	}

	if statusMap[SubsystemConfig].Status != StatusHealthy {
		t.Errorf("config should be healthy")
	}
	if statusMap[SubsystemProvider].Status != StatusDegraded {
		t.Errorf("provider should be degraded")
	}
	if statusMap[SubsystemStorage].Status != StatusUnknown {
		t.Errorf("storage should be unknown")
	}
}

func TestReadinessReporter_Uptime(t *testing.T) {
	r := NewReadinessReporter()
	time.Sleep(10 * time.Millisecond)
	uptime := r.Uptime()
	if uptime < 10*time.Millisecond {
		t.Errorf("uptime too small: %v", uptime)
	}
}

func TestReadinessReporter_ReadyDuration(t *testing.T) {
	r := NewReadinessReporter()

	// Not ready yet
	if r.ReadyDuration() != 0 {
		t.Error("ready duration should be 0 before ready")
	}

	r.SetHealthy(SubsystemConfig, "ok")
	r.SetHealthy(SubsystemStorage, "ok")
	r.SetHealthy(SubsystemProvider, "ok")
	r.SetHealthy(SubsystemSandbox, "ok")
	r.SetHealthy(SubsystemSSH, "ok")
	r.SetHealthy(SubsystemTerminal, "ok")
	r.SetHealthy(SubsystemRouting, "ok")
	r.SetHealthy(SubsystemHealth, "ok")

	// Small delay to ensure time advances
	time.Sleep(1 * time.Millisecond)

	readyDur := r.ReadyDuration()
	if readyDur == 0 {
		t.Error("ready duration should be > 0 after ready")
	}
}

func TestReadinessReporter_RunChecks(t *testing.T) {
	r := NewReadinessReporter()

	checkConfig := func(ctx context.Context, rep *ReadinessReporter) {
		// Simulate check
		time.Sleep(5 * time.Millisecond)
		rep.SetHealthy(SubsystemConfig, "config loaded")
	}

	checkStorage := func(ctx context.Context, rep *ReadinessReporter) {
		time.Sleep(5 * time.Millisecond)
		rep.SetHealthy(SubsystemStorage, "db connected")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	r.RunChecks(ctx, 500*time.Millisecond, checkConfig, checkStorage)

	status, _ := r.Overall()
	if status != StatusUnknown { // other subsystems still unknown
		t.Errorf("expected unknown, got %s", status)
	}

	// But the checked ones should be healthy
	st := r.Status()
	if st[SubsystemConfig].Status != StatusHealthy {
		t.Error("config should be healthy")
	}
	if st[SubsystemStorage].Status != StatusHealthy {
		t.Error("storage should be healthy")
	}
}

func TestReadinessReporter_UnknownSubsystemIgnored(t *testing.T) {
	r := NewReadinessReporter()

	// This should not panic - unknown subsystems are silently ignored
	r.SetStatus("unknown_subsystem", StatusHealthy, "ignored")

	// Overall should still be unknown (no known subsystems set)
	status, _ := r.Overall()
	if status != StatusUnknown {
		t.Errorf("expected unknown, got %s", status)
	}
}

func TestReadinessReporter_StatusString(t *testing.T) {
	testCases := []struct {
		status   ReadinessStatus
		expected string
	}{
		{StatusHealthy, "healthy"},
		{StatusDegraded, "degraded"},
		{StatusUnhealthy, "unhealthy"},
		{StatusUnknown, "unknown"},
		{ReadinessStatus(99), "unknown"},
	}

	for _, tc := range testCases {
		t.Run(tc.expected, func(t *testing.T) {
			if tc.status.String() != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, tc.status.String())
			}
		})
	}
}

func TestReadinessReporter_ConcurrentAccess(t *testing.T) {
	r := NewReadinessReporter()

	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func(idx int) {
			for {
				select {
				case <-done:
					return
				default:
					r.SetHealthy(SubsystemConfig, "check")
					r.SetDegraded(SubsystemProvider, "slow")
					_ = r.FailingSubsystems()
					_, _ = r.Overall()
				}
			}
		}(i)
	}

	time.Sleep(50 * time.Millisecond)
	close(done)

	// Should not panic
	_ = r.Status()
}
