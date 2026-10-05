package termdecode_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"j0s.at/vibeshell/experiments/ssh-terminal/internal/termdecode"
)

// collector records decoded events for assertions.
type collector struct {
	events []termdecode.Event
}

func (c *collector) emit(event termdecode.Event) { c.events = append(c.events, event) }

// decode feeds input through one decoder, optionally one byte per write, and
// returns the collected events.
func decode(t *testing.T, bounds termdecode.Bounds, perByte bool, chunks ...string) []termdecode.Event {
	t.Helper()
	collector := &collector{}
	decoder := termdecode.NewDecoder(bounds, collector.emit)
	for _, chunk := range chunks {
		if perByte {
			for i := 0; i < len(chunk); i++ {
				if _, err := decoder.Write([]byte{chunk[i]}); err != nil {
					t.Fatalf("write byte %q: %v", chunk[i], err)
				}
			}
			continue
		}
		if _, err := decoder.Write([]byte(chunk)); err != nil {
			t.Fatalf("write %q: %v", chunk, err)
		}
	}
	decoder.Flush()
	return collector.events
}

// describe renders events the way the receipts and failures read best.
func describe(events []termdecode.Event) string {
	parts := make([]string, 0, len(events))
	for _, event := range events {
		switch typed := event.(type) {
		case termdecode.Key:
			parts = append(parts, "key("+typed.String()+")")
		case termdecode.Paste:
			parts = append(parts, fmt.Sprintf("paste(%q,truncated=%t)", typed.Text, typed.Truncated))
		case termdecode.Resize:
			parts = append(parts, fmt.Sprintf("resize(%dx%d)", typed.Cols, typed.Rows))
		default:
			parts = append(parts, fmt.Sprintf("%T", event))
		}
	}
	return strings.Join(parts, " ")
}

func requireEvents(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
}

func TestDecodeRunesAndUTF8(t *testing.T) {
	// "héllo" split so that the two bytes of é arrive in separate writes, the
	// way SSH can deliver them.
	events := decode(t, termdecode.DefaultBounds, false, "h\xc3", "\xa9llo")
	requireEvents(t, describe(events), "key(rune h) key(rune é) key(rune l) key(rune l) key(rune o)")
}

func TestDecodeInvalidUTF8BecomesMalformed(t *testing.T) {
	events := decode(t, termdecode.DefaultBounds, false, "a\xffb")
	requireEvents(t, describe(events), "key(rune a) key(malformed) key(rune b)")
}

func TestDecodeControlKeys(t *testing.T) {
	// 0x03 is how a client in a pty session delivers Ctrl-C, and 0x04 is
	// Ctrl-D. Neither is an SSH signal.
	events := decode(t, termdecode.DefaultBounds, false, "\x03\x04\r\n\t\x7f\x15\x17")
	want := "key(ctrl+c) key(ctrl+d) key(enter) key(enter) key(tab) " +
		"key(backspace) key(ctrl+u) key(ctrl+w)"
	requireEvents(t, describe(events), want)
}

func TestDecodeEscapeSequencesFragmentedOneByteAtATime(t *testing.T) {
	input := "ls\x1b[A\x1b[1;2A\x1bOB\x1b[3~\x1b[1;5D"
	want := "key(rune l) key(rune s) key(up) key(shift+up) key(down) " +
		"key(delete) key(ctrl+left)"
	requireEvents(t, describe(decode(t, termdecode.DefaultBounds, true, input)), want)
}

func TestDecodeEditKeys(t *testing.T) {
	events := decode(t, termdecode.DefaultBounds, false,
		"\x1b[H\x1b[F\x1b[1~\x1b[4~\x1b[5~\x1b[6~\x1b[2~\x1b[Z")
	want := "key(home) key(end) key(home) key(end) key(page-up) key(page-down) " +
		"key(insert) key(shift+tab)"
	requireEvents(t, describe(events), want)
}

func TestDecodeBareEscapeNeedsAFlush(t *testing.T) {
	collector := &collector{}
	decoder := termdecode.NewDecoder(termdecode.DefaultBounds, collector.emit)
	if _, err := decoder.Write([]byte("a\x1b")); err != nil {
		t.Fatalf("write: %v", err)
	}
	requireEvents(t, describe(collector.events), "key(rune a)")
	decoder.Flush()
	requireEvents(t, describe(collector.events), "key(rune a) key(escape)")
}

func TestDecodeBracketedPasteSplitAcrossWrites(t *testing.T) {
	events := decode(t, termdecode.DefaultBounds, true, "\x1b[200~line one\nline two\x1b[201~")
	requireEvents(t, describe(events), `paste("line one\nline two",truncated=false)`)
}

