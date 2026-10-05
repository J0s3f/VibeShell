package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// Compile-time proof that one adapter stays behind both research ports.
var (
	_ ports.EventStore     = (*Events)(nil)
	_ ports.RetrievalStore = (*Events)(nil)
)

// EventsOptions configures the research/event adapter.
type EventsOptions struct {
	// QueueCapacity bounds the accepted-but-uncommitted append depth. Zero
	// selects the default.
	QueueCapacity int
	// MaxInlinePayloadBytes bounds envelope.Payload.Inline; larger payloads
	// must travel as content references. The domain envelope contract allows
	// up to 1 KiB inlined.
	MaxInlinePayloadBytes int
	// MaxBatch bounds how many queued appends the writer commits in one
	// transaction. A group commit amortizes one synchronous=FULL commit across
	// the batch, so a saturated queue records many short events at the cost of
	// one fsync. The batch never spans a caller's acknowledgement: every event
	// in it is durable before any caller is told the append succeeded. Zero
	// selects the default, and one disables batching.
	MaxBatch int
}

func (o EventsOptions) withDefaults() EventsOptions {
	if o.QueueCapacity <= 0 {
		o.QueueCapacity = 256
	}
	if o.MaxInlinePayloadBytes <= 0 {
		o.MaxInlinePayloadBytes = 1024
	}
	if o.MaxBatch <= 0 {
		o.MaxBatch = defaultMaxBatch
	}
	return o
}

// defaultMaxBatch is the group-commit batch bound. It is large enough to
// amortize a fsync across a loaded queue yet small enough that one batch
// commit stays short and its acknowledgement latency stays bounded.
const defaultMaxBatch = 64

// Events is the SQLite-backed research store: an append-only event log with
// per-session sequence ordering, content-ref payloads, command/path lookup
// indexes, an FTS5 transcript projection, scope-enforced retrieval, and a
// bounded writer queue for backpressure. Create it with NewEvents over the
// migrated database handle (see DB.SQL) and Close it before closing the DB.
type Events struct {
	sql   *sql.DB
	opts  EventsOptions
	queue *eventsWriter
}

// SetRecordingGuard installs the durable-store health gate shared with the
// world store (PLAN 10.3), so one failed durable write stops semantic work
// across both stores.
func (e *Events) SetRecordingGuard(guard *RecordingGuard) {
	e.queue.setGuard(guard)
}

