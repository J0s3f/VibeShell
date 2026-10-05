package editor

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"j0s.at/vibeshell/internal/terminal/screen"
)

// Control sequences the local layer emits when it repaints the prompt region.
// They are emitted only by this package and by the renderer; data text never
// reaches them, because the screen model drops every control byte.
const (
	// sequenceEraseLine erases the whole line.
	sequenceEraseLine = "\x1b[2K"
	// sequenceCursorUp moves the cursor up n rows.
	sequenceCursorUp = "\x1b[%dA"
	// sequenceCursorDown moves the cursor down n rows.
	sequenceCursorDown = "\x1b[%dB"
	// sequenceCursorColumn moves the cursor to a column on its current row.
	sequenceCursorColumn = "\x1b[%dG"
)

// Painter owns the terminal state of a prompt: which rows the prompt currently
// occupies and where its cursor is. A command line changes the prompt's width
// on every keystroke, so the local layer repaints the whole prompt region
// instead of relying on the client to redraw it, and tracks the rows it used so
// a shrinking draft also erases the rows it no longer needs.
type Painter struct {
	prompt string
	rows   int
}

// NewPainter returns a painter for a prompt. The editor is not owned by the
// painter: it reads the draft and cursor on every repaint.
func NewPainter(prompt string) *Painter {
	return &Painter{prompt: prompt}
}

// Prompt returns the prompt text.
func (p *Painter) Prompt() string { return p.prompt }

// Rows returns how many terminal rows the prompt region currently occupies.
func (p *Painter) Rows() int { return p.rows }

// Redraw returns the bytes that repaint the prompt region and leave the cursor
// where the editor needs it. Only the rows that change are touched, and rows
// the shorter draft no longer needs are erased, so a wrapped prompt does not
// leave fragments of itself behind.
func (p *Painter) Redraw(e *Editor, cols int) []byte {
	// The layout covers the prompt and the draft together, so the editor's
	// cursor index has to move past the prompt to address the same character.
	cursor := utf8.RuneCountInString(p.prompt) + e.Cursor()
	layout := screen.WrapText(p.prompt+e.Draft(), cols, screen.DefaultTabWidth, cursor)
	var out strings.Builder

	if up := p.rows - 1; up > 0 {
		writeSequence(&out, sequenceCursorUp, up)
	}
	for row, text := range layout.Rows {
		if row > 0 {
			writeSequence(&out, sequenceCursorDown, 1)
		}
		out.WriteString("\r")
		out.WriteString(sequenceEraseLine)
		out.WriteString(text)
	}
	if rows := p.rows - len(layout.Rows); rows > 0 {
		for range rows {
			writeSequence(&out, sequenceCursorDown, 1)
			out.WriteString("\r")
			out.WriteString(sequenceEraseLine)
		}
		writeSequence(&out, sequenceCursorUp, rows)
	}
	writeSequence(&out, sequenceCursorColumn, layout.CursorCol+1)
	p.rows = len(layout.Rows)
	return []byte(out.String())
}

func writeSequence(out *strings.Builder, format string, value int) {
	out.WriteString("\x1b[")
	out.WriteString(strconv.Itoa(value))
	out.WriteByte(format[len(format)-1])
}
