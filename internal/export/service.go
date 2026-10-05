package export

import (
	"context"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// Batch bounds. Exports page through the event store in batches no larger than
// the ports' documented per-call maxima, so a large export stays bounded in
// memory regardless of total size.
const (
	// DefaultBatchSize is the page size when Options.BatchSize is zero.
	DefaultBatchSize = 200
	// MaxBatchSize is the largest accepted page size (EventStore.List and
	// Pagination both cap at 1000).
	MaxBatchSize = 1000
	// ScopeExperiment labels an export with no narrower selector.
	ScopeExperiment = "experiment"
)

// Options configures a Service.
type Options struct {
	// Policy authorizes the RetrievalStore path. It defaults to
	// domain.DefaultScopePolicy; an administrator export normally uses a
	// policy that permits cross-user and shared reads.
	Policy domain.ScopePolicy
	// BatchSize is the event page size. Zero selects DefaultBatchSize.
	BatchSize int
	// ExporterVersion is written into every projection.
	ExporterVersion string
}

// Service implements ports.Exporter: it turns a domain.ExportQuery into one of
// the three PLAN 10.5 projections. It owns no storage; events, retrieval, and
// content arrive through ports, and time and randomness through the Clock and
// Random ports so an export is deterministic under test.
type Service struct {
	events          ports.EventStore
	retrieval       ports.RetrievalStore
	content         ports.ContentStore
	clock           ports.Clock
	random          ports.Random
	policy          domain.ScopePolicy
	batchSize       int
	exporterVersion string
}

// Compile-time proof that the service satisfies the outbound port.
var _ ports.Exporter = (*Service)(nil)

// NewService builds the export use case over the three stores plus the clock
// and randomness ports. All five are required.
func NewService(events ports.EventStore, retrieval ports.RetrievalStore, content ports.ContentStore, clock ports.Clock, random ports.Random, opts Options) (*Service, error) {
	if events == nil || retrieval == nil || content == nil || clock == nil || random == nil {
		return nil, domain.NewValidationError(domain.CodeInvalidInput,
			"export service requires event, retrieval, content, clock, and random ports", nil)
	}
	if opts.BatchSize == 0 {
		opts.BatchSize = DefaultBatchSize
	}
	if opts.BatchSize < 1 || opts.BatchSize > MaxBatchSize {
		return nil, domain.NewValidationError(domain.CodeInvalidInput,
			"batch size out of range [1,1000]", map[string]string{"batch_size": fmt.Sprint(opts.BatchSize)})
	}
	if opts.ExporterVersion == "" {
		opts.ExporterVersion = "dev"
	}
	if opts.Policy == (domain.ScopePolicy{}) {
		opts.Policy = domain.DefaultScopePolicy()
	}
	return &Service{
		events:          events,
		retrieval:       retrieval,
		content:         content,
		clock:           clock,
		random:          random,
		policy:          opts.Policy,
		batchSize:       opts.BatchSize,
		exporterVersion: opts.ExporterVersion,
	}, nil
}

// Export streams one projection for q. It assembles the canonical records once
// and dispatches to the requested format, then reports the primary file's size
// and sha256.
func (s *Service) Export(ctx context.Context, q domain.ExportQuery) (domain.ExportResult, error) {
	if err := validateExportQuery(q); err != nil {
		return domain.ExportResult{}, err
	}
	started := s.clock.NowUnixMilli()
	rd := newRedactor(q.Redact)
	src := &recordSource{
		events:    s.events,
		retrieval: s.retrieval,
		content:   s.content,
		query:     q,
		policy:    s.policy,
		batchSize: s.batchSize,
		redactor:  rd,
	}
	proj, err := s.newProjection(q, rd)
	if err != nil {
		return domain.ExportResult{}, err
	}
	summary, err := src.each(ctx, proj.write)
	if err != nil {
		_ = proj.abort()
		return domain.ExportResult{}, err
	}
	if err := proj.close(summary); err != nil {
		return domain.ExportResult{}, err
	}
	size, digest, err := fileDigest(proj.primaryPath())
	if err != nil {
		return domain.ExportResult{}, err
	}
	exportID, err := s.newExportID()
	if err != nil {
		return domain.ExportResult{}, err
	}
	return domain.ExportResult{
		ExportID:    exportID,
		EventCount:  int64(summary.count),
		SizeBytes:   size,
		Checksum:    digest,
		StartedAt:   started,
		CompletedAt: s.clock.NowUnixMilli(),
		Format:      q.Format,
	}, nil
}

// projection is one streaming output format.
type projection interface {
	write(record) error
	close(streamSummary) error
	abort() error
	primaryPath() string
}

func (s *Service) newProjection(q domain.ExportQuery, rd *redactor) (projection, error) {
	switch q.Format {
	case FormatJSONL:
		return newBundle(q.OutputPath, s.exporterVersion, scopeName(q), s.clock.NowUnixMilli(), rd.secrets)
	case FormatTranscript:
		return newTranscript(q.OutputPath)
	case FormatAsciicast:
		return newAsciicast(q.OutputPath)
	default:
		return nil, domain.NewValidationError(domain.CodeInvalidInput,
			"unsupported export format", map[string]string{"format": q.Format})
	}
}

func (s *Service) newExportID() (string, error) {
	raw, err := s.random.Bytes(16)
	if err != nil {
		return "", err
	}
	return "exp_" + hex.EncodeToString(raw), nil
}

// validateExportQuery rejects queries the projections cannot honor, before any
// output file is created.
func validateExportQuery(q domain.ExportQuery) error {
	if strings.TrimSpace(q.OutputPath) == "" {
		return domain.NewValidationError(domain.CodeInvalidInput, "export output path is required", nil)
	}
	switch q.Format {
	case FormatJSONL, FormatTranscript, FormatAsciicast:
	default:
		return domain.NewValidationError(domain.CodeInvalidInput,
			"unsupported export format", map[string]string{"format": q.Format})
	}
	// An asciicast recording is one terminal stream. Multi-session exports are
	// supported by the JSONL bundle and transcript; a replay of several
	// interleaved sessions would not be a truthful recording.
	if q.Format == FormatAsciicast && len(q.Scope.SessionIDs) != 1 {
		return domain.NewValidationError(domain.CodeInvalidInput,
			"asciicast export requires exactly one session", nil)
	}
	return nil
}

// scopeName labels the export's scope. The JSONL bundle also carries the
// resolution of these scope labels in its manifest sessions.
func scopeName(q domain.ExportQuery) string {
	switch {
	case len(q.Scope.SessionIDs) > 0:
		return "session"
	case len(q.Scope.UserIDs) > 0:
		return "user"
	case q.Filter.FromTime != 0 || q.Filter.ToTime != 0:
		return "timerange"
	default:
		return ScopeExperiment
	}
}

// OutputPathFor derives the default output location for a format under dir for
// a scope label. It is a convenience for the admin command; callers may still
// set OutputPath explicitly.
func OutputPathFor(dir, format, scope, name string) string {
	switch format {
	case FormatJSONL:
		return filepath.Join(dir, scope+"-"+name+".bundle")
	case FormatTranscript:
		return filepath.Join(dir, scope+"-"+name+".transcript.txt")
	case FormatAsciicast:
		return filepath.Join(dir, scope+"-"+name+".cast")
	default:
		return filepath.Join(dir, scope+"-"+name)
	}
}
