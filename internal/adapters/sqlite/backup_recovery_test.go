package sqlite

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// fixedTestClock is a deterministic clock for recovery timestamps.
type fixedTestClock struct{ now int64 }

func (c fixedTestClock) NowUnixMilli() int64   { return c.now }
func (c fixedTestClock) MonotonicNanos() int64 { return c.now * 1_000_000 }

// appendTurnEvent appends an event carrying a turn reference, which the
// recovery queries group by.
func appendTurnEvent(t *testing.T, events *Events, session domain.SessionID, kind domain.EventKind, turn domain.TurnID, payload any, ts int64) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	envelope := makeEnvelope(session, kind, raw, ts)
	envelope.TurnID = &turn
	if _, err := events.Append(context.Background(), envelope); err != nil {
		t.Fatalf("append %s with turn: %v", kind, err)
	}
}

// TestIncompleteTurnRecoveryMarksInterruptedWork requires startup recovery to
// find a session that started but never ended, mark it recovered with a
// durable event, and list its recorded turns as interrupted. A second run is
// a no-op.
func TestIncompleteTurnRecoveryMarksInterruptedWork(t *testing.T) {
	ctx := context.Background()
	db, events := testEvents(t)
	recovery := NewRecovery(db, events, fixedTestClock{now: 1_700_000_100_000})

	interrupted := testSessionID(t)
	appendTestEvent(t, events, interrupted, domain.EventKindSessionStart, map[string]string{"kind": "start"}, 1_700_000_000_000)
	turnOne, _ := testChangeIDs(t)
	turnTwo, _ := testChangeIDs(t)
	appendTurnEvent(t, events, interrupted, domain.EventKindModelRequest, turnOne, map[string]string{"step": "one"}, 1_700_000_000_001)
	appendTurnEvent(t, events, interrupted, domain.EventKindModelResponse, turnOne, map[string]string{"step": "one-done"}, 1_700_000_000_002)
	appendTurnEvent(t, events, interrupted, domain.EventKindModelRequest, turnTwo, map[string]string{"step": "two"}, 1_700_000_000_003)

	complete := testSessionID(t)
	appendTestEvent(t, events, complete, domain.EventKindSessionStart, map[string]string{"kind": "start"}, 1_700_000_000_000)
	appendTestEvent(t, events, complete, domain.EventKindSessionEnd, domain.SessionEndPayload{Reason: "exit"}, 1_700_000_000_010)

	incomplete, err := recovery.IncompleteSessions(ctx)
	if err != nil {
		t.Fatalf("IncompleteSessions: %v", err)
	}
	if len(incomplete) != 1 || incomplete[0].SessionID != interrupted {
		t.Fatalf("IncompleteSessions = %+v, want only %s", incomplete, interrupted)
	}
	if incomplete[0].InterruptedTurns != 2 {
		t.Errorf("interrupted turns = %d, want 2", incomplete[0].InterruptedTurns)
	}

	report, err := recovery.RecoverIncomplete(ctx)
	if err != nil {
		t.Fatalf("RecoverIncomplete: %v", err)
	}
	if report.IncompleteSessions != 1 || report.MarkedSessions != 1 || report.InterruptedTurns != 2 {
		t.Fatalf("recovery report = %+v, want 1 incomplete / 1 marked / 2 turns", report)
	}

	// The recovery marker is durable and carries the interrupted-turn count.
	records, err := events.List(ctx, interrupted, 0, 100)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("no events for the interrupted session")
	}
	last := records[len(records)-1]
	if last.Envelope.Kind != domain.EventKindSessionRecovered {
		t.Fatalf("last event kind = %s, want session.recovered", last.Envelope.Kind)
	}
	var payload sessionRecoveredPayload
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		t.Fatalf("decode recovery payload: %v", err)
	}
	if payload.InterruptedTurnCount != 2 || len(payload.InterruptedTurns) != 2 {
		t.Errorf("recovery payload = %+v, want 2 interrupted turns", payload)
	}
	if payload.Reason != "startup_recovery" {
		t.Errorf("recovery reason = %q, want startup_recovery", payload.Reason)
	}

	// Recovery is idempotent: the marker excludes the session next time.
	again, err := recovery.IncompleteSessions(ctx)
	if err != nil {
		t.Fatalf("IncompleteSessions after recovery: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("IncompleteSessions after recovery = %+v, want none", again)
	}
	second, err := recovery.RecoverIncomplete(ctx)
	if err != nil {
		t.Fatalf("second RecoverIncomplete: %v", err)
	}
	if second.MarkedSessions != 0 {
		t.Errorf("second recovery marked %d sessions, want 0", second.MarkedSessions)
	}
}

