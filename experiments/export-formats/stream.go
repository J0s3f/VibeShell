// Package exportformats is a bounded experiment qualifying the three
// research export formats from PLAN 10.5: a versioned JSONL research
// bundle, a readable UTF-8 transcript, and an asciicast v2 terminal
// recording. All three projections are derived from one canonical
// synthetic event stream built on the committed domain event envelope.
package exportformats

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"j0s.at/vibeshell/internal/domain"
)

// Record is one canonical event plus the bytes its payload/content
// reference points to. It is the single source for all three projections.
type Record struct {
	Env         domain.EventEnvelope
	PayloadJSON []byte            // canonical JSON of the typed payload
	Blobs       map[string][]byte // content_id -> exact bytes (frames, results)
}

// Stream is an ordered sequence of records sharing one session.
type Stream struct {
	SessionID string
	StartedAt int64 // UTC unix milliseconds of the session.start event
	Complete  bool  // false when the recording ends without session.end
	Records   []Record
}

// ContentIDFor returns a deterministic synthetic content ID for payload
// bytes, mirroring how the store keys immutable blobs by content.
func ContentIDFor(b []byte) domain.ContentID {
	sum := sha256.Sum256(b)
	id, err := domain.ParseContentID("cnt_" + strings.ToUpper(hex.EncodeToString(sum[:]))[:26])
	if err != nil {
		panic(err)
	}
	return id
}

// inlinePayload JSON-encodes a typed payload into the envelope's inline
// payload reference and also exposes the same bytes via blobs when the
// payload is large.
func inlinePayload(v domain.EventPayload) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// newRecord builds an envelope with an inline payload and placeholder
// IDs that are valid per the domain identity grammar.
func newRecord(session domain.SessionID, seq uint64, kind domain.EventKind, payload domain.EventPayload, tsMillis, monoNanos int64, turn *domain.TurnID) Record {
	env := domain.NewEventEnvelope(session, seq, kind, domain.Provenance{
		Source:        "synthetic-spike",
		Actor:         "export-formats-spike",
		ConfigVersion: "cfg_2026-10-03",
		PromptVersion: "prmt_2026-10-03",
	}, tsMillis, monoNanos)
	evt, err := domain.ParseEventID(fmt.Sprintf("evt_%026X", seq))
	if err != nil {
		panic(err)
	}
	env.EventID = evt
	env.TurnID = turn
	env.Payload = domain.PayloadRef{Inline: inlinePayload(payload)}
	return Record{Env: env, PayloadJSON: env.Payload.Inline, Blobs: map[string][]byte{}}
}

