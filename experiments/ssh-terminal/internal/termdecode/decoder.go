package termdecode

import (
	"bytes"
	"unicode/utf8"
)

// Byte values the decoder matches directly.
const (
	escapeByte    = 0x1b
	controlCByte  = 0x03
	controlDByte  = 0x04
	tabByte       = 0x09
	lineFeedByte  = 0x0a
	carriageRetrn = 0x0d
	backspaceByte = 0x08
	delByte       = 0x7f
)

// Bracketed paste delimiters, as sent by terminals in paste mode.
var (
	pasteStart = []byte("\x1b[200~")
	pasteEnd   = []byte("\x1b[201~")
)

// Bounds limit the buffers one decoder may hold for a single input stream.
type Bounds struct {
	// MaxPasteBytes bounds the bytes kept for one bracketed paste. A longer
	// paste yields one Paste with Truncated set, preceded by a
	// KeyPasteOverflow key, and the remainder of that paste is discarded.
	MaxPasteBytes int
	// MaxPendingBytes bounds an escape sequence that never terminates. Beyond
	// it the decoder emits KeyMalformed and resumes at the next byte, so a
	// client cannot make the server buffer an unbounded prefix.
	MaxPendingBytes int
}

// DefaultBounds are the limits the spike used to qualify the transport.
var DefaultBounds = Bounds{
	MaxPasteBytes:   64 << 10,
	MaxPendingBytes: 256,
}

// Decoder turns terminal input bytes into events.
//
// A Decoder is not safe for concurrent use: the transport read loop that feeds
// it is the only writer, and the event sink is called on that same goroutine.
type Decoder struct {
	bounds         Bounds
	emit           func(Event)
	buf            []byte
	pasting        bool
	paste          []byte
	pasteTruncated bool
	// flushing is set while Flush decodes the tail, where an unterminated
	// sequence must become literal events instead of waiting for more bytes.
	flushing bool
}

// NewDecoder returns a decoder that reports events through emit. A nil emit
// discards events, which keeps a caller from having to guard every call site.
func NewDecoder(bounds Bounds, emit func(Event)) *Decoder {
	if emit == nil {
		emit = func(Event) {}
	}
	return &Decoder{bounds: bounds, emit: emit}
}

// Write decodes p, which the caller may split at any byte boundary. It never
// returns an error and always reports len(p) written.
func (d *Decoder) Write(p []byte) (int, error) {
	d.buf = append(d.buf, p...)
	d.consume()
	return len(p), nil
}

// InjectResize reports a terminal size change delivered by the transport
// rather than by the byte stream, such as an SSH window-change request.
func (d *Decoder) InjectResize(cols, rows int) {
	d.emit(Resize{Cols: cols, Rows: rows})
}

// Flush emits whatever bytes remain buffered, then forces out any sequence that
// cannot be completed because no more bytes are coming. The transport calls it
// when the input stream ends, so a trailing partial escape sequence becomes an
// observable event instead of silently disappearing.
func (d *Decoder) Flush() {
	if d.pasting {
		// No more bytes can arrive, so the bytes held back as a possible marker
		// prefix are paste content after all.
		d.appendPaste(d.buf)
		d.buf = d.buf[:0]
		d.finishPaste()
	}
	d.flushing = true
	for len(d.buf) > 0 {
		if !d.consumeOne() {
			// consumeEscape always makes progress while flushing.
			d.emitKey(KeyEscape, 0, 0)
			d.buf = d.buf[1:]
		}
	}
	d.flushing = false
	d.buf = nil
}

func (d *Decoder) emitKey(name KeyName, r rune, mods Mods) {
	d.emit(Key{Name: name, Rune: r, Mods: mods})
}

// consume decodes as much of the accumulator as the available bytes allow.
func (d *Decoder) consume() {
	for d.consumeOne() {
	}
}

// consumeOne decodes one event and reports whether it made progress.
func (d *Decoder) consumeOne() bool {
	if len(d.buf) == 0 {
		return false
	}
	if d.pasting {
		return d.consumePaste()
	}
	if d.buf[0] == escapeByte {
		return d.consumeEscape()
	}
	return d.consumeLiteral()
}

