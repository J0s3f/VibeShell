package input

import (
	"errors"
	"strings"
	"testing"
)

// testLimits returns small but usable bounds so a test can reach a limit
// without building a large input.
func testLimits() Limits {
	return Limits{
		MaxPasteBytes:  64,
		MaxSequenceLen: 8,
		MinCols:        1,
		MinRows:        1,
		MaxCols:        500,
		MaxRows:        500,
	}
}

func newTestDecoder(t *testing.T) *Decoder {
	t.Helper()
	decoder, err := NewDecoder(testLimits())
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	return decoder
}

// feedAll decodes every chunk and returns the combined events.
func feedAll(decoder *Decoder, chunks ...string) []Event {
	var events []Event
	for _, chunk := range chunks {
		events = append(events, decoder.Feed([]byte(chunk))...)
	}
	return events
}

// describe renders events as a compact string so a failure names the exact
// difference.
func describe(events []Event) string {
	parts := make([]string, 0, len(events))
	for _, event := range events {
		switch event.Kind {
		case KindKey:
			if event.Key == KeyRune {
				parts = append(parts, "rune("+string(event.Rune)+")")
				continue
			}
			parts = append(parts, "key:"+event.Key.String())
		case KindPaste:
			parts = append(parts, "paste("+strings.ReplaceAll(event.Text, "\n", "\\n")+")")
		case KindResize:
			parts = append(parts, "resize")
		case KindEOF:
			parts = append(parts, "eof")
		case KindRejected:
			parts = append(parts, "rejected("+string(event.Raw)+")")
		}
	}
	return strings.Join(parts, "")
}

func assertEvents(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("event %d = %s, want %s (all: %v)", i, got[i], want[i], got)
		}
	}
}

func TestDecoderDecodesText(t *testing.T) {
	decoder := newTestDecoder(t)
	events := feedAll(decoder, "ls -la")
	if len(events) != len("ls -la") {
		t.Fatalf("got %d events for 6 characters: %v", len(events), describe(events))
	}
	for i, event := range events {
		if event.Key != KeyRune || event.Rune != rune("ls -la"[i]) {
			t.Fatalf("event %d = %s, want rune %q", i, describe([]Event{event}), "ls -la"[i])
		}
	}
	if decoder.Pending() != 0 {
		t.Fatalf("Pending = %d, want 0 after complete input", decoder.Pending())
	}
}

func TestDecoderDecodesMultiByteRunesSplitAcrossReads(t *testing.T) {
	decoder := newTestDecoder(t)
	// The euro sign is three bytes; feed it one byte at a time.
	events := feedAll(decoder, "\xe2", "\x82", "\xac")
	if len(events) != 1 || events[0].Rune != '€' {
		t.Fatalf("events = %v, want one euro sign", describe(events))
	}
	if decoder.Pending() != 0 {
		t.Fatalf("Pending = %d after a complete rune", decoder.Pending())
	}
}

func TestDecoderReplacesInvalidUTF8(t *testing.T) {
	decoder := newTestDecoder(t)
	events := feedAll(decoder, "a\xffb")
	assertEvents(t, []string{describe([]Event{events[0]}), describe([]Event{events[1]}), describe([]Event{events[2]})},
		[]string{"rune(a)", "rune(�)", "rune(b)"})
}

func TestDecoderRejectsC1Runes(t *testing.T) {
	decoder := newTestDecoder(t)
	// U+009B is the 8-bit CSI introducer: it must never become a key press.
	events := feedAll(decoder, "\u009b")
	assertEvents(t, []string{describe(events)}, []string{"rejected(\u009b)"})
}