// NewEvents wraps db with the research store. Migrations must already have
// run (Open does that); NewEvents verifies the events tables exist so a
// programming error fails fast instead of on the first append.
func NewEvents(db *sql.DB, opts EventsOptions) (*Events, error) {
	opts = opts.withDefaults()
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type IN ('table','view') AND name='events'`).Scan(&name)
	if err != nil || name != "events" {
		return nil, fmt.Errorf("sqlite: events table missing (run Migrate first): %v", err)
	}
	return &Events{sql: db, opts: opts, queue: newEventsWriter(db, opts.QueueCapacity, opts.MaxBatch)}, nil
}

// Close stops the writer queue and drains accepted appends. Read paths keep
// working on the underlying handle until the database itself is closed.
func (e *Events) Close() { e.queue.close() }

// Stats reports observable writer-queue backpressure.
func (e *Events) Stats() WriterQueueStats { return e.queue.stats() }

// ErrWriterStopped is returned by Append after Close.
var ErrWriterStopped = errors.New("sqlite: events writer stopped")

// ErrQueueFull reports that a bounded queue stayed saturated past the
// caller's context budget; the event was NOT persisted.
var ErrQueueFull = errors.New("sqlite: event writer queue full")

// ---------------------------------------------------------------------------
// ports.EventStore
// ---------------------------------------------------------------------------

// Append validates the envelope, assigns the event ID and the per-session
// sequence (last+1; a caller-provided non-zero sequence must match it), then
// persists the event together with its command/path indexes and FTS5
// projection in one short transaction on the single writer worker.
func (e *Events) Append(ctx context.Context, evt domain.EventEnvelope) (domain.EventRecord, error) {
	if err := e.validateEnvelope(evt); err != nil {
		return domain.EventRecord{}, err
	}
	// Refuse new work once permanent recording has failed, so an append is
	// never acknowledged when its durable write cannot be guaranteed.
	if err := e.queue.guard.RefuseRecording(); err != nil {
		return domain.EventRecord{}, err
	}
	result := make(chan appendOutcome, 1)
	job := appendJob{ctx: ctx, env: evt, opts: e.opts, out: result}
	if err := e.queue.submit(ctx, job); err != nil {
		return domain.EventRecord{}, err
	}
	select {
	case outcome := <-result:
		return outcome.record, outcome.err
	case <-ctx.Done():
		// The job may still run later; report that the append was not
		// acknowledged instead of guessing.
		return domain.EventRecord{}, fmt.Errorf("sqlite: append event: %w", ctx.Err())
	}
}

func (e *Events) validateEnvelope(evt domain.EventEnvelope) error {
	if evt.SchemaVersion != domain.EventSchemaVersion {
		return domain.NewValidationError(domain.CodeInvalidInput, "unsupported event schema version", map[string]string{"schema_version": strconv.Itoa(evt.SchemaVersion)})
	}
	if evt.SessionID.IsZero() {
		return domain.NewValidationError(domain.CodeInvalidInput, "event has no session id", nil)
	}
	if !domain.IsValidEventKind(evt.Kind) {
		return domain.NewValidationError(domain.CodeInvalidInput, "unknown event kind", map[string]string{"kind": string(evt.Kind)})
	}
	if evt.Timestamp < 0 {
		return domain.NewValidationError(domain.CodeInvalidInput, "negative event timestamp", nil)
	}
	hasInline := len(bytes.TrimSpace(evt.Payload.Inline)) > 0
	hasContent := evt.Payload.ContentID != nil && !evt.Payload.ContentID.IsZero()
	if argc := len(evt.Payload.Inline); hasInline && argc > e.opts.MaxInlinePayloadBytes {
		return domain.NewLimitError(domain.CodeOutputTooLarge, "inline payload exceeds bound; use a content reference", map[string]string{"max_inline_bytes": strconv.Itoa(e.opts.MaxInlinePayloadBytes)})
	}
	if !hasInline && !hasContent {
		return domain.NewValidationError(domain.CodeInvalidInput, "event payload must be inline or reference stored content", nil)
	}
	if evt.Provenance.Source == "" {
		return domain.NewValidationError(domain.CodeInvalidInput, "event provenance source is required", nil)
	}
	return nil
}

// GetByID fetches one event by its stable ID.
func (e *Events) GetByID(ctx context.Context, id domain.EventID) (domain.EventRecord, error) {
	row := e.sql.QueryRowContext(ctx, `SELECT envelope, payload_inline FROM events WHERE event_id = ?`, id.String())
	var envelopeJSON string
	var inline sql.NullString
	if err := row.Scan(&envelopeJSON, &inline); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.EventRecord{}, domain.NewNotFoundError(domain.CodeEventNotFound, "event not found", map[string]string{"event_id": id.String()})
		}
		return domain.EventRecord{}, fmt.Errorf("sqlite: get event: %w", err)
	}
	return recordFromRow(envelopeJSON, inline)
}

// List returns up to limit events for a session from a minimum sequence,
// ordered by per-session sequence (never by timestamp alone).
func (e *Events) List(ctx context.Context, session domain.SessionID, minSeq uint64, limit int) ([]domain.EventRecord, error) {
	if limit <= 0 || limit > 1000 {
		return nil, domain.NewValidationError(domain.CodeInvalidInput, "list limit out of range [1,1000]", map[string]string{"limit": strconv.Itoa(limit)})
	}
	rows, err := e.sql.QueryContext(ctx,
		`SELECT envelope, payload_inline FROM events WHERE session_id = ? AND sequence >= ? ORDER BY sequence LIMIT ?`,
		session.String(), minSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list events: %w", err)
	}
	defer rows.Close()
	return scanRecords(rows)
}

// ---------------------------------------------------------------------------
// Command/path index lookup helpers (not on the port yet; C02 may adopt)
// ---------------------------------------------------------------------------

// SearchCommands returns events that recorded the exact command. Normalization
// groups an invocation typed with different whitespace; case is preserved.
func (e *Events) SearchCommands(ctx context.Context, command string) ([]domain.EventRecord, error) {
	rows, err := e.sql.QueryContext(ctx,
		`SELECT e.envelope, e.payload_inline FROM command_index c JOIN events e ON e.rowid = c.event_rowid
		 WHERE c.normalized_command = ? ORDER BY e.session_id, e.sequence`, NormalizeCommand(command))
	if err != nil {
		return nil, fmt.Errorf("sqlite: command search: %w", err)
	}
	defer rows.Close()
	return scanRecords(rows)
}

// SearchPaths returns events that touched prefix or anything beneath it, using
// the boundary-aware path key range: "/some" matches "/some" and "/some/x"
// but never "/somewhere". Use SearchPathExact when only the exact path is
// meant.
func (e *Events) SearchPaths(ctx context.Context, prefix string) ([]domain.EventRecord, error) {
	low, high := SubtreeRange(prefix)
	rows, err := e.sql.QueryContext(ctx,
		`SELECT e.envelope, e.payload_inline FROM path_index p JOIN events e ON e.rowid = p.event_rowid
		 WHERE p.path_key >= ? AND p.path_key < ? ORDER BY e.session_id, e.sequence`, low, high)
	if err != nil {
		return nil, fmt.Errorf("sqlite: path search: %w", err)
	}
	defer rows.Close()
	return scanRecords(rows)
}

// SearchPathExact returns events whose recorded path equals path exactly. The
// indexed key carries a trailing separator, so this is a boundary-aware
// equality: searching "/some" never returns "/some/place" or "/somewhere".
func (e *Events) SearchPathExact(ctx context.Context, path string) ([]domain.EventRecord, error) {
	rows, err := e.sql.QueryContext(ctx,
		`SELECT e.envelope, e.payload_inline FROM path_index p JOIN events e ON e.rowid = p.event_rowid
		 WHERE p.path_key = ? ORDER BY e.session_id, e.sequence`, PathKey(path))
	if err != nil {
		return nil, fmt.Errorf("sqlite: exact path search: %w", err)
	}
	defer rows.Close()
	return scanRecords(rows)
}

// SearchTranscript runs a full-text query over the transcript projection and
// returns the originating events. The FTS table is derived, never the source
// of truth; deleted or rewritten world state never rewrites history.
func (e *Events) SearchTranscript(ctx context.Context, match string) ([]domain.EventRecord, error) {
	rows, err := e.sql.QueryContext(ctx,
		`SELECT e.envelope, e.payload_inline FROM transcript_fts f JOIN events e ON e.event_id = f.event_id
		 WHERE transcript_fts MATCH ? ORDER BY e.session_id, e.sequence`, match)
	if err != nil {
		return nil, fmt.Errorf("sqlite: transcript search: %w", err)
	}
	defer rows.Close()
	return scanRecords(rows)
}

// ---------------------------------------------------------------------------
// Retrieval helpers shared by Query and Surrounding
// ---------------------------------------------------------------------------

// allowedScopes returns the event visibilities visible under policy at the
// research boundary. Sharing-off denies cross-user and shared events; user
// scope is conservative because the store cannot prove the reader owns the
// data, so it requires explicit cross-user read permission.
func allowedScopes(policy domain.ScopePolicy) map[string]bool {
	allowed := map[string]bool{domain.ScopeSession.String(): true, domain.ScopeBaseline.String(): true}
	if policy.SharingEnabled {
		allowed[domain.ScopeShared.String()] = true
		if policy.CrossUserReadAllowed {
			allowed[domain.ScopeUser.String()] = true
		}
	}
	return allowed
}

// checkQueryScopes enforces the policy at the boundary: any requested scope
// the policy forbids fails the whole query, regardless of other filters.
func checkQueryScopes(q domain.RetrievalQuery, policy domain.ScopePolicy) error {
	allowed := allowedScopes(policy)
	for _, scope := range q.Scope.Scopes {
		if !allowed[scope.String()] {
			return fmt.Errorf("%w: scope %q", domain.ErrRetrievalDenied, scope.String())
		}
	}
	if q.Scope.IncludeShared && !policy.SharingEnabled {
		return fmt.Errorf("%w: shared scope requested while sharing is disabled", domain.ErrRetrievalDenied)
	}
	if len(q.Scope.UserIDs) > 0 && (!policy.SharingEnabled || !policy.CrossUserReadAllowed) {
		return fmt.Errorf("%w: cross-user retrieval denied by scope policy", domain.ErrRetrievalDenied)
	}
	return nil
}

func recordFromRow(envelopeJSON string, inline sql.NullString) (domain.EventRecord, error) {
	var env domain.EventEnvelope
	if err := json.Unmarshal([]byte(envelopeJSON), &env); err != nil {
		return domain.EventRecord{}, fmt.Errorf("sqlite: decode envelope: %w", err)
	}
	payload := []byte(nil)
	if inline.Valid {
		payload = []byte(inline.String)
	} else {
		if env.Payload.ContentID != nil {
			payload, _ = json.Marshal(map[string]string{"content_id": env.Payload.ContentID.String()})
		}
	}
	return domain.EventRecord{Envelope: env, Payload: payload, Provenance: env.Provenance}, nil
}

func scanRecords(rows *sql.Rows) ([]domain.EventRecord, error) {
	var out []domain.EventRecord
	for rows.Next() {
		var envelopeJSON string
		var inline sql.NullString
		if err := rows.Scan(&envelopeJSON, &inline); err != nil {
			return nil, err
		}
		rec, err := recordFromRow(envelopeJSON, inline)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Bounded writer queue
// ---------------------------------------------------------------------------

type appendJob struct {
	ctx  context.Context
	env  domain.EventEnvelope
	opts EventsOptions
	out  chan<- appendOutcome
}

type appendOutcome struct {
	record domain.EventRecord
	err    error
}

type eventsWriter struct {
	db       *sql.DB
	queue    chan appendJob
	closed   chan struct{}
	maxBatch int
	guard    *RecordingGuard

	mu   sync.Mutex
	done bool
	wg   sync.WaitGroup
	subs sync.WaitGroup

	submitted atomic.Int64
	completed atomic.Int64
	rejected  atomic.Int64
	failures  atomic.Int64
	maxDepth  atomic.Int64
	closeOnce sync.Once
}

// WriterQueueStats is the observable backpressure signal for the event
// writer: depth approaching capacity, rejections, and failure counts.
type WriterQueueStats struct {
	Capacity  int
	Depth     int
	Submitted int64
	Completed int64
	Rejected  int64
	Failures  int64
	MaxDepth  int64
}

func newEventsWriter(db *sql.DB, capacity, maxBatch int) *eventsWriter {
	if maxBatch < 1 {
		maxBatch = 1
	}
	w := &eventsWriter{db: db, queue: make(chan appendJob, capacity), closed: make(chan struct{}), maxBatch: maxBatch, guard: NewRecordingGuard(nil, nil)}
	w.wg.Add(1)
	go w.run()
	return w
}

func (w *eventsWriter) setGuard(guard *RecordingGuard) {
	if guard != nil {
		w.guard = guard
	}
}

func (w *eventsWriter) submit(ctx context.Context, job appendJob) error {
	w.mu.Lock()
	if w.done {
		w.mu.Unlock()
		w.rejected.Add(1)
		return ErrWriterStopped
	}
	w.subs.Add(1)
	w.mu.Unlock()
	defer w.subs.Done()
	w.submitted.Add(1)
	select {
	case w.queue <- job:
		w.raiseMaxDepth(int64(len(w.queue)))
		return nil
	case <-ctx.Done():
		w.rejected.Add(1)
		return fmt.Errorf("%w: %v", ErrQueueFull, ctx.Err())
	case <-w.closed:
		w.rejected.Add(1)
		return ErrWriterStopped
	}
}

func (w *eventsWriter) close() {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		w.done = true
		w.mu.Unlock()
		close(w.closed)
	})
	// Every submit has now either enqueued or been rejected, so draining the
	// queue to empty loses nothing: acknowledged appends always run.
	w.subs.Wait()
	close(w.queue)
	w.wg.Wait()
}

func (w *eventsWriter) run() {
	defer w.wg.Done()
	for {
		job, ok := <-w.queue
		if !ok {
			return
		}
		w.commitBatch(drainBatch(w.queue, w.maxBatch, job))
	}
}

// commitBatch records one drained batch and delivers each caller its outcome.
// It tries a single group commit first; when that transaction fails it rolls
// back and re-runs each append individually, so one rejected event (a sequence
// gap, say) cannot fail unrelated events and every caller still gets the error
// its own append produced.
func (w *eventsWriter) commitBatch(batch []appendJob) {
	records, err := w.appendBatchTx(batch)
	if err != nil {
		records = nil
		for _, job := range batch {
			record, appendErr := w.appendTx(job)
			w.deliver(job, record, appendErr)
		}
		return
	}
	for i, job := range batch {
		w.deliver(job, records[i], nil)
	}
}

// deliver reports one completed append and updates the writer counters.
func (w *eventsWriter) deliver(job appendJob, record domain.EventRecord, err error) {
	w.completed.Add(1)
	if err != nil {
		w.failures.Add(1)
	}
	job.out <- appendOutcome{record: record, err: err}
}

// appendBatchTx commits the whole batch in one transaction. synchronous=FULL
// makes that single commit durable for every event it contains before any
// caller is acknowledged, so group commit never weakens the ack guarantee.
func (w *eventsWriter) appendBatchTx(batch []appendJob) ([]domain.EventRecord, error) {
	// The transaction outlives any one caller's context: an append is
	// deliberately recorded even when the caller's turn was cancelled (see
	// eventJournal), so batching must not let one expired context abort the
	// whole batch.
	tx, err := w.db.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin batch: %w", err)
	}
	records := make([]domain.EventRecord, len(batch))
	for i, job := range batch {
		record, err := appendRecord(context.Background(), tx, job.env, job.opts)
		if err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		records[i] = record
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit batch: %w", err)
	}
	return records, nil
}

func (w *eventsWriter) appendTx(job appendJob) (domain.EventRecord, error) {
	ctx := job.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		if refusal := w.guard.ObserveWriteErr(err); refusal != nil {
			return domain.EventRecord{}, refusal
		}
		return domain.EventRecord{}, fmt.Errorf("sqlite: begin: %w", err)
	}
	record, err := appendRecord(ctx, tx, job.env, job.opts)
	if err != nil {
		_ = tx.Rollback()
		if refusal := w.guard.ObserveWriteErr(err); refusal != nil {
			return domain.EventRecord{}, refusal
		}
		return domain.EventRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		if refusal := w.guard.ObserveWriteErr(err); refusal != nil {
			return domain.EventRecord{}, refusal
		}
		return domain.EventRecord{}, fmt.Errorf("sqlite: commit event: %w", err)
	}
	w.guard.ObserveWriteSuccess()
	return record, nil
}

func (w *eventsWriter) stats() WriterQueueStats {
	return WriterQueueStats{Capacity: cap(w.queue), Depth: len(w.queue), Submitted: w.submitted.Load(), Completed: w.completed.Load(), Rejected: w.rejected.Load(), Failures: w.failures.Load(), MaxDepth: w.maxDepth.Load()}
}

func (w *eventsWriter) raiseMaxDepth(depth int64) {
	for {
		current := w.maxDepth.Load()
		if depth <= current || w.maxDepth.CompareAndSwap(current, depth) {
			return
		}
	}
}

// appendRecord runs one event append inside the caller's transaction so the
// envelope, its index rows, and its search projection become durable together.
func appendRecord(ctx context.Context, tx *sql.Tx, env domain.EventEnvelope, opts EventsOptions) (domain.EventRecord, error) {
	var next uint64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(sequence), 0) + 1 FROM events WHERE session_id = ?`, env.SessionID.String()).Scan(&next); err != nil {
		return domain.EventRecord{}, fmt.Errorf("sqlite: next sequence: %w", err)
	}
	if env.Sequence != 0 && env.Sequence != next {
		return domain.EventRecord{}, domain.NewConflictError(domain.CodeRevisionMismatch, "session sequence gap", map[string]string{"session_id": env.SessionID.String(), "want": strconv.FormatUint(env.Sequence, 10), "have": strconv.FormatUint(next, 10)})
	}
	env.Sequence = next
	id, err := newEventID()
	if err != nil {
		return domain.EventRecord{}, err
	}
	env.EventID = id

	hasInline := len(bytes.TrimSpace(env.Payload.Inline)) > 0
	end := encodeJSON(env)
	payloadRef := encodeJSON(env.Payload)
	var inline sql.NullString
	if hasInline {
		inline = sql.NullString{String: string(env.Payload.Inline), Valid: true}
	}
	turnID, attemptID, appVersionID := "", "", ""
	if env.TurnID != nil {
		turnID = env.TurnID.String()
	}
	if env.AttemptID != nil {
		attemptID = env.AttemptID.String()
	}
	if env.AppVersionID != nil {
		appVersionID = env.AppVersionID.String()
	}
	scope := scopeFor(env)
	owner := ownerFor(env)
	res, err := tx.ExecContext(ctx,
		`INSERT INTO events (event_id, session_id, sequence, timestamp, monotonic_offset, kind, scope, owner_user, provenance_source, turn_id, attempt_id, app_version_id, payload_ref, payload_inline, envelope)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		env.EventID.String(), env.SessionID.String(), env.Sequence, env.Timestamp, env.MonotonicOffset,
		string(env.Kind), scope, owner, env.Provenance.Source, turnID, attemptID, appVersionID, payloadRef, inline, end)
	if err != nil {
		return domain.EventRecord{}, fmt.Errorf("sqlite: insert event: %w", err)
	}
	rowID, err := res.LastInsertId()
	if err != nil {
		return domain.EventRecord{}, fmt.Errorf("sqlite: event row id: %w", err)
	}
	if err := indexEvent(ctx, tx, rowID, env); err != nil {
		return domain.EventRecord{}, err
	}
	return domain.EventRecord{Envelope: env, Payload: payloadFor(env), Provenance: env.Provenance}, nil
}

func payloadFor(env domain.EventEnvelope) json.RawMessage {
	if len(bytes.TrimSpace(env.Payload.Inline)) > 0 {
		return env.Payload.Inline
	}
	if env.Payload.ContentID != nil {
		payload, _ := json.Marshal(map[string]string{"content_id": env.Payload.ContentID.String()})
		return payload
	}
	return nil
}

func encodeJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("sqlite: marshal envelope: %v", err))
	}
	return string(b)
}

// indexEvent derives the command/path lookup rows and the FTS5 transcript
// projection from the envelope in the same transaction as the event insert.
func indexEvent(ctx context.Context, tx *sql.Tx, rowID int64, env domain.EventEnvelope) error {
	commands, paths := indexHints(env)
	for _, command := range commands {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO command_index (event_rowid, command, normalized_command) VALUES (?,?,?)`,
			rowID, command, NormalizeCommand(command)); err != nil {
			return fmt.Errorf("sqlite: index command: %w", err)
		}
	}
	for _, path := range paths {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO path_index (event_rowid, path, path_key) VALUES (?,?,?)`,
			rowID, path, PathKey(path)); err != nil {
			return fmt.Errorf("sqlite: index path: %w", err)
		}
	}
	text := transcriptText(env)
	if strings.TrimSpace(text) != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO transcript_fts (event_id, text) VALUES (?, ?)`, env.EventID.String(), text); err != nil {
			return fmt.Errorf("sqlite: index transcript: %w", err)
		}
	}
	return nil
}

