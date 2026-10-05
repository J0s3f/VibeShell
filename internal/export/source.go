package export

import (
	"context"
	"encoding/json"
	"fmt"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// recordSource assembles the canonical stream for one export query. It pages
// through an EventStore (explicit session scope) or a RetrievalStore
// (user/time-range/experiment scope) in bounded batches and resolves content
// references through the ContentStore, one blob at a time.
type recordSource struct {
	events    ports.EventStore
	retrieval ports.RetrievalStore
	content   ports.ContentStore
	query     domain.ExportQuery
	policy    domain.ScopePolicy
	batchSize int
	redactor  *redactor
}

// each streams every record in order to fn, returning the explicit stream
// summary. A nil payload on an event is normalized to an empty JSON object so
// every projection can decode it. The redaction policy is applied before fn
// sees the record.
func (s *recordSource) each(ctx context.Context, fn func(record) error) (streamSummary, error) {
	if len(s.query.Scope.SessionIDs) > 0 {
		return s.eachSession(ctx, fn)
	}
	return s.eachScoped(ctx, fn)
}

// eachSession walks the explicitly requested sessions in order, using
// EventStore.List so ordering is the durable per-session sequence rather than
// a timestamp. The session-end event is detected regardless of the query
// filter so the recorded session status stays accurate even when a filter
// omits the end event from the emitted projection.
func (s *recordSource) eachSession(ctx context.Context, fn func(record) error) (streamSummary, error) {
	var summary streamSummary
	for _, sid := range s.query.Scope.SessionIDs {
		if err := ctx.Err(); err != nil {
			return streamSummary{}, err
		}
		st := sessionStatus{SessionID: sid}
		minSeq := s.query.Filter.MinSequence
		for {
			events, err := s.events.List(ctx, sid, minSeq, s.batchSize)
			if err != nil {
				return streamSummary{}, err
			}
			if len(events) == 0 {
				break
			}
			for _, ev := range events {
				if ev.Envelope.Sequence < minSeq {
					continue
				}
				minSeq = ev.Envelope.Sequence + 1
				if ev.Envelope.Kind == domain.EventKindSessionEnd {
					st.Complete = true
					st.EndReason = endReason(ev.Payload)
				}
				if !s.keep(ev.Envelope) {
					continue
				}
				if err := s.emit(ctx, ev, &summary, &st, fn); err != nil {
					return streamSummary{}, err
				}
			}
			if len(events) < s.batchSize {
				break
			}
		}
		summary.sessions = append(summary.sessions, st)
	}
	return summary, nil
}

// eachScoped pages through RetrievalStore.Query under the injected scope
// policy. The store applies the scope/filter; this method only tracks which
// sessions appeared and whether each recorded an end.
func (s *recordSource) eachScoped(ctx context.Context, fn func(record) error) (streamSummary, error) {
	var summary streamSummary
	seen := map[domain.SessionID]*sessionStatus{}
	var order []domain.SessionID
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return streamSummary{}, err
		}
		res, err := s.retrieval.Query(ctx, domain.RetrievalQuery{
			Scope:  s.query.Scope,
			Filter: s.query.Filter,
			Pagination: domain.Pagination{
				Limit:      s.batchSize,
				Cursor:     cursor,
				Descending: false,
			},
		}, s.policy)
		if err != nil {
			return streamSummary{}, err
		}
		for _, ev := range res.Events {
			st, ok := seen[ev.Envelope.SessionID]
			if !ok {
				st = &sessionStatus{SessionID: ev.Envelope.SessionID}
				seen[ev.Envelope.SessionID] = st
				order = append(order, ev.Envelope.SessionID)
			}
			if ev.Envelope.Kind == domain.EventKindSessionEnd {
				st.Complete = true
				st.EndReason = endReason(ev.Payload)
			}
			if !s.keep(ev.Envelope) {
				continue
			}
			if err := s.emit(ctx, ev, &summary, st, fn); err != nil {
				return streamSummary{}, err
			}
		}
		if !res.Truncated || res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	for _, sid := range order {
		summary.sessions = append(summary.sessions, *seen[sid])
	}
	return summary, nil
}

// emit builds, redacts, and forwards one record while updating the stream
// summary and the session status.
func (s *recordSource) emit(ctx context.Context, ev domain.EventRecord, summary *streamSummary, st *sessionStatus, fn func(record) error) error {
	rec, err := s.build(ctx, ev)
	if err != nil {
		return err
	}
	if summary.startedAt == 0 {
		summary.startedAt = rec.env.Timestamp
	}
	collectSnapshot(rec.env.Provenance, summary)
	if err := fn(rec); err != nil {
		return err
	}
	st.EventCount++
	summary.count++
	return nil
}

