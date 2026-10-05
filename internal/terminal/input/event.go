// Package input decodes a client's terminal byte stream into ordered session
// input events (PLAN 6.1). The decoder is transport-independent: it consumes
// bytes and terminal metadata exactly as an SSH channel delivers them, never a
// local process terminal, and it tolerates a sequence split across any number
// of reads.
//
// Events are the session input contract (PLAN 14): key presses, bracketed
// pastes, window resizes, and end of input. Every bound is explicit, because
// a client that never finishes a sequence or paste must cost a fixed amount of
// memory rather than an unbounded one.
package input

import (
	"errors"
	"fmt"
)

// Control bytes and markers the decoder recognises in the byte stream.
const (
	escapeByte = 0x1b // ESC
	deleteByte = 0x7f // DEL, sent as Backspace by most clients
	nulByte    = 0x00 // NUL, ignored like a real terminal ignores it

	// pasteStart and pasteEnd bracket a paste whose content is literal text.
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// Kind classifies a decoded event.
type Kind int

const (
	// KindKey is one key press; Key and Rune describe it.
	KindKey Kind = iota
	// KindPaste is the literal content of one bracketed paste.
	KindPaste
	// KindResize reports a new terminal size from a window-change request.
	KindResize
	// KindEOF marks the end of the client's input stream.
	KindEOF
	// KindRejected reports bytes the decoder refused: an unusable escape
	// sequence, an incomplete sequence that outgrew Limits.MaxSequenceLen, or
	// an unfinished sequence when the stream ended. The bytes travel with the
	// event so a session can record them; they are never input.
	KindRejected
)

// String returns the event kind name.
func (k Kind) String() string {
	switch k {
	case KindKey:
		return "key"
	case KindPaste:
		return "paste"
	case KindResize:
		return "resize"
	case KindEOF:
		return "eof"
	case KindRejected:
		return "rejected"
	default:
		return fmt.Sprintf("unknown(%d)", int(k))
	}
}

// Event is one decoded session input event.
type Event struct {
	Kind Kind

	// Key and Rune describe a KindKey event. Rune is meaningful only for
	// KeyRune; every other key is fully described by Key.
	Key  Key
	Rune rune

	// Text is the literal content of a KindPaste event. Terminal controls were
	// removed while decoding, so pasting text cannot inject a sequence.
	Text string
	// Truncated reports that a paste reached Limits.MaxPasteBytes and only its
	// leading bytes were kept.
	Truncated bool

	// Cols and Rows describe a KindResize event.
	Cols int
	Rows int

	// Raw holds the bytes of a KindRejected event for diagnostics.
	Raw []byte
}

// KeyEvent builds a KindKey event for a printable rune.
func KeyEvent(r rune) Event {
	return Event{Kind: KindKey, Key: KeyRune, Rune: r}
}

// NamedKeyEvent builds a KindKey event for a named key.
func NamedKeyEvent(k Key) Event {
	return Event{Kind: KindKey, Key: k}
}

// ResizeEvent builds a KindResize event.
func ResizeEvent(cols, rows int) Event {
	return Event{Kind: KindResize, Cols: cols, Rows: rows}
}

// EOFEvent builds the KindEOF event.
func EOFEvent() Event { return Event{Kind: KindEOF} }

// RejectedEvent builds a KindRejected event carrying raw bytes.
func RejectedEvent(raw []byte) Event {
	return Event{Kind: KindRejected, Raw: raw}
}

// Limits bounds what one decoder may buffer. Every field is required to be
// positive; NewDecoder rejects a Limits that leaves the decoder unbounded.
type Limits struct {
	// MaxPasteBytes is the largest amount of one bracketed paste kept. A
	// longer paste is truncated at that length; the decoder still scans to the
	// end marker so the stream stays synchronized.
	MaxPasteBytes int
	// MaxSequenceLen is the largest incomplete escape sequence kept between
	// reads. A longer run of bytes with no final byte is rejected, which stops
	// a client from growing the buffer by never finishing a sequence.
	MaxSequenceLen int
	// MinCols, MinRows, MaxCols, and MaxRows bound an accepted resize.
	MinCols int
	MinRows int
	MaxCols int
	MaxRows int
}

// DefaultLimits returns bounds sized for an interactive shell over SSH: a
// paste is capped at one megabyte, an unfinished escape sequence at 64 bytes
// (a mouse report or focus report fits comfortably), and the terminal between
// 1x1 and 1000x1000 cells.
func DefaultLimits() Limits {
	return Limits{
		MaxPasteBytes:  1 << 20,
		MaxSequenceLen: 64,
		MinCols:        1,
		MinRows:        1,
		MaxCols:        1000,
		MaxRows:        1000,
	}
}

// Errors reported by the decoder.
var (
	// ErrInvalidLimits reports a Limits with a non-positive field.
	ErrInvalidLimits = errors.New("invalid input decoder limits")
	// ErrSizeOutOfRange reports a resize outside the configured bounds.
	ErrSizeOutOfRange = errors.New("terminal size out of range")
)

// Validate reports whether every bound is usable.
func (l Limits) Validate() error {
	for name, value := range map[string]int{
		"MaxPasteBytes":  l.MaxPasteBytes,
		"MaxSequenceLen": l.MaxSequenceLen,
		"MinCols":        l.MinCols,
		"MinRows":        l.MinRows,
		"MaxCols":        l.MaxCols,
		"MaxRows":        l.MaxRows,
	} {
		if value <= 0 {
			return fmt.Errorf("%w: %s must be positive, got %d", ErrInvalidLimits, name, value)
		}
	}
	if l.MinCols > l.MaxCols {
		return fmt.Errorf("%w: MinCols %d exceeds MaxCols %d", ErrInvalidLimits, l.MinCols, l.MaxCols)
	}
	if l.MinRows > l.MaxRows {
		return fmt.Errorf("%w: MinRows %d exceeds MaxRows %d", ErrInvalidLimits, l.MinRows, l.MaxRows)
	}
	return nil
}