// CanonicalStream builds the single accepted synthetic session used by
// all projection tests: session, command input, model/tool work, a
// failure, an interruption, a resize, alternate-screen interaction, and
// a disconnected (incomplete) ending. No successful ending is recorded.
func CanonicalStream() Stream {
	session, _ := domain.ParseSessionID("ses_00000000000000000000000001")
	user, _ := domain.ParseUserID("usr_00000000000000000000000001")
	turn, _ := domain.ParseTurnID("trn_00000000000000000000000001")
	route, _ := domain.ParseRouteID("rte_" + strings.Repeat("0", 25) + "A")
	account, _ := domain.ParseAccountID("acc_" + strings.Repeat("0", 25) + "B")

	t0 := int64(1788422400000) // 2026-10-03T00:00:00Z-ish placeholder; exact value irrelevant
	mono := int64(0)
	step := func(ms int64, nsec int64) (int64, int64) {
		t0 += ms
		mono += nsec
		return t0, mono
	}

	var recs []Record
	add := func(kind domain.EventKind, payload domain.EventPayload, ms, nsec int64, t *domain.TurnID) Record {
		tsms, mns := step(ms, nsec)
		r := newRecord(session, uint64(len(recs)+1), kind, payload, tsms, mns, t)
		recs = append(recs, r)
		return r
	}

	add(domain.EventKindSessionStart, domain.SessionStartPayload{
		UserID: user, AuthMode: "secure", TerminalType: "xterm-256color",
		TerminalSize: domain.TermSize{Cols: 80, Rows: 24}, ClientAddr: "10.0.0.7", SharingEnabled: false,
	}, 0, 0, nil)

	add(domain.EventKindInputRaw, domain.InputRawPayload{Bytes: []byte("ls -la\r"), ByteCount: 6, IsPaste: false}, 120, 120e6, &turn)
	add(domain.EventKindInputDecoded, domain.InputDecodedPayload{Events: []domain.TerminalEvent{{Type: "key", Chars: "ls -la\r"}}}, 5, 5e6, &turn)
	add(domain.EventKindInputAccepted, domain.InputAcceptedPayload{Action: "command", Command: "ls -la"}, 2, 2e6, &turn)

	frame := []byte("drwxr-xr-x 2 user user 4096 Oct  3 00:00 .\r\n-rw-r--r-- 1 user user   42 Oct  3 00:00 README.md\r\n")
	blobID := ContentIDFor(frame)
	fr := add(domain.EventKindTerminalFrame, domain.TerminalFramePayload{
		FrameID: "f_0001", ContentRef: domain.ContentRef{Hash: blobID, Size: int64(len(frame)), MediaType: "application/octet-stream"},
	}, 80, 80e6, &turn)
	fr.Blobs[blobID.String()] = frame
	recs[len(recs)-1] = fr

	add(domain.EventKindTerminalPrompt, domain.TerminalPromptPayload{Prompt: "user@vibeos:~$ ", CWD: "/home/user", ExitCode: 0}, 40, 40e6, &turn)

	add(domain.EventKindTerminalMode, TerminalModePayload{From: "line", To: "app"}, 10, 10e6, &turn)
	alt := []byte("\x1b[H\x1b[2J[top] rows=24 cols=80 (alt screen)\r\n")
	altID := ContentIDFor(alt)
	fr2 := add(domain.EventKindTerminalFrame, domain.TerminalFramePayload{
		FrameID: "f_0002", Mode: "alt",
		ContentRef: domain.ContentRef{Hash: altID, Size: int64(len(alt)), MediaType: "application/octet-stream"},
	}, 60, 60e6, &turn)
	fr2.Blobs[altID.String()] = alt
	recs[len(recs)-1] = fr2
	add(domain.EventKindTerminalMode, TerminalModePayload{From: "app", To: "line"}, 200, 200e6, &turn)

	add(domain.EventKindModelRequest, domain.ModelRequestPayload{
		RouteID: route, AccountID: account, Messages: json.RawMessage(`[{"role":"user","content":"list files"}]`),
		Tools: json.RawMessage(`[]`), MaxTokens: 512, DeadlineMs: 30000, ContextBytes: 1234,
	}, 15, 15e6, &turn)
	add(domain.EventKindModelError, domain.ModelErrorPayload{
		RouteID: route, AccountID: account, ErrorClass: domain.FailureNetworkTimeout,
		ErrorMessage: "upstream timeout after 30s", Retryable: true, StatusCode: 504,
	}, 30040, 30040e6, &turn)
	add(domain.EventKindInputAccepted, domain.InputAcceptedPayload{Action: "cancel"}, 20, 20e6, &turn)
	add(domain.EventKindInputAccepted, domain.InputAcceptedPayload{Action: "resize", Resize: &domain.TermSize{Cols: 120, Rows: 40}}, 30, 30e6, &turn)
	add(domain.EventKindToolRequest, domain.ToolRequestPayload{ToolName: "world.read", Arguments: json.RawMessage(`{"path":"/home/user"}`), Scope: domain.ScopeUser, TimeoutMs: 1000}, 10, 10e6, &turn)
	add(domain.EventKindWorldCommit, WorldCommitRec(), 12, 12e6, &turn)
	app, _ := domain.ParseAppID("app_" + strings.Repeat("0", 25) + "C")
	av, _ := domain.ParseAppVersionID("av_" + strings.Repeat("0", 25) + "D")
	add(domain.EventKindAppActivated, domain.AppActivatedPayload{AppID: app, VersionID: av}, 8, 8e6, &turn)
	evtid, _ := domain.ParseEventID("evt_" + strings.Repeat("0", 25) + "E")
	add(domain.EventKindContextSummary, domain.ContextSummaryPayload{SummaryID: "sum_1", SourceEvents: []domain.EventID{evtid}, Scope: domain.ScopeUser, TokenCount: 128}, 6, 6e6, &turn)
	// No session.end: the client disconnected. Incomplete by construction.
	return Stream{SessionID: session.String(), StartedAt: 1788422400000 - 0, Complete: false, Records: recs}
}

// TerminalModePayload describes a screen-mode transition. The committed
// domain defines the event kind but no payload struct yet, so the
// experiment uses this local payload (still recorded verbatim as JSON).
type TerminalModePayload struct {
	From string `json:"from"` // "line", "app", "alt"
	To   string `json:"to"`
}

func (TerminalModePayload) EventKind() domain.EventKind { return domain.EventKindTerminalMode }

// WorldCommitRec builds a minimal committed-change payload.
func WorldCommitRec() domain.WorldCommitPayload {
	return domain.WorldCommitPayload{
		ChangeSet:     domain.ChangeSet{Mutations: nil, Timestamp: 1788422400123},
		CommittedRev:  2,
		ContentHashes: []domain.ContentID{ContentIDFor([]byte("# hello"))},
	}
}
