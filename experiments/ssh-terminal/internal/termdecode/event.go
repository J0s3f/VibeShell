// Package termdecode turns a terminal byte stream into semantic input events.
//
// The decoder is a pure byte-stream state machine: it never reads a terminal,
// never starts a timer, and never spawns a process. A caller pushes the bytes it
// read from a transport (an SSH session channel today) and receives typed events
// through a sink. Incomplete escape sequences are held until the next write, so
// a key sequence split across two SSH packets decodes identically to one that
// arrives whole. Every buffer the decoder keeps is bounded by Bounds.
package termdecode

// KeyName identifies a decoded key. A name that is not KeyRune carries no
// rune; the name alone is the key's meaning.
type KeyName int

// Key names produced by the decoder.
const (
	KeyUnknown KeyName = iota
	// KeyRune is a printable character; Key.Rune holds it.
	KeyRune
	KeyEnter
	KeyTab
	KeyBackspace
	KeyEscape
	KeyUp
	KeyDown
	KeyRight
	KeyLeft
	KeyHome
	KeyEnd
	KeyDelete
	KeyInsert
	KeyPageUp
	KeyPageDown
	KeyCtrlA
	KeyCtrlB
	KeyCtrlC
	KeyCtrlD
	KeyCtrlE
	KeyCtrlF
	KeyCtrlK
	KeyCtrlU
	KeyCtrlW
	KeyCtrlL
	// KeyMalformed reports an unterminated or invalid escape sequence whose
	// bytes the decoder discarded to stay inside its bounds.
	KeyMalformed
	// KeyPasteOverflow reports that one bracketed paste exceeded
	// Bounds.MaxPasteBytes; the rest of that paste was discarded.
	KeyPasteOverflow
)

// String returns the name used in logs and receipts.
func (k KeyName) String() string {
	switch k {
	case KeyRune:
		return "rune"
	case KeyEnter:
		return "enter"
	case KeyTab:
		return "tab"
	case KeyBackspace:
		return "backspace"
	case KeyEscape:
		return "escape"
	case KeyUp:
		return "up"
	case KeyDown:
		return "down"
	case KeyRight:
		return "right"
	case KeyLeft:
		return "left"
	case KeyHome:
		return "home"
	case KeyEnd:
		return "end"
	case KeyDelete:
		return "delete"
	case KeyInsert:
		return "insert"
	case KeyPageUp:
		return "page-up"
	case KeyPageDown:
		return "page-down"
	case KeyCtrlA:
		return "ctrl+a"
	case KeyCtrlB:
		return "ctrl+b"
	case KeyCtrlC:
		return "ctrl+c"
	case KeyCtrlD:
		return "ctrl+d"
	case KeyCtrlE:
		return "ctrl+e"
	case KeyCtrlF:
		return "ctrl+f"
	case KeyCtrlK:
		return "ctrl+k"
	case KeyCtrlU:
		return "ctrl+u"
	case KeyCtrlW:
		return "ctrl+w"
	case KeyCtrlL:
		return "ctrl+l"
	case KeyMalformed:
		return "malformed"
	case KeyPasteOverflow:
		return "paste-overflow"
	default:
		return "unknown"
	}
}

// Mods is a set of modifier keys held with a key press.
type Mods uint8

// Modifier flags. The numbering matches the xterm modifyOtherKeys values that
// terminal input uses, so an ESC [ 1 ; m X sequence maps straight onto it.
const (
	ModShift Mods = 1 << iota
	ModAlt
	ModCtrl
)

// Has reports whether every bit in other is set.
func (m Mods) Has(other Mods) bool { return m&other == other }

// Key is one key press.
type Key struct {
	Name KeyName
	// Rune is the character for KeyRune and the character typed with Alt for a
	// modified rune press.
	Rune rune
	Mods Mods
}

// String returns a stable, log-friendly description.
func (k Key) String() string {
	name := k.Name.String()
	if k.Mods != 0 {
		name = k.Mods.String() + "+" + name
	}
	if k.Name == KeyRune {
		return name + " " + string(k.Rune)
	}
	return name
}

// String returns a stable, log-friendly description of the modifier set.
func (m Mods) String() string {
	switch m {
	case 0:
		return ""
	case ModShift:
		return "shift"
	case ModAlt:
		return "alt"
	case ModCtrl:
		return "ctrl"
	case ModShift | ModAlt:
		return "shift+alt"
	case ModShift | ModCtrl:
		return "shift+ctrl"
	case ModAlt | ModCtrl:
		return "alt+ctrl"
	default:
		return "shift+alt+ctrl"
	}
}

// Paste is one bracketed paste. Truncated reports that the paste exceeded
// Bounds.MaxPasteBytes and that Text holds only its leading bytes.
type Paste struct {
	Text      string
	Truncated bool
}

// Resize is a new terminal size in cells. SSH delivers it as a window-change
// request; a terminal that also supports xterm's in-band resize notification
// delivers it as a CSI 8 ; rows ; cols t sequence.
type Resize struct {
	Cols int
	Rows int
}

// Event is one decoded terminal input event.
type Event interface {
	isTerminalEvent()
}

func (Key) isTerminalEvent()    {}
func (Paste) isTerminalEvent()  {}
func (Resize) isTerminalEvent() {}
