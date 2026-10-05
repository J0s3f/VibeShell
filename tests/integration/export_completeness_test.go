package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/export"
)

// exportFixture seeds one accepted session with exact terminal output, a
// model failure, an incomplete (disconnected) end, a resize, and an injected
// secret, so every research projection has something meaningful to cover.
type exportFixture struct {
	store   *testStore
	session domain.SessionID
	user    domain.UserID
	secret  string
	frames  [][]byte
}

func newExportFixture(t *testing.T, secret string) *exportFixture {
	t.Helper()
	store := openStore(t)
	user := userID(t, 1)
	sess := sessionID(t, 1)
	routeID := route(t, 1)
	acct := account(t, 1)

	// A short token that the redactor's sk-* pattern detects.
	if secret == "" {
		secret = "sk-abcdefghijklmnopqrstuvwxyz0123456789"
	}
	frame1 := []byte("drwxr-xr-x 2 user user 4096 .\r\n-rw-r--r-- 1 user user  42 README.md\r\n")
	frame2 := []byte("\x1b[H\x1b[2J[top] rows=24 cols=80\r\n")
	// A secret echoed into terminal output and a model response body.
	frame1 = append(frame1, []byte("api_key="+secret+"\r\n")...)

	f1 := store.putContent(t, frame1, "application/octet-stream")
	f2 := store.putContent(t, frame2, "application/octet-stream")

	ts := int64(1_700_000_000_000)
	step := func() int64 { ts += 100; return ts }

	store.appendEvent(t, sess, domain.EventKindSessionStart, domain.SessionStartPayload{
		UserID: user, AuthMode: "secure", TerminalType: "xterm-256color",
		TerminalSize: domain.TermSize{Cols: 80, Rows: 24}, SharingEnabled: true,
	}, step())
	store.appendEvent(t, sess, domain.EventKindInputAccepted, domain.InputAcceptedPayload{
		Action: "command", Command: "ls -la",
	}, step())
	store.appendEvent(t, sess, domain.EventKindTerminalFrame, domain.TerminalFramePayload{
		FrameID: "f_0001", ContentRef: f1,
	}, step())
	store.appendEvent(t, sess, domain.EventKindTerminalFrame, domain.TerminalFramePayload{
		FrameID: "f_0002", Mode: "alt", ContentRef: f2,
	}, step())
	store.appendEvent(t, sess, domain.EventKindModelRequest, domain.ModelRequestPayload{
		RouteID: routeID, AccountID: acct, Messages: json.RawMessage(`[]`), Tools: json.RawMessage(`[]`),
		MaxTokens: 512, DeadlineMs: 30_000, ContextBytes: 10,
	}, step())
	store.appendEvent(t, sess, domain.EventKindModelResponse, domain.ModelResponsePayload{
		RouteID: routeID, AccountID: acct, FinishReason: "stop",
		Text: "Authorization: Bearer " + secret,
	}, step())
	store.appendEvent(t, sess, domain.EventKindModelError, domain.ModelErrorPayload{
		RouteID: routeID, AccountID: acct, ErrorClass: domain.FailureNetworkTimeout,
		ErrorMessage: "upstream timeout", Retryable: true, StatusCode: 504,
	}, step())
	store.appendEvent(t, sess, domain.EventKindInputAccepted, domain.InputAcceptedPayload{
		Action: "resize", Resize: &domain.TermSize{Cols: 120, Rows: 40},
	}, step())
	// Deliberately no session.end: an incomplete/disconnected session.

	return &exportFixture{
		store: store, session: sess, user: user, secret: secret,
		frames: [][]byte{frame1, frame2},
	}
}

func (f *exportFixture) service(t *testing.T, opts export.Options) *export.Service {
	t.Helper()
	// The real SQLite adapter satisfies EventStore, RetrievalStore, and
	// ContentStore; the Clock and Random are deterministic doubles.
	svc, err := export.NewService(f.store.events, f.store.events, f.store.db,
		newTestClock(1_700_000_100_000), newSeededRandom(1), opts)
	if err != nil {
		t.Fatalf("export.NewService: %v", err)
	}
	return svc
}

func sessionQuery(sess domain.SessionID, format, out string, redact domain.RedactionPolicy) domain.ExportQuery {
	return domain.ExportQuery{
		Scope:      domain.RetrievalScope{SessionIDs: []domain.SessionID{sess}},
		Format:     format,
		OutputPath: out,
		Redact:     redact,
	}
}

// TestExportJSONLBundleChecksums verifies the JSONL research bundle over real
// SQLite events: every emitted file's sha256 in the manifest matches, the
// manifest records the incomplete session truthfully, and the bundle carries
// the exact recorded bytes (PLAN 10.5).
func TestExportJSONLBundleChecksums(t *testing.T) {
	ctx := context.Background()
	f := newExportFixture(t, "")
	svc := f.service(t, export.Options{ExporterVersion: "test"})

	dir := filepath.Join(t.TempDir(), "bundle")
	res, err := svc.Export(ctx, sessionQuery(f.session, export.FormatJSONL, dir, domain.RedactionPolicy{}))
	if err != nil {
		t.Fatalf("Export JSONL: %v", err)
	}
	if res.EventCount == 0 || res.Checksum == "" || res.SizeBytes == 0 {
		t.Fatalf("export result = %+v, want events, checksum, and bytes", res)
	}
	problems, err := export.VerifyBundle(dir)
	if err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("bundle integrity problems: %v", problems)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest export.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if len(manifest.Sessions) != 1 {
		t.Fatalf("manifest sessions = %d, want 1", len(manifest.Sessions))
	}
	if manifest.Sessions[0].Status != "incomplete_disconnected" {
		t.Errorf("session status = %q, want incomplete_disconnected (no fabricated ending)", manifest.Sessions[0].Status)
	}
	// Without a redaction policy the bundle truthfully reports that secrets
	// were not excluded; the redaction test covers the enabled case.
	if manifest.SecretsExcluded {
		t.Errorf("manifest SecretsExcluded = true without a redaction policy")
	}

	// The exact terminal frame bytes are emitted as content blobs.
	blob1, err := os.ReadFile(filepath.Join(dir, "contents", f.contentID(t, 0)))
	if err != nil {
		t.Fatalf("read content blob: %v", err)
	}
	if string(blob1) != string(f.frames[0]) {
		t.Errorf("emitted blob differs from recorded bytes")
	}
}

