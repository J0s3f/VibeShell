package sqlite

import (
	"errors"
	"os"
	"strings"
	"sync"
	"syscall"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/observability"
)

// DurableFailureKind classifies why a durable write could not be recorded.
// It distinguishes conditions that are permanent for the current storage
// (read-only filesystem, full disk, WAL/corruption) from ordinary conflicts,
// which are transient and policy-driven (a stale revision, a duplicate key).
// Only the durable kinds take permanent recording out of service.
type DurableFailureKind string

const (
	// DurableFailureNone means the write succeeded; recording stays available.
	DurableFailureNone DurableFailureKind = ""
	// DurableFailureReadOnly covers EROFS and SQLITE_READONLY.
	DurableFailureReadOnly DurableFailureKind = "read_only"
	// DurableFailureFull covers ENOSPC and SQLITE_FULL.
	DurableFailureFull DurableFailureKind = "disk_full"
	// DurableFailureIO covers EIO and other unclassified I/O errors.
	DurableFailureIO DurableFailureKind = "io_error"
	// DurableFailureCorrupt covers SQLITE_CORRUPT/NOTADB and WAL failures.
	DurableFailureCorrupt DurableFailureKind = "corruption"
)

// ClassifyDurableFailure maps a write error to a durable-failure kind. It
// returns DurableFailureNone for conflicts and other ordinary errors, so a
// conflict never disables recording. The classification is deliberately
// conservative and message-based because modernc.org/sqlite surfaces driver
// errors as text; known conflict shapes are excluded by name before any
// durable keyword is honored.
func ClassifyDurableFailure(err error) DurableFailureKind {
	if err == nil {
		return DurableFailureNone
	}
	// A typed conflict or validation is ordinary work; it never means the
	// store is unable to record.
	var de *domain.DomainError
	if errors.As(err, &de) {
		switch de.Category {
		case domain.CategoryConflict, domain.CategoryValidation, domain.CategoryNotFound,
			domain.CategoryDenied, domain.CategoryLimit, domain.CategoryCancelled:
			return DurableFailureNone
		}
	}
	if errors.Is(err, os.ErrPermission) {
		return DurableFailureReadOnly
	}
	if errors.Is(err, syscall.ENOSPC) {
		return DurableFailureFull
	}
	if errors.Is(err, syscall.EROFS) {
		return DurableFailureReadOnly
	}
	if errors.Is(err, syscall.EIO) {
		return DurableFailureIO
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "readonly") || strings.Contains(msg, "read-only"):
		return DurableFailureReadOnly
	case strings.Contains(msg, "disk is full") || strings.Contains(msg, "database or disk is full") || strings.Contains(msg, "no space"):
		return DurableFailureFull
	case strings.Contains(msg, "malformed") || strings.Contains(msg, "corrupt") || strings.Contains(msg, "not a database") || strings.Contains(msg, "checksum mismatch"):
		return DurableFailureCorrupt
	case strings.Contains(msg, "wal") && (strings.Contains(msg, "fail") || strings.Contains(msg, "error")):
		return DurableFailureCorrupt
	case strings.Contains(msg, "input/output error") || strings.Contains(msg, "i/o error"):
		return DurableFailureIO
	}
	return DurableFailureNone
}

// RecordingGuard is the durable-store health gate. Once a permanent write
// fails it marks recording unavailable and refuses new semantic work with a
// typed error, instead of pretending the append succeeded. A later successful
// write clears the condition. Reads stay available while recording is down:
// the recorded history is still inspectable, only new work is refused.
//
// The guard is shared by every writer of one durable store. It is safe for
// concurrent use.
type RecordingGuard struct {
	mu       sync.RWMutex
	kind     DurableFailureKind
	detail   string
	log      *observability.Logger
	reporter *observability.ReadinessReporter
}

// NewRecordingGuard returns an available guard. logger and readiness may be
// nil, in which case failure reporting falls back to the standard logger and
// the package-level readiness reporter.
func NewRecordingGuard(logger *observability.Logger, readiness *observability.ReadinessReporter) *RecordingGuard {
	return &RecordingGuard{log: logger, reporter: readiness}
}

