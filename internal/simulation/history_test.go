package simulation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// seedCommandEvent records an input.accepted event with a command.
func seedCommandEvent(t *testing.T, events *fakeEvents, retrieval *fakeRetrieval, seq uint64, command string) domain.EventRecord {
	t.Helper()
	record := eventWithID(t, testSession, seq, domain.EventKindInputAccepted, domain.InputAcceptedPayload{
		Action:  "command",
		Command: command,
	})
	appended, err := events.Append(context.Background(), record.Envelope)
	if err != nil {
		t.Fatalf("append event: %v", err)
	}
	retrieval.add(appended, domain.ScopeSession, testUser)
	return appended
}

// seedPathEvent records a world.read event with paths.
func seedPathEvent(t *testing.T, events *fakeEvents, retrieval *fakeRetrieval, seq uint64, paths ...string) domain.EventRecord {
	t.Helper()
	valid := make([]domain.ValidPath, 0, len(paths))
	for _, p := range paths {
		valid = append(valid, domain.MustParsePath(p))
	}
	record := eventWithID(t, testSession, seq, domain.EventKindWorldRead, domain.WorldReadPayload{
		Scope: domain.ScopeUser,
		Paths: valid,
	})
	appended, err := events.Append(context.Background(), record.Envelope)
	if err != nil {
		t.Fatalf("append event: %v", err)
	}
	retrieval.add(appended, domain.ScopeSession, testUser)
	return appended
}

// TestHistorySearchExactCommand verifies exact command matching: only the
// exact command matches, not prefixes, suffixes, or substrings.
func TestHistorySearchExactCommand(t *testing.T) {
	registry, _, events, retrieval, _, _, _, _, _, _ := newTestRegistry()
	call := testCallContext()

	seedCommandEvent(t, events, retrieval, 1, "ls -la")
	seedCommandEvent(t, events, retrieval, 2, "ls -laX")
	seedCommandEvent(t, events, retrieval, 3, "ls")
	seedCommandEvent(t, events, retrieval, 4, "ls -la /tmp")

	result := mustExec(t, registry, call, "history.search", map[string]any{
		"query": "ls -la",
		"match": "exact_command",
	})
	var search historySearchResult
	if err := json.Unmarshal(result, &search); err != nil {
		t.Fatalf("unmarshal search: %v", err)
	}
	if len(search.Matches) != 1 {
		t.Fatalf("got %d matches, want exactly 1: %+v", len(search.Matches), search.Matches)
	}
	assertMatchFields(t, search.Matches[0], search.Matches[0].EventID, "exact_command")
	if search.Matches[0].Command != "ls -la" {
		t.Errorf("command = %q, want %q", search.Matches[0].Command, "ls -la")
	}
	if search.Matches[0].Sequence != 1 {
		t.Errorf("sequence = %d, want 1", search.Matches[0].Sequence)
	}
}

// TestHistorySearchBoundaryPath verifies normalized path matching with
// boundaries: the exact path matches, but a sibling with a longer name or a
// child path does not.
func TestHistorySearchBoundaryPath(t *testing.T) {
	registry, _, events, retrieval, _, _, _, _, _, _ := newTestRegistry()
	call := testCallContext()

	seedPathEvent(t, events, retrieval, 1, "/home/alice")
	seedPathEvent(t, events, retrieval, 2, "/home/alice2")
	seedPathEvent(t, events, retrieval, 3, "/home/alice/file.txt")
	seedPathEvent(t, events, retrieval, 4, "/home/bob")

	result := mustExec(t, registry, call, "history.search", map[string]any{
		"query": "/home/alice",
		"match": "path",
	})
	var search historySearchResult
	if err := json.Unmarshal(result, &search); err != nil {
		t.Fatalf("unmarshal search: %v", err)
	}
	if len(search.Matches) != 1 {
		t.Fatalf("got %d matches, want exactly 1: %+v", len(search.Matches), search.Matches)
	}
	if search.Matches[0].Sequence != 1 {
		t.Errorf("sequence = %d, want 1", search.Matches[0].Sequence)
	}
	if search.Matches[0].MatchType != "path" {
		t.Errorf("match type = %q, want path", search.Matches[0].MatchType)
	}

	// A trailing slash normalizes to the same path and still matches.
	result = mustExec(t, registry, call, "history.search", map[string]any{
		"query": "/home/alice/",
		"match": "path",
	})
	var normalized historySearchResult
	if err := json.Unmarshal(result, &normalized); err != nil {
		t.Fatalf("unmarshal normalized search: %v", err)
	}
	if len(normalized.Matches) != 1 {
		t.Fatalf("normalized query got %d matches, want 1", len(normalized.Matches))
	}

	// An invalid query path matches nothing rather than erroring.
	result = mustExec(t, registry, call, "history.search", map[string]any{
		"query": "relative/path",
		"match": "path",
	})
	var invalid historySearchResult
	if err := json.Unmarshal(result, &invalid); err != nil {
		t.Fatalf("unmarshal invalid search: %v", err)
	}
	if len(invalid.Matches) != 0 {
		t.Errorf("invalid query got %d matches, want 0", len(invalid.Matches))
	}
}

