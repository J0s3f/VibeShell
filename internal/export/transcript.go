package export

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"j0s.at/vibeshell/internal/domain"
)

// transcriptFormatName identifies the readable-transcript layout.
const transcriptFormatName = "vibeshell-transcript"

// transcript streams format 2: a readable, explicitly lossy UTF-8 projection
// of the same canonical events. It states its losses up front and never
// fabricates a successful ending.
type transcript struct {
	f *os.File
	w *bufio.Writer
}

func newTranscript(path string) (*transcript, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	t := &transcript{f: f, w: bufio.NewWriterSize(f, 64*1024)}
	fmt.Fprintf(t.w, "VibeShell research transcript — %s v%d — LOSSY PROJECTION\n", transcriptFormatName, TranscriptFormatVersion)
	fmt.Fprintln(t.w, "Derived from the canonical event stream. This projection intentionally")
	fmt.Fprintln(t.w, "drops: exact payload bytes, checksums, provenance detail, transport byte")
	fmt.Fprintln(t.w, "streams, full-screen cursor motion, and timing beyond ordering. Alternate-")
	fmt.Fprintln(t.w, "screen output appears only as labelled screen snapshots/transitions.")
	fmt.Fprintln(t.w, "---")
	return t, nil
}

func (t *transcript) primaryPath() string { return t.f.Name() }

func (t *transcript) write(rec record) error {
	switch rec.env.Kind {
	case domain.EventKindSessionStart:
		var p domain.SessionStartPayload
		_ = json.Unmarshal(rec.payload, &p)
		fmt.Fprintf(t.w, "== SESSION %s start: user=%s auth=%s term=%s %dx%d sharing=%v ==\n",
			rec.env.SessionID, p.UserID, p.AuthMode, p.TerminalType, p.TerminalSize.Cols, p.TerminalSize.Rows, p.SharingEnabled)
	case domain.EventKindSessionEnd:
		var p domain.SessionEndPayload
		_ = json.Unmarshal(rec.payload, &p)
		fmt.Fprintf(t.w, "== SESSION %s end: reason=%s turns=%d duration_ms=%d ==\n",
			rec.env.SessionID, p.Reason, p.TurnsCount, p.DurationMs)
	case domain.EventKindInputAccepted:
		var p domain.InputAcceptedPayload
		_ = json.Unmarshal(rec.payload, &p)
		switch p.Action {
		case "command":
			fmt.Fprintf(t.w, "[USER INPUT] command: %s\n", p.Command)
		case "key":
			fmt.Fprintf(t.w, "[USER INPUT] key: %s\n", p.Key)
		case "resize":
			if p.Resize != nil {
				fmt.Fprintf(t.w, "[TERMINAL] window resized to %dx%d\n", p.Resize.Cols, p.Resize.Rows)
			} else {
				fmt.Fprintln(t.w, "[TERMINAL] window resized (size unknown)")
			}
		case "paste":
			fmt.Fprintln(t.w, "[USER INPUT] paste")
		case "cancel":
			fmt.Fprintln(t.w, "[INTERRUPTION] user cancelled the running operation")
		case "eof":
			fmt.Fprintln(t.w, "[SESSION] input ended (EOF)")
		default:
			fmt.Fprintf(t.w, "[USER INPUT] action: %s\n", p.Action)
		}
	case domain.EventKindInputRaw, domain.EventKindInputDecoded:
		// Raw/decoded input is evidence for the accepted action; the
		// transcript keeps only the accepted semantic line.
	case domain.EventKindTerminalFrame:
		var p domain.TerminalFramePayload
		_ = json.Unmarshal(rec.payload, &p)
		blob := rec.blobs[p.ContentRef.Hash.String()]
		label := "[TERMINAL OUTPUT]"
		if p.IsPrompt {
			label = "[TERMINAL OUTPUT: prompt]"
		}
		if p.Mode == "alt" || p.Mode == "app" {
			label = "[SCREEN SNAPSHOT: " + p.Mode + " screen]"
		}
		fmt.Fprintf(t.w, "%s frame=%s bytes=%d:\n%s\n", label, p.FrameID, p.ContentRef.Size, renderFrame(blob))
	case domain.EventKindTerminalMode:
		var p terminalModePayload
		_ = json.Unmarshal(rec.payload, &p)
		fmt.Fprintf(t.w, "[SCREEN TRANSITION] mode %s -> %s (full-screen cursor motion is not preserved by this projection)\n", p.From, p.To)
	case domain.EventKindTerminalPrompt:
		var p domain.TerminalPromptPayload
		_ = json.Unmarshal(rec.payload, &p)
		fmt.Fprintf(t.w, "[PROMPT] cwd=%s exit=%d %q\n", p.CWD, p.ExitCode, p.Prompt)
	case domain.EventKindModelError, domain.EventKindToolError, domain.EventKindRouteFailure:
		fmt.Fprintf(t.w, "[FAILURE] %s: %s\n", rec.env.Kind, stringField(rec.payload, "error_message"))
	case domain.EventKindModelRequest:
		fmt.Fprintf(t.w, "[MODEL] request route=%s\n", stringField(rec.payload, "route_id"))
	case domain.EventKindModelResponse:
		fmt.Fprintf(t.w, "[MODEL] response finish=%s\n", stringField(rec.payload, "finish_reason"))
	case domain.EventKindToolRequest, domain.EventKindToolResult:
		fmt.Fprintf(t.w, "[TOOL] %s\n", rec.env.Kind)
	case domain.EventKindWorldRead, domain.EventKindWorldStage, domain.EventKindWorldCommit,
		domain.EventKindWorldConflict, domain.EventKindWorldMaterialize:
		fmt.Fprintf(t.w, "[WORLD] %s rev=%d\n", rec.env.Kind, rec.env.NodeRevision)
	case domain.EventKindAppCreated, domain.EventKindAppExtended, domain.EventKindAppValidated,
		domain.EventKindAppActivated, domain.EventKindAppRolledBack, domain.EventKindAppSandboxRun:
		fmt.Fprintf(t.w, "[APP] %s\n", rec.env.Kind)
	case domain.EventKindContextSummary, domain.EventKindContextRetrieval:
		fmt.Fprintf(t.w, "[CONTEXT] %s\n", rec.env.Kind)
	default:
		fmt.Fprintf(t.w, "[EVENT] %s\n", rec.env.Kind)
	}
	return nil
}

