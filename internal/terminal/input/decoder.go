package input

import (
	"bytes"
	"fmt"
	"unicode/utf8"
)

// cancelByte and subByte abort a control sequence in progress.
const (
	cancelByte = 0x18
	subByte    = 0x1a
)

// Decoder turns client bytes into events. It is not safe for concurrent use;
// a session feeds it from a single reader.
type Decoder struct {
	limits  Limits
	pending []byte // undecided tail: a partial rune or an unfinished sequence
	paste   pasteState
	cols    int
	rows    int
	// afterCR remembers that the last complete unit was a carriage return, so
	// the line feed of a CRLF pair does not submit a second empty line.
	afterCR bool
}

// NewDecoder returns a decoder with the given bounds. The terminal size starts
// unknown and is reported by Size as zero until the first accepted resize.
func NewDecoder(limits Limits) (*Decoder, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	return &Decoder{limits: limits}, nil
}

// Limits returns the bounds this decoder enforces.
func (d *Decoder) Limits() Limits { return d.limits }

// Size returns the last accepted terminal size, or (0, 0) before any resize.
func (d *Decoder) Size() (cols, rows int) { return d.cols, d.rows }

// Pending returns how many buffered bytes belong to an unfinished rune or
// escape sequence. A caller that stops feeding the stream can report it as the
// cost a half-typed key sequence holds.
func (d *Decoder) Pending() int { return len(d.pending) }

// InPaste reports whether the decoder is inside a bracketed paste.
func (d *Decoder) InPaste() bool { return d.paste.active }

// Escape reports a buffered lone escape byte as the Escape key. A client that
// presses Escape sends one byte and waits, which the stream cannot tell apart
// from the start of a sequence that has not arrived yet, so the session calls
// this once its read timeout shows no sequence is coming. It reports nothing
// when the buffered bytes are not exactly one escape byte.
func (d *Decoder) Escape() []Event {
	if len(d.pending) != 1 || d.pending[0] != escapeByte {
		return nil
	}
	d.pending = d.pending[:0]
	d.afterCR = false
	return []Event{NamedKeyEvent(KeyEscape)}
}

// Resize accepts a new terminal size and returns the event that tells the rest
// of the session about it. A size outside the configured bounds is refused
// without changing the recorded size, because a client must not be able to
// shrink the session below a usable size or inflate it without limit.
func (d *Decoder) Resize(cols, rows int) ([]Event, error) {
	switch {
	case cols < d.limits.MinCols || cols > d.limits.MaxCols:
		return nil, sizeError("columns", cols, d.limits.MinCols, d.limits.MaxCols)
	case rows < d.limits.MinRows || rows > d.limits.MaxRows:
		return nil, sizeError("rows", rows, d.limits.MinRows, d.limits.MaxRows)
	}
	d.cols, d.rows = cols, rows
	return []Event{ResizeEvent(cols, rows)}, nil
}

// EOF ends the stream. An unfinished escape sequence is rejected with its
// buffered bytes and an unfinished paste is reported as the content received
// so far, so a session records the anomaly instead of silently dropping it. A
// final KindEOF event marks the end of input.
func (d *Decoder) EOF() []Event {
	events := make([]Event, 0, 2)
	d.afterCR = false
	if len(d.pending) > 0 {
		events = append(events, RejectedEvent(append([]byte(nil), d.pending...)))
		d.pending = d.pending[:0]
	}
	if d.paste.active {
		events = append(events, d.paste.finish()...)
	}
	return append(events, EOFEvent())
}

// Feed decodes p and returns every event it completes. Bytes that form an
// unfinished rune or escape sequence are buffered for the next call, so the
// result never depends on how the client chunked its writes.
func (d *Decoder) Feed(p []byte) []Event {
	if len(p) == 0 {
		return nil
	}
	var events []Event
	buf := make([]byte, 0, len(d.pending)+len(p))
	buf = append(buf, d.pending...)
	buf = append(buf, p...)
	d.pending = d.pending[:0]

	consumed := 0
	for consumed < len(buf) {
		if d.paste.active {
			// The rest of the buffer is literal content until the end marker,
			// which may fall in the middle of the buffer.
			used, pasteEvents := d.consumePaste(buf[consumed:])
			events = append(events, pasteEvents...)
			consumed += used
			continue
		}
		if d.afterCR && buf[consumed] == 0x0a {
			// The line feed that completes a CRLF pair belongs to the Enter the
			// carriage return already produced.
			d.afterCR = false
			consumed++
			continue
		}
		next := d.nextUnit(buf[consumed:])
		if !next.complete {
			break
		}
		d.afterCR = next.consumed == 1 && buf[consumed] == 0x0d
		consumed += next.consumed
		if next.event != nil {
			events = append(events, *next.event)
		}
		if next.startsPaste {
			d.paste.start(d.limits.MaxPasteBytes)
		}
	}

	d.pending = append(d.pending, buf[consumed:]...)
	if len(d.pending) > d.limits.MaxSequenceLen {
		// The client keeps sending bytes that will never complete a sequence.
		// Reject them now instead of letting the buffer grow with the stream.
		events = append(events, RejectedEvent(append([]byte(nil), d.pending...)))
		d.pending = d.pending[:0]
	}
	return events
}

