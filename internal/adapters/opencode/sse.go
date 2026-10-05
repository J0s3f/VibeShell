package opencode

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
)

// defaultMaxEvents is the decoded-event bound for a streamed response. It is
// sized for a reasoning model that streams a generation-sized response in small
// frames, with headroom above the one-frame-per-token worst case for the
// configured generation budget.
const defaultMaxEvents = 65536

// Bounds applied to every streamed response. Zero values select the
// documented defaults via withDefaults.
type Bounds struct {
	// MaxFrameBytes caps one SSE frame (event block). Default 1 MiB.
	MaxFrameBytes int
	// MaxBodyBytes caps the whole stream body. Default 8 MiB.
	MaxBodyBytes int64
	// MaxEvents caps decoded SSE events. Default 65536: a reasoning model
	// streams its chain of thought and the artifact in many small frames, so a
	// generation-sized response (32768 tokens) can exceed a few thousand events
	// and must not be rejected as a runaway stream.
	MaxEvents int
}

func (b Bounds) withDefaults() Bounds {
	out := Bounds{
		MaxFrameBytes: 1 << 20,
		MaxBodyBytes:  8 << 20,
		MaxEvents:     defaultMaxEvents,
	}
	if b.MaxFrameBytes > 0 {
		out.MaxFrameBytes = b.MaxFrameBytes
	}
	if b.MaxBodyBytes > 0 {
		out.MaxBodyBytes = b.MaxBodyBytes
	}
	if b.MaxEvents > 0 {
		out.MaxEvents = b.MaxEvents
	}
	return out
}

// SSEEvent is one decoded Server-Sent Events block: the optional event
// name plus concatenated data lines.
type SSEEvent struct {
	Type string
	Data string
}

// scanSSE reads SSE frames from r until EOF or ctx cancellation. It
// tolerates frames split across arbitrarily small reads and multiple
// events per read. Comment lines (":...") are skipped. Lines that are
// neither blank, comments, nor SSE field lines are returned as bare lines
// so callers can detect a non-SSE error body (HTTP 200 carrying a JSON
// error envelope). scanSSE returns io.EOF only after the final frame;
// callers treat a truncated mid-frame tail as a truncation error.
func scanSSE(ctx context.Context, r io.Reader, maxFrameBytes, maxEvents int) (events []SSEEvent, bare []string, err error) {
	br, ok := r.(*bufio.Reader)
	if !ok {
		// A small read size keeps framing correct when the transport
		// delivers one TCP segment per Read; correctness never depends
		// on read granularity.
		br = bufio.NewReaderSize(r, 512)
	}
	var (
		eventType string
		data      []string
		frameLen  int
		flush     = func() error {
			if eventType == "" && len(data) == 0 {
				return nil
			}
			if len(events) >= maxEvents {
				return fmt.Errorf("opencode: too many SSE events (limit %d)", maxEvents)
			}
			events = append(events, SSEEvent{Type: eventType, Data: strings.Join(data, "\n")})
			eventType = ""
			data = nil
			frameLen = 0
			return nil
		}
	)
	for {
		if err := ctx.Err(); err != nil {
			return events, bare, err
		}
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			if frameLen+len(line) > maxFrameBytes {
				return events, bare, fmt.Errorf("opencode: SSE frame exceeds %d bytes", maxFrameBytes)
			}
			frameLen += len(line)
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if err := flush(); err != nil {
					return events, bare, err
				}
			case strings.HasPrefix(line, ":"):
				// Comment / heartbeat: ignored per SSE.
			default:
				name, value, hasColon := strings.Cut(line, ":")
				if !hasColon || !isSSEFieldName(name) {
					// Not an SSE field line: possible bare error body
					// (a 200 carrying a JSON envelope instead of SSE).
					bare = append(bare, line)
					break
				}
				if strings.HasPrefix(value, " ") {
					value = value[1:]
				}
				switch name {
				case "event":
					eventType = value
				case "data":
					data = append(data, value)
				default:
					// Unknown SSE field names are ignored.
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				if eventType != "" || len(data) != 0 {
					return events, bare, fmt.Errorf("opencode: %w: stream ended mid-frame", errTruncated{})
				}
				return events, bare, nil
			}
			return events, bare, err
		}
	}
}

// isSSEFieldName accepts only plausible SSE field tokens; JSON error
// bodies contain colons but never parse as field names.
func isSSEFieldName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if c != '-' && (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// errTruncated marks a stream that ended inside a frame or JSON payload.
type errTruncated struct{}

func (errTruncated) Error() string { return "truncated stream" }

// IsTruncated reports whether err is a mid-stream interruption.
func IsTruncated(err error) bool {
	return err != nil && strings.Contains(err.Error(), errTruncated{}.Error())
}

// boundedReader caps the bytes a decode may consume.
func boundedReader(r io.Reader, maxBytes int64) io.Reader {
	if maxBytes <= 0 {
		return r
	}
	return &limitReader{r: r, n: maxBytes, max: maxBytes}
}

type limitReader struct {
	r   io.Reader
	n   int64
	max int64
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, fmt.Errorf("opencode: response body exceeds %d bytes", l.max)
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	return n, err
}
