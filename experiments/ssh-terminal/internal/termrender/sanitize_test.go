package termrender_test

import (
	"strings"
	"testing"

	"j0s.at/vibeshell/experiments/ssh-terminal/internal/termrender"
)

// frame holds sequences a renderer may send and sequences that must never reach
// a client's terminal.
const allowedFrame = "\x1b[2J\x1b[1;1H\x1b[31merror:\x1b[0m permission denied\r\n"

func TestSanitizeKeepsAllowedSequences(t *testing.T) {
	report := termrender.Sanitize([]byte(allowedFrame), termrender.Policy{})
	if len(report.Removals) != 0 {
		t.Fatalf("removals = %v, want none", report.Removals)
	}
	if got := string(report.Kept); got != allowedFrame {
		t.Fatalf("kept %q, want the frame unchanged", got)
	}
}

func TestSanitizeRemovesEveryUnapprovedControl(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		sequence   string
		wantKind   string
		wantKept   string
		wantAbsent bool
	}{
		{
			name:       "clipboard write",
			sequence:   "\x1b]52;c;cGFzc3dvcmQ=\x07",
			wantKind:   "OSC",
			wantAbsent: true,
		},
		{
			name:       "window title",
			sequence:   "\x1b]0;stolen title\x07",
			wantKind:   "OSC",
			wantAbsent: true,
		},
		{
			name:       "hyperlink",
			sequence:   "\x1b]8;;http://example.invalid\x1b\\click\x1b]8;;\x1b\\",
			wantKind:   "OSC",
			wantKept:   "click",
			wantAbsent: true,
		},
		{
			name:       "device control string",
			sequence:   "\x1bPq#0;2;0;0;0\x1b\\",
			wantKind:   "DCS",
			wantAbsent: true,
		},
		{
			name:       "application program command",
			sequence:   "\x1b_Gf=100,a=T;AAAA\x1b\\",
			wantKind:   "APC",
			wantAbsent: true,
		},
		{
			name:       "privacy message",
			sequence:   "\x1b^private\x1b\\",
			wantKind:   "PM",
			wantAbsent: true,
		},
		{
			name:       "unapproved cursor save",
			sequence:   "\x1b7",
			wantKind:   "ESC",
			wantAbsent: true,
		},
		{
			name:       "device attributes query",
			sequence:   "\x1b[c",
			wantKind:   "CSI c",
			wantAbsent: true,
		},
		{
			name:       "private mode set",
			sequence:   "\x1b[?1049h",
			wantKind:   "CSI h",
			wantAbsent: true,
		},
		{
			name:       "unapproved CSI",
			sequence:   "\x1b[6n",
			wantKind:   "CSI n",
			wantAbsent: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			frame := "before" + testCase.sequence + "after"
			report := termrender.Sanitize([]byte(frame), termrender.Policy{})
			if len(report.Removals) == 0 {
				t.Fatalf("nothing was removed from %q", frame)
			}
			found := false
			for _, removal := range report.Removals {
				if removal.Kind == testCase.wantKind {
					found = true
				}
				if strings.Contains(string(report.Kept), removal.Bytes) {
					t.Errorf("kept bytes contain the rejected sequence %q", removal.Bytes)
				}
			}
			if !found {
				t.Errorf("removals = %v, want one of kind %q", report.Removals, testCase.wantKind)
			}
			kept := string(report.Kept)
			if testCase.wantAbsent && strings.Contains(kept, "\x1b") {
				t.Fatalf("kept %q, want no escape sequence left", kept)
			}
			if testCase.wantKept != "" && !strings.Contains(kept, testCase.wantKept) {
				t.Errorf("kept %q, want it to contain %q", kept, testCase.wantKept)
			}
			if !strings.HasPrefix(kept, "before") || !strings.HasSuffix(kept, "after") {
				t.Errorf("kept %q, want the surrounding text preserved", kept)
			}
		})
	}
}

func TestSanitizeKeepsMultibyteTextAndWideCharacters(t *testing.T) {
	frame := "\x1b[1;1Hhéllo 世界"
	report := termrender.Sanitize([]byte(frame), termrender.Policy{})
	if len(report.Removals) != 0 {
		t.Fatalf("removals = %v, want none", report.Removals)
	}
	if got := string(report.Kept); got != frame {
		t.Fatalf("kept %q, want the frame unchanged", got)
	}
}

func TestSanitizeHonoursACustomPolicy(t *testing.T) {
	policy := termrender.Policy{
		AllowCSI:        map[byte]bool{'H': true},
		AllowESC:        map[byte]bool{},
		AllowPrivateCSI: false,
	}
	frame := "\x1b[H\x1b[31mred\x1b[0m"
	report := termrender.Sanitize([]byte(frame), policy)
	if got, want := string(report.Kept), "\x1b[Hred"; got != want {
		t.Fatalf("kept %q, want %q (removals %v)", got, want, report.Removals)
	}

	// A private sequence is only allowed when the policy says so.
	private := termrender.Policy{AllowCSI: map[byte]bool{'h': true}, AllowPrivateCSI: true}
	report = termrender.Sanitize([]byte("\x1b[?1049h"), private)
	if len(report.Removals) != 0 || string(report.Kept) != "\x1b[?1049h" {
		t.Fatalf("kept %q, removals %v; want the private sequence allowed", report.Kept, report.Removals)
	}
}

func TestWidthMeasuresCellsNotBytes(t *testing.T) {
	for _, testCase := range []struct {
		text string
		want int
	}{
		{text: "abc", want: 3},
		{text: "世界", want: 4},
		{text: "\x1b[31mred\x1b[0m", want: 3},
		{text: "héllo", want: 5},
		{text: "", want: 0},
	} {
		if got := termrender.Width(testCase.text); got != testCase.want {
			t.Errorf("Width(%q) = %d, want %d", testCase.text, got, testCase.want)
		}
	}
}

func TestPlainTextRemovesSequences(t *testing.T) {
	if got, want := termrender.PlainText("\x1b[1;31merror\x1b[0m"), "error"; got != want {
		t.Fatalf("PlainText = %q, want %q", got, want)
	}
}

func TestSanitizeRejectsAnOversizedFrame(t *testing.T) {
	oversized := make([]byte, 1<<20+16)
	for i := range oversized {
		oversized[i] = 'x'
	}
	report := termrender.Sanitize(oversized, termrender.Policy{})
	if len(report.Kept) != 1<<20 {
		t.Fatalf("kept %d bytes, want the %d byte cap", len(report.Kept), 1<<20)
	}
	if len(report.Removals) != 1 || report.Removals[0].Kind != "frame-too-large" {
		t.Fatalf("removals = %v, want one frame-too-large", report.Removals)
	}
}
