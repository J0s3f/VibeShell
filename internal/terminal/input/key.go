package input

import "strings"

// Key names one decoded key press. Control keys have their own constants so a
// caller decides what was pressed without inspecting a rune, and the Escape
// key is distinct from the byte 0x1b that starts an escape sequence.
type Key int

const (
	// KeyUnknown is never produced; it is the zero value so a zeroed Event is
	// recognisable as "no key".
	KeyUnknown Key = iota
	// KeyRune carries printable text in Event.Rune.
	KeyRune
	KeyEnter
	KeyTab
	// KeyShiftTab is Shift-Tab, which clients send as CSI Z.
	KeyShiftTab
	KeyBackspace
	KeyDelete
	KeyEscape
	KeyInsert
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyPageUp
	KeyPageDown
	KeyF1
	KeyF2
	KeyF3
	KeyF4
	// Control keys occupy one contiguous block so the editor can recognise
	// them with a range check and translate them to letters with CtrlLetter.
	KeyCtrlA
	KeyCtrlB
	KeyCtrlC
	KeyCtrlD
	KeyCtrlE
	KeyCtrlF
	KeyCtrlG
	KeyCtrlH
	KeyCtrlI
	KeyCtrlJ
	KeyCtrlK
	KeyCtrlL
	KeyCtrlM
	KeyCtrlN
	KeyCtrlO
	KeyCtrlP
	KeyCtrlQ
	KeyCtrlR
	KeyCtrlS
	KeyCtrlT
	KeyCtrlU
	KeyCtrlV
	KeyCtrlW
	KeyCtrlX
	KeyCtrlY
	KeyCtrlZ
	// The remaining C0 codes that do not spell a control letter: NUL is
	// Ctrl-Space, and 0x1c..0x1f are the classic punctuation controls.
	KeyCtrlSpace
	KeyCtrlBackslash
	KeyCtrlBracketClose
	KeyCtrlCaret
	KeyCtrlUnderscore
	// keyCtrlLast ends the control block.
	keyCtrlLast
)

// keyNames gives every named key its wire-independent name. Control keys are
// derived from the letter instead, so only the punctuation controls appear
// here.
var keyNames = map[Key]string{
	KeyRune:      "rune",
	KeyEnter:     "enter",
	KeyTab:       "tab",
	KeyShiftTab:  "shift-tab",
	KeyBackspace: "backspace",
	KeyDelete:    "delete",
	KeyEscape:    "escape",
	KeyInsert:    "insert",
	KeyUp:        "up",
	KeyDown:      "down",
	KeyLeft:      "left",
	KeyRight:     "right",
	KeyHome:      "home",
	KeyEnd:       "end",
	KeyPageUp:    "page-up",
	KeyPageDown:  "page-down",
	KeyF1:        "f1",
	KeyF2:        "f2",
	KeyF3:        "f3",
	KeyF4:        "f4",

	KeyCtrlSpace:        "ctrl-space",
	KeyCtrlBackslash:    "ctrl-backslash",
	KeyCtrlBracketClose: "ctrl-bracket-close",
	KeyCtrlCaret:        "ctrl-caret",
	KeyCtrlUnderscore:   "ctrl-underscore",
}

// String returns the stable key name used in logs and in view key bindings.
func (k Key) String() string {
	if name, ok := keyNames[k]; ok {
		return name
	}
	if letter, ok := k.CtrlLetter(); ok {
		return "ctrl-" + string(letter)
	}
	return "unknown"
}

// CtrlLetter reports the letter a control key stands for, so KeyCtrlC is "c"
// and KeyCtrlSpace is " ".
func (k Key) CtrlLetter() (rune, bool) {
	switch {
	case k >= KeyCtrlA && k <= KeyCtrlZ:
		return 'a' + rune(k-KeyCtrlA), true
	case k == KeyCtrlSpace:
		return ' ', true
	case k == KeyCtrlBackslash:
		return '\\', true
	case k == KeyCtrlBracketClose:
		return ']', true
	case k == KeyCtrlCaret:
		return '^', true
	case k == KeyCtrlUnderscore:
		return '_', true
	default:
		return 0, false
	}
}

// IsCtrl reports whether k is one of the control keys.
func (k Key) IsCtrl() bool { return k >= KeyCtrlA && k <= keyCtrlLast }

// ParseKey returns the key with the given name, which accepts both the named
// keys ("enter", "page-up", "f3") and the control keys ("ctrl-c"). It is the
// inverse of Key.String and is used to resolve declarative view key bindings.
func ParseKey(name string) (Key, error) {
	trimmed := strings.ToLower(strings.TrimSpace(name))
	for key, candidate := range keyNames {
		if candidate == trimmed {
			return key, nil
		}
	}
	if rest, ok := strings.CutPrefix(trimmed, "ctrl-"); ok && len(rest) == 1 {
		switch rest[0] {
		case ' ':
			return KeyCtrlSpace, nil
		case '\\':
			return KeyCtrlBackslash, nil
		case ']':
			return KeyCtrlBracketClose, nil
		case '^':
			return KeyCtrlCaret, nil
		case '_':
			return KeyCtrlUnderscore, nil
		}
		if rest[0] >= 'a' && rest[0] <= 'z' {
			return KeyCtrlA + Key(rest[0]-'a'), nil
		}
	}
	return KeyUnknown, &UnknownKeyError{Name: name}
}

// UnknownKeyError reports a key name no terminal core key matches.
type UnknownKeyError struct {
	Name string
}

func (e *UnknownKeyError) Error() string {
	return "unknown terminal key: " + e.Name
}