// scopeFor extracts a payload-declared visibility scope; envelopes whose
// payloads carry no scope are session-local.
func scopeFor(env domain.EventEnvelope) string {
	if len(env.Payload.Inline) == 0 {
		return domain.ScopeSession.String()
	}
	var holder struct {
		Scope domain.Scope `json:"scope"`
	}
	if err := json.Unmarshal(env.Payload.Inline, &holder); err == nil {
		switch holder.Scope {
		case domain.ScopeSession, domain.ScopeUser, domain.ScopeShared, domain.ScopeBaseline:
			return holder.Scope.String()
		}
	}
	return domain.ScopeSession.String()
}

// ownerFor extracts the owning user when the payload carries one (session
// start, app creation); otherwise the event is unowned session data.
func ownerFor(env domain.EventEnvelope) string {
	if len(env.Payload.Inline) == 0 {
		return ""
	}
	var holder struct {
		UserID domain.UserID `json:"user_id"`
		Owner  domain.UserID `json:"owner"`
	}
	if err := json.Unmarshal(env.Payload.Inline, &holder); err != nil {
		return ""
	}
	if !holder.UserID.IsZero() {
		return holder.UserID.String()
	}
	if !holder.Owner.IsZero() {
		return holder.Owner.String()
	}
	return ""
}