func TestDecoderDecodesControlKeys(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "tab", input: "\t", want: "key:tab"},
		{name: "enter as CR", input: "\r", want: "key:enter"},
		{name: "enter as LF", input: "\n", want: "key:enter"},
		{name: "CRLF is one enter", input: "\r\n", want: "key:enter"},
		{name: "backspace as DEL", input: "\x7f", want: "key:backspace"},
		{name: "backspace as BS", input: "\x08", want: "key:backspace"},
		{name: "ctrl-c", input: "\x03", want: "key:ctrl-c"},
		{name: "ctrl-d", input: "\x04", want: "key:ctrl-d"},
		{name: "ctrl-space", input: "\x00", want: ""},
		{name: "ctrl-underscore", input: "\x1f", want: "key:ctrl-underscore"},
		{name: "shift-tab", input: "\x1b[Z", want: "key:shift-tab"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decoder := newTestDecoder(t)
			assertEvents(t, []string{describe(feedAll(decoder, test.input))}, []string{test.want})
		})
	}
}

func TestDecoderReportsALoneEscapeAfterTheFlush(t *testing.T) {
	decoder := newTestDecoder(t)
	// A pressed Escape arrives as one byte with nothing after it, so nothing is
	// reported until the session flushes it.
	if events := feedAll(decoder, "\x1b"); len(events) != 0 {
		t.Fatalf("events = %v, want none before the flush", describe(events))
	}
	assertEvents(t, []string{describe(decoder.Escape())}, []string{"key:escape"})
	if events := decoder.Escape(); len(events) != 0 {
		t.Fatalf("events = %v on a second flush, want none", describe(events))
	}
}

func TestDecoderFlushDoesNotStealAnUnfinishedSequence(t *testing.T) {
	decoder := newTestDecoder(t)
	feedAll(decoder, "\x1b[1;")
	if events := decoder.Escape(); len(events) != 0 {
		t.Fatalf("events = %v, want the buffered sequence kept", describe(events))
	}
	if decoder.Pending() != 4 {
		t.Fatalf("Pending = %d, want the sequence still buffered", decoder.Pending())
	}
}

func TestDecoderDecodesEscapeSequences(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "arrow up", input: "\x1b[A", want: "key:up"},
		{name: "arrow down", input: "\x1b[B", want: "key:down"},
		{name: "arrow right", input: "\x1b[C", want: "key:right"},
		{name: "arrow left", input: "\x1b[D", want: "key:left"},
		{name: "modified arrow is accepted", input: "\x1b[1;5C", want: "key:right"},
		{name: "home", input: "\x1b[H", want: "key:home"},
		{name: "home numeric", input: "\x1b[1~", want: "key:home"},
		{name: "end numeric", input: "\x1b[4~", want: "key:end"},
		{name: "end letter", input: "\x1b[F", want: "key:end"},
		{name: "delete", input: "\x1b[3~", want: "key:delete"},
		{name: "insert", input: "\x1b[2~", want: "key:insert"},
		{name: "page up", input: "\x1b[5~", want: "key:page-up"},
		{name: "page down", input: "\x1b[6~", want: "key:page-down"},
		{name: "function key one", input: "\x1bOP", want: "key:f1"},
		{name: "function key four", input: "\x1b[14~", want: "key:f4"},
		{name: "ss3 arrow", input: "\x1bOD", want: "key:left"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decoder := newTestDecoder(t)
			assertEvents(t, []string{describe(feedAll(decoder, test.input))}, []string{test.want})
		})
	}
}

func TestDecoderRejectsUnsupportedSequences(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "device attributes", input: "\x1b[c", want: "rejected(\x1b[c)"},
		{name: "mouse report", input: "\x1b[<0;1;1M", want: "rejected(\x1b[<0;1;1M)"},
		{name: "intermediate bytes", input: "\x1b[1 q", want: "rejected(\x1b[1 q)"},
		{name: "unknown function key", input: "\x1b[15~", want: "rejected(\x1b[15~)"},
		{name: "escape aborts a sequence", input: "\x1b[1\x1b[A", want: "rejected(\x1b[1)key:up"},
		{name: "cancel aborts a sequence", input: "\x1b[1\x18x", want: "rejected(\x1b[1\x18)rune(x)"},
		{name: "intermediate escape sequence", input: "\x1b(Bx", want: "rejected(\x1b(B)rune(x)"},
		{name: "escape ends an intermediate sequence", input: "\x1b \x1b[A", want: "key:escaperune( )key:up"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decoder := newTestDecoder(t)
			assertEvents(t, []string{describe(feedAll(decoder, test.input))}, []string{test.want})
		})
	}
}