// unit is the result of decoding one self-contained piece of the stream.
type unit struct {
	// consumed is how many bytes the unit used; zero when complete is false.
	consumed int
	complete bool
	// event is the decoded event, or nil when the bytes carried none, such as
	// an ignored NUL.
	event *Event
	// startsPaste reports that these bytes opened a bracketed paste.
	startsPaste bool
}

func namedUnit(consumed int, key Key) unit {
	if key == KeyUnknown {
		return unit{consumed: consumed, complete: true}
	}
	event := NamedKeyEvent(key)
	return unit{consumed: consumed, complete: true, event: &event}
}

func rejectedUnit(consumed int, raw []byte) unit {
	event := RejectedEvent(raw)
	return unit{consumed: consumed, complete: true, event: &event}
}

func runeUnit(r rune, size int) unit {
	event := KeyEvent(r)
	return unit{consumed: size, complete: true, event: &event}
}

// nextUnit decodes the first complete unit of b, or reports an incomplete one.
func (d *Decoder) nextUnit(b []byte) unit {
	switch c := b[0]; {
	case c == escapeByte:
		return d.nextEscape(b)
	case c < 0x20 || c == deleteByte:
		return controlUnit(c)
	case c < utf8.RuneSelf:
		return runeUnit(rune(c), 1)
	default:
		return d.nextRune(b)
	}
}

// nextEscape decodes a byte sequence introduced by ESC.
func (d *Decoder) nextEscape(b []byte) unit {
	if len(b) == 1 {
		// A lone ESC may be the Escape key or the start of a sequence that the
		// next read completes.
		return unit{}
	}
	switch second := b[1]; {
	case second == escapeByte:
		// ESC ESC: the first Escape is complete; the second starts its own unit.
		return namedUnit(1, KeyEscape)
	case second == '[':
		return d.nextCSI(b)
	case second == 'O':
		if len(b) < 3 {
			return unit{}
		}
		if key := ss3Key(b[2]); key != KeyUnknown {
			return namedUnit(3, key)
		}
		return rejectedUnit(3, append([]byte(nil), b[:3]...))
	case second >= 0x20 && second <= 0x2f:
		return nextEscapedSequence(b)
	default:
		// A control byte directly after ESC is a separate key press, and so is
		// a plain character: ESC x is Alt-x, which no approved action uses, so
		// the Escape key is reported and the character keeps its own meaning.
		return namedUnit(1, KeyEscape)
	}
}

// nextEscapedSequence decodes a sequence whose parameters are intermediate
// bytes, such as the character-set selection a terminal sends before it knows
// what it will use. No approved sequence has a final byte in this shape, so the
// sequence is rejected once it is complete and abandoned when another introducer
// arrives.
func nextEscapedSequence(b []byte) unit {
	for i := 1; i < len(b); i++ {
		switch c := b[i]; {
		case c >= 0x20 && c <= 0x2f:
			continue
		case c >= 0x30 && c <= 0x7e:
			return rejectedUnit(i+1, append([]byte(nil), b[:i+1]...))
		default:
			// ESC, CAN, SUB, or a control byte ends the sequence without a
			// final byte. Only the introducer is consumed, so whatever follows
			// is decoded on its own.
			return namedUnit(1, KeyEscape)
		}
	}
	return unit{}
}