// consumeLiteral decodes one non-escape byte or one UTF-8 rune. A rune split
// across writes is held until the bytes that complete it arrive.
func (d *Decoder) consumeLiteral() bool {
	first := d.buf[0]
	if first >= utf8.RuneSelf {
		if !utf8.FullRune(d.buf) {
			if !d.flushing {
				return false
			}
			d.emitKey(KeyMalformed, 0, 0)
			d.buf = d.buf[1:]
			return true
		}
		r, size := utf8.DecodeRune(d.buf)
		if r == utf8.RuneError && size == 1 {
			d.emitKey(KeyMalformed, 0, 0)
		} else {
			d.emitKey(KeyRune, r, 0)
		}
		d.buf = d.buf[size:]
		return true
	}
	if !isControl(first) {
		d.emitKey(KeyRune, rune(first), 0)
		d.buf = d.buf[1:]
		return true
	}
	switch first {
	case controlCByte:
		d.emitKey(KeyCtrlC, 0, 0)
	case controlDByte:
		d.emitKey(KeyCtrlD, 0, 0)
	case tabByte:
		d.emitKey(KeyTab, 0, 0)
	case lineFeedByte, carriageRetrn:
		d.emitKey(KeyEnter, 0, 0)
	case backspaceByte, delByte:
		d.emitKey(KeyBackspace, 0, 0)
	default:
		if name, ok := controlKeys[first]; ok {
			d.emitKey(name, 0, 0)
		} else {
			d.emitKey(KeyMalformed, 0, 0)
		}
	}
	d.buf = d.buf[1:]
	return true
}

// consumePaste accumulates bracketed paste content up to the end marker. The
// bytes that could still be the start of that marker stay buffered, so a paste
// end split across two writes is still recognised.
func (d *Decoder) consumePaste() bool {
	rest := d.buf
	if idx := bytes.Index(rest, pasteEnd); idx >= 0 {
		d.appendPaste(rest[:idx])
		d.finishPaste()
		d.buf = rest[idx+len(pasteEnd):]
		return true
	}
	held := min(len(rest), len(pasteEnd)-1)
	d.appendPaste(rest[:len(rest)-held])
	d.buf = append(d.buf[:0], rest[len(rest)-held:]...)
	return false
}

// appendPaste keeps paste content inside MaxPasteBytes and inside whole UTF-8
// characters, so a bounded paste still produces text a screen can show.
func (d *Decoder) appendPaste(p []byte) {
	if d.pasteTruncated || len(d.paste)+len(p) > d.bounds.MaxPasteBytes {
		keep := d.bounds.MaxPasteBytes - len(d.paste)
		if keep < 0 {
			keep = 0
		}
		if len(p) > keep {
			p = trimToRuneBoundary(p[:keep])
		}
		if !d.pasteTruncated {
			d.pasteTruncated = true
		}
	}
	d.paste = append(d.paste, p...)
	if d.pasteTruncated && len(d.paste) >= d.bounds.MaxPasteBytes {
		d.emitKey(KeyPasteOverflow, 0, 0)
	}
}

// trimToRuneBoundary drops trailing bytes until b is valid UTF-8, so a paste
// cut at its byte limit ends on a character boundary and stays displayable.
func trimToRuneBoundary(b []byte) []byte {
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return b
}

func (d *Decoder) finishPaste() {
	text := string(d.paste)
	truncated := d.pasteTruncated
	d.paste = d.paste[:0]
	d.pasteTruncated = false
	d.pasting = false
	d.emit(Paste{Text: text, Truncated: truncated})
}

