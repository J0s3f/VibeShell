package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

func appendTestEvent(t *testing.T, evts *Events, session domain.SessionID, kind domain.EventKind, payload any, ts int64) domain.EventRecord {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := evts.Append(context.Background(), makeEnvelope(session, kind, b, ts))
	if err != nil {
		t.Fatalf("append %s: %v", kind, err)
	}
	return rec
}

func TestQueryFiltersByKindTimeAndProvenance(t *testing.T) {
	_, evts := testEvents(t)
	ctx := context.Background()
	session := testSessionID(t)
	evtA := makeEnvelope(session, domain.EventKindShutdown, inlineJSON(t, domain.ShutdownPayload{}), 1000)
	evtA.Provenance = domain.Provenance{Source: "admin"}
	recA, err := evts.Append(ctx, evtA)
	if err != nil {
		t.Fatal(err)
	}
	_ = recA
	appendTestEvent(t, evts, session, domain.EventKindModelResponse, domain.ModelResponsePayload{Text: "ok"}, 2000)
	appendTestEvent(t, evts, session, domain.EventKindShutdown, domain.ShutdownPayload{}, 3000)

	res, err := evts.Query(ctx, domain.RetrievalQuery{
		Scope:      domain.RetrievalScope{},
		Filter:     domain.RetrievalFilter{Kinds: []domain.EventKind{domain.EventKindShutdown}},
		Pagination: domain.Pagination{Limit: 10, Descending: false},
	}, domain.DefaultScopePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 2 {
		t.Fatalf("kind filter: got %d events", len(res.Events))
	}

	res, err = evts.Query(ctx, domain.RetrievalQuery{
		Filter:     domain.RetrievalFilter{FromTime: 2000, ToTime: 3000},
		Pagination: domain.Pagination{Limit: 10},
	}, domain.DefaultScopePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 || res.Events[0].Envelope.Kind != domain.EventKindModelResponse {
		t.Fatalf("time filter: %+v", res.Events)
	}

	res, err = evts.Query(ctx, domain.RetrievalQuery{
		Filter:     domain.RetrievalFilter{ProvenanceSource: "admin"},
		Pagination: domain.Pagination{Limit: 10},
	}, domain.DefaultScopePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 || res.Events[0].Provenance.Source != "admin" {
		t.Fatalf("provenance filter: %+v", res.Events)
	}
}

func TestQueryPaginationWalksAllPages(t *testing.T) {
	_, evts := testEvents(t)
	ctx := context.Background()
	session := testSessionID(t)
	for i := 0; i < 25; i++ {
		appendTestEvent(t, evts, session, domain.EventKindShutdown, domain.ShutdownPayload{}, int64(1000+i))
	}
	seen := 0
	cursor := ""
	for page := 0; ; page++ {
		res, err := evts.Query(ctx, domain.RetrievalQuery{
			Filter:     domain.RetrievalFilter{MinSequence: 1},
			Pagination: domain.Pagination{Limit: 10, Cursor: cursor, Descending: false},
		}, domain.DefaultScopePolicy())
		if err != nil {
			t.Fatal(err)
		}
		seen += len(res.Events)
		if seen > 25 || page > 5 {
			t.Fatalf("runaway pagination: seen=%d page=%d", seen, page)
		}
		if !res.Truncated {
			if seen != 25 {
				t.Fatalf("final page total = %d, want 25", seen)
			}
			break
		}
		if res.NextCursor == "" {
			t.Fatal("truncated page without cursor")
		}
		cursor = res.NextCursor
	}
	if _, err := evts.Query(ctx, domain.RetrievalQuery{
		Pagination: domain.Pagination{Limit: 10, Cursor: "!!!not-base64!!!"},
	}, domain.DefaultScopePolicy()); !errors.Is(err, domain.ErrInvalidCursor) {
		t.Fatalf("err = %v, want ErrInvalidCursor", err)
	}
}

func TestQueryScopeEnforcementSharingOff(t *testing.T) {
	_, evts := testEvents(t)
	ctx := context.Background()
	session := testSessionID(t)
	appendTestEvent(t, evts, session, domain.EventKindShutdown, domain.ShutdownPayload{}, 1000) // session scope
	appendTestEvent(t, evts, session, domain.EventKindToolRequest, domain.ToolRequestPayload{ToolName: "x", Scope: domain.ScopeShared}, 2000)
	appendTestEvent(t, evts, session, domain.EventKindWorldRead, domain.WorldReadPayload{Scope: domain.ScopeUser}, 2500) // user scope
	user := testUserID(t)
	appendTestEvent(t, evts, session, domain.EventKindSessionStart, domain.SessionStartPayload{UserID: user}, 3000) // session scope, unowned scope column? session start has no Scope field → session column

	restricted := domain.RestrictedScopePolicy()
	res, err := evts.Query(ctx, domain.RetrievalQuery{
		Scope:      domain.RetrievalScope{Scopes: []domain.Scope{domain.ScopeSession}},
		Pagination: domain.Pagination{Limit: 10},
	}, restricted)
	if err != nil {
		t.Fatalf("session-only retrieval must stay allowed: %v", err)
	}
	for _, rec := range res.Events {
		if rec.Envelope.SessionID != session {
			t.Error("unexpected session")
		}
	}
	if _, err := evts.Query(ctx, domain.RetrievalQuery{
		Scope:      domain.RetrievalScope{Scopes: []domain.Scope{domain.ScopeShared}},
		Pagination: domain.Pagination{Limit: 10},
	}, restricted); !errors.Is(err, domain.ErrRetrievalDenied) {
		t.Fatalf("shared scope under sharing-off: err = %v", err)
	}
	if _, err := evts.Query(ctx, domain.RetrievalQuery{
		Scope:      domain.RetrievalScope{Scopes: []domain.Scope{domain.ScopeUser}},
		Pagination: domain.Pagination{Limit: 10},
	}, restricted); !errors.Is(err, domain.ErrRetrievalDenied) {
		t.Fatalf("user scope under sharing-off: err = %v", err)
	}
	if _, err := evts.Query(ctx, domain.RetrievalQuery{
		Scope:      domain.RetrievalScope{IncludeShared: true},
		Pagination: domain.Pagination{Limit: 10},
	}, restricted); !errors.Is(err, domain.ErrRetrievalDenied) {
		t.Fatalf("include-shared under sharing-off: err = %v", err)
	}
	if _, err := evts.Query(ctx, domain.RetrievalQuery{
		Scope:      domain.RetrievalScope{UserIDs: []domain.UserID{user}},
		Pagination: domain.Pagination{Limit: 10},
	}, restricted); !errors.Is(err, domain.ErrRetrievalDenied) {
		t.Fatalf("user filter under sharing-off: err = %v", err)
	}
	// Even without an explicit forbidden request, sharing-off results must not
	// include shared-scope rows.
	res, err = evts.Query(ctx, domain.RetrievalQuery{Pagination: domain.Pagination{Limit: 10}}, restricted)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range res.Events {
		if rec.Envelope.Kind == domain.EventKindToolRequest {
			t.Fatalf("shared-scope event leaked under restricted policy: %+v", rec.Envelope.Kind)
		}
		if rec.Envelope.Kind == domain.EventKindWorldRead {
			t.Fatalf("user-scope event leaked under restricted policy: %+v", rec.Envelope.Kind)
		}
	}
	// Default policy permits shared reads.
	res, err = evts.Query(ctx, domain.RetrievalQuery{
		Scope:      domain.RetrievalScope{Scopes: []domain.Scope{domain.ScopeShared}},
		Pagination: domain.Pagination{Limit: 10},
	}, domain.DefaultScopePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 {
		t.Fatalf("default policy shared query: got %d events", len(res.Events))
	}
}

func TestSurroundingReturnsProvenanceCarryingWindow(t *testing.T) {
	_, evts := testEvents(t)
	ctx := context.Background()
	session := testSessionID(t)
	var recs []domain.EventRecord
	for i := 0; i < 5; i++ {
		recs = append(recs, appendTestEvent(t, evts, session, domain.EventKindTerminalWrite, domain.ShutdownPayload{}, int64(1000+i)))
	}
	window, err := evts.Surrounding(ctx, domain.EventReference{EventID: recs[2].Envelope.EventID}, 2, 1, domain.DefaultScopePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(window) != 3 {
		t.Fatalf("window size = %d, want 3", len(window))
	}
	for i, wantSeq := range []uint64{1, 2, 4} {
		if window[i].Envelope.Sequence != wantSeq {
			t.Fatalf("window order off at %d: got seq %d, want %d", i, window[i].Envelope.Sequence, wantSeq)
		}
		if window[i].Provenance.Source == "" {
			t.Error("provenance missing on record")
		}
	}
	if _, err := evts.Surrounding(ctx, domain.EventReference{EventID: recs[2].Envelope.EventID}, 2, 1, domain.DefaultScopePolicy()); err != nil {
		t.Fatal(err)
	}
	// Sharing-off denies surrounding access to shared-scope events.
	sharedRec := appendTestEvent(t, evts, session, domain.EventKindToolRequest, domain.ToolRequestPayload{Scope: domain.ScopeShared}, 5000)
	if _, err := evts.Surrounding(ctx, domain.EventReference{EventID: sharedRec.Envelope.EventID}, 0, 0, domain.RestrictedScopePolicy()); !errors.Is(err, domain.ErrRetrievalDenied) {
		t.Fatalf("shared surrounding under restricted policy: err = %v", err)
	}
}

func TestFTS5TranscriptProjection(t *testing.T) {
	_, evts := testEvents(t)
	ctx := context.Background()
	session := testSessionID(t)
	appendTestEvent(t, evts, session, domain.EventKindModelResponse, domain.ModelResponsePayload{Text: "acknowledged the request"}, 1000)
	appendTestEvent(t, evts, session, domain.EventKindTerminalPrompt, domain.TerminalPromptPayload{Prompt: "root@vibeos", CWD: "/root"}, 2000)

	hits, err := evts.SearchTranscript(ctx, "acknowledged")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Envelope.Kind != domain.EventKindModelResponse {
		t.Fatalf("FTS hits: %+v", hits)
	}
	hits, err = evts.SearchTranscript(ctx, "vibeos")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Envelope.Kind != domain.EventKindTerminalPrompt {
		t.Fatalf("FTS hits for prompt: %+v", hits)
	}
}

func TestCommandAndPathIndexesAreBoundaryAware(t *testing.T) {
	_, evts := testEvents(t)
	ctx := context.Background()
	session := testSessionID(t)
	appendTestEvent(t, evts, session, domain.EventKindInputAccepted, domain.InputAcceptedPayload{Action: "command", Command: "ls  -la /some"}, 1000)
	appendTestEvent(t, evts, session, domain.EventKindWorldRead, domain.WorldReadPayload{Paths: []domain.ValidPath{"/some"}}, 2000)
	appendTestEvent(t, evts, session, domain.EventKindWorldMaterialize, domain.WorldMaterializePayload{Path: "/some/place"}, 3000)
	appendTestEvent(t, evts, session, domain.EventKindWorldMaterialize, domain.WorldMaterializePayload{Path: "/somewhere"}, 4000)

	byCommand, err := evts.SearchCommands(ctx, "ls -la /some")
	if err != nil {
		t.Fatal(err)
	}
	if len(byCommand) != 1 {
		t.Fatalf("normalized command index: got %d", len(byCommand))
	}
	byPath, err := evts.SearchPaths(ctx, "/some")
	if err != nil {
		t.Fatal(err)
	}
	if len(byPath) != 2 {
		t.Fatalf("boundary-aware path index must match /some and /some/place only, got %d", len(byPath))
	}
	for _, hit := range byPath {
		if strings.Contains(string(hit.Payload), "/somewhere") {
			t.Fatalf("/some leaked to /somewhere sibling: %s", hit.Payload)
		}
	}
	exact, err := evts.SearchPaths(ctx, "/somewhere")
	if err != nil {
		t.Fatal(err)
	}
	if len(exact) != 1 {
		t.Fatalf("exact subtree: got %d", len(exact))
	}

	// Exact lookup is boundary-aware equality: "/some" must not match
	// "/some/place" (a descendant) nor "/somewhere" (a sibling).
	exactSome, err := evts.SearchPathExact(ctx, "/some")
	if err != nil {
		t.Fatal(err)
	}
	if len(exactSome) != 1 {
		t.Fatalf("exact /some: got %d events, want 1", len(exactSome))
	}
	for _, hit := range exactSome {
		payload := string(hit.Payload)
		if strings.Contains(payload, "/some/place") || strings.Contains(payload, "/somewhere") {
			t.Fatalf("exact /some leaked to a descendant or sibling: %s", payload)
		}
	}
	exactPlace, err := evts.SearchPathExact(ctx, "/some/place")
	if err != nil {
		t.Fatal(err)
	}
	if len(exactPlace) != 1 || !strings.Contains(string(exactPlace[0].Payload), "/some/place") {
		t.Fatalf("exact /some/place: %+v", exactPlace)
	}
}
