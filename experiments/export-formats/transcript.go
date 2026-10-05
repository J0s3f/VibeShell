package exportformats

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"j0s.at/vibeshell/internal/domain"
)

// Transcript is format 2: a readable UTF-8, explicitly lossy projection
// of the same canonical events.
type Transcript struct {
	w *bufio.Writer
	f *os.File
}

// NewTranscript creates the transcript writer.
func NewTranscript(path string) (*Transcript, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	t := &Transcript{f: f, w: bufio.NewWriterSize(f, 64*1024)}
	fmt.Fprintln(t.w, "VibeShell research transcript — LOSSY PROJECTION")
	fmt.Fprintln(t.w, "This transcript is derived from the canonical event stream and")
	fmt.Fprintln(t.w, "intentionally drops: exact payload bytes, checksums, provenance")
	fmt.Fprintln(t.w, "detail, transport byte streams, and timing beyond ordering.")
	fmt.Fprintln(t.w, "Full-screen (alternate screen) output appears as labelled screen")
	fmt.Fprintln(t.w, "snapshots; cursor motion and styling is not preserved.")
	fmt.Fprintln(t.w, "---")
	return t, nil
}

// Write renders one record.
func (t *Transcript) Write(r Record) error {
	switch r.Env.Kind {
	case domain.EventKindSessionStart:
		var p domain.SessionStartPayload
		json.Unmarshal(r.PayloadJSON, &p)
		fmt.Fprintf(t.w, "== SESSION %s start: user=%s auth=%s term=%s %dx%d sharing=%v ==\n",
			r.Env.SessionID, p.UserID, p.AuthMode, p.TerminalType, p.TerminalSize.Cols, p.TerminalSize.Rows, p.SharingEnabled)
	case domain.EventKindSessionEnd:
		var p domain.SessionEndPayload
		json.Unmarshal(r.PayloadJSON, &p)
		fmt.Fprintf(t.w, "== SESSION %s end: reason=%s turns=%d duration_ms=%d ==\n", r.Env.SessionID, p.Reason, p.TurnsCount, p.DurationMs)
	case domain.EventKindInputAccepted:
		var p domain.InputAcceptedPayload
		json.Unmarshal(r.PayloadJSON, &p)
		switch p.Action {
		case "command":
			fmt.Fprintf(t.w, "[USER INPUT] command: %s\n", p.Command)
		case "key":
			fmt.Fprintf(t.w, "[USER INPUT] key: %s\n", p.Key)
		case "resize":
			fmt.Fprintf(t.w, "[TERMINAL] window resized to %dx%d\n", p.Resize.Cols, p.Resize.Rows)
		case "paste":
			fmt.Fprintln(t.w, "[USER INPUT] paste boundary")
		case "cancel":
			fmt.Fprintln(t.w, "[INTERRUPTION] user cancelled the running operation")
		default:
			fmt.Fprintf(t.w, "[USER INPUT] action: %s\n", p.Action)
		}
	case domain.EventKindInputRaw, domain.EventKindInputDecoded:
		// Raw/decoded input is evidence for the accepted action; the
		// transcript keeps only the accepted semantic line.
	case domain.EventKindTerminalFrame:
		var p domain.TerminalFramePayload
		json.Unmarshal(r.PayloadJSON, &p)
		blob := r.Blobs[p.ContentRef.Hash.String()]
		label := "[TERMINAL OUTPUT]"
		if p.Mode == "alt" {
			label = "[SCREEN SNAPSHOT: alt screen]"
		}
		fmt.Fprintf(t.w, "%s frame=%s bytes=%d:\n%s\n", label, p.FrameID, p.ContentRef.Size, renderFrame(blob))
	case domain.EventKindTerminalMode:
		var p TerminalModePayload
		json.Unmarshal(r.PayloadJSON, &p)
		fmt.Fprintf(t.w, "[SCREEN TRANSITION] mode %s -> %s (full-screen content is not preserved by this projection)\n", p.From, p.To)
	case domain.EventKindTerminalPrompt:
		var p domain.TerminalPromptPayload
		json.Unmarshal(r.PayloadJSON, &p)
		fmt.Fprintf(t.w, "[PROMPT] cwd=%s exit=%d %q\n", p.CWD, p.ExitCode, p.Prompt)
	case domain.EventKindModelError, domain.EventKindToolError, domain.EventKindRouteFailure:
		fmt.Fprintf(t.w, "[FAILURE] %s: %s\n", r.Env.Kind, stringField(r.PayloadJSON, "error_message"))
	case domain.EventKindModelRequest:
		fmt.Fprintf(t.w, "[MODEL] request route=%s\n", routingField(r.PayloadJSON))
	case domain.EventKindModelResponse:
		fmt.Fprintf(t.w, "[MODEL] response finish=%s\n", stringField(r.PayloadJSON, "finish_reason"))
	case domain.EventKindToolRequest, domain.EventKindToolResult:
		fmt.Fprintf(t.w, "[TOOL] %s\n", r.Env.Kind)
	case domain.EventKindWorldCommit, domain.EventKindWorldRead, domain.EventKindWorldConflict, domain.EventKindWorldMaterialize, domain.EventKindWorldStage:
		fmt.Fprintf(t.w, "[WORLD] %s rev=%d\n", r.Env.Kind, r.Env.NodeRevision)
	case domain.EventKindAppCreated, domain.EventKindAppActivated, domain.EventKindAppExtended, domain.EventKindAppValidated, domain.EventKindAppRolledBack, domain.EventKindAppSandboxRun:
		fmt.Fprintf(t.w, "[APP] %s\n", r.Env.Kind)
	case domain.EventKindContextSummary, domain.EventKindContextRetrieval:
		fmt.Fprintf(t.w, "[CONTEXT] %s\n", r.Env.Kind)
	default:
		fmt.Fprintf(t.w, "[EVENT] %s\n", r.Env.Kind)
	}
	return nil
}

// Close finalizes; if the stream never recorded session.end it MUST say
// so and must not fabricate a successful ending.
func (t *Transcript) Close(s Stream) error {
	if !s.Complete {
		fmt.Fprintf(t.w, "[STATUS] session %s has NO recorded end: connection incomplete/disconnected; no successful ending was recorded or reconstructed.\n", s.SessionID)
	}
	if err := t.w.Flush(); err != nil {
		return err
	}
	return t.f.Close()
}

func renderFrame(b []byte) string {
	s := string(b)
	s = strings.ReplaceAll(s, "\x1b[H", "<ESC>H")
	s = strings.ReplaceAll(s, "\x1b[2J", "<ESC>[2J>")
	s = strings.TrimRight(s, "\r\n")
	lines := strings.Split(s, "\r\n")
	if len(lines) > 4 {
		return strings.Join(lines[:4], "\n") + fmt.Sprintf("\n... (%d lines total)", len(lines))
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

func routingField(payload []byte) string {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return "?"
	}
	if v, ok := m["route_id"].(string); ok {
		return v
	}
	return "?"
}