// indexHints extracts exact commands and paths from the payload's JSON
// shape. A lenient decode is deliberate: zero-valued identity fields on one
// payload must not abort indexing of the fields that are present.
func indexHints(env domain.EventEnvelope) (commands []string, paths []string) {
	if len(env.Payload.Inline) == 0 {
		return nil, nil
	}
	var fields map[string]any
	if err := json.Unmarshal(env.Payload.Inline, &fields); err != nil {
		return nil, nil
	}
	stringAt := func(key string) string {
		if v, ok := fields[key].(string); ok {
			return v
		}
		return ""
	}
	if command := stringAt("command"); command != "" {
		commands = append(commands, command)
	}
	for _, key := range []string{"path", "cwd"} {
		if p := stringAt(key); p != "" {
			paths = append(paths, p)
		}
	}
	if list, ok := fields["paths"].([]any); ok {
		for _, item := range list {
			if p, ok := item.(string); ok && p != "" {
				paths = append(paths, p)
			}
		}
	}
	return commands, paths
}

// transcriptText derives the human-meaningful search text per event kind so
// the FTS projection indexes prompts and responses rather than JSON punctuation.
func transcriptText(env domain.EventEnvelope) string {
	if len(env.Payload.Inline) == 0 {
		return ""
	}
	parts := []string{}
	switch env.Kind {
	case domain.EventKindModelResponse:
		var p domain.ModelResponsePayload
		if json.Unmarshal(env.Payload.Inline, &p) == nil {
			parts = append(parts, p.Text)
		}
	case domain.EventKindTerminalPrompt:
		var p domain.TerminalPromptPayload
		if json.Unmarshal(env.Payload.Inline, &p) == nil {
			parts = append(parts, p.Prompt, p.CWD)
		}
	case domain.EventKindTerminalFrame:
		var p domain.TerminalFramePayload
		if json.Unmarshal(env.Payload.Inline, &p) == nil {
			parts = append(parts, p.PromptText)
		}
	case domain.EventKindInputAccepted:
		var p domain.InputAcceptedPayload
		if json.Unmarshal(env.Payload.Inline, &p) == nil {
			parts = append(parts, p.Command, p.Action)
		}
	}
	if len(parts) == 0 {
		return string(env.Payload.Inline)
	}
	return strings.Join(parts, "\n")
}

