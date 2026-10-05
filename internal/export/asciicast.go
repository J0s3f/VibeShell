package export

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"j0s.at/vibeshell/internal/domain"
)

// Terminal defaults when a recording has no session-start metadata. They are
// the conventional 80x24 xterm so a replay never fails on a missing header.
const (
	defaultCols = 80
	defaultRows = 24
	defaultTerm = "xterm-256color"
)

// asciicastMaxPendingLookahead bounds how many leading records are buffered
// while waiting for the session-start metadata that sizes the v2 header.
const asciicastMaxPendingLookahead = 64

// asciicast streams format 3: an asciinema-compatible asciicast v2 recording.
// The header is written lazily once the session-start metadata is known; only a
// small bounded prefix is buffered while waiting.
type asciicast struct {
	f *os.File
	w *bufio.Writer

	headerWritten bool
	gotMeta       bool
	pending       []record

	cols, rows uint16
	term       string
	t0         int64 // first event timestamp (ms)
	lastT      float64
}

func newAsciicast(path string) (*asciicast, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &asciicast{
		f:     f,
		w:     bufio.NewWriterSize(f, 64*1024),
		cols:  defaultCols,
		rows:  defaultRows,
		term:  defaultTerm,
		lastT: -1,
	}, nil
}

func (a *asciicast) primaryPath() string { return a.f.Name() }

// write consumes one record. Until the header is written, records are buffered;
// the header is emitted as soon as session-start metadata arrives or the
// lookahead bound is reached.
func (a *asciicast) write(rec record) error {
	if a.headerWritten {
		return a.emit(rec)
	}
	a.pending = append(a.pending, rec)
	if meta, ok := sessionMeta(rec.env); ok {
		a.cols, a.rows, a.term = meta.cols, meta.rows, meta.term
		a.t0 = meta.startedAt
		a.gotMeta = true
	}
	if !a.gotMeta && len(a.pending) <= asciicastMaxPendingLookahead {
		return nil
	}
	if err := a.writeHeader(); err != nil {
		return err
	}
	pending := a.pending
	a.pending = nil
	for _, p := range pending {
		if err := a.emit(p); err != nil {
			return err
		}
	}
	return nil
}

// close flushes the recording. Asciicast v2 has no end marker, so an
// incomplete session simply ends mid-stream; the JSONL bundle carries the
// explicit disconnected status and the transcript states it.
func (a *asciicast) close(streamSummary) error {
	if !a.headerWritten {
		if err := a.writeHeader(); err != nil {
			return err
		}
		pending := a.pending
		a.pending = nil
		for _, p := range pending {
			if err := a.emit(p); err != nil {
				return err
			}
		}
	}
	if err := a.w.Flush(); err != nil {
		return err
	}
	return a.f.Close()
}

func (a *asciicast) abort() error { return a.f.Close() }

func (a *asciicast) writeHeader() error {
	if a.t0 == 0 && len(a.pending) > 0 {
		a.t0 = a.pending[0].env.Timestamp
	}
	header := map[string]any{
		"version":   AsciicastFormatVersion,
		"width":     a.cols,
		"height":    a.rows,
		"timestamp": a.t0 / 1000,
		"env":       map[string]string{"SHELL": "/usr/bin/vibeshell", "TERM": a.term},
	}
	raw, err := json.Marshal(header)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(a.w, string(raw)); err != nil {
		return err
	}
	a.headerWritten = true
	return nil
}

// emit maps one canonical record to zero or one asciicast event.
func (a *asciicast) emit(rec record) error {
	switch rec.env.Kind {
	case domain.EventKindTerminalFrame:
		var p domain.TerminalFramePayload
		if err := json.Unmarshal(rec.payload, &p); err != nil {
			return err
		}
		return a.emitEvent(a.eventSeconds(rec.env.Timestamp), "o", rec.blobs[p.ContentRef.Hash.String()])
	case domain.EventKindInputAccepted:
		var p domain.InputAcceptedPayload
		if err := json.Unmarshal(rec.payload, &p); err != nil {
			return err
		}
		switch p.Action {
		case "command":
			return a.emitEvent(a.eventSeconds(rec.env.Timestamp), "i", []byte(p.Command+"\r"))
		case "key":
			return a.emitEvent(a.eventSeconds(rec.env.Timestamp), "i", []byte(p.Key))
		case "resize":
			if p.Resize == nil {
				return nil
			}
			a.cols, a.rows = p.Resize.Cols, p.Resize.Rows
			data := []byte(fmt.Sprintf("%dx%d", p.Resize.Cols, p.Resize.Rows))
			return a.emitEvent(a.eventSeconds(rec.env.Timestamp), "r", data)
		default:
			// cancel/eof/paste carry no byte stream the replay format can
			// express; they are preserved in the JSONL bundle.
			return nil
		}
	default:
		return nil
	}
}

