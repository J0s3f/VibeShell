package export

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

func defaultRedaction() domain.RedactionPolicy {
	return domain.DefaultRedactionPolicy()
}

func sessionQuery(format, path string, sid domain.SessionID) domain.ExportQuery {
	return domain.ExportQuery{
		Scope:      domain.RetrievalScope{Scopes: []domain.Scope{domain.ScopeSession}, SessionIDs: []domain.SessionID{sid}},
		Format:     format,
		OutputPath: path,
		Redact:     defaultRedaction(),
	}
}

func TestExportAsciicastReplaysAcceptedTerminalSequence(t *testing.T) {
	f := canonicalFixture(t, false, "")
	path := filepath.Join(t.TempDir(), "session.cast")
	res, err := f.service(t, Options{ExporterVersion: "test"}).Export(context.Background(), sessionQuery(FormatAsciicast, path, f.session))
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if res.Format != FormatAsciicast || res.EventCount != int64(len(f.records)) {
		t.Fatalf("result = %+v", res)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	header, events, err := Replay(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if header.Version != AsciicastFormatVersion || header.Width != 80 || header.Height != 24 {
		t.Fatalf("bad header: %+v", header)
	}
	if header.Env["TERM"] != "xterm-256color" {
		t.Fatalf("term = %q", header.Env["TERM"])
	}
	if want := float64(f.records[0].Envelope.Timestamp / 1000); header.Timestamp != want {
		t.Fatalf("header timestamp = %v, want %v", header.Timestamp, want)
	}

	prev := -1.0
	for _, e := range events {
		if e.At < prev {
			t.Fatalf("event times not monotonic: %v after %v", e.At, prev)
		}
		prev = e.At
	}

	want := bytes.Join(f.frames, nil)
	if got := AcceptedOutput(events); !bytes.Equal(got, want) {
		t.Fatalf("accepted output mismatch:\n got %q\nwant %q", got, want)
	}
	if got := string(InputBytes(events)); got != "ls -la\r" {
		t.Fatalf("input bytes = %q", got)
	}
	if rs := ResizeSchedule(events); len(rs) != 1 || !strings.HasSuffix(rs[0], ":120x40") {
		t.Fatalf("resize schedule = %v", rs)
	}
	if cols, rows, ok := parseResize("120x40"); !ok || cols != 120 || rows != 40 {
		t.Fatalf("parseResize failed: %d %d %v", cols, rows, ok)
	}
}

func TestBundleManifestVerifiesAndTamperedBlobFails(t *testing.T) {
	f := canonicalFixture(t, false, "")
	dir := filepath.Join(t.TempDir(), "bundle")
	if _, err := f.service(t, Options{ExporterVersion: "test"}).Export(context.Background(), sessionQuery(FormatJSONL, dir, f.session)); err != nil {
		t.Fatalf("Export: %v", err)
	}

	problems, err := VerifyBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("manifest problems: %v", problems)
	}

	rawManifest, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.FormatVersion != BundleFormatVersion || manifest.EventSchemaVersion != domain.EventSchemaVersion {
		t.Fatalf("manifest versions: %+v", manifest)
	}
	if manifest.EventCount != len(f.records) || manifest.ContentBlobsVerified != true || !manifest.SecretsExcluded {
		t.Fatalf("manifest counts/flags: %+v", manifest)
	}
	if manifest.ConfigSnapshot != "cfg_1" || manifest.PromptSnapshot != "prm_1" || manifest.CatalogSnapshot != "cat_1" {
		t.Fatalf("snapshots: %+v", manifest)
	}
	if len(manifest.Sessions) != 1 || manifest.Sessions[0].Status != statusIncompleteDisconnect {
		t.Fatalf("session status: %+v", manifest.Sessions)
	}

	entries, err := os.ReadDir(filepath.Join(dir, contentsDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 content blobs, got %d", len(entries))
	}
	target := filepath.Join(dir, contentsDir, entries[0].Name())
	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, append([]byte("tampered"), original...), 0o644); err != nil {
		t.Fatal(err)
	}
	problems, err = VerifyBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) == 0 {
		t.Fatal("expected a checksum mismatch for the tampered blob")
	}
}