// close states explicitly when a session has no recorded end. It never writes
// an end line the stream did not contain.
func (t *transcript) close(summary streamSummary) error {
	for _, st := range summary.sessions {
		if !st.Complete {
			fmt.Fprintf(t.w, "[STATUS] session %s has NO recorded end: connection incomplete/disconnected; "+
				"no successful ending was recorded or reconstructed (events=%d).\n", st.SessionID, st.EventCount)
		}
	}
	if err := t.w.Flush(); err != nil {
		return err
	}
	return t.f.Close()
}

func (t *transcript) abort() error { return t.f.Close() }

// terminalModePayload describes a screen-mode transition. The domain declares
// the event kind but no payload struct, so the transcript decodes the recorded
// JSON directly.
type terminalModePayload struct {
	From string `json:"from"` // "line", "app", "alt"
	To   string `json:"to"`
}

// EventKind marks the payload as satisfying domain.EventPayload so it can be
// recorded and marshalled like the domain's typed payloads.
func (terminalModePayload) EventKind() domain.EventKind { return domain.EventKindTerminalMode }

// renderFrame makes a terminal frame readable and safe as text: escape
// sequences become visible and other C0 controls are dropped, then the frame
// is bounded to a few lines.
func renderFrame(b []byte) string {
	s := string(b)
	s = strings.ReplaceAll(s, "\x1b", "<ESC>")
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			sb.WriteRune(r)
		case r == '\r':
			// normalized below
		case r < 0x20 || r == 0x7f:
			// drop remaining control characters
		case r == utf8.RuneError:
			sb.WriteRune('?')
		default:
			sb.WriteRune(r)
		}
	}
	s = strings.ReplaceAll(sb.String(), "\r\n", "\n")
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return "(no printable output)"
	}
	lines := strings.Split(s, "\n")
	const maxLines = 6
	if len(lines) > maxLines {
		return strings.Join(lines[:maxLines], "\n") + fmt.Sprintf("\n... (%d lines total)", len(lines))
	}
	return strings.Join(lines, "\n")
}

func stringField(payload []byte, key string) string {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return "?"
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return "?"
}
