package sqlite

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

func TestClassifyDurableFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want DurableFailureKind
	}{
		{"nil", nil, DurableFailureNone},
		{"readonly text", errors.New("attempt to write a readonly database (8)"), DurableFailureReadOnly},
		{"read-only text", errors.New("read-only filesystem"), DurableFailureReadOnly},
		{"erofs", fmt.Errorf("open: %w", syscall.EROFS), DurableFailureReadOnly},
		{"enospc", fmt.Errorf("write: %w", syscall.ENOSPC), DurableFailureFull},
		{"disk full text", errors.New("database or disk is full (13)"), DurableFailureFull},
		{"corrupt", errors.New("database disk image is malformed (11)"), DurableFailureCorrupt},
		{"notadb", errors.New("file is not a database"), DurableFailureCorrupt},
		{"wal failure", errors.New("WAL write failed (disk I/O error)"), DurableFailureCorrupt},
		{"eio", fmt.Errorf("read: %w", syscall.EIO), DurableFailureIO},
		{"io text", errors.New("input/output error"), DurableFailureIO},
		// Ordinary work must never disable recording.
		{"conflict", domain.NewConflictError(domain.CodeRevisionMismatch, "revision mismatch", nil), DurableFailureNone},
		{"validation", domain.NewValidationError(domain.CodeInvalidInput, "bad input", nil), DurableFailureNone},
		{"notfound", domain.NewNotFoundError(domain.CodeNodeNotFound, "missing", nil), DurableFailureNone},
		{"unrelated", errors.New("some other SQL error"), DurableFailureNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyDurableFailure(tc.err); got != tc.want {
				t.Fatalf("ClassifyDurableFailure(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestGuardRefusesAndRecovers(t *testing.T) {
	guard := NewRecordingGuard(nil, nil)
	if !guard.Available() {
		t.Fatal("new guard is not available")
	}
	if err := guard.RefuseRecording(); err != nil {
		t.Fatalf("available guard refused: %v", err)
	}

	// A durable error trips the guard and returns a typed refusal.
	refusal := guard.ObserveWriteErr(errors.New("disk I/O error"))
	if refusal == nil {
		t.Fatal("durable error did not produce a refusal")
	}
	if !errors.Is(refusal, domain.CategoryUnavailable) {
		t.Fatalf("refusal category = %v, want unavailable", domain.GetErrorCategory(refusal))
	}
	if code := domain.GetErrorCode(refusal); code != domain.CodeRecordingUnavailable {
		t.Fatalf("refusal code = %q, want %q", code, domain.CodeRecordingUnavailable)
	}
	if guard.Available() || guard.Kind() != DurableFailureIO {
		t.Fatalf("guard still available or wrong kind: available=%v kind=%q", guard.Available(), guard.Kind())
	}
	// While down, every semantic write is refused with the same typed error.
	if err := guard.RefuseRecording(); !errors.Is(err, domain.CategoryUnavailable) {
		t.Fatalf("RefuseRecording = %v, want unavailable", err)
	}

	// A later success clears the condition.
	guard.ObserveWriteSuccess()
	if !guard.Available() || guard.Kind() != DurableFailureNone {
		t.Fatalf("guard did not recover: available=%v kind=%q", guard.Available(), guard.Kind())
	}
	if err := guard.RefuseRecording(); err != nil {
		t.Fatalf("recovered guard refused: %v", err)
	}
}

func TestGuardIgnoresConflicts(t *testing.T) {
	guard := NewRecordingGuard(nil, nil)
	conflict := domain.NewConflictError(domain.CodeStaleRead, "stale", nil)
	if err := guard.ObserveWriteErr(conflict); err != nil {
		t.Fatalf("conflict produced a refusal: %v", err)
	}
	if !guard.Available() {
		t.Fatal("conflict disabled recording")
	}
}

// TestReadOnlyDatabaseRefusesSemanticWrites simulates a read-only database by
// enabling query_only on the store's single connection: semantic writes fail
// with a durable read-only error and are refused with the typed error, while
// reads of already-recorded history keep working.
func TestReadOnlyDatabaseRefusesSemanticWrites(t *testing.T) {
	db := testDB(t, DefaultOptions())
	ctx := context.Background()
	guard := NewRecordingGuard(nil, nil)
	db.SetRecordingGuard(guard)

	evts, err := NewEvents(db.SQL(), EventsOptions{})
	if err != nil {
		t.Fatalf("NewEvents: %v", err)
	}
	t.Cleanup(evts.Close)
	evts.SetRecordingGuard(guard)

	user := testUserID(t)
	_, _, personal := mustNamespaces(t, ctx, db, user)
	session := testSessionID(t)
	payload := inlineJSON(t, domain.ShutdownPayload{Reason: "signal", Graceful: true})

	// One write first, so there is recorded history to read afterwards.
	first, err := evts.Append(ctx, makeEnvelope(session, domain.EventKindShutdown, payload, 1000))
	if err != nil {
		t.Fatalf("first Append: %v", err)
	}

	// Make the connection read-only; writes now fail as a durable condition.
	if _, err := db.SQL().ExecContext(ctx, `PRAGMA query_only=ON`); err != nil {
		t.Fatalf("enable query_only: %v", err)
	}

	// Semantic append is refused with the typed recording-unavailable error.
	_, err = evts.Append(ctx, makeEnvelope(session, domain.EventKindShutdown, payload, 1001))
	if !errors.Is(err, domain.CategoryUnavailable) {
		t.Fatalf("Append while read-only = %v, want unavailable", err)
	}
	if code := domain.GetErrorCode(err); code != domain.CodeRecordingUnavailable {
		t.Fatalf("Append refusal code = %q, want %q", code, domain.CodeRecordingUnavailable)
	}
	if guard.Available() {
		t.Fatal("guard available after durable append failure")
	}

	// World commit is refused with the same typed condition.
	turn, attempt := testChangeIDs(t)
	cs := domain.ChangeSet{
		TurnID:    turn,
		AttemptID: attempt,
		Mutations: []domain.Mutation{{
			Type:        domain.MutationCreate,
			NamespaceID: personal.ID,
			Path:        mustPath(t, "/new-file"),
			Kind:        domain.NodeKindFile,
		}},
	}
	if _, err := db.Commit(ctx, cs, domain.DefaultScopePolicy()); !errors.Is(err, domain.CategoryUnavailable) {
		t.Fatalf("Commit while read-only = %v, want unavailable", err)
	}

	// Reads of recorded history keep working: read-only access is preserved.
	got, err := evts.GetByID(ctx, first.Envelope.EventID)
	if err != nil {
		t.Fatalf("GetByID while recording down: %v", err)
	}
	if got.Envelope.Sequence != 1 {
		t.Fatalf("read sequence = %d, want 1", got.Envelope.Sequence)
	}
	if _, err := db.LookupPath(ctx, personal.ID, domain.RootPath()); err != nil {
		t.Fatalf("world read (LookupPath root) while recording down: %v", err)
	}
	list, err := evts.List(ctx, session, 0, 100)
	if err != nil {
		t.Fatalf("List while recording down: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("listed %d events, want 1", len(list))
	}

	// Recovery: restoring writability and one successful durable probe clears
	// the condition; semantic appends are refused while down, so an explicit
	// probe is the path back.
	if _, err := db.SQL().ExecContext(ctx, `PRAGMA query_only=OFF`); err != nil {
		t.Fatalf("disable query_only: %v", err)
	}
	if err := guard.ProbeWritable(func() error {
		return db.SQL().PingContext(ctx)
	}); err != nil {
		t.Fatalf("ProbeWritable after recovery: %v", err)
	}
	if !guard.Available() {
		t.Fatal("guard still down after a successful probe")
	}
	recovered, err := evts.Append(ctx, makeEnvelope(session, domain.EventKindShutdown, payload, 1002))
	if err != nil {
		t.Fatalf("Append after recovery: %v", err)
	}
	if recovered.Envelope.Sequence != 2 {
		t.Fatalf("recovered sequence = %d, want 2", recovered.Envelope.Sequence)
	}
}

func mustPath(t *testing.T, raw string) domain.ValidPath {
	t.Helper()
	p, err := domain.ParsePath(raw)
	if err != nil {
		t.Fatalf("ParsePath(%q): %v", raw, err)
	}
	return p
}