// TestHistorySearchTextAndLimits covers full-text matching, kind filtering,
// and result bounds.
func TestHistorySearchTextAndLimits(t *testing.T) {
	registry, _, events, retrieval, _, _, _, _, _, _ := newTestRegistry()
	call := testCallContext()

	seedCommandEvent(t, events, retrieval, 1, "cat /etc/hostname")
	seedCommandEvent(t, events, retrieval, 2, "grep hostname /var/log/syslog")
	seedCommandEvent(t, events, retrieval, 3, "reboot")

	result := mustExec(t, registry, call, "history.search", map[string]any{
		"query": "hostname",
		"match": "text",
	})
	var search historySearchResult
	if err := json.Unmarshal(result, &search); err != nil {
		t.Fatalf("unmarshal search: %v", err)
	}
	if len(search.Matches) != 2 {
		t.Fatalf("text search got %d matches, want 2", len(search.Matches))
	}

	// Kind filtering narrows to input.accepted events only.
	result = mustExec(t, registry, call, "history.search", map[string]any{
		"query": "hostname",
		"match": "text",
		"kind":  "input.accepted",
	})
	var filtered historySearchResult
	if err := json.Unmarshal(result, &filtered); err != nil {
		t.Fatalf("unmarshal filtered search: %v", err)
	}
	if len(filtered.Matches) != 2 {
		t.Fatalf("kind-filtered search got %d matches, want 2", len(filtered.Matches))
	}

	// The limit bounds the result page.
	result = mustExec(t, registry, call, "history.search", map[string]any{
		"query": "hostname",
		"match": "text",
		"limit": 1,
	})
	var limited historySearchResult
	if err := json.Unmarshal(result, &limited); err != nil {
		t.Fatalf("unmarshal limited search: %v", err)
	}
	if len(limited.Matches) != 1 {
		t.Fatalf("limited search got %d matches, want 1", len(limited.Matches))
	}
	if !limited.Truncated {
		t.Error("limited search should report truncation")
	}

	// An unknown match type is a validation error.
	err := mustExecErr(t, registry, call, "history.search", map[string]any{
		"query": "hostname",
		"match": "fuzzy",
	})
	if !domain.IsValidationError(err) {
		t.Fatalf("unknown match type: error = %v, want validation", err)
	}
}

// TestHistorySearchSharedScopeDenied verifies that naming the shared scope
// while sharing is disabled is a denial, and that the default scope set
// silently excludes shared history.
func TestHistorySearchSharedScopeDenied(t *testing.T) {
	registry, _, _, retrieval, _, _, _, _, _, _ := newTestRegistry()
	call := sharingOffCall()

	shared := eventWithID(t, testSession, 100, domain.EventKindInputAccepted, domain.InputAcceptedPayload{
		Action: "command", Command: "shared-command",
	})
	retrieval.add(shared, domain.ScopeShared, testUser2)

	err := mustExecErr(t, registry, call, "history.search", map[string]any{
		"query": "shared-command",
		"match": "exact_command",
		"scope": "shared",
	})
	if !domain.IsDeniedError(err) {
		t.Fatalf("shared search while sharing off: error = %v, want denied", err)
	}
	var de *domain.DomainError
	if !errors.As(err, &de) || de.Code != domain.CodeSharingDisabled {
		t.Fatalf("shared search: error = %v, want code %q", err, domain.CodeSharingDisabled)
	}

	// The default search must not surface the shared event either.
	result := mustExec(t, registry, call, "history.search", map[string]any{
		"query": "shared-command",
		"match": "exact_command",
	})
	var search historySearchResult
	if err := json.Unmarshal(result, &search); err != nil {
		t.Fatalf("unmarshal search: %v", err)
	}
	if len(search.Matches) != 0 {
		t.Errorf("default search surfaced %d shared matches, want 0", len(search.Matches))
	}
}

