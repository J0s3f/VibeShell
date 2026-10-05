// Package termrender holds the small output-side terminal primitives the
// qualification spike chose: ANSI validation of rendered frames and cell
// measurement.
//
// The recommendation it encodes is narrow. github.com/charmbracelet/x/ansi is
// used as a pure byte-stream library: it parses a frame the renderer produced,
// classifies every control sequence in it, and measures display width. It never
// opens a terminal, reads an environment variable, or starts a goroutine, so it
// fits a server whose "terminal" is the far end of an SSH channel. The
// ultraviolet screen layer is not used; see
// docs/research/2026-10-03-ssh-terminal-spike.md for the evidence.
package termrender

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/parser"
)

// maxPolicyBytes bounds one frame handed to Sanitize. A frame is a screen redraw
// produced by the renderer, so a few hundred kilobytes is generous and keeps a
// bug in the renderer from turning into unbounded work.
const maxPolicyBytes = 1 << 20

// Policy is the set of terminal controls a rendered frame may contain.
//
// PLAN.md section 6.1 requires that text displayed as data cannot activate
// clipboard, hyperlink, title-change, or any other unapproved terminal control,
// so the default policy allows only cursor movement, erasing, and SGR styling.
// Everything else, including every OSC, DCS, APC, and PM sequence, is removed.
type Policy struct {
	// AllowCSI lists the CSI final bytes a frame may use, such as 'A' for cursor
	// up or 'm' for SGR. A nil map allows the built-in conservative set.
	AllowCSI map[byte]bool
	// AllowESC lists the two-byte escape sequences a frame may use, keyed by the
	// byte after ESC. A nil map allows the built-in conservative set.
	AllowESC map[byte]bool
	// AllowPrivateCSI permits sequences that carry a private parameter prefix such
	// as the '?' of a mode report. Off by default.
	AllowPrivateCSI bool
}

// conservativeCSI is the xterm subset the renderer needs: cursor positioning and
// movement, erasing, scroll region, and SGR styling.
var conservativeCSI = map[byte]bool{
	'A': true, 'B': true, 'C': true, 'D': true, // cursor up/down/right/left
	'E': true, 'F': true, 'G': true, // cursor next/previous line, column
	'H': true, 'f': true, // cursor position
	'J': true, 'K': true, // erase display, erase line
	'm': true,            // select graphic rendition
	'S': true, 'T': true, // scroll up/down
	'd': true,            // line position absolute
	'r': true,            // set scroll region
	'X': true,            // erase characters
	'L': true, 'M': true, // insert/delete lines
	'P': true, '@': true, // delete/insert characters
	's': true,  // save/restore cursor
	'u': true,  // restore cursor
	'c': false, // device attributes are never sent
}

// conservativeESC is the two-byte escape set a renderer needs. Save and restore
// cursor (ESC 7 and ESC 8) are deliberately absent: the screen model owns the
// cursor, so client-side cursor state would be a second source of truth.
var conservativeESC = map[byte]bool{
	'D': true, // index
	'E': true, // next line
	'M': true, // reverse index
	'c': false,
}

// allowedC0 are the C0 controls a frame may contain. Everything else, including
// BEL, ESC on its own, and DEL, is dropped.
var allowedC0 = map[byte]bool{
	0x08: true, // backspace
	0x09: true, // tab
	0x0a: true, // line feed
	0x0b: true, // vertical tab
	0x0c: true, // form feed
	0x0d: true, // carriage return
}

func (p Policy) allowsCSI(final byte) bool {
	if p.AllowCSI != nil {
		return p.AllowCSI[final]
	}
	return conservativeCSI[final]
}

func (p Policy) allowsESC(final byte) bool {
	if p.AllowESC != nil {
		return p.AllowESC[final]
	}
	return conservativeESC[final]
}

// Removal records a control sequence that the policy rejected, so a renderer or
// a transcript can report what a frame tried to do.
type Removal struct {
	// Kind is a short stable name such as "OSC", "DCS", or "CSI 0x0d".
	Kind string
	// Bytes is the rejected sequence, truncated for logging.
	Bytes string
}

// maxRemovalBytes bounds what a Removal keeps of the rejected sequence.
const maxRemovalBytes = 64

// describeByte renders one byte for a Removal.
func describeByte(b byte) string { return quote([]byte{b}) }

// Report is the result of checking one frame.
type Report struct {
	// Kept is the frame with rejected sequences removed.
	Kept []byte
	// Removals lists every rejected sequence, in the order it appeared.
	Removals []Removal
}

