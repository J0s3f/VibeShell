package interactions

import (
	"fmt"
	"strings"
	"unicode"
)

// KeyKind distinguishes a literal character from a named terminal key.
type KeyKind uint8

const (
	// KeyChar is a single literal character, for example "j" or "G".
	KeyChar KeyKind = iota + 1
	// KeyNamed is a key with no literal character, for example "enter".
	KeyNamed
)

// NamedKey enumerates the terminal keys this package understands. The set is
// deliberately small: it covers what line editing, editors, pagers, and tables
// need. Keys outside it are ordinary characters and reach an application as
// literal text.
type NamedKey uint8

const (
	KeyEnter NamedKey = iota + 1
	KeyEscape
	KeyTab
	KeyBackspace
	KeyDelete
	KeyInsert
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyPageUp
	KeyPageDown
	// KeySpace is the space bar. It is named rather than written as a literal
	// space because a sequence separates its chords with spaces.
	KeySpace
	// KeyText is not a terminal key but an input class: it matches any
	// printable character or printable text run. It is what lets one binding
	// serve text entry in every mode that wants it, instead of a binding per
	// character. A mode that does not declare it leaves printable keys
	// unbound, and those reach the application as ordinary input events.
	KeyText
)

// namedKeyNames is the canonical spelling used in specs, in bindings, and in
// the renderer-facing view, so the same key has one name everywhere.
var namedKeyNames = map[NamedKey]string{
	KeyEnter:     "enter",
	KeyEscape:    "escape",
	KeyTab:       "tab",
	KeyBackspace: "backspace",
	KeyDelete:    "delete",
	KeyInsert:    "insert",
	KeyUp:        "up",
	KeyDown:      "down",
	KeyLeft:      "left",
	KeyRight:     "right",
	KeyHome:      "home",
	KeyEnd:       "end",
	KeyPageUp:    "page-up",
	KeyPageDown:  "page-down",
	KeySpace:     "space",
	KeyText:      "text",
}

var namedKeysByName = invertNamedKeys()

func invertNamedKeys() map[string]NamedKey {
	byName := make(map[string]NamedKey, len(namedKeyNames))
	for key, name := range namedKeyNames {
		byName[name] = key
	}
	return byName
}

// String returns the canonical spelling of a named key.
func (k NamedKey) String() string {
	if name, ok := namedKeyNames[k]; ok {
		return name
	}
	return "unknown-key"
}

// Chord is one key press: a literal character or a named key, plus the
// modifiers a terminal reports. Specs bind chords, not raw bytes, so the
// terminal decoder's naming decisions stay in the terminal layer.
type Chord struct {
	Kind  KeyKind
	Named NamedKey
	Rune  rune
	Ctrl  bool
	Alt   bool
}

// ParseChord parses one canonical chord: "j", "G", "3", "ctrl-r", "page-down",
// "enter". Modifiers are written as prefixes so the names above stay readable.
func ParseChord(s string) (Chord, error) {
	if s == "" {
		return Chord{}, fmt.Errorf("%w: empty chord", errKeySyntax)
	}
	rest := s
	ctrl, alt := false, false
	for {
		switch {
		case strings.HasPrefix(rest, "ctrl-"):
			ctrl, rest = true, strings.TrimPrefix(rest, "ctrl-")
		case strings.HasPrefix(rest, "alt-"):
			alt, rest = true, strings.TrimPrefix(rest, "alt-")
		default:
			if ctrl && alt {
				return Chord{}, fmt.Errorf("%w: %q", errKeySyntax, s)
			}
			return finishChord(s, rest, ctrl, alt)
		}
	}
}

func finishChord(original, body string, ctrl, alt bool) (Chord, error) {
	if body == "" {
		return Chord{}, fmt.Errorf("%w: %q has modifiers but no key", errKeySyntax, original)
	}
	if named, ok := namedKeysByName[body]; ok {
		if ctrl || alt {
			return Chord{}, fmt.Errorf("%w: named key %q takes no modifiers", errKeySyntax, body)
		}
		return Chord{Kind: KeyNamed, Named: named}, nil
	}
	r := []rune(body)
	if len(r) != 1 {
		return Chord{}, fmt.Errorf("%w: %q is not a single character", errKeySyntax, original)
	}
	if ctrl {
		// Ctrl chords address control codes, and only letters have them.
		if !unicode.IsLetter(r[0]) || unicode.IsUpper(r[0]) {
			return Chord{}, fmt.Errorf("%w: ctrl requires a lowercase letter, got %q", errKeySyntax, original)
		}
		return Chord{Kind: KeyChar, Rune: r[0], Ctrl: true}, nil
	}
	if alt {
		return Chord{Kind: KeyChar, Rune: r[0], Alt: true}, nil
	}
	if unicode.IsControl(r[0]) {
		return Chord{}, fmt.Errorf("%w: %q is a control character; use a named key", errKeySyntax, original)
	}
	return Chord{Kind: KeyChar, Rune: r[0]}, nil
}

// String renders the canonical spelling of the chord. Character chords print
// as the character itself so specs stay readable; modified and named chords use
// the prefixes ParseChord accepts.
func (c Chord) String() string {
	var body string
	switch c.Kind {
	case KeyChar:
		body = string(c.Rune)
	case KeyNamed:
		body = c.Named.String()
	default:
		body = "unknown-key"
	}
	switch {
	case c.Ctrl:
		return "ctrl-" + strings.ToLower(body)
	case c.Alt:
		return "alt-" + body
	default:
		return body
	}
}

// PrintableText returns the literal character the chord inserts and whether the
// chord is plain printable input. Modified chords and named keys are not text.
func (c Chord) PrintableText() (string, bool) {
	if c.Kind != KeyChar || c.Ctrl || c.Alt {
		return "", false
	}
	return string(c.Rune), true
}

// Sequence is an ordered chord sequence, the unit a binding matches. Sequences
// let a spec express multi-key commands ("d d") without the runtime knowing
// anything about the command's meaning.
type Sequence []Chord

// ParseSequence parses a space-separated chord sequence such as "d d", "3 G",
// or "escape". A single space separates chords; the space bar is the named key
// "space".
func ParseSequence(s string) (Sequence, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: empty key sequence", errKeySyntax)
	}
	parts := strings.Split(trimmed, " ")
	seq := make(Sequence, 0, len(parts))
	for _, part := range parts {
		chord, err := ParseChord(part)
		if err != nil {
			return nil, err
		}
		seq = append(seq, chord)
	}
	return seq, nil
}

// String renders the canonical space-separated spelling.
func (s Sequence) String() string {
	parts := make([]string, len(s))
	for i, chord := range s {
		parts[i] = chord.String()
	}
	return strings.Join(parts, " ")
}

// IsPrefixOf reports whether s is a prefix of other, which is how the machine
// distinguishes a partial multi-key command from an unbound key.
func (s Sequence) IsPrefixOf(other Sequence) bool {
	if len(s) > len(other) {
		return false
	}
	for i := range s {
		if s[i] != other[i] {
			return false
		}
	}
	return true
}

// Append returns the sequence with one more chord pressed.
func (s Sequence) Append(chord Chord) Sequence {
	next := make(Sequence, len(s), len(s)+1)
	copy(next, s)
	return append(next, chord)
}