func TestDecoderDoesNotInsertAnAltModifiedCharacter(t *testing.T) {
	decoder := newTestDecoder(t)
	// Escape followed by a character is a modified key. The Escape key is
	// reported and the character keeps its own meaning, so a binding that uses
	// the Escape key sees it instead of losing it to a character the user
	// pressed together with Escape.
	assertEvents(t, []string{describe(feedAll(decoder, "\x1bx"))}, []string{"key:escaperune(x)"})
}

func TestDecoderReportsTwoEscapesSeparately(t *testing.T) {
	decoder := newTestDecoder(t)
	events := feedAll(decoder, "\x1b\x1b")
	assertEvents(t, []string{describe(events)}, []string{"key:escape"})
	if decoder.Pending() != 1 {
		t.Fatalf("Pending = %d, want the second escape still buffered", decoder.Pending())
	}
}

func TestDecoderRejectsAnIncompleteSequenceOverTheLimit(t *testing.T) {
	decoder := newTestDecoder(t)
	// A CSI whose final byte never arrives grows the buffer until the limit,
	// then is rejected, so a client cannot grow memory by never finishing.
	events := feedAll(decoder, "\x1b[1;2;3")
	if len(events) != 0 {
		t.Fatalf("events = %v, want none while the sequence is unfinished", describe(events))
	}
	if pending := decoder.Pending(); pending != 7 {
		t.Fatalf("Pending = %d, want the 7 bytes of the unfinished sequence", pending)
	}
	if events := feedAll(decoder, "45"); len(events) != 1 || events[0].Kind != KindRejected {
		t.Fatalf("events = %v, want the sequence rejected once it passed the limit", describe(events))
	}
	if decoder.Pending() != 0 {
		t.Fatalf("Pending = %d after a rejection, want 0", decoder.Pending())
	}
	// The decoder resynchronizes: ordinary input still works afterwards.
	assertEvents(t, []string{describe(feedAll(decoder, "ok"))}, []string{"rune(o)rune(k)"})
}

func TestDecoderDecodesTheSameEventsWhateverTheChunking(t *testing.T) {
	// A stream with text, keys, and sequences in it, split at every position.
	const stream = "ab\x1b[A\x1b[3~c\t\x1bOP\r\n\x7f\x1b[200~pasted\x1b[201~z"
	for split := 0; split <= len(stream); split++ {
		whole := newTestDecoder(t)
		chunked := newTestDecoder(t)
		want := describe(feedAll(whole, stream))
		got := describe(feedAll(chunked, stream[:split], stream[split:]))
		if got != want {
			t.Fatalf("split at %d: events = %v, want %v", split, got, want)
		}
		if whole.Pending() != chunked.Pending() {
			t.Fatalf("split at %d: pending = %d, want %d", split, chunked.Pending(), whole.Pending())
		}
	}
}

func TestDecoderDecodesBracketedPaste(t *testing.T) {
	decoder := newTestDecoder(t)
	events := feedAll(decoder, "\x1b[200~line one\nline two\x1b[201~")
	if len(events) != 1 || events[0].Kind != KindPaste {
		t.Fatalf("events = %v, want one paste", describe(events))
	}
	if events[0].Text != "line one\nline two" {
		t.Fatalf("paste text = %q, want two lines", events[0].Text)
	}
	if events[0].Truncated {
		t.Fatal("paste reported truncation although it is short")
	}
	if decoder.InPaste() {
		t.Fatal("decoder still reports an open paste after the end marker")
	}
}