// Sanitize returns the frame with every control sequence the policy does not
// allow removed, together with what it removed.
//
// Sanitize is a byte-stream function: it holds no terminal state, consults no
// environment, and creates no process. A frame may therefore be checked on any
// goroutine, and the same code runs in a server and in a test.
func Sanitize(frame []byte, policy Policy) Report {
	report := Report{Kept: make([]byte, 0, len(frame))}
	if len(frame) > maxPolicyBytes {
		// A frame this large is a bug, not a screen. Keep the report honest
		// instead of scanning unbounded input.
		report.Kept = append(report.Kept, frame[:maxPolicyBytes]...)
		report.Removals = append(report.Removals, Removal{Kind: "frame-too-large"})
		return report
	}

	decoder := ansi.NewParser()
	var state byte = byte(parser.GroundState)
	for rest := frame; len(rest) > 0; {
		sequence, width, size, next := ansi.DecodeSequence(rest, state, decoder)
		if size <= 0 {
			// The decoder made no progress; drop the byte so the loop ends.
			report.Removals = append(report.Removals, Removal{Kind: "unparsed", Bytes: describeByte(rest[0])})
			rest = rest[1:]
			state = byte(parser.GroundState)
			continue
		}
		state = next
		chunk := rest[:size]
		rest = rest[size:]

		kind, allowed := classify(sequence, width, state, policy)
		if allowed {
			report.Kept = append(report.Kept, chunk...)
			continue
		}
		report.Removals = append(report.Removals, Removal{Kind: kind, Bytes: quote(chunk)})
	}
	return report
}

// classify names one decoded item and decides whether the policy allows it.
//
// The decoder returns printable graphemes as well as control sequences and
// reports a cell width of zero for every control, so the width is what tells
// text from a sequence.
func classify(sequence []byte, width int, state byte, policy Policy) (string, bool) {
	if width > 0 {
		return "text", true
	}
	if len(sequence) == 0 {
		return "empty", false
	}
	switch sequence[0] {
	case 0x1b:
		if len(sequence) < 2 {
			return "ESC", false
		}
		switch sequence[1] {
		case '[':
			return classifyCSI(sequence, state, policy)
		case ']', 'P', '_', '^', 'X':
			// OSC, DCS, APC, PM, SOS: window titles, clipboard, sixel, and
			// every other control a client would act on without being asked.
			return kindName(sequence[1]), false
		default:
			return "ESC", policy.allowsESC(sequence[1])
		}
	case 0x9b:
		// The 8-bit CSI introducer is the same request as ESC [.
		return classifyCSI(sequence, state, policy)
	default:
		if allowedC0[sequence[0]] {
			return "C0", true
		}
		return fmt.Sprintf("C0 0x%02x", sequence[0]), false
	}
}

// classifyCSI decides whether a CSI sequence may reach the client.
func classifyCSI(sequence []byte, state byte, policy Policy) (string, bool) {
	final := sequence[len(sequence)-1]
	kind := "CSI " + describeByte(final)
	if final < 0x40 || final > 0x7e {
		return kind, false
	}
	if !policy.allowsCSI(final) {
		return kind, false
	}
	if state >= byte(parser.DcsEntryState) && state <= byte(parser.DcsStringState) {
		return "DCS", false
	}
	if hasPrivateParameter(sequence) && !policy.AllowPrivateCSI {
		return kind, false
	}
	return kind, true
}

// hasPrivateParameter reports whether a CSI sequence carries a private prefix
// byte such as the '?' of a mode query.
func hasPrivateParameter(sequence []byte) bool {
	if len(sequence) < 3 {
		return false
	}
	prefix := sequence[2]
	return prefix >= 0x3c && prefix <= 0x3f
}

// Width returns the number of cells s occupies on a terminal, ignoring the ANSI
// control sequences in it. It is the measurement a layout needs, and it is why
// a byte count is never used for column arithmetic.
func Width(s string) int { return ansi.StringWidth(s) }

// PlainText returns s with its ANSI control sequences removed, which is what a
// transcript, a log, or a search index stores.
func PlainText(s string) string { return ansi.Strip(s) }

// quote renders bytes for a Removal without letting control characters
// reach a log unescaped.
func quote(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if len(b) > maxRemovalBytes {
		b = b[:maxRemovalBytes]
	}
	var quoted strings.Builder
	for _, c := range b {
		switch {
		case c >= 0x20 && c < 0x7f:
			quoted.WriteByte(c)
		default:
			quoted.WriteString(`\x`)
			const hex = "0123456789abcdef"
			quoted.WriteByte(hex[c>>4])
			quoted.WriteByte(hex[c&0x0f])
		}
	}
	return quoted.String()
}

// kindName names a string-introduced control sequence.
func kindName(introducer byte) string {
	switch introducer {
	case ']':
		return "OSC"
	case 'P':
		return "DCS"
	case '_':
		return "APC"
	case '^':
		return "PM"
	case 'X':
		return "SOS"
	default:
		return "control"
	}
}