// nextCSI decodes a control sequence of the form ESC [ ... final, where the
// final byte is in the range 0x40 to 0x7e.
func (d *Decoder) nextCSI(b []byte) unit {
	for i := 2; i < len(b); i++ {
		c := b[i]
		switch {
		case c >= 0x40 && c <= 0x7e:
			seq := b[:i+1]
			if string(seq) == pasteStart {
				return unit{consumed: len(seq), complete: true, startsPaste: true}
			}
			return csiUnit(seq)
		case c == escapeByte:
			// An escape aborts the sequence it interrupts, and is then decoded
			// again as the start of the next one, which is what lets a client
			// recover without resynchronizing by hand.
			return rejectedUnit(i, append([]byte(nil), b[:i]...))
		case c == cancelByte || c == subByte:
			// CAN and SUB are the abort characters and belong to the sequence.
			return rejectedUnit(i+1, append([]byte(nil), b[:i+1]...))
		}
	}
	return unit{}
}

// csiUnit decodes one control sequence including its introducer. No approved
// sequence carries intermediate bytes, so a sequence that does is rejected
// rather than guessed at.
func csiUnit(seq []byte) unit {
	final := seq[len(seq)-1]
	params := seq[2 : len(seq)-1]
	for _, c := range params {
		if c >= 0x20 && c <= 0x2f {
			return rejectedUnit(len(seq), append([]byte(nil), seq...))
		}
	}
	if key := csiKey(final, parseParams(params)); key != KeyUnknown {
		return namedUnit(len(seq), key)
	}
	return rejectedUnit(len(seq), append([]byte(nil), seq...))
}

// nextRune decodes one UTF-8 rune, keeping an unfinished encoding for the next
// read and turning an invalid byte into U+FFFD so the user sees the break
// instead of losing input silently.
func (d *Decoder) nextRune(b []byte) unit {
	r, size := utf8.DecodeRune(b)
	if r == utf8.RuneError && size <= 1 {
		if incompleteRune(b) {
			return unit{}
		}
		return runeUnit(utf8.RuneError, 1)
	}
	if r >= 0x80 && r <= 0x9f {
		// A C1 control character typed as UTF-8 is the 8-bit form of the
		// introducer that starts a control sequence, so it is refused as input
		// rather than carried into a command line.
		return rejectedUnit(size, append([]byte(nil), b[:size]...))
	}
	return runeUnit(r, size)
}

// controlUnit maps one C0 control byte or DEL to a key press.
func controlUnit(c byte) unit {
	switch c {
	case nulByte:
		// A real terminal ignores NUL; reporting it as input would put a
		// control character into a command line.
		return unit{consumed: 1, complete: true}
	case 0x08:
		// BS, which clients send instead of DEL.
		return namedUnit(1, KeyBackspace)
	case deleteByte:
		return namedUnit(1, KeyBackspace)
	case 0x09:
		return namedUnit(1, KeyTab)
	case 0x0a, 0x0d:
		// LF and CR are both Enter. A CRLF pair is one Enter: Feed suppresses the
		// line feed that follows a carriage return, so a client that sends line
		// endings does not submit a second, empty line.
		return namedUnit(1, KeyEnter)
	}
	if c >= 0x01 && c <= 0x1a {
		return namedUnit(1, KeyCtrlA+Key(c-0x01))
	}
	switch c {
	case 0x1c:
		return namedUnit(1, KeyCtrlBackslash)
	case 0x1d:
		return namedUnit(1, KeyCtrlBracketClose)
	case 0x1e:
		return namedUnit(1, KeyCtrlCaret)
	case 0x1f:
		return namedUnit(1, KeyCtrlUnderscore)
	}
	// The remaining controls (VT, FF, and the unused code points) are layout
	// artefacts of other programs and never valid input.
	return unit{consumed: 1, complete: true}
}

// csiKey maps the final byte of a control sequence to its key. A modifier
// parameter is accepted and ignored for the navigation keys: the approved
// actions are not modifier-sensitive, and refusing the sequence would make
// ctrl-arrow unusable.
func csiKey(final byte, params []int) Key {
	switch final {
	case 'A':
		return KeyUp
	case 'B':
		return KeyDown
	case 'C':
		return KeyRight
	case 'D':
		return KeyLeft
	case 'H':
		return KeyHome
	case 'F':
		return KeyEnd
	case 'Z':
		return KeyShiftTab
	case '~':
		return tildeKey(params)
	}
	return KeyUnknown
}

// tildeKey maps the numeric parameter of a CSI ... ~ sequence.
func tildeKey(params []int) Key {
	if len(params) == 0 {
		return KeyUnknown
	}
	switch params[0] {
	case 1, 7:
		return KeyHome
	case 2:
		return KeyInsert
	case 3:
		return KeyDelete
	case 4, 8:
		return KeyEnd
	case 5:
		return KeyPageUp
	case 6:
		return KeyPageDown
	case 11, 12, 13, 14:
		return KeyF1 + Key(params[0]-11)
	}
	return KeyUnknown
}