// consumeEscape decodes one escape-introduced sequence, holding an incomplete
// one in the accumulator until later bytes arrive.
func (d *Decoder) consumeEscape() bool {
	rest := d.buf[1:]
	if len(rest) == 0 {
		// A bare ESC at the end of the buffer. It is a real key press, and a
		// following byte that would have made it a sequence arrives in the next
		// write, so wait instead of deciding now.
		if d.flushing {
			d.emitKey(KeyEscape, 0, 0)
			d.buf = d.buf[1:]
			return true
		}
		return false
	}
	switch rest[0] {
	case '[':
		return d.consumeCSI(rest)
	case 'O':
		return d.consumeSS3(rest)
	case ']', 'P', '^', '_':
		// OSC, DCS, PM, and APC are output-side constructs. Terminals do not
		// send them on input; if one arrives it is either a client bug or an
		// attempt to make the server buffer a long string, so let the pending
		// bound discard it.
		return d.holdOrMalformed()
	default:
		if rest[0] >= 0x20 && rest[0] < 0x7f {
			d.emitKey(KeyRune, rune(rest[0]), ModAlt)
			d.buf = d.buf[2:]
			return true
		}
		// ESC followed by a control byte: report the ESC alone and re-read the
		// control byte on the next pass.
		d.emitKey(KeyEscape, 0, 0)
		d.buf = d.buf[1:]
		return true
	}
}

// holdOrMalformed keeps an unterminated sequence until the bound is exceeded.
func (d *Decoder) holdOrMalformed() bool {
	if len(d.buf) > d.bounds.MaxPendingBytes || d.flushing {
		d.emitKey(KeyMalformed, 0, 0)
		d.buf = d.buf[1:]
		return true
	}
	return false
}

// consumeCSI decodes a CSI sequence whose parameter bytes start at rest[1].
func (d *Decoder) consumeCSI(rest []byte) bool {
	i := 1
	for i < len(rest) && rest[i] >= 0x30 && rest[i] <= 0x3f {
		i++
	}
	for i < len(rest) && rest[i] >= 0x20 && rest[i] <= 0x2f {
		i++
	}
	if i == len(rest) {
		return d.holdOrMalformed()
	}
	final := rest[i]
	if final < 0x40 || final > 0x7e {
		d.emitKey(KeyMalformed, 0, 0)
		d.buf = d.buf[1:]
		return true
	}
	params := string(rest[1:i])
	consumed := 1 + i + 1

	if params == "200" && final == '~' {
		d.pasting = true
		d.paste = d.paste[:0]
		d.pasteTruncated = false
		d.buf = d.buf[consumed:]
		return true
	}
	if event, ok := decodeCSI(params, final); ok {
		d.emit(event)
		d.buf = d.buf[consumed:]
		return true
	}
	if event, ok := decodeTilde(params, final); ok {
		d.emit(event)
		d.buf = d.buf[consumed:]
		return true
	}
	d.emitKey(KeyUnknown, 0, 0)
	d.buf = d.buf[consumed:]
	return true
}

// consumeSS3 decodes the ESC O x forms used by arrows, Home/End, and F1-F4.
func (d *Decoder) consumeSS3(rest []byte) bool {
	if len(rest) < 2 {
		return d.holdOrMalformed()
	}
	key, ok := ss3Keys[rest[1]]
	if !ok {
		d.emitKey(KeyUnknown, 0, 0)
	} else {
		d.emitKey(key, 0, 0)
	}
	d.buf = d.buf[3:]
	return true
}

// decodeCSI decodes the cursor, editing, and in-band resize CSI forms.
func decodeCSI(params string, final byte) (Event, bool) {
	if final == 't' {
		rows, cols, ok := parseInBandResize(params)
		if ok {
			return Resize{Cols: cols, Rows: rows}, true
		}
		return nil, false
	}
	numbers := parseParams(params)
	var mods Mods
	switch len(numbers) {
	case 1:
	case 2:
		// The only two-parameter form this decoder accepts is a modifier on an
		// otherwise empty parameter list.
		if numbers[0] != 1 {
			return nil, false
		}
		modified, ok := modifierFromParam(numbers[1])
		if !ok {
			return nil, false
		}
		mods = modified
	default:
		return nil, false
	}
	return keyForFinal(final, mods)
}

