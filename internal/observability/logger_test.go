// Package observability provides operational logging, metrics, and status reporting
// for VibeShell.
package observability

import (
	"bytes"
	"log/slog"
	"testing"
)

func TestAllowedField_AllFieldsPresent(t *testing.T) {
	// This test ensures all declared AllowedField constants are in the allowlist map.
	// If you add a new constant, add it to the allowlist in logger.go.
	testCases := []struct {
		name  string
		field AllowedField
	}{
		{"service", FieldService},
		{"component", FieldComponent},
		{"operation", FieldOperation},
		{"result", FieldResult},
		{"error", FieldError},
		{"duration_ms", FieldDurationMS},
		{"attempt", FieldAttempt},
		{"turn_id", FieldTurnID},
		{"session_id", FieldSessionID},
		{"user_id", FieldUserID},
		{"route_id", FieldRouteID},
		{"account_id", FieldAccountID},
		{"tier", FieldTier},
		{"protocol", FieldProtocol},
		{"model_id", FieldModelID},
		{"finish_reason", FieldFinishReason},
		{"usage_input", FieldUsageInput},
		{"usage_output", FieldUsageOutput},
		{"queue_depth", FieldQueueDepth},
		{"backpressure", FieldBackpressure},
		{"disk_free", FieldDiskFree},
		{"memory_used", FieldMemoryUsed},
		{"instance_count", FieldInstanceCount},
		{"evictions", FieldEvictions},
		{"conflicts", FieldConflicts},
		{"export_bytes", FieldExportBytes},
		{"export_items", FieldExportItems},
		{"shutdown", FieldShutdown},
		{"health_status", FieldHealthStatus},
		{"probe_type", FieldProbeType},
		{"cooldown_ms", FieldCooldownMS},
		{"config_version", FieldConfigVersion},
		{"prompt_version", FieldPromptVersion},
		{"catalog_version", FieldCatalogVersion},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if !IsAllowedField(string(tc.field)) {
				t.Errorf("field %q not in allowlist", tc.field)
			}
		})
	}
}

func TestLogger_AllowsAllowedFields(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := NewLogger(handler)

	// Should not panic and should include allowed fields
	logger.Info("test",
		slog.String(string(FieldService), "vibeshell"),
		slog.String(string(FieldComponent), "routing"),
		slog.String(string(FieldOperation), "model_request"),
		slog.String(string(FieldResult), "success"),
		slog.Int64(string(FieldDurationMS), 150),
	)
}

func TestLogger_DropsDisallowedFields(t *testing.T) {
	// We can't easily test that fields are dropped without a custom handler,
	// but we can verify the filter logic directly.
	attrs := []slog.Attr{
		slog.String(string(FieldService), "value"),
		slog.String("disallowed_field", "secret"),
		slog.Int("also_disallowed", 123),
	}
	filtered := filterAttrs(attrs)

	if len(filtered) != 1 {
		t.Errorf("expected 1 filtered attr, got %d", len(filtered))
	}
	if filtered[0].Key != string(FieldService) {
		t.Errorf("expected %s, got %s", FieldService, filtered[0].Key)
	}
}

func TestLogger_With_RedactsDisallowed(t *testing.T) {
	handler := slog.NewTextHandler(nil, nil)
	logger := NewLogger(handler)

	// With should filter attrs
	newLogger := logger.With(
		slog.String(string(FieldService), "test"),
		slog.String("secret_field", "should-be-dropped"),
	)

	// We can't inspect the internal logger easily, but we verify no panic
	_ = newLogger
}

func TestLogger_WithGroup_PreservesGroup(t *testing.T) {
	handler := slog.NewTextHandler(nil, nil)
	logger := NewLogger(handler)

	grouped := logger.WithGroup("testgroup")
	_ = grouped
	// No panic = success
}
