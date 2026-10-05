package exportformats

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"j0s.at/vibeshell/internal/domain"
)

// exportAll runs all three projections over the stream into dir.
func exportAll(t *testing.T, dir string, s Stream) (transcriptPath, castPath string) {
	t.Helper()
	b, err := NewBundle(filepath.Join(dir, "bundle"))
	if err != nil {
		t.Fatal(err)
	}
	tr, err := NewTranscript(filepath.Join(dir, "transcript.txt"))
	if err != nil {
		t.Fatal(err)
	}
	ac, err := NewAsciicast(filepath.Join(dir, "session.cast"), 80, 24, s.StartedAt/1000, "xterm-256color")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range s.Records {
		if err := b.Write(r); err != nil {
			t.Fatal(err)
		}
		if err := tr.Write(r); err != nil {
			t.Fatal(err)
		}
		if err := ac.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.Close(s); err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(s); err != nil {
		t.Fatal(err)
	}
	if err := ac.Close(); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "transcript.txt"), filepath.Join(dir, "session.cast")
}

func TestProjectionsFromCanonicalStream(t *testing.T) {
	s := CanonicalStream()
	dir := t.TempDir()
	transcriptPath, castPath := exportAll(t, dir, s)

	// 1. JSONL bundle: every manifest entry verifies.
	problems, err := VerifyManifest(filepath.Join(dir, "bundle"))
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("manifest problems: %v", problems)
	}
	mraw, _ := os.ReadFile(filepath.Join(dir, "bundle", "manifest.json"))
	var m Manifest
	json.Unmarshal(mraw, &m)
	if m.SessionStatus != "incomplete_disconnected" {
		t.Fatalf("session status = %q", m.SessionStatus)
	}
	if m.EventCount != len(s.Records) {
		t.Fatalf("event count %d != %d", m.EventCount, len(s.Records))
	}
	if m.FormatVersion != 1 || m.EventSchemaVersion != domain.EventSchemaVersion || !m.SecretsExcluded {
		t.Fatalf("manifest header wrong: %+v", m)
	}

	// 2. Transcript: labels distinguish input/output/interruption/failure;
	//    states lossiness and never fabricates a successful ending.
	tr, _ := os.ReadFile(transcriptPath)
	trs := string(tr)
	for _, want := range []string{"LOSSY PROJECTION", "[USER INPUT] command: ls -la", "[TERMINAL OUTPUT] frame=f_0001", "[INTERRUPTION]", "[FAILURE] model.error", "[SCREEN SNAPSHOT: alt screen]", "[SCREEN TRANSITION] mode line -> app", "[PROMPT]", "[STATUS] session ses_"} {
		if !strings.Contains(trs, want) {
			t.Errorf("transcript missing %q", want)
		}
	}
	if strings.Contains(trs, "end: reason=") {
		t.Error("transcript fabricated a session end")
	}
	if !strings.Contains(trs, "NO recorded end") {
		t.Error("transcript lacks explicit incomplete status")
	}

	// 3. Asciicast: header conforms; replay equals the accepted output.
	castRaw, _ := os.ReadFile(castPath)
	h, events, err := ReplayAsciicast(bytes.NewReader(castRaw))
	if err != nil {
		t.Fatal(err)
	}
	if h.Version != 2 || h.Width != 80 || h.Height != 24 || h.Env["TERM"] != "xterm-256color" {
		t.Fatalf("bad header %+v", h)
	}
	var gotOut bytes.Buffer
	var prevAt float64 = -1
	inputBytes := bytes.Buffer{}
	resizes := []string{}
	for _, e := range events {
		if e.At < prevAt {
			t.Fatal("event times not monotonic")
		}
		prevAt = e.At
		switch e.Code {
		case "o":
			gotOut.Write(e.Data)
		case "i":
			inputBytes.Write(e.Data)
		case "r":
			resizes = append(resizes, string(e.Data))
		default:
			t.Fatalf("unexpected code %q", e.Code)
		}
	}
	var wantOut bytes.Buffer
	for _, r := range s.Records {
		if r.Env.Kind != domain.EventKindTerminalFrame {
			continue
		}
		var p domain.TerminalFramePayload
		json.Unmarshal(r.PayloadJSON, &p)
		wantOut.Write(r.Blobs[p.ContentRef.Hash.String()])
	}
	if !bytes.Equal(gotOut.Bytes(), wantOut.Bytes()) {
		t.Fatalf("replay output mismatch: got %d bytes, want %d", gotOut.Len(), wantOut.Len())
	}
	if inputBytes.String() != "ls -la\r" {
		t.Fatalf("input events mismatch %q", inputBytes.String())
	}
	if len(resizes) != 1 || resizes[0] != "120x40" {
		t.Fatalf("resize events %v", resizes)
	}
}

func TestTamperedBlobFailsChecksum(t *testing.T) {
	s := CanonicalStream()
	dir := t.TempDir()
	exportAll(t, dir, s)
	var blobName string
	entries, _ := os.ReadDir(filepath.Join(dir, "bundle", "contents"))
	for _, e := range entries {
		blobName = e.Name()
	}
	p := filepath.Join(dir, "bundle", "contents", blobName)
	orig, _ := os.ReadFile(p)
	os.WriteFile(p, append([]byte("x"), orig...), 0o644)
	problems, err := VerifyManifest(filepath.Join(dir, "bundle"))
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) == 0 {
		t.Fatal("expected checksum failure")
	}
}

func TestJSONLEventLinesRoundTrip(t *testing.T) {
	s := CanonicalStream()
	dir := t.TempDir()
	exportAll(t, dir, s)
	f, _ := os.Open(filepath.Join(dir, "bundle", "events.jsonl"))
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 1024*1024)
	sc.Buffer(buf, 16*1024*1024)
	for sc.Scan() {
		var row struct {
			Envelope domain.EventEnvelope `json:"envelope"`
			Payload  json.RawMessage      `json:"payload"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		n++
		if row.Envelope.Sequence != uint64(n) {
			t.Fatalf("sequence gap at %d", n)
		}
		if len(row.Payload) == 0 {
			t.Fatal("empty payload")
		}
	}
	if n != len(s.Records) {
		t.Fatalf("decoded %d lines, want %d", n, len(s.Records))
	}
}