// keyForFinal maps a CSI final byte without parameters to a key.
func keyForFinal(final byte, mods Mods) (Event, bool) {
	var name KeyName
	switch final {
	case 'A':
		name = KeyUp
	case 'B':
		name = KeyDown
	case 'C':
		name = KeyRight
	case 'D':
		name = KeyLeft
	case 'H':
		name = KeyHome
	case 'F':
		name = KeyEnd
	case 'Z':
		name = KeyTab
		mods |= ModShift
	default:
		return nil, false
	}
	return Key{Name: name, Mods: mods}, true
}

// tildeKeys maps the ESC [ <n> ~ editing keys.
var tildeKeys = map[int]KeyName{
	1: KeyHome,
	2: KeyInsert,
	3: KeyDelete,
	4: KeyEnd,
	5: KeyPageUp,
	6: KeyPageDown,
	7: KeyHome,
	8: KeyEnd,
}

// decodeTilde decodes ESC [ <n> ~ and its modifier form.
func decodeTilde(params string, final byte) (Event, bool) {
	if final != '~' {
		return nil, false
	}
	numbers := parseParams(params)
	switch len(numbers) {
	case 1:
	case 2:
		mods, ok := modifierFromParam(numbers[1])
		if !ok {
			return nil, false
		}
		name, ok := tildeKeys[numbers[0]]
		if !ok {
			return nil, false
		}
		return Key{Name: name, Mods: mods}, true
	default:
		return nil, false
	}
	name, ok := tildeKeys[numbers[0]]
	if !ok {
		return nil, false
	}
	return Key{Name: name}, true
}

// parseInBandResize decodes the xterm in-band resize notification parameters,
// which are "8 ; rows ; cols".
func parseInBandResize(params string) (rows, cols int, ok bool) {
	numbers := parseParams(params)
	if len(numbers) != 3 || numbers[0] != 8 {
		return 0, 0, false
	}
	if numbers[1] <= 0 || numbers[2] <= 0 {
		return 0, 0, false
	}
	return numbers[1], numbers[2], true
}

// ss3Keys maps the ESC O x keys. F1-F4 share this form and are not part of the
// spike's key contract, so they fall through to KeyUnknown.
var ss3Keys = map[byte]KeyName{
	'A': KeyUp,
	'B': KeyDown,
	'C': KeyRight,
	'D': KeyLeft,
	'H': KeyHome,
	'F': KeyEnd,
}

// controlKeys maps the remaining ASCII control bytes to key names.
var controlKeys = map[byte]KeyName{
	0x01: KeyCtrlA,
	0x02: KeyCtrlB,
	0x05: KeyCtrlE,
	0x06: KeyCtrlF,
	0x0b: KeyCtrlK,
	0x0c: KeyCtrlL,
	0x15: KeyCtrlU,
	0x17: KeyCtrlW,
}

// isControl reports whether b is an ASCII control byte the decoder maps to a
// named key or discards.
func isControl(b byte) bool { return b < 0x20 || b == delByte }

// modifierFromParam converts an xterm modifier parameter into Mods. The
// parameter is one more than the bit set: 1 none, 2 shift, 3 alt, 4 alt+shift,
// 5 ctrl, and so on.
func modifierFromParam(param int) (Mods, bool) {
	if param < 1 || param > 8 {
		return 0, false
	}
	return Mods(param - 1), true
}

// parseParams splits a CSI parameter string into its numeric parameters. A
// parameter that is not a number makes the whole sequence unsupported.
func parseParams(params string) []int {
	if params == "" {
		return []int{0}
	}
	out := make([]int, 0, 3)
	value := 0
	for i := 0; i < len(params); i++ {
		b := params[i]
		switch {
		case b >= '0' && b <= '9':
			value = value*10 + int(b-'0')
		case b == ';' || b == ':':
			out = append(out, value)
			value = 0
		default:
			return nil
		}
		if value > maxParameter {
			return nil
		}
	}
	return append(out, value)
}

// maxParameter keeps a hostile parameter string from overflowing an int.
const maxParameter = 1 << 20

// ValidUTF8 reports whether p is well-formed UTF-8. The transport uses it to
// reject a pasted stream that is not text before the decoder sees it.
func ValidUTF8(p []byte) bool { return utf8.Valid(p) }
