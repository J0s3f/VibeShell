package exportformats

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"j0s.at/vibeshell/internal/domain"
)

// genRecord builds synthetic event n of a large session without
// materializing the whole stream.
func genRecord(n int, session domain.SessionID, turn *domain.TurnID, blobID string, blob []byte) Record {
	_ = blobID
	_ = blob
	ts := int64(1788422400000) + int64(n)*25
	mono := int64(n) * 25_000_000
	switch n % 20 {
	case 0:
		frame := []byte(fmt.Sprintf("line %d of synthetic output\r\n", n))
		id := ContentIDFor(frame)
		p := domain.TerminalFramePayload{FrameID: fmt.Sprintf("f_%06d", n), ContentRef: domain.ContentRef{Hash: id, Size: int64(len(frame)), MediaType: "application/octet-stream"}}
		r := newRecord(session, uint64(n+1), domain.EventKindTerminalFrame, p, ts, mono, turn)
		r.Blobs = map[string][]byte{id.String(): frame}
		return r
	case 1:
		return newRecord(session, uint64(n+1), domain.EventKindInputAccepted, domain.InputAcceptedPayload{Action: "command", Command: fmt.Sprintf("cmd-%d", n)}, ts, mono, turn)
	case 2:
		return newRecord(session, uint64(n+1), domain.EventKindModelRequest, domain.ModelRequestPayload{Messages: jsonRaw(n), Tools: jsonRawTools(), MaxTokens: 256, DeadlineMs: 30000}, ts, mono, turn)
	default:
		return newRecord(session, uint64(n+1), domain.EventKindToolResult, domain.ToolResultPayload{ToolName: "world.read", Result: jsonRaw(n)}, ts, mono, turn)
	}
}

func jsonRaw(n int) []byte { return []byte(fmt.Sprintf(`{"seq":%d}`, n)) }
func jsonRawTools() []byte { return []byte(`[]`) }

// TestLargeSessionStreaming exports ~100k events through all three
// formats with bounded buffers and records time/memory receipts.
func TestLargeSessionStreaming(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	const events = 100_000
	session, _ := domain.ParseSessionID("ses_" + strings.Repeat("0", 25) + "F")
	turn, _ := domain.ParseTurnID("trn_" + strings.Repeat("0", 25) + "F")
	dir := t.TempDir()
	b, _ := NewBundle(filepath.Join(dir, "bundle"))
	tr, _ := NewTranscript(filepath.Join(dir, "transcript.txt"))
	ac, _ := NewAsciicast(filepath.Join(dir, "session.cast"), 80, 24, 1788422400, "xterm-256color")

	runtime.GC()
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)
	start := time.Now()
	for i := 0; i < events; i++ {
		r := genRecord(i, session, &turn, "", nil)
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
	s := Stream{SessionID: session.String(), StartedAt: 1788422400000, Complete: false}
	if _, err := b.Close(s); err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(s); err != nil {
		t.Fatal(err)
	}
	if err := ac.Close(); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	runtime.GC()
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)

	sizes := map[string]int64{}
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			sizes[p] = fi.Size()
		}
		return nil
	})
	var total int64
	for _, v := range sizes {
		total += v
	}
	receipt := fmt.Sprintf("events=%d elapsed=%s heap_alloc_before=%d heap_alloc_after=%d total_alloc_delta=%d bytes_written=%d\n",
		events, elapsed, memBefore.HeapAlloc, memAfter.HeapAlloc, memAfter.TotalAlloc-memBefore.TotalAlloc, total)
	t.Log(receipt)
	os.MkdirAll("receipts", 0o755)
	os.WriteFile("receipts/large-session-metrics.txt", []byte(receipt), 0o644)

	// Manifest checksums must still verify on the large bundle.
	problems, err := VerifyManifest(filepath.Join(dir, "bundle"))
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("manifest problems: %v", problems[:min(3, len(problems))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
