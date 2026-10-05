package exportformats

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

// Asciicast is format 3: an asciicast v2 terminal recording
// (https://docs.asciinema.org/manual/asciicast/v2/).
type Asciicast struct {
	w     *bufio.Writer
	f     *os.File
	t0    int64 // first event timestamp (ms)
	start bool
	cols  uint16
	rows  uint16
}

// NewAsciicast writes the v2 header immediately.
func NewAsciicast(path string, cols, rows uint16, startedAtUnixSec int64, termType string) (*Asciicast, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	a := &Asciicast{f: f, w: bufio.NewWriterSize(f, 64*1024), cols: cols, rows: rows}
	header := map[string]any{
		"version":   2,
		"width":     cols,
		"height":    rows,
		"timestamp": startedAtUnixSec,
		"env":       map[string]string{"SHELL": "/usr/bin/vibeshell", "TERM": termType},
	}
	hb, _ := json.Marshal(header)
	fmt.Fprintln(a.w, string(hb))
	return a, nil
}

// Write maps one canonical record to zero or one asciicast events.
func (a *Asciicast) Write(r Record) error {
	t := eventSeconds(a, r.Env.Timestamp)
	switch r.Env.Kind {
	case domain.EventKindTerminalFrame:
		var p domain.TerminalFramePayload
		if err := json.Unmarshal(r.PayloadJSON, &p); err != nil {
			return err
		}
		blob := r.Blobs[p.ContentRef.Hash.String()]
		return a.emit(t, "o", blob)
	case domain.EventKindInputAccepted:
		var p domain.InputAcceptedPayload
		if err := json.Unmarshal(r.PayloadJSON, &p); err != nil {
			return err
		}
		switch p.Action {
		case "command":
			return a.emit(t, "i", []byte(p.Command+"\r"))
		case "key":
			return a.emit(t, "i", []byte(p.Key))
		case "cancel", "eof":
			// No byte stream to record; preserved in the JSONL bundle.
			return nil
		case "resize":
			if p.Resize == nil {
				return nil
			}
			a.cols, a.rows = p.Resize.Cols, p.Resize.Rows
			return a.emit(t, "r", []byte(fmt.Sprintf("%dx%d", p.Resize.Cols, p.Resize.Rows)))
		}
	}
	return nil
}

func eventSeconds(a *Asciicast, tsMillis int64) float64 {
	if !a.start {
		a.t0 = tsMillis
		a.start = true
	}
	return float64(tsMillis-a.t0) / 1000.0
}

func (a *Asciicast) emit(t float64, code string, data []byte) error {
	b, err := json.Marshal(string(data))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.w, "[%.3f, %q, %s]\n", t, code, b)
	return err
}

// Close flushes and closes the recording. Asciicast v2 has no end
// marker; an incomplete session simply ends mid-stream (the JSONL
// bundle carries the explicit disconnected status).
func (a *Asciicast) Close() error {
	if err := a.w.Flush(); err != nil {
		return err
	}
	return a.f.Close()
}

// ReplayHeader is the decoded v2 header.
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

// ReplayAsciicast parses a v2 recording: first line header, then one
// JSON [time, code, data] array per line.
func ReplayAsciicast(r io.Reader) (ReplayHeader, []ReplayEvent, error) {
	sc := bufio.NewScanner(r)
	buf := make([]byte, 0, 1024*1024)
	sc.Buffer(buf, 16*1024*1024)
	if !sc.Scan() {
		return ReplayHeader{}, nil, io.ErrUnexpectedEOF
	}
	var h ReplayHeader
	if err := json.Unmarshal(sc.Bytes(), &h); err != nil {
		return ReplayHeader{}, nil, fmt.Errorf("bad header: %w", err)
	}
	if h.Version != 2 {
		return h, nil, fmt.Errorf("unsupported asciicast version %d", h.Version)
	}
	var events []ReplayEvent
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var row []json.RawMessage
		if err := json.Unmarshal(line, &row); err != nil || len(row) != 3 {
			return h, events, fmt.Errorf("bad event line: %q", line)
		}
		var at float64
		if err := json.Unmarshal(row[0], &at); err != nil {
			return h, events, err
		}
		var code string
		if err := json.Unmarshal(row[1], &code); err != nil {
			return h, events, err
		}
		var s string
		if err := json.Unmarshal(row[2], &s); err != nil {
			return h, events, fmt.Errorf("event data not a string: %w", err)
		}
		events = append(events, ReplayEvent{At: at, Code: code, Data: []byte(s)})
	}
	return h, events, sc.Err()
}

func parseResize(s string) (int, int, bool) {
	i := strings.IndexByte(s, 'x')
	if i <= 0 {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(s[:i])
	h, err2 := strconv.Atoi(s[i+1:])
	return w, h, err1 == nil && err2 == nil
}

// ResizeSchedule returns the accepted (t, cols, rows) resize list.
func ResizeSchedule(events []ReplayEvent) []string {
	var out []string
	for _, e := range events {
		if e.Code == "r" {
			out = append(out, fmt.Sprintf("%.3f:%s", e.At, e.Data))
		}
	}
	return out
}