// TestHistoryContextSurrounding verifies the surrounding-event fetch with
// bounded before/after windows.
func TestHistoryContextSurrounding(t *testing.T) {
	registry, _, events, retrieval, _, _, _, _, _, _ := newTestRegistry()
	call := testCallContext()

	var middle domain.EventRecord
	for i := uint64(1); i <= 5; i++ {
		record := seedCommandEvent(t, events, retrieval, i, "cmd"+string(rune('0'+i)))
		if i == 3 {
			middle = record
		}
	}

	result := mustExec(t, registry, call, "history.context", map[string]any{
		"event_id": middle.Envelope.EventID.String(),
		"before":   2,
		"after":    1,
	})
	var ctx historyContextResult
	if err := json.Unmarshal(result, &ctx); err != nil {
		t.Fatalf("unmarshal context: %v", err)
	}
	if ctx.Target == nil || ctx.Target.Envelope.EventID != middle.Envelope.EventID {
		t.Fatalf("target = %v, want event %v", ctx.Target, middle.Envelope.EventID)
	}
	if len(ctx.Before) != 2 {
		t.Fatalf("got %d before events, want 2", len(ctx.Before))
	}
	if len(ctx.After) != 1 {
		t.Fatalf("got %d after events, want 1", len(ctx.After))
	}
	if ctx.Before[0].Envelope.Sequence != 1 || ctx.Before[1].Envelope.Sequence != 2 {
		t.Errorf("before sequences = %d,%d want 1,2", ctx.Before[0].Envelope.Sequence, ctx.Before[1].Envelope.Sequence)
	}
	if ctx.After[0].Envelope.Sequence != 4 {
		t.Errorf("after sequence = %d, want 4", ctx.After[0].Envelope.Sequence)
	}
}

// TestHistoryContextTurn verifies fetching a referenced turn's events.
func TestHistoryContextTurn(t *testing.T) {
	registry, _, events, retrieval, _, _, _, _, _, _ := newTestRegistry()
	call := testCallContext()
	turnID := mustParseTurnID("trn_01ARZ3NDEKTSV4RRFFQ69G5FAW")

	for i := uint64(1); i <= 3; i++ {
		record := eventWithID(t, testSession, i, domain.EventKindInputAccepted, domain.InputAcceptedPayload{
			Action: "command", Command: "turn-cmd",
		})
		record.Envelope.TurnID = &turnID
		appended, err := events.Append(context.Background(), record.Envelope)
		if err != nil {
			t.Fatalf("append event: %v", err)
		}
		retrieval.add(appended, domain.ScopeSession, testUser)
	}

	result := mustExec(t, registry, call, "history.context", map[string]any{
		"turn_id": turnID.String(),
	})
	var ctx historyContextResult
	if err := json.Unmarshal(result, &ctx); err != nil {
		t.Fatalf("unmarshal turn context: %v", err)
	}
	if len(ctx.TurnEvents) != 3 {
		t.Fatalf("got %d turn events, want 3", len(ctx.TurnEvents))
	}
}

// TestHistoryContextDropsSecretRecords verifies that a record consisting
// solely of secret material is dropped, never disclosed.
func TestHistoryContextDropsSecretRecords(t *testing.T) {
	registry, _, events, retrieval, _, _, _, _, _, redactor := newTestRegistry()
	call := testCallContext()

	secretPayload, _ := json.Marshal(map[string]string{"api_key": "sk-abcdefghijklmnopqrstuvwxyz123456"})
	secret := eventWithID(t, testSession, 1, domain.EventKindInputAccepted, nil)
	secret.Payload = secretPayload
	secret.Envelope.Payload.Inline = secretPayload
	appended, err := events.Append(context.Background(), secret.Envelope)
	if err != nil {
		t.Fatalf("append secret event: %v", err)
	}
	retrieval.add(appended, domain.ScopeSession, testUser)
	redactor.dropped[string(secretPayload)] = true

	plain := seedCommandEvent(t, events, retrieval, 2, "ls")

	result := mustExec(t, registry, call, "history.context", map[string]any{
		"event_id": plain.Envelope.EventID.String(),
		"before":   5,
		"after":    0,
	})
	var ctx historyContextResult
	if err := json.Unmarshal(result, &ctx); err != nil {
		t.Fatalf("unmarshal context: %v", err)
	}
	if len(ctx.Before) != 0 {
		t.Fatalf("secret record disclosed in before window: %d records", len(ctx.Before))
	}
	if !ctx.Truncated {
		t.Error("dropping a secret record should set the truncated flag")
	}
}

