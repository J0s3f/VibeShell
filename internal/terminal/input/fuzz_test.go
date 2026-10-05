package input

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// fuzzLimits keeps every bound far above the size of a fuzz input, so the
// chunking property below cannot be broken by the bounds themselves; the bounds
// are covered by their own tests.
func fuzzLimits() Limits {
	return Limits{
		MaxPasteBytes:  1 << 20,
		MaxSequenceLen: 1 << 20,
		MinCols:        1,
		MinRows:        1,
		MaxCols:        1000,
		MaxRows:        1000,
	}
}

// FuzzDecoderFeed checks the two properties a byte-stream decoder must hold for
// any input at all: the events never depend on how the client chunked its
// writes, and every event is something the rest of the application can act on.
//
// The fuzz input is a split point followed by the bytes, so the same input
// checks both the decoder and the chunking around it.
func FuzzDecoderFeed(f *testing.F) {
	f.Add(3, "a\x1b[A\x1b[200~paste\x1b[201~b\x7f\x1b")
	f.Add(0, "")
	f.Add(1, "\x1b[1;")
	f.Add(2, "\xe2\x82")
	f.Add(len("hello"), "hello\r\n世界")
	f.Add(4, "\x1b]52;c;x\x07")

	decoder := func(t *testing.T) *Decoder {
		decoder, err := NewDecoder(fuzzLimits())
		if err != nil {
			t.Fatalf("NewDecoder: %v", err)
		}
		return decoder
	}

	f.Fuzz(func(t *testing.T, split int, input string) {
		if split < 0 || split > len(input) {
			t.Skip()
		}
		whole := decoder(t)
		chunked := decoder(t)

		want := append(feedAll(whole, input), whole.EOF()...)
		got := append(feedAll(chunked, input[:split], input[split:]), chunked.EOF()...)

		if describe(got) != describe(want) {
			t.Fatalf("split at %d changed the events:\n got %v\nwant %v", split, describe(got), describe(want))
		}
		if whole.Pending() != chunked.Pending() {
			t.Fatalf("split at %d left %d buffered bytes, want %d", split, chunked.Pending(), whole.Pending())
		}
		if whole.InPaste() != chunked.InPaste() {
			t.Fatalf("split at %d disagreed about an open paste", split)
		}
		for _, event := range got {
			assertEventIsActionable(t, event)
		}
	})
}

// assertEventIsActionable checks the invariants that hold for every event the
// decoder can produce, whatever the bytes were.
func assertEventIsActionable(t *testing.T, event Event) {
	t.Helper()
	switch event.Kind {
	case KindKey:
		if event.Key == KeyRune {
			// A printable rune, never a control character: the decoder turns an
			// invalid byte into U+FFFD and refuses the C1 controls that would
			// otherwise start a sequence in the 8-bit form.
			if event.Rune < 0x20 || event.Rune == deleteByte || (event.Rune >= 0x80 && event.Rune <= 0x9f) {
				t.Fatalf("key event carries the control rune %U", event.Rune)
			}
			return
		}
		if event.Key <= KeyUnknown || event.Key >= keyCtrlLast {
			t.Fatalf("key event carries the key %v", event.Key)
		}
	case KindPaste:
		if !utf8.ValidString(event.Text) {
			t.Fatalf("paste text is not valid UTF-8: %q", event.Text)
		}
		for _, r := range event.Text {
			switch {
			case r == '\t' || r == '\n':
			case r < 0x20, r == deleteByte, r >= 0x80 && r <= 0x9f:
				t.Fatalf("paste text carries the control rune %U", r)
			}
		}
	case KindResize:
		if event.Cols <= 0 || event.Rows <= 0 {
			t.Fatalf("resize event = %dx%d", event.Cols, event.Rows)
		}
	case KindEOF, KindRejected:
	default:
		t.Fatalf("unexpected event kind %v", event.Kind)
	}
}

// FuzzDecodePaste checks that a paste always terminates, reports its content
// once, and leaves the decoder able to read what follows, however the paste and
// its end marker are split.
func FuzzDecodePaste(f *testing.F) {
	f.Add("content", 1)
	f.Add("multi\nline\r\npaste", 6)
	f.Add("\x1b[201~nested\x1b[200~", 3)
	f.Add("", 2)
	f.Add("世界\x00\x07", 4)

	f.Fuzz(func(t *testing.T, content string, split int) {
		if split < 0 || split > len(content) {
			t.Skip()
		}
		stream := pasteStart + content + pasteEnd + "after"
		decoder, err := NewDecoder(fuzzLimits())
		if err != nil {
			t.Fatalf("NewDecoder: %v", err)
		}
		events := append(
			feedAll(decoder, stream[:split], stream[split:]),
			decoder.EOF()...,
		)
		if decoder.InPaste() {
			t.Fatalf("the decoder still holds a paste open: %v", describe(events))
		}
		var pastes int
		for _, event := range events {
			assertEventIsActionable(t, event)
			if event.Kind == KindPaste {
				pastes++
			}
		}
		// Content may itself contain the markers, which ends the paste early and
		// can open another one, so the count is not fixed; what must hold is
		// that the stream ends in a state where no paste is open.
		if pastes == 0 {
			t.Fatalf("got no paste event at all: %v", describe(events))
		}
		if got := describe(events); !strings.HasSuffix(got, "rune(a)rune(f)rune(t)rune(e)rune(r)eof") {
			t.Fatalf("events = %v, want the text after the paste decoded", got)
		}
	})
}