// ss3Key maps the final byte of an SS3 sequence, which clients use for the
// arrows and the first function keys in application cursor mode.
func ss3Key(final byte) Key {
	switch final {
	case 'A':
		return KeyUp
	case 'B':
		return KeyDown
	case 'C':
		return KeyRight
	case 'D':
		return KeyLeft
	case 'H':
		return KeyHome
	case 'F':
		return KeyEnd
	case 'P', 'Q', 'R', 'S':
		return KeyF1 + Key(final-'P')
	}
	return KeyUnknown
}

// parseParams extracts the numeric parameters of a control sequence. A missing
// parameter reads as 1, the way xterm interprets "CSI A" as the up arrow.
func parseParams(b []byte) []int {
	var params []int
	current := -1
	for _, c := range b {
		switch {
		case c >= '0' && c <= '9':
			if current < 0 {
				current = 0
			}
			if current < 1_000_000 {
				current = current*10 + int(c-'0')
			}
		case c == ';' || c == ':':
			if current >= 0 {
				params = append(params, current)
			}
			current = -1
		}
	}
	if current >= 0 {
		params = append(params, current)
	}
	for i, p := range params {
		if p == 0 {
			params[i] = 1
		}
	}
	return params
}

// runeEncodingLen returns how many bytes the UTF-8 encoding of a rune starting
// with c occupies, or 0 when c cannot start a valid encoding.
func runeEncodingLen(c byte) int {
	switch {
	case c < 0x80:
		return 1
	case c >= 0xc2 && c <= 0xdf:
		return 2
	case c >= 0xe0 && c <= 0xef:
		return 3
	case c >= 0xf0 && c <= 0xf4:
		return 4
	}
	return 0
}

// incompleteRune reports whether b is a valid but unfinished UTF-8 encoding.
func incompleteRune(b []byte) bool {
	n := runeEncodingLen(b[0])
	if n <= 1 || len(b) >= n {
		return false
	}
	for _, c := range b[1:] {
		if c < 0x80 || c > 0xbf {
			return false
		}
	}
	return true
}

// markerOverlapLen returns the length of the longest suffix of b that is also
// a prefix of marker, so an end marker split across two reads is still found.
func markerOverlapLen(b []byte, marker string) int {
	limit := min(len(marker)-1, len(b))
	for n := limit; n > 0; n-- {
		if string(b[len(b)-n:]) == marker[:n] {
			return n
		}
	}
	return 0
}

// incompleteTail returns how many trailing bytes of b form an unfinished UTF-8
// encoding, so a rune split across two reads is not corrupted.
func incompleteTail(b []byte) int {
	for n := 1; n < utf8.UTFMax && n <= len(b); n++ {
		if incompleteRune(b[len(b)-n:]) {
			return n
		}
	}
	return 0
}

// consumePaste consumes literal paste content until the end marker and reports
// how many of the chunk's bytes it used.
func (d *Decoder) consumePaste(chunk []byte) (int, []Event) {
	tailLen := len(d.paste.tail)
	buf := make([]byte, 0, tailLen+len(chunk))
	buf = append(buf, d.paste.tail...)
	buf = append(buf, chunk...)

	for offset := 0; ; {
		index := bytes.Index(buf[offset:], []byte(pasteEnd))
		if index < 0 {
			break
		}
		end := offset + index
		d.paste.append(buf[offset:end])
		events := d.paste.finish()
		// The retained tail is not part of chunk, so the count reported to the
		// caller covers only the bytes this read contributed.
		return max(end+len(pasteEnd)-tailLen, 0), events
	}

	// No end marker yet. Retain the bytes that may still turn into one, plus an
	// unfinished trailing rune, and treat everything before them as content.
	keep := markerOverlapLen(buf, pasteEnd)
	if tail := incompleteTail(buf); tail > keep {
		keep = tail
	}
	if keep > len(buf) {
		keep = len(buf)
	}
	d.paste.append(buf[:len(buf)-keep])
	d.paste.tail = append(d.paste.tail[:0], buf[len(buf)-keep:]...)
	return len(chunk), nil
}

// sizeError describes a refused resize.
func sizeError(axis string, value, minValue, maxValue int) error {
	return fmt.Errorf("%w: %s %d is outside [%d, %d]", ErrSizeOutOfRange, axis, value, minValue, maxValue)
}