// eventSeconds converts an absolute timestamp to seconds relative to the first
// event. A non-monotonic wall clock is clamped so replay timing never runs
// backwards.
func (a *asciicast) eventSeconds(tsMillis int64) float64 {
	t := float64(tsMillis-a.t0) / 1000.0
	if a.lastT >= 0 && t < a.lastT {
		t = a.lastT
	}
	a.lastT = t
	return t
}

func (a *asciicast) emitEvent(t float64, code string, data []byte) error {
	encoded, err := json.Marshal(string(data))
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(a.w, "[%.3f, %q, %s]\n", t, code, encoded); err != nil {
		return err
	}
	return nil
}

// sessionMetaInfo is the header-relevant metadata from a session-start event.
type sessionMetaInfo struct {
	cols, rows uint16
	term       string
	startedAt  int64
}

func sessionMeta(env domain.EventEnvelope) (sessionMetaInfo, bool) {
	if env.Kind != domain.EventKindSessionStart || len(env.Payload.Inline) == 0 {
		return sessionMetaInfo{}, false
	}
	var p domain.SessionStartPayload
	if err := json.Unmarshal(env.Payload.Inline, &p); err != nil {
		return sessionMetaInfo{}, false
	}
	cols, rows := p.TerminalSize.Cols, p.TerminalSize.Rows
	if cols == 0 {
		cols = defaultCols
	}
	if rows == 0 {
		rows = defaultRows
	}
	term := p.TerminalType
	if term == "" {
		term = defaultTerm
	}
	return sessionMetaInfo{cols: cols, rows: rows, term: term, startedAt: env.Timestamp}, true
}

// ReplayHeader is the decoded asciicast v2 header.
type ReplayHeader struct {
	Version   int               `json:"version"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Timestamp float64           `json:"timestamp"`
	Env       map[string]string `json:"env"`
}

// ReplayEvent is one decoded asciicast event.
type ReplayEvent struct {
	At   float64
	Code string
	Data []byte
}

// Replay parses an asciicast v2 recording: the first line is the header, then
// one JSON [time, code, data] array per line. It accepts no other version.
func Replay(r io.Reader) (ReplayHeader, []ReplayEvent, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	if !sc.Scan() {
		return ReplayHeader{}, nil, io.ErrUnexpectedEOF
	}
	var header ReplayHeader
	if err := json.Unmarshal(sc.Bytes(), &header); err != nil {
		return ReplayHeader{}, nil, fmt.Errorf("export: bad asciicast header: %w", err)
	}
	if header.Version != AsciicastFormatVersion {
		return header, nil, fmt.Errorf("export: unsupported asciicast version %d", header.Version)
	}
	var events []ReplayEvent
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var row []json.RawMessage
		if err := json.Unmarshal(line, &row); err != nil || len(row) != 3 {
			return header, events, fmt.Errorf("export: bad asciicast event line: %q", line)
		}
		var at float64
		if err := json.Unmarshal(row[0], &at); err != nil {
			return header, events, err
		}
		var code string
		if err := json.Unmarshal(row[1], &code); err != nil {
			return header, events, err
		}
		var data string
		if err := json.Unmarshal(row[2], &data); err != nil {
			return header, events, fmt.Errorf("export: asciicast event data is not a string: %w", err)
		}
		events = append(events, ReplayEvent{At: at, Code: code, Data: []byte(data)})
	}
	return header, events, sc.Err()
}

// ResizeSchedule returns the accepted (t, "COLSxROWS") resize events, so a
// test or reader can assert the recorded window changes.
func ResizeSchedule(events []ReplayEvent) []string {
	var out []string
	for _, e := range events {
		if e.Code == "r" {
			out = append(out, fmt.Sprintf("%.3f:%s", e.At, e.Data))
		}
	}
	return out
}

// AcceptedOutput concatenates the terminal output ("o") events in order,
// reconstructing the accepted terminal byte stream.
func AcceptedOutput(events []ReplayEvent) []byte {
	var buf bytes.Buffer
	for _, e := range events {
		if e.Code == "o" {
			buf.Write(e.Data)
		}
	}
	return buf.Bytes()
}

// InputBytes concatenates the input ("i") events in order.
func InputBytes(events []ReplayEvent) []byte {
	var buf bytes.Buffer
	for _, e := range events {
		if e.Code == "i" {
			buf.Write(e.Data)
		}
	}
	return buf.Bytes()
}

// parseResize decodes a "COLSxROWS" resize payload.
func parseResize(s string) (int, int, bool) {
	i := strings.IndexByte(s, 'x')
	if i <= 0 {
		return 0, 0, false
	}
	cols, err1 := strconv.Atoi(s[:i])
	rows, err2 := strconv.Atoi(s[i+1:])
	return cols, rows, err1 == nil && err2 == nil
}
