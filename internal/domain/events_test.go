package domain_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// TestEventKindsAreRegisteredAndUnique guards the vocabulary the event store
// validates an append against. A kind that is not registered is refused on
// write, so a producer that invents one loses the record instead of storing an
// event no reader can decode.
func TestEventKindsAreRegisteredAndUnique(t *testing.T) {
	seen := make(map[domain.EventKind]bool, len(domain.AllEventKinds()))
	for _, kind := range domain.AllEventKinds() {
		if seen[kind] {
			t.Errorf("event kind %q is registered more than once", kind)
		}
		seen[kind] = true
		if !domain.IsValidEventKind(kind) {
			t.Errorf("registered kind %q is rejected by IsValidEventKind", kind)
		}
	}

	// PLAN 7.1 requires a durable event for every turn transition, so the
	// turn-lifecycle kind must be part of the shared vocabulary.
	if !domain.IsValidEventKind(domain.EventKindTurnTransition) {
		t.Errorf("event kind %q is not registered", domain.EventKindTurnTransition)
	}
	if domain.IsValidEventKind("turn.unknown") {
		t.Error("an unregistered event kind is accepted")
	}
}

// TestTurnTransitionPayloadRoundTrips proves a turn-lifecycle record survives
// the shared payload decoder, which is how a research reader reconstructs a
// turn path from the event stream.
func TestTurnTransitionPayloadRoundTrips(t *testing.T) {
	attempt := mustParse(t, "att_0123456789ABCDEFGHJKMNPQRS", domain.ParseAttemptID)
	payload := domain.TurnTransitionPayload{
		From:             domain.StateGenerating,
		To:               domain.StateValidating,
		Generation:       2,
		Attempt:          &attempt,
		ConfigVersion:    7,
		PromptVersion:    "prompt-v3",
		CatalogueVersion: "catalogue-v2",
		Reason:           "validating candidate",
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const wantJSON = `{"from":"generating","to":"validating","generation":2,` +
		`"attempt_id":"att_0123456789ABCDEFGHJKMNPQRS","config_version":7,` +
		`"prompt_version":"prompt-v3","catalogue_version":"catalogue-v2",` +
		`"reason":"validating candidate"}`
	if string(raw) != wantJSON {
		t.Errorf("marshals\n %s\nwant\n %s", raw, wantJSON)
	}

	decoded, err := domain.UnmarshalEventPayload(domain.EventKindTurnTransition, raw)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	typed, ok := decoded.(*domain.TurnTransitionPayload)
	if !ok {
		t.Fatalf("decoded into %T, want *domain.TurnTransitionPayload", decoded)
	}
	if !reflect.DeepEqual(*typed, payload) {
		t.Errorf("round trip:\n want %+v\n got  %+v", payload, *typed)
	}
	if typed.EventKind() != domain.EventKindTurnTransition {
		t.Errorf("payload reports kind %q, want %q", typed.EventKind(), domain.EventKindTurnTransition)
	}
}

// TestTurnTransitionPayloadOmitsAbsentAttemptAndVersions pins the shape of the
// transition that establishes a turn: it has no predecessor state, no attempt,
// and no pinned versions, and none of them may appear in the record.
func TestTurnTransitionPayloadOmitsAbsentAttemptAndVersions(t *testing.T) {
	payload := domain.TurnTransitionPayload{From: "", To: domain.StateReceived, Generation: 0}

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const wantJSON = `{"from":"","to":"received","generation":0}`
	if string(raw) != wantJSON {
		t.Errorf("marshals\n %s\nwant\n %s", raw, wantJSON)
	}

	decoded, err := domain.UnmarshalEventPayload(domain.EventKindTurnTransition, raw)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	typed, ok := decoded.(*domain.TurnTransitionPayload)
	if !ok {
		t.Fatalf("decoded into %T, want *domain.TurnTransitionPayload", decoded)
	}
	if !reflect.DeepEqual(*typed, payload) {
		t.Errorf("round trip:\n want %+v\n got  %+v", payload, *typed)
	}
}

// TestTerminalWriteOutcomePayloadRoundTrips proves a transport write outcome
// decodes under the kind the envelope already declared, so a recorded write is
// readable as research metadata (PLAN 10.1).
func TestTerminalWriteOutcomePayloadRoundTrips(t *testing.T) {
	cases := []struct {
		name     string
		payload  domain.TerminalWriteOutcomePayload
		wantJSON string
	}{
		{
			name:     "written",
			payload:  domain.TerminalWriteOutcomePayload{OutputSequence: 3, Status: "written", ByteCount: 12},
			wantJSON: `{"output_sequence":3,"status":"written","byte_count":12}`,
		},
		{
			name:     "failed",
			payload:  domain.TerminalWriteOutcomePayload{OutputSequence: 4, Status: "failed", Error: "connection reset"},
			wantJSON: `{"output_sequence":4,"status":"failed","byte_count":0,"error":"connection reset"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(raw) != tc.wantJSON {
				t.Errorf("marshals\n %s\nwant\n %s", raw, tc.wantJSON)
			}

			decoded, err := domain.UnmarshalEventPayload(domain.EventKindTerminalWrite, raw)
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			typed, ok := decoded.(*domain.TerminalWriteOutcomePayload)
			if !ok {
				t.Fatalf("decoded into %T, want *domain.TerminalWriteOutcomePayload", decoded)
			}
			if !reflect.DeepEqual(*typed, tc.payload) {
				t.Errorf("round trip:\n want %+v\n got  %+v", tc.payload, *typed)
			}
			if typed.EventKind() != domain.EventKindTerminalWrite {
				t.Errorf("payload reports kind %q, want %q", typed.EventKind(), domain.EventKindTerminalWrite)
			}
		})
	}
}

// TestUnmarshalEventPayloadRejectsUnknownKind keeps the decoder total: only a
// registered kind with a declared payload shape decodes.
func TestUnmarshalEventPayloadRejectsUnknownKind(t *testing.T) {
	if _, err := domain.UnmarshalEventPayload(domain.EventKind("turn.unknown"), []byte(`{}`)); err == nil {
		t.Fatal("an unregistered kind decoded, want an error")
	}
}