// TestHistoryContextDirectIDCrossSessionDenied verifies the direct-ID boundary
// of PLAN 10.4: while sharing is disabled, an event recorded in another session
// is not readable by its ID, whichever scope it carries.
func TestHistoryContextDirectIDCrossSessionDenied(t *testing.T) {
	registry, _, events, retrieval, _, _, _, _, _, _ := newTestRegistry()
	call := sharingOffCall()

	// An event recorded in another session, registered as session-scoped so a
	// store that only checks scope would disclose it.
	other := eventWithID(t, testSession2, 1, domain.EventKindInputAccepted, domain.InputAcceptedPayload{
		Action: "command", Command: "secret-other-session",
	})
	appended, err := events.Append(context.Background(), other.Envelope)
	if err != nil {
		t.Fatalf("append other-session event: %v", err)
	}
	retrieval.add(appended, domain.ScopeSession, testUser2)

	err = mustExecErr(t, registry, call, "history.context", map[string]any{
		"event_id": appended.Envelope.EventID.String(),
	})
	if !domain.IsDeniedError(err) {
		t.Fatalf("cross-session direct ID while sharing off: error = %v, want denied", err)
	}
	var de *domain.DomainError
	if !errors.As(err, &de) || de.Code != domain.CodeSharingDisabled {
		t.Fatalf("cross-session direct ID: error = %v, want code %q", err, domain.CodeSharingDisabled)
	}
}

// TestHistoryContextDirectIDOwnSessionAllowed verifies the caller's own session
// events stay readable by ID while sharing is disabled.
func TestHistoryContextDirectIDOwnSessionAllowed(t *testing.T) {
	registry, _, events, retrieval, _, _, _, _, _, _ := newTestRegistry()
	call := sharingOffCall()

	record := seedCommandEvent(t, events, retrieval, 1, "own-command")
	result := mustExec(t, registry, call, "history.context", map[string]any{
		"event_id": record.Envelope.EventID.String(),
	})
	var ctx historyContextResult
	if err := json.Unmarshal(result, &ctx); err != nil {
		t.Fatalf("unmarshal context: %v", err)
	}
	if ctx.Target == nil || ctx.Target.Envelope.EventID != record.Envelope.EventID {
		t.Fatalf("own-session direct ID denied unexpectedly: %+v", ctx.Target)
	}
}

// TestHistoryContextSecretTargetDenied verifies that referencing a secret
// record directly is a typed denial.
func TestHistoryContextSecretTargetDenied(t *testing.T) {
	registry, _, events, retrieval, _, _, _, _, _, redactor := newTestRegistry()
	call := testCallContext()

	secretPayload, _ := json.Marshal(map[string]string{"password": "hunter2"})
	secret := eventWithID(t, testSession, 1, domain.EventKindInputAccepted, nil)
	secret.Payload = secretPayload
	secret.Envelope.Payload.Inline = secretPayload
	appended, err := events.Append(context.Background(), secret.Envelope)
	if err != nil {
		t.Fatalf("append secret event: %v", err)
	}
	retrieval.add(appended, domain.ScopeSession, testUser)
	redactor.dropped[string(secretPayload)] = true

	err = mustExecErr(t, registry, call, "history.context", map[string]any{
		"event_id": secret.Envelope.EventID.String(),
	})
	var de *domain.DomainError
	if !errors.As(err, &de) || de.Code != CodeRecordUndisclosable {
		t.Fatalf("secret target: error = %v, want code %q", err, CodeRecordUndisclosable)
	}
}
