// Package observability provides operational logging, metrics, and status reporting
// for VibeShell.
package observability

import (
	"context"
	"log/slog"
	"sync"
)

// AllowedField is a field name that is permitted in operational logs.
// The allowlist ensures operational logs never carry raw commands, usernames,
// file contents, or credentials. Fields not on this list are silently dropped
// by the logger wrapper.
//
// When adding a new allowed field, update this list and the corresponding test.
// The field name should be a descriptive, stable identifier (lowercase,
// dot-separated for namespacing if needed).
type AllowedField string

// Field allowlist for operational logs.
// Categories:
//
// Core service fields
const (
	FieldService        AllowedField = "service"         // service name, e.g., "vibeshell"
	FieldComponent      AllowedField = "component"       // subsystem, e.g., "ssh", "sandbox", "routing"
	FieldOperation      AllowedField = "operation"       // high-level operation, e.g., "session_start", "model_request"
	FieldResult         AllowedField = "result"          // outcome, e.g., "success", "error", "timeout"
	FieldError          AllowedField = "error"           // error message (sanitized, no secrets)
	FieldDurationMS     AllowedField = "duration_ms"     // operation duration in milliseconds
	FieldAttempt        AllowedField = "attempt"         // attempt number for retries
	FieldTurnID         AllowedField = "turn_id"         // opaque turn identifier
	FieldSessionID      AllowedField = "session_id"      // opaque session identifier
	FieldUserID         AllowedField = "user_id"         // opaque internal user ID (NOT username)
	FieldRouteID        AllowedField = "route_id"        // opaque model route identifier
	FieldAccountID      AllowedField = "account_id"      // opaque account identifier
	FieldTier           AllowedField = "tier"            // routing tier number
	FieldProtocol       AllowedField = "protocol"        // protocol family, e.g., "chat", "responses"
	FieldModelID        AllowedField = "model_id"        // model identifier from catalogue
	FieldFinishReason   AllowedField = "finish_reason"   // provider finish reason
	FieldUsageInput     AllowedField = "usage_input"     // input token count
	FieldUsageOutput    AllowedField = "usage_output"    // output token count
	FieldQueueDepth     AllowedField = "queue_depth"     // pending work queue depth
	FieldBackpressure   AllowedField = "backpressure"    // backpressure signal active
	FieldDiskFree       AllowedField = "disk_free"       // available disk bytes
	FieldMemoryUsed     AllowedField = "memory_used"     // memory usage bytes
	FieldInstanceCount  AllowedField = "instance_count"  // sandbox instance count
	FieldEvictions      AllowedField = "evictions"       // instance eviction count
	FieldConflicts      AllowedField = "conflicts"       // world commit conflicts
	FieldExportBytes    AllowedField = "export_bytes"    // export progress bytes
	FieldExportItems    AllowedField = "export_items"    // export progress items
	FieldShutdown       AllowedField = "shutdown"        // shutdown phase
	FieldHealthStatus   AllowedField = "health_status"   // health check status
	FieldProbeType      AllowedField = "probe_type"      // health probe type
	FieldCooldownMS     AllowedField = "cooldown_ms"     // cooldown duration
	FieldConfigVersion  AllowedField = "config_version"  // config snapshot version
	FieldPromptVersion  AllowedField = "prompt_version"  // prompt snapshot version
	FieldCatalogVersion AllowedField = "catalog_version" // catalogue snapshot version
)

// allowedFields is the internal set of permitted field names for fast lookup.
var allowedFields = map[string]struct{}{
	string(FieldService):        {},
	string(FieldComponent):      {},
	string(FieldOperation):      {},
	string(FieldResult):         {},
	string(FieldError):          {},
	string(FieldDurationMS):     {},
	string(FieldAttempt):        {},
	string(FieldTurnID):         {},
	string(FieldSessionID):      {},
	string(FieldUserID):         {},
	string(FieldRouteID):        {},
	string(FieldAccountID):      {},
	string(FieldTier):           {},
	string(FieldProtocol):       {},
	string(FieldModelID):        {},
	string(FieldFinishReason):   {},
	string(FieldUsageInput):     {},
	string(FieldUsageOutput):    {},
	string(FieldQueueDepth):     {},
	string(FieldBackpressure):   {},
	string(FieldDiskFree):       {},
	string(FieldMemoryUsed):     {},
	string(FieldInstanceCount):  {},
	string(FieldEvictions):      {},
	string(FieldConflicts):      {},
	string(FieldExportBytes):    {},
	string(FieldExportItems):    {},
	string(FieldShutdown):       {},
	string(FieldHealthStatus):   {},
	string(FieldProbeType):      {},
	string(FieldCooldownMS):     {},
	string(FieldConfigVersion):  {},
	string(FieldPromptVersion):  {},
	string(FieldCatalogVersion): {},
}

