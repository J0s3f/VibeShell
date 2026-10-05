package screen

import (
	"strings"
	"unicode"
)

// dataAction classifies a rune of data text.
type dataAction int

const (
	// dataPrinted writes a cell at the cursor.
	dataPrinted dataAction = iota
	// dataMotion moves the cursor without writing a cell.
	dataMotion
	// dataDropped discards the rune.
	dataDropped
)

// classifyDataRune decides what one rune of data text does. Only the four
// cursor-motion controls are honored; ESC, every other C0 code, DEL, and the
// C1 block are dropped, so data text can never introduce a control sequence.
// Dropping C1 matters as much as dropping ESC: U+009B is the 8-bit form of the
// introducer that starts a control sequence.
func classifyDataRune(r rune) dataAction {
	switch r {
	case '\r', '\n', '\t', '\b':
		return dataMotion
	}
	switch {
	case r < 0x20, r == deleteRune, r >= 0x80 && r <= 0x9f:
		return dataDropped
	}
	return dataPrinted
}

// deleteRune is DEL, which the decoder maps to Backspace but data text carries
// as a control that must not reach a cell.
const deleteRune = 0x7f

// SanitizeText returns data text with every rune a terminal could interpret as a
// control sequence removed, keeping printable text and the four cursor-motion
// codes. The renderer uses it for text that is written straight to the client,
// where no screen model sits between the data and the terminal.
func SanitizeText(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	for _, r := range text {
		switch classifyDataRune(r) {
		case dataPrinted:
			out.WriteRune(r)
		case dataMotion:
			out.WriteRune(r)
		}
	}
	return out.String()
}

// NormalizeLineFeeds returns text with every line feed that is not already
// preceded by a carriage return given one, so each new line starts at column 0.
// The SSH transport writes application bytes straight to the channel and an
// interactive client runs its terminal in raw mode, so the line discipline's
// ONLCR translation never happens and a bare line feed would leave the cursor
// where the previous line ended.
func NormalizeLineFeeds(text string) string {
	if !strings.ContainsRune(text, '\n') {
		return text
	}
	var out strings.Builder
	out.Grow(len(text) + 8)
	prev := byte(0)
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == '\n' && prev != '\r' {
			out.WriteByte('\r')
		}
		out.WriteByte(c)
		prev = c
	}
	return out.String()
}

// zeroWidth reports whether a rune combines with the cell before it instead of
// occupying a column of its own.
func zeroWidth(r rune) bool {
	// Mn and Me are non-spacing and enclosing marks, and Cf are the format
	// characters a terminal shows as nothing. Hangul Jamo medial and final
	// forms and the interlinear annotation block are also zero width.
	switch {
	case r < 0x300:
		return false
	case unicode.Is(unicode.Mn, r), unicode.Is(unicode.Me, r), unicode.Is(unicode.Cf, r):
		return true
	case r >= 0x1160 && r <= 0x11ff: // Hangul Jamo medial and final forms
		return true
	case r >= 0xfff9 && r <= 0xfffb: // interlinear annotation
		return true
	}
	return false
}

// wideRanges lists the code point ranges that occupy two columns, mirroring the
// East Asian Wide and Fullwidth classes. Ambiguous width stays one column
// because VibeShell presents a Latin-default shell, and a cell model that
// guessed wider would misalign the common case.
var wideRanges = [...][2]rune{
	{0x1100, 0x115f},   // Hangul Jamo initial consonants
	{0x2329, 0x232a},   // angle brackets
	{0x2e80, 0x303e},   // CJK radicals, Kangxi, CJK symbols and punctuation
	{0x3041, 0x33ff},   // Hiragana, Katakana, Bopomofo, Hangul compatibility, CJK compatibility
	{0x3400, 0x4dbf},   // CJK unified ideographs extension A
	{0x4e00, 0x9fff},   // CJK unified ideographs
	{0xa000, 0xa4cf},   // Yi syllables
	{0xa960, 0xa97f},   // Hangul Jamo extended A
	{0xac00, 0xd7a3},   // Hangul syllables
	{0xf900, 0xfaff},   // CJK compatibility ideographs
	{0xfe10, 0xfe19},   // vertical forms
	{0xfe30, 0xfe6f},   // CJK compatibility forms, small form variants
	{0xff00, 0xff60},   // fullwidth forms
	{0xffe0, 0xffe6},   // fullwidth signs
	{0x1f300, 0x1f64f}, // emoji
	{0x1f900, 0x1f9ff}, // supplemental symbols and pictographs
	{0x20000, 0x2fffd}, // CJK unified ideographs extensions B and later
	{0x30000, 0x3fffd}, // CJK unified ideographs extension G
}