func newEventID() (domain.EventID, error) {
	raw, err := random128()
	if err != nil {
		return domain.EventID{}, err
	}
	return domain.ParseEventID(domain.PrefixEvent + "_" + encodeLower128(raw))
}

// ---------------------------------------------------------------------------
// Cursor encoding for paginated retrieval
// ---------------------------------------------------------------------------

func encodeCursor(rowID int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(rowID, 10)))
}

func decodeCursor(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, fmt.Errorf("%w: undecodable cursor", domain.ErrInvalidCursor)
	}
	id, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || id < 0 {
		return 0, fmt.Errorf("%w: malformed cursor", domain.ErrInvalidCursor)
	}
	return id, nil
}

// NormalizeCommand collapses whitespace so the same invocation typed with
// different spacing shares one index row; case is preserved (Unix commands
// are case-sensitive).
func NormalizeCommand(command string) string {
	return strings.Join(strings.Fields(command), " ")
}

// PathKey is the indexed form of a path: the path with a trailing separator,
// making descendant lookups a plain string-range query.
func PathKey(path string) string {
	return strings.TrimSuffix(path, "/") + "/"
}

// SubtreeRange returns the half-open key range covering prefix and its
// descendants and excluding siblings whose names merely share characters:
// "/some" covers "/some" and "/some/x" but not "/somewhere".
func SubtreeRange(prefix string) (low, high string) {
	trimmed := strings.TrimSuffix(prefix, "/")
	return trimmed + "/", trimmed + "0"
}

