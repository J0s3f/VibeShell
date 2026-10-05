package input

import "unicode/utf8"

// pasteState accumulates the literal content of one bracketed paste. Pasted
// bytes are never escape-decoded, and the controls a terminal would act on are
// removed while they are collected: a paste is data, not a program, and must
// not be able to introduce a control sequence through either the 7-bit or the
// C1 form of ESC.
type pasteState struct {
	active    bool
	limit     int
	content   []byte
	tail      []byte // bytes that may still turn into the end marker
	truncated bool
}

// start opens a paste with the given byte budget.
func (p *pasteState) start(limit int) {
	p.active = true
	p.limit = limit
	p.content = p.content[:0]
	p.tail = p.tail[:0]
	p.truncated = false
}

// append adds literal paste content, keeping at most the configured number of
// bytes. Past the limit the content is dropped and the paste is marked
// truncated, because a caller has to be able to tell a shortened paste from a
// complete one.
func (p *pasteState) append(chunk []byte) {
	for len(chunk) > 0 {
		r, size := utf8.DecodeRune(chunk)
		chunk = chunk[size:]
		if !pasteRune(r) {
			continue
		}
		if len(p.content)+utf8.RuneLen(r) > p.limit {
			p.truncated = true
			continue
		}
		p.content = utf8.AppendRune(p.content, r)
	}
}

// pasteRune reports whether a pasted rune is content. Tab and newline survive
// because a bracketed paste is multiline and may indent; every other control,
// including ESC and the C1 block, is dropped.
func pasteRune(r rune) bool {
	switch {
	case r == '\t' || r == '\n':
		return true
	case r < 0x20, r == deleteByte, r >= 0x80 && r <= 0x9f:
		return false
	}
	return true
}

// finish reports the collected paste and closes it. An unfinished paste is
// reported with whatever arrived, which is what a user pasting and then
// disconnecting sees.
func (p *pasteState) finish() []Event {
	events := []Event{{
		Kind:      KindPaste,
		Text:      string(p.content),
		Truncated: p.truncated,
	}}
	p.active = false
	p.content = p.content[:0]
	p.tail = p.tail[:0]
	p.truncated = false
	return events
}