// build resolves the record's content references and applies the redaction
// policy.
func (s *recordSource) build(ctx context.Context, ev domain.EventRecord) (record, error) {
	blobs, err := s.resolveBlobs(ctx, ev.Envelope)
	if err != nil {
		return record{}, err
	}
	return s.redactor.apply(record{env: ev.Envelope, payload: ev.Payload, blobs: blobs}), nil
}

// resolveBlobs fetches the exact bytes of every content reference a typed
// payload declares. A missing or size-mismatched blob is reported instead of
// silently emitting an incomplete bundle.
func (s *recordSource) resolveBlobs(ctx context.Context, env domain.EventEnvelope) (map[string][]byte, error) {
	refs, err := contentRefs(env)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, nil
	}
	blobs := make(map[string][]byte, len(refs))
	for _, ref := range refs {
		if ref.Hash.IsZero() {
			continue
		}
		id := ref.Hash.String()
		if _, ok := blobs[id]; ok {
			continue
		}
		data, err := s.loadContent(ctx, ref)
		if err != nil {
			return nil, err
		}
		blobs[id] = data
	}
	return blobs, nil
}

func (s *recordSource) loadContent(ctx context.Context, ref domain.ContentRef) ([]byte, error) {
	if ref.Size <= 0 {
		return []byte{}, nil
	}
	data, err := s.content.Get(ctx, ref, 0, ref.Size)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != ref.Size {
		return nil, domain.NewInternalError(
			domain.CodeInvariantViolation,
			"content reference size mismatch",
			fmt.Errorf("content %s: got %d bytes, want %d", ref.Hash, len(data), ref.Size),
		)
	}
	return data, nil
}

// contentRefs extracts the content references a typed payload declares. Only
// references to separately stored blobs are returned; an envelope whose whole
// payload is itself a content reference keeps that reference in place (the
// bundle records references as references).
func contentRefs(env domain.EventEnvelope) ([]domain.ContentRef, error) {
	raw := env.Payload.Inline
	if len(raw) == 0 {
		return nil, nil
	}
	switch env.Kind {
	case domain.EventKindTerminalFrame:
		var p domain.TerminalFramePayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, payloadDecodeError(env, err)
		}
		return nonZeroRefs(p.ContentRef), nil
	case domain.EventKindToolResult:
		var p domain.ToolResultPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, payloadDecodeError(env, err)
		}
		return nonZeroRefs(p.ReferencedRef), nil
	default:
		return nil, nil
	}
}

func nonZeroRefs(refs ...domain.ContentRef) []domain.ContentRef {
	var out []domain.ContentRef
	for _, ref := range refs {
		if !ref.Hash.IsZero() {
			out = append(out, ref)
		}
	}
	return out
}

func payloadDecodeError(env domain.EventEnvelope, err error) error {
	return domain.NewInternalError(domain.CodeDeserializationFailed,
		fmt.Sprintf("decode %s payload", env.Kind),
		fmt.Errorf("event %s: %w", env.EventID, err))
}

// keep applies the query filter to an envelope. The store applies the same
// filter where it can; this is the authoritative check for the EventStore path,
// which lists every session event.
func (s *recordSource) keep(env domain.EventEnvelope) bool {
	f := s.query.Filter
	if len(f.Kinds) > 0 && !containsKind(f.Kinds, env.Kind) {
		return false
	}
	if f.FromTime != 0 && env.Timestamp < f.FromTime {
		return false
	}
	if f.ToTime != 0 && env.Timestamp >= f.ToTime {
		return false
	}
	if f.TurnID != nil && (env.TurnID == nil || *env.TurnID != *f.TurnID) {
		return false
	}
	if f.AttemptID != nil && (env.AttemptID == nil || *env.AttemptID != *f.AttemptID) {
		return false
	}
	if f.AppVersionID != nil && (env.AppVersionID == nil || *env.AppVersionID != *f.AppVersionID) {
		return false
	}
	if f.ProvenanceSource != "" && env.Provenance.Source != f.ProvenanceSource {
		return false
	}
	if f.MinSequence != 0 && env.Sequence < f.MinSequence {
		return false
	}
	if f.MaxSequence != 0 && env.Sequence > f.MaxSequence {
		return false
	}
	return true
}

func containsKind(kinds []domain.EventKind, kind domain.EventKind) bool {
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}

func endReason(payload []byte) string {
	var p domain.SessionEndPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return p.Reason
}

// collectSnapshot records the first non-empty version of each accountable
// snapshot referenced by the exported events. The bundle manifest reports the
// versions that actually produced the history; it does not invent defaults.
func collectSnapshot(p domain.Provenance, summary *streamSummary) {
	if summary.configVersion == "" {
		summary.configVersion = p.ConfigVersion
	}
	if summary.promptVersion == "" {
		summary.promptVersion = p.PromptVersion
	}
	if summary.catalogueVersion == "" {
		summary.catalogueVersion = p.CatalogueVersion
	}
}