// IsAllowedField reports whether a field name is on the operational log allowlist.
func IsAllowedField(name string) bool {
	_, ok := allowedFields[name]
	return ok
}

// Logger wraps slog.Logger to enforce the field allowlist.
// It drops any attribute whose key is not in the allowlist.
// This prevents accidental leakage of sensitive data into operational logs.
type Logger struct {
	*slog.Logger
	mu sync.RWMutex
}

// NewLogger creates a new operational logger with the given handler.
// The returned logger enforces the field allowlist on all log calls.
func NewLogger(handler slog.Handler) *Logger {
	return &Logger{Logger: slog.New(handler)}
}

// DefaultLogger is a package-level logger usable before the composition root
// wires its configured handler: it writes allowlisted fields to the standard
// library's default slog handler.
var DefaultLogger = NewLogger(slog.Default().Handler())

// Attr builds an allowlisted string attribute. It exists so callers outside
// this package can emit operational fields by name without repeating the
// AllowedField string conversion, keeping the allowlist the single source of
// truth for what may be logged.
func Attr(field AllowedField, value string) slog.Attr {
	return slog.String(string(field), value)
}

// NewLoggerWithAttrs creates a new logger with pre-set attributes.
// All attributes are filtered through the allowlist.
func NewLoggerWithAttrs(handler slog.Handler, attrs ...slog.Attr) *Logger {
	l := NewLogger(handler)
	if len(attrs) > 0 {
		filtered := filterAttrs(attrs)
		anyAttrs := make([]any, len(filtered))
		for i, a := range filtered {
			anyAttrs[i] = a
		}
		l.Logger = l.Logger.With(anyAttrs...)
	}
	return l
}

// With returns a new Logger with the given attributes added.
// Attributes not on the allowlist are silently dropped.
func (l *Logger) With(attrs ...slog.Attr) *Logger {
	filtered := filterAttrs(attrs)
	// Convert to []any for slog.Logger.With
	anyAttrs := make([]any, len(filtered))
	for i, a := range filtered {
		anyAttrs[i] = a
	}
	return &Logger{Logger: l.Logger.With(anyAttrs...)}
}

// WithGroup returns a new Logger with a group name.
// Group names are not validated against the allowlist; they are structural.
func (l *Logger) WithGroup(name string) *Logger {
	return &Logger{Logger: l.Logger.WithGroup(name)}
}

// filterAttrs returns only the attributes whose keys are on the allowlist.
func filterAttrs(attrs []slog.Attr) []slog.Attr {
	if len(attrs) == 0 {
		return nil
	}
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		if IsAllowedField(a.Key) {
			out = append(out, a)
		}
	}
	return out
}

// LogAttrs logs at the given level with attributes, enforcing the allowlist.
// This is the core logging method; level-specific helpers call this.
func (l *Logger) LogAttrs(ctx context.Context, level slog.Level, msg string, attrs ...slog.Attr) {
	filtered := filterAttrs(attrs)
	l.Logger.LogAttrs(ctx, level, msg, filtered...)
}

// Debug logs at DebugLevel with allowlist filtering.
func (l *Logger) Debug(msg string, attrs ...slog.Attr) {
	l.LogAttrs(context.Background(), slog.LevelDebug, msg, attrs...)
}

// DebugContext logs at DebugLevel with context and allowlist filtering.
func (l *Logger) DebugContext(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.LogAttrs(ctx, slog.LevelDebug, msg, attrs...)
}

// Info logs at InfoLevel with allowlist filtering.
func (l *Logger) Info(msg string, attrs ...slog.Attr) {
	l.LogAttrs(context.Background(), slog.LevelInfo, msg, attrs...)
}

// InfoContext logs at InfoLevel with context and allowlist filtering.
func (l *Logger) InfoContext(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.LogAttrs(ctx, slog.LevelInfo, msg, attrs...)
}

// Warn logs at WarnLevel with allowlist filtering.
func (l *Logger) Warn(msg string, attrs ...slog.Attr) {
	l.LogAttrs(context.Background(), slog.LevelWarn, msg, attrs...)
}

// WarnContext logs at WarnLevel with context and allowlist filtering.
func (l *Logger) WarnContext(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.LogAttrs(ctx, slog.LevelWarn, msg, attrs...)
}

// Error logs at ErrorLevel with allowlist filtering.
func (l *Logger) Error(msg string, attrs ...slog.Attr) {
	l.LogAttrs(context.Background(), slog.LevelError, msg, attrs...)
}

// ErrorContext logs at ErrorLevel with context and allowlist filtering.
func (l *Logger) ErrorContext(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.LogAttrs(ctx, slog.LevelError, msg, attrs...)
}