func TestDecoderNormalizesPasteLineEndingsAndDropsControls(t *testing.T) {
	decoder := newTestDecoder(t)
	events := feedAll(decoder, "\x1b[200~a\r\nb\rc\x1b[31md\x00\x07e\x1b]0;title\x07\x1b[201~")
	if len(events) != 1 {
		t.Fatalf("events = %v, want one paste", describe(events))
	}
	if want := "a\nbc[31mde]0;title"; events[0].Text != want {
		// The control characters are removed and the printable characters around
		// them stay, which is what a user pasting a sequence expects to see: the
		// text, with the controls gone.
		t.Fatalf("paste text = %q, want %q", events[0].Text, want)
	}
	if strings.ContainsRune(events[0].Text, 0x1b) {
		t.Fatal("a pasted escape survived into the paste text")
	}
}

func TestDecoderDecodesPasteSplitAcrossReads(t *testing.T) {
	const stream = "\x1b[200~first\nsecond\x1b[201~after"
	for split := 0; split <= len(stream); split++ {
		decoder := newTestDecoder(t)
		events := feedAll(decoder, stream[:split], stream[split:])
		want := []string{"paste(first\\nsecond)", "rune(a)", "rune(f)", "rune(t)", "rune(e)", "rune(r)"}
		if got := []string{describe(events)}; !equal(got, []string{strings.Join(want, "")}) {
			t.Fatalf("split at %d: events = %v, want %v", split, describe(events), strings.Join(want, ""))
		}
	}
}

func TestDecoderTruncatesAnOversizedPaste(t *testing.T) {
	decoder := newTestDecoder(t)
	oversized := strings.Repeat("x", 200)
	events := feedAll(decoder, "\x1b[200~"+oversized+"\x1b[201~tail")
	if len(events) < 2 {
		t.Fatalf("events = %v, want a paste and the following text", describe(events))
	}
	paste := events[0]
	if paste.Kind != KindPaste || !paste.Truncated {
		t.Fatalf("first event = %v, want a truncated paste", describe([]Event{paste}))
	}
	if len(paste.Text) != decoder.Limits().MaxPasteBytes {
		t.Fatalf("paste kept %d bytes, want the %d byte limit", len(paste.Text), decoder.Limits().MaxPasteBytes)
	}
	// The decoder must resynchronize after a truncated paste.
	if got := describe(events[1:]); got != "rune(t)rune(a)rune(i)rune(l)" {
		t.Fatalf("events after the paste = %v, want the trailing text", got)
	}
}

func TestDecoderReportsAnUnfinishedPasteAtEOF(t *testing.T) {
	decoder := newTestDecoder(t)
	events := feedAll(decoder, "\x1b[200~half typed")
	if len(events) != 0 || !decoder.InPaste() {
		t.Fatalf("events = %v, want an open paste with no events", describe(events))
	}
	events = append(events, decoder.EOF()...)
	if got := describe(events); got != "paste(half typed)eof" {
		t.Fatalf("EOF events = %v, want the paste content and then EOF", got)
	}
}

func TestDecoderReportsBufferedBytesAtEOF(t *testing.T) {
	decoder := newTestDecoder(t)
	feedAll(decoder, "\x1b[1;")
	events := decoder.EOF()
	if got := describe(events); got != "rejected(\x1b[1;)eof" {
		t.Fatalf("EOF events = %v, want the buffered bytes and then EOF", got)
	}
	if decoder.Pending() != 0 {
		t.Fatalf("Pending = %d after EOF, want 0", decoder.Pending())
	}
}

func TestDecoderAcceptsResizeWithinBounds(t *testing.T) {
	decoder := newTestDecoder(t)
	events, err := decoder.Resize(120, 40)
	if err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if got := describe(events); got != "resize" {
		t.Fatalf("events = %v, want a resize", got)
	}
	if cols, rows := decoder.Size(); cols != 120 || rows != 40 {
		t.Fatalf("Size = %dx%d, want 120x40", cols, rows)
	}
}