// migrationV2 adds the research/event schema. It is registered through the
// shared registry because B01 owns migration numbering; the name is unique to
// the event store.
const migrationV2 = `
CREATE TABLE events (
	rowid INTEGER PRIMARY KEY AUTOINCREMENT,
	event_id TEXT NOT NULL UNIQUE,
	session_id TEXT NOT NULL,
	sequence INTEGER NOT NULL,
	timestamp INTEGER NOT NULL,
	monotonic_offset INTEGER NOT NULL,
	kind TEXT NOT NULL,
	scope TEXT NOT NULL DEFAULT 'session',
	owner_user TEXT NOT NULL DEFAULT '',
	provenance_source TEXT NOT NULL DEFAULT '',
	turn_id TEXT NOT NULL DEFAULT '',
	attempt_id TEXT NOT NULL DEFAULT '',
	app_version_id TEXT NOT NULL DEFAULT '',
	payload_ref TEXT NOT NULL,
	payload_inline TEXT,
	envelope TEXT NOT NULL,
	UNIQUE(session_id, sequence)
);
CREATE INDEX idx_events_session_seq ON events(session_id, sequence);
CREATE INDEX idx_events_kind_time ON events(kind, timestamp);
CREATE INDEX idx_events_time ON events(timestamp);
CREATE INDEX idx_events_owner ON events(owner_user);

CREATE TABLE command_index (
	event_rowid INTEGER NOT NULL REFERENCES events(rowid),
	command TEXT NOT NULL,
	normalized_command TEXT NOT NULL
);
CREATE INDEX idx_command_normalized ON command_index(normalized_command);
CREATE INDEX idx_command_rowid ON command_index(event_rowid);

CREATE TABLE path_index (
	event_rowid INTEGER NOT NULL REFERENCES events(rowid),
	path TEXT NOT NULL,
	path_key TEXT NOT NULL
);
CREATE INDEX idx_path_key ON path_index(path_key);

CREATE VIRTUAL TABLE transcript_fts USING fts5(event_id UNINDEXED, text);
`

func init() {
	RegisterMigration(Migration{Version: 2, Name: "events-research-store", SQL: migrationV2})
}