// RuneWidth returns the number of columns a rune occupies: 0 for a combining
// mark, 2 for a wide rune, and 1 otherwise. A negative result is impossible,
// so a caller can use the result directly as a cell width.
func RuneWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case zeroWidth(r):
		return 0
	}
	for _, span := range wideRanges {
		if r >= span[0] && r <= span[1] {
			return 2
		}
	}
	return 1
}

// Layout is the result of laying text out into the rows of a fixed-width
// terminal.
type Layout struct {
	// Rows holds the display rows, each at most cols columns wide.
	Rows []string
	// CursorRow and CursorCol locate the rune at Cursor in Rows.
	CursorRow int
	CursorCol int
	// CursorOnRow reports whether the cursor index fell inside Rows, which it
	// does not when the index is past the last row.
	CursorOnRow bool
}

// WrapText lays data text out in rows of at most cols columns using the same
// column arithmetic as the screen model: wide runes occupy two columns, a
// wide rune that does not fit moves to the next row, tabs advance to the next
// tab stop, and a combining mark attaches to the cell before it. The cursor
// index is a rune index into text and is reported in the same row coordinates
// the screen uses.
func WrapText(text string, cols, tabWidth int, cursor int) Layout {
	if cols < 1 {
		cols = 1
	}
	if tabWidth < 1 {
		tabWidth = DefaultTabWidth
	}
	var layout Layout
	layout.Rows = []string{""}
	row, col := 0, 0
	cursorRow, cursorCol := 0, 0
	cursorLocated := cursor <= 0

	for i, r := range text {
		if !cursorLocated && i == cursor {
			cursorRow, cursorCol = cursorCell(row, col, cols)
			cursorLocated = true
		}
		switch r {
		case '\n':
			layout.Rows = append(layout.Rows, "")
			row, col = row+1, 0
			continue
		case '\r':
			col = 0
			continue
		case '\t':
			// Advance to the next tab stop. When no stop is left in this row, fill
			// to the right margin and let the next character wrap, which is what a
			// terminal does and what keeps a tab from eating whole rows.
			stop := min(((col/tabWidth)+1)*tabWidth, cols)
			for col < stop {
				appendRune(layout.Rows, row, ' ')
				col++
			}
			continue
		}
		width := RuneWidth(r)
		if width == 0 {
			appendRune(layout.Rows, row, r)
			continue
		}
		if col+width > cols {
			if col > 0 {
				layout.Rows = append(layout.Rows, "")
				row, col = row+1, 0
			}
			// A grapheme too wide even for an empty row still has to be shown,
			// and it takes the columns the row has, as in the screen model.
			width = min(width, cols-col)
		}
		appendRune(layout.Rows, row, r)
		col += width
	}
	if !cursorLocated {
		cursorRow, cursorCol = cursorCell(row, col, cols)
	}
	layout.CursorRow, layout.CursorCol = cursorRow, cursorCol
	layout.CursorOnRow = cursorRow < len(layout.Rows)
	return layout
}

// cursorCell reports the cursor position the screen model holds. A cursor that
// has filled the right margin stays on the last column until the next character
// wraps it, so a caller that positions the real cursor lands in the same place.
func cursorCell(row, col, cols int) (int, int) {
	if col >= cols {
		return row, cols - 1
	}
	return row, col
}

// appendRune adds one rune to a layout row.
func appendRune(rows []string, row int, r rune) {
	if row < 0 || row >= len(rows) {
		return
	}
	rows[row] += string(r)
}