// Available reports whether permanent recording can currently continue.
func (g *RecordingGuard) Available() bool {
	if g == nil {
		return true
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.kind == DurableFailureNone
}

// Kind returns the current failure kind, or DurableFailureNone when available.
func (g *RecordingGuard) Kind() DurableFailureKind {
	if g == nil {
		return DurableFailureNone
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.kind
}

// RefuseRecording returns the typed error a semantic write must return while
// recording is unavailable, or nil when recording is available. The message
// is data-free: it names the condition, never the failing statement or bytes.
func (g *RecordingGuard) RefuseRecording() error {
	if g == nil {
		return nil
	}
	g.mu.RLock()
	kind, down := g.kind, g.kind != DurableFailureNone
	g.mu.RUnlock()
	if !down {
		return nil
	}
	return domain.NewUnavailableError(domain.CodeRecordingUnavailable,
		"permanent recording is unavailable; new semantic work is refused",
		map[string]string{"kind": string(kind)}, nil)
}

// ObserveWriteErr classifies a write error and, when durable, marks recording
// unavailable and reports the condition. Conflicts and ordinary errors are
// ignored, leaving the guard available. It returns the typed refusal error
// when the write tripped the guard, or nil otherwise.
func (g *RecordingGuard) ObserveWriteErr(err error) error {
	if g == nil || err == nil {
		return nil
	}
	kind := ClassifyDurableFailure(err)
	if kind == DurableFailureNone {
		return nil
	}
	g.markUnavailable(kind, err)
	return g.RefuseRecording()
}

// ObserveWriteSuccess clears any unavailable condition after a successful
// write proves the store is writable again, and reports recovery.
func (g *RecordingGuard) ObserveWriteSuccess() {
	if g == nil {
		return
	}
	g.mu.Lock()
	was := g.kind
	g.kind = DurableFailureNone
	g.detail = ""
	g.mu.Unlock()
	if was == DurableFailureNone {
		return
	}
	g.logger().Info("recording recovered",
		observability.Attr(observability.FieldComponent, "sqlite"),
		observability.Attr(observability.FieldOperation, "recording_recovery"),
		observability.Attr(observability.FieldResult, "success"),
	)
	g.readiness().SetHealthy(observability.SubsystemStorage, "permanent recording recovered")
}

// ProbeWritable lets an operator or maintenance path attempt one durable write
// while recording is down, since ordinary semantic writes are refused and
// could never clear the condition themselves. It runs write unfiltered: a
// durable failure keeps the guard down and returns the refusal; any other
// failure is returned verbatim; success clears the condition. The write must
// be a small, bounded probe, not a replay of the semantic work that failed.
func (g *RecordingGuard) ProbeWritable(write func() error) error {
	if g == nil {
		return write()
	}
	err := write()
	if err == nil {
		g.ObserveWriteSuccess()
		return nil
	}
	if refusal := g.ObserveWriteErr(err); refusal != nil {
		return refusal
	}
	return err
}

func (g *RecordingGuard) markUnavailable(kind DurableFailureKind, err error) {
	g.mu.Lock()
	first := g.kind == DurableFailureNone
	g.kind = kind
	// Detail is the driver message with secret shapes scrubbed; it never
	// contains commands, arguments, or payload bytes.
	g.detail = observability.RedactString(err.Error())
	g.mu.Unlock()
	if !first {
		return
	}
	g.logger().Error("durable write failed; recording unavailable",
		observability.Attr(observability.FieldComponent, "sqlite"),
		observability.Attr(observability.FieldOperation, "durable_write"),
		observability.Attr(observability.FieldResult, "error"),
		observability.Attr(observability.FieldError, g.detail),
		observability.Attr(observability.FieldHealthStatus, string(kind)),
	)
	g.readiness().SetUnhealthy(observability.SubsystemStorage, "permanent recording unavailable ("+string(kind)+")")
}

func (g *RecordingGuard) logger() *observability.Logger {
	if g.log != nil {
		return g.log
	}
	return observability.DefaultLogger
}

func (g *RecordingGuard) readiness() *observability.ReadinessReporter {
	if g.reporter != nil {
		return g.reporter
	}
	return observability.DefaultReadinessReporter
}