func TestDecodeBracketedPasteIsBounded(t *testing.T) {
	const limit = 64
	bounds := termdecode.Bounds{MaxPasteBytes: limit, MaxPendingBytes: 256}
	payload := strings.Repeat("x", 500)
	events := decode(t, bounds, false, "\x1b[200~"+payload+"\x1b[201~")

	pastes := 0
	overflows := 0
	kept := 0
	for _, event := range events {
		switch typed := event.(type) {
		case termdecode.Paste:
			pastes++
			kept = len(typed.Text)
			if !typed.Truncated {
				t.Fatalf("paste reported as complete: %d bytes kept", kept)
			}
		case termdecode.Key:
			if typed.Name == termdecode.KeyPasteOverflow {
				overflows++
			}
		}
	}
	if pastes != 1 {
		t.Fatalf("paste events = %d, want 1", pastes)
	}
	if overflows != 1 {
		t.Fatalf("paste overflow keys = %d, want 1", overflows)
	}
	if kept != limit {
		t.Fatalf("kept paste bytes = %d, want %d", kept, limit)
	}
}

func TestDecodeUnterminatedEscapeIsBounded(t *testing.T) {
	bounds := termdecode.Bounds{MaxPasteBytes: 64, MaxPendingBytes: 16}
	// A client that sends ESC [ and then digits forever must not make the
	// server buffer them. Once the bound is exceeded the decoder reports one
	// malformed sequence and then treats the remaining bytes as literal input,
	// which is what a real terminal does when its escape timeout expires.
	input := "\x1b[" + strings.Repeat("1", 500)
	events := decode(t, bounds, false, input)

	if len(events) != len(input) {
		t.Fatalf("decoder produced %d events for %d input bytes; some bytes stayed buffered",
			len(events), len(input))
	}
	malformed := 0
	for _, event := range events {
		key, ok := event.(termdecode.Key)
		if ok && key.Name == termdecode.KeyMalformed {
			malformed++
		}
	}
	if malformed != 1 {
		t.Fatalf("malformed keys = %d, want 1", malformed)
	}
}

func TestDecodeInBandResizeNotification(t *testing.T) {
	// xterm's in-band resize notification is how a client that is not SSH
	// reports a new size; the SSH path delivers the same event from a
	// window-change request.
	events := decode(t, termdecode.DefaultBounds, false, "\x1b[8;40;132t")
	requireEvents(t, describe(events), "resize(132x40)")
}

func TestDecodeInjectedResizeNeedsNoBytes(t *testing.T) {
	collector := &collector{}
	decoder := termdecode.NewDecoder(termdecode.DefaultBounds, collector.emit)
	decoder.InjectResize(100, 30)
	requireEvents(t, describe(collector.events), "resize(100x30)")
}

func TestDecodeUnterminatedPasteIsEmittedOnFlush(t *testing.T) {
	collector := &collector{}
	decoder := termdecode.NewDecoder(termdecode.DefaultBounds, collector.emit)
	if _, err := decoder.Write([]byte("\x1b[200~partial")); err != nil {
		t.Fatalf("write: %v", err)
	}
	decoder.Flush()
	requireEvents(t, describe(collector.events), `paste("partial",truncated=false)`)
}

func TestDecodeUnknownFinalByteIsUnknownNotMalformed(t *testing.T) {
	events := decode(t, termdecode.DefaultBounds, false, "\x1b[<0;1;1M")
	requireEvents(t, describe(events), "key(unknown)")
}

func FuzzDecoderStaysInsideItsBounds(f *testing.F) {
	f.Add("echo \x1b[A hi\x1b[200~paste\x1b[201~")
	f.Add("\x03\x04\x1b")
	f.Add("\x1b[8;24;80t")
	f.Add("\x1b[200~unterminated")

	bounds := termdecode.Bounds{MaxPasteBytes: 32, MaxPendingBytes: 32}
	f.Fuzz(func(t *testing.T, input string) {
		events := 0
		decoder := termdecode.NewDecoder(bounds, func(termdecode.Event) { events++ })
		for i := 0; i < len(input); i++ {
			if _, err := decoder.Write([]byte{input[i]}); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
		decoder.Flush()
		if events > len(input) {
			t.Fatalf("decoder produced %d events for %d bytes", events, len(input))
		}
	})
}

func TestBoundedPasteEndsOnCharacterBoundary(t *testing.T) {
	// The decoder keeps bytes, not runes, so a paste cut at its byte limit must
	// drop the half character that the limit landed in. What is kept has to be
	// text a screen can show.
	bounds := termdecode.Bounds{MaxPasteBytes: 3, MaxPendingBytes: 64}
	events := decode(t, bounds, false, "\x1b[200~\xc3\xa9\xc3\xa9\x1b[201~")
	paste, ok := events[len(events)-1].(termdecode.Paste)
	if !ok {
		t.Fatalf("last event is %T, want a paste", events[len(events)-1])
	}
	if !paste.Truncated {
		t.Fatalf("paste over its byte limit reported as complete: %q", paste.Text)
	}
	if !bytes.Equal([]byte(paste.Text), []byte("\xc3\xa9")) {
		t.Fatalf("paste = %q, want one whole character", paste.Text)
	}
}