func TestBundleEventLinesAreOrderedCompleteAndChecksummed(t *testing.T) {
	f := canonicalFixture(t, true, "")
	dir := filepath.Join(t.TempDir(), "bundle")
	res, err := f.service(t, Options{BatchSize: 2}).Export(context.Background(), sessionQuery(FormatJSONL, dir, f.session))
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	if res.Checksum != hex.EncodeToString(sum[:]) || res.SizeBytes != int64(len(content)) {
		t.Fatalf("result checksum/size do not match events.jsonl")
	}

	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	seen := map[uint64]int{}
	lines := 0
	for sc.Scan() {
		var row struct {
			Envelope domain.EventEnvelope `json:"envelope"`
			Payload  json.RawMessage      `json:"payload"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatalf("bad line: %v", err)
		}
		if len(row.Payload) == 0 {
			t.Fatal("empty payload")
		}
		seen[row.Envelope.Sequence]++
		lines++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if lines != len(f.records) {
		t.Fatalf("bundle has %d lines, want %d", lines, len(f.records))
	}
	for seq := uint64(1); seq <= uint64(len(f.records)); seq++ {
		if seen[seq] != 1 {
			t.Fatalf("sequence %d appeared %d times", seq, seen[seq])
		}
	}
}

func TestTranscriptLabelsDistinguishKinds(t *testing.T) {
	f := canonicalFixture(t, false, "")
	path := filepath.Join(t.TempDir(), "transcript.txt")
	if _, err := f.service(t, Options{}).Export(context.Background(), sessionQuery(FormatTranscript, path, f.session)); err != nil {
		t.Fatalf("Export: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{
		"LOSSY PROJECTION",
		"[USER INPUT] command: ls -la",
		"[TERMINAL OUTPUT] frame=f_0001",
		"[SCREEN SNAPSHOT: alt screen]",
		"[SCREEN TRANSITION] mode line -> app",
		"[FAILURE] model.error",
		"[INTERRUPTION] user cancelled the running operation",
		"[PROMPT]",
		"[STATUS] session " + f.session.String(),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("transcript missing %q\n---\n%s", want, text)
		}
	}
	if strings.Contains(text, "end: reason=") {
		t.Error("transcript fabricated a session end for an incomplete session")
	}
	if !strings.Contains(text, "NO recorded end") {
		t.Error("transcript lacks the explicit incomplete status")
	}
}

func TestCompleteSessionTranscriptReportsRecordedEnd(t *testing.T) {
	f := canonicalFixture(t, true, "")
	path := filepath.Join(t.TempDir(), "transcript.txt")
	if _, err := f.service(t, Options{}).Export(context.Background(), sessionQuery(FormatTranscript, path, f.session)); err != nil {
		t.Fatalf("Export: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "end: reason=exit") {
		t.Fatalf("complete session end missing:\n%s", text)
	}
	if strings.Contains(text, "NO recorded end") {
		t.Fatal("complete session wrongly marked incomplete")
	}
}

func TestExportRedactsSecretsFromEveryProjection(t *testing.T) {
	const secret = "sk-abcdefghijklmnopqrstuvwxyz012345"
	f := canonicalFixture(t, true, secret)
	dir := t.TempDir()
	svc := f.service(t, Options{})

	bundleDir := filepath.Join(dir, "bundle")
	transcriptPath := filepath.Join(dir, "transcript.txt")
	castPath := filepath.Join(dir, "session.cast")
	ctx := context.Background()
	if _, err := svc.Export(ctx, sessionQuery(FormatJSONL, bundleDir, f.session)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Export(ctx, sessionQuery(FormatTranscript, transcriptPath, f.session)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Export(ctx, sessionQuery(FormatAsciicast, castPath, f.session)); err != nil {
		t.Fatal(err)
	}

	foundMarker := false
	leaked := false
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte(secret)) {
			leaked = true
		}
		if bytes.Contains(b, []byte("[REDACTED]")) {
			foundMarker = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk export: %v", err)
	}
	if leaked {
		t.Fatal("secret leaked into an export file")
	}
	if !foundMarker {
		t.Fatal("expected a redaction marker somewhere in the export")
	}
}

func TestExportUserScopeStreamsThroughRetrievalInPages(t *testing.T) {
	f := canonicalFixture(t, true, "")
	dir := filepath.Join(t.TempDir(), "bundle")
	res, err := f.service(t, Options{BatchSize: 3}).Export(context.Background(), domain.ExportQuery{
		Scope:      domain.RetrievalScope{Scopes: []domain.Scope{domain.ScopeUser}, UserIDs: []domain.UserID{f.user}},
		Format:     FormatJSONL,
		OutputPath: dir,
		Redact:     defaultRedaction(),
	})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if res.EventCount != int64(len(f.records)) {
		t.Fatalf("event count = %d, want %d", res.EventCount, len(f.records))
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Scope != "user" || len(manifest.Sessions) != 1 || manifest.Sessions[0].Status != statusComplete {
		t.Fatalf("manifest = %+v", manifest)
	}
}

func TestExportTimeRangeFiltersEvents(t *testing.T) {
	f := canonicalFixture(t, true, "")
	dir := filepath.Join(t.TempDir(), "bundle")
	from := f.records[3].Envelope.Timestamp
	res, err := f.service(t, Options{}).Export(context.Background(), domain.ExportQuery{
		Scope:      domain.RetrievalScope{Scopes: []domain.Scope{domain.ScopeSession}, SessionIDs: []domain.SessionID{f.session}},
		Filter:     domain.RetrievalFilter{FromTime: from},
		Format:     FormatJSONL,
		OutputPath: dir,
		Redact:     defaultRedaction(),
	})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	wantCount := int64(0)
	for _, r := range f.records {
		if r.Envelope.Timestamp >= from {
			wantCount++
		}
	}
	if res.EventCount != wantCount {
		t.Fatalf("event count = %d, want %d", res.EventCount, wantCount)
	}
}

func TestExportValidationRejectsBadQueries(t *testing.T) {
	f := canonicalFixture(t, true, "")
	svc := f.service(t, Options{})
	ctx := context.Background()

	if _, err := svc.Export(ctx, domain.ExportQuery{Format: FormatJSONL}); !domain.IsValidationError(err) {
		t.Errorf("empty output path: err = %v, want validation", err)
	}
	if _, err := svc.Export(ctx, domain.ExportQuery{Format: "pdf", OutputPath: t.TempDir()}); !domain.IsValidationError(err) {
		t.Errorf("bad format: err = %v, want validation", err)
	}
	noSession := sessionQuery(FormatAsciicast, filepath.Join(t.TempDir(), "a.cast"), f.session)
	noSession.Scope.SessionIDs = nil
	if _, err := svc.Export(ctx, noSession); !domain.IsValidationError(err) {
		t.Errorf("zero-session asciicast: err = %v, want validation", err)
	}
	two := sessionQuery(FormatAsciicast, filepath.Join(t.TempDir(), "b.cast"), f.session)
	two.Scope.SessionIDs = append(two.Scope.SessionIDs, f.session)
	if _, err := svc.Export(ctx, two); !domain.IsValidationError(err) {
		t.Errorf("multi-session asciicast: err = %v, want validation", err)
	}
}

func TestNewServiceRejectsMissingPortsAndBadBatch(t *testing.T) {
	f := canonicalFixture(t, true, "")
	if _, err := NewService(nil, f.retrieval, f.content, f.clock, f.random, Options{}); !domain.IsValidationError(err) {
		t.Errorf("nil events: err = %v", err)
	}
	if _, err := NewService(f.events, f.retrieval, f.content, f.clock, f.random, Options{BatchSize: MaxBatchSize + 1}); !domain.IsValidationError(err) {
		t.Errorf("bad batch: err = %v", err)
	}
}