func TestDecoderRefusesResizeOutsideBounds(t *testing.T) {
	decoder := newTestDecoder(t)
	if _, err := decoder.Resize(120, 40); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	for _, size := range [][2]int{{0, 40}, {40, 0}, {-1, 40}, {501, 40}, {40, 501}} {
		events, err := decoder.Resize(size[0], size[1])
		if !errors.Is(err, ErrSizeOutOfRange) {
			t.Fatalf("Resize(%d, %d) error = %v, want ErrSizeOutOfRange", size[0], size[1], err)
		}
		if len(events) != 0 {
			t.Fatalf("Resize(%d, %d) returned %v, want no events", size[0], size[1], describe(events))
		}
		if cols, rows := decoder.Size(); cols != 120 || rows != 40 {
			t.Fatalf("Size = %dx%d after a refused resize, want the previous size", cols, rows)
		}
	}
}

func TestNewDecoderRejectsUnboundedLimits(t *testing.T) {
	for name, mutate := range map[string]func(*Limits){
		"paste":    func(l *Limits) { l.MaxPasteBytes = 0 },
		"sequence": func(l *Limits) { l.MaxSequenceLen = -1 },
		"columns":  func(l *Limits) { l.MinCols = 0 },
		"rows":     func(l *Limits) { l.MaxRows = 0 },
		"order":    func(l *Limits) { l.MinCols, l.MaxCols = 100, 10 },
	} {
		t.Run(name, func(t *testing.T) {
			limits := testLimits()
			mutate(&limits)
			if _, err := NewDecoder(limits); !errors.Is(err, ErrInvalidLimits) {
				t.Fatalf("NewDecoder error = %v, want ErrInvalidLimits", err)
			}
		})
	}
}

func TestKeyNamesRoundTrip(t *testing.T) {
	keys := []Key{
		KeyRune, KeyEnter, KeyTab, KeyShiftTab, KeyBackspace, KeyDelete, KeyEscape,
		KeyInsert, KeyUp, KeyDown, KeyLeft, KeyRight, KeyHome, KeyEnd,
		KeyPageUp, KeyPageDown, KeyF1, KeyF2, KeyF3, KeyF4,
		KeyCtrlA, KeyCtrlC, KeyCtrlD, KeyCtrlSpace, KeyCtrlBackslash,
		KeyCtrlBracketClose, KeyCtrlCaret, KeyCtrlUnderscore,
	}
	for _, key := range keys {
		parsed, err := ParseKey(key.String())
		if err != nil {
			t.Fatalf("ParseKey(%q): %v", key.String(), err)
		}
		if parsed != key {
			t.Fatalf("ParseKey(%q) = %v, want %v", key.String(), parsed, key)
		}
	}
}

func TestParseKeyAcceptsAnyCaseButRejectsUnknownNames(t *testing.T) {
	key, err := ParseKey("CTRL-Q")
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	if key != KeyCtrlQ {
		t.Fatalf("ParseKey = %v, want KeyCtrlQ", key)
	}
	for _, name := range []string{"", "hyperlink", "ctrl-1", "ctrl-"} {
		if _, err := ParseKey(name); err == nil {
			t.Fatalf("ParseKey(%q) accepted an unknown key", name)
		}
	}
}

func TestKeyControlLetters(t *testing.T) {
	for r := rune('a'); r <= 'z'; r++ {
		key := KeyCtrlA + Key(r-'a')
		if !key.IsCtrl() {
			t.Fatalf("%v is not reported as a control key", key)
		}
		letter, ok := key.CtrlLetter()
		if !ok || letter != r {
			t.Fatalf("CtrlLetter(%v) = %q, %t, want %q", key, letter, ok, r)
		}
		if got := key.String(); got != "ctrl-"+string(r) {
			t.Fatalf("String(%v) = %q, want ctrl-%q", key, got, r)
		}
	}
	if letter, ok := KeyCtrlSpace.CtrlLetter(); !ok || letter != ' ' {
		t.Fatal("KeyCtrlSpace must stand for a space")
	}
	if KeyUp.IsCtrl() || KeyRune.IsCtrl() {
		t.Fatal("only the control block may report IsCtrl")
	}
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