// TestExportTranscriptLabels verifies the readable transcript distinguishes
// user input, terminal output, failures, interruptions, and the incomplete
// session status without fabricating a successful ending (PLAN 10.5).
func TestExportTranscriptLabels(t *testing.T) {
	ctx := context.Background()
	f := newExportFixture(t, "")
	svc := f.service(t, export.Options{})

	out := filepath.Join(t.TempDir(), "session.transcript.txt")
	if _, err := svc.Export(ctx, sessionQuery(f.session, export.FormatTranscript, out, domain.RedactionPolicy{})); err != nil {
		t.Fatalf("Export transcript: %v", err)
	}
	text, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	s := string(text)
	for _, want := range []string{
		"LOSSY PROJECTION",
		"[USER INPUT] command: ls -la",
		"[TERMINAL OUTPUT]",
		"[SCREEN SNAPSHOT: alt screen]",
		"[FAILURE]",
		"[STATUS] session",
		"NO recorded end",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("transcript missing %q", want)
		}
	}
}

// TestExportAsciicastReplay verifies the asciicast v2 recording replays to the
// accepted terminal byte stream and records the resize, agreeing with the
// committed frame bytes (PLAN 10.5, 16.1 replay/JSONL agreement).
func TestExportAsciicastReplay(t *testing.T) {
	ctx := context.Background()
	f := newExportFixture(t, "")
	svc := f.service(t, export.Options{})

	out := filepath.Join(t.TempDir(), "session.cast")
	if _, err := svc.Export(ctx, sessionQuery(f.session, export.FormatAsciicast, out, domain.RedactionPolicy{})); err != nil {
		t.Fatalf("Export asciicast: %v", err)
	}
	file, err := os.Open(out)
	if err != nil {
		t.Fatalf("open cast: %v", err)
	}
	defer file.Close()
	header, events, err := export.Replay(file)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if header.Version != export.AsciicastFormatVersion {
		t.Errorf("asciicast version = %d, want %d", header.Version, export.AsciicastFormatVersion)
	}
	if header.Width != 80 || header.Height != 24 {
		t.Errorf("header size = %dx%d, want 80x24", header.Width, header.Height)
	}
	// The accepted output must contain both committed frames in order.
	got := string(export.AcceptedOutput(events))
	if !strings.Contains(got, "README.md") || !strings.Contains(got, "[top]") {
		t.Errorf("replayed output missing committed frames: %q", got)
	}
	if resizes := export.ResizeSchedule(events); len(resizes) == 0 {
		t.Errorf("asciicast recorded no resize event")
	}
}

// TestExportRedactsSecrets verifies no configured secret shape leaks into any
// projection when the export redaction policy is enabled (PLAN 10.4 no secret
// leakage), while the bundle's own manifest checksums stay valid post-redaction.
func TestExportRedactsSecrets(t *testing.T) {
	ctx := context.Background()
	f := newExportFixture(t, "")
	svc := f.service(t, export.Options{})
	redact := domain.RedactionPolicy{RedactSecrets: true, ReplacementText: "[REDACTED]"}

	for _, tc := range []struct {
		name, format string
		isDir        bool
	}{
		{"jsonl", export.FormatJSONL, true},
		{"transcript", export.FormatTranscript, false},
		{"asciicast", export.FormatAsciicast, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			out := filepath.Join(base, "out"+tc.name)
			if _, err := svc.Export(ctx, sessionQuery(f.session, tc.format, out, redact)); err != nil {
				t.Fatalf("Export: %v", err)
			}
			for _, file := range filesOf(t, out) {
				raw, err := os.ReadFile(file)
				if err != nil {
					t.Fatalf("read %s: %v", file, err)
				}
				if strings.Contains(string(raw), f.secret) {
					t.Errorf("%s leaked the secret", file)
				}
			}
			if tc.isDir {
				problems, err := export.VerifyBundle(out)
				if err != nil {
					t.Fatalf("VerifyBundle after redaction: %v", err)
				}
				if len(problems) != 0 {
					t.Errorf("redacted bundle integrity problems: %v", problems)
				}
			}
		})
	}
}

// contentID returns the content id of the fixture's n-th frame as stored.
func (f *exportFixture) contentID(t *testing.T, n int) string {
	t.Helper()
	ref, err := f.store.db.Put(context.Background(), f.frames[n], "application/octet-stream")
	if err != nil {
		t.Fatalf("Put frame %d: %v", n, err)
	}
	return ref.Hash.String()
}

// filesOf returns every regular file under path (or path itself for a file).
func filesOf(t *testing.T, path string) []string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if !info.IsDir() {
		return []string{path}
	}
	var out []string
	if err := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out = append(out, p)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", path, err)
	}
	return out
}