// TestSessionListingRendersRecoveryState requires the session listing to
// distinguish complete, recovered, and still-incomplete sessions and to page
// by session id.
func TestSessionListingRendersRecoveryState(t *testing.T) {
	ctx := context.Background()
	db, events := testEvents(t)
	recovery := NewRecovery(db, events, fixedTestClock{now: 1_700_000_200_000})

	open := testSessionID(t)
	appendTestEvent(t, events, open, domain.EventKindSessionStart, map[string]string{"kind": "start"}, 1_700_000_000_000)

	closed := testSessionID(t)
	appendTestEvent(t, events, closed, domain.EventKindSessionStart, map[string]string{"kind": "start"}, 1_700_000_000_000)
	appendTestEvent(t, events, closed, domain.EventKindSessionEnd, domain.SessionEndPayload{Reason: "exit"}, 1_700_000_000_020)

	third := testSessionID(t)
	appendTestEvent(t, events, third, domain.EventKindSessionStart, map[string]string{"kind": "start"}, 1_700_000_000_000)
	appendTestEvent(t, events, third, domain.EventKindSessionEnd, domain.SessionEndPayload{Reason: "disconnect"}, 1_700_000_000_030)

	if _, err := recovery.RecoverIncomplete(ctx); err != nil {
		t.Fatalf("RecoverIncomplete: %v", err)
	}

	all, next, err := recovery.ListSessions(ctx, 2, "")
	if err != nil {
		t.Fatalf("ListSessions page 1: %v", err)
	}
	if len(all) != 2 || next == "" {
		t.Fatalf("page 1 = %d sessions next=%q, want 2 and a cursor", len(all), next)
	}
	rest, next2, err := recovery.ListSessions(ctx, 2, next)
	if err != nil {
		t.Fatalf("ListSessions page 2: %v", err)
	}
	if len(rest) != 1 || next2 != "" {
		t.Fatalf("page 2 = %d sessions next=%q, want 1 and no cursor", len(rest), next2)
	}

	byID := map[domain.SessionID]SessionSummaryForTest{}
	for _, summary := range append(all, rest...) {
		byID[summary.SessionID] = SessionSummaryForTest{Ended: summary.Ended, Recovered: summary.Recovered}
	}
	if got := byID[open]; !got.Recovered || got.Ended {
		t.Errorf("recovered session flags = %+v, want Recovered=true, Ended=false", got)
	}
	if got := byID[closed]; !got.Ended || got.Recovered {
		t.Errorf("closed session flags = %+v, want Ended=true", got)
	}
	if got := byID[third]; !got.Ended || got.Recovered {
		t.Errorf("third session flags = %+v, want Ended=true", got)
	}

	expected := []string{open.String(), closed.String(), third.String()}
	sort.Strings(expected)
	returned := []string{all[0].SessionID.String(), all[1].SessionID.String(), rest[0].SessionID.String()}
	sort.Strings(returned)
	for i := range expected {
		if expected[i] != returned[i] {
			t.Fatalf("listed sessions = %v, want %v", returned, expected)
		}
	}
}

// SessionSummaryForTest is a tiny projection so the assertion above reads
// clearly.
type SessionSummaryForTest struct {
	Ended     bool
	Recovered bool
}
