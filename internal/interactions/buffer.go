package interactions

import (
	"strconv"
	"strings"
	"unicode"

	"j0s.at/vibeshell/internal/domain"
)

// bufferDoc is the editable document a text edit acts on. Text-like views edit
// the interaction buffer; a form edits the focused field. Both are one line of
// text, so both share the same edit primitives and the same undo stack.
type bufferDoc struct {
	lines  []string
	cursor Cursor
	anchor *Cursor
	// single marks a one-line document (a form field), where splitting and
	// joining are not applicable and a document always has exactly one line.
	single bool
}

// clone copies the document so an edit can be applied and then refused without
// having touched the state.
func (d bufferDoc) clone() bufferDoc {
	out := d
	out.lines = append([]string(nil), d.lines...)
	if d.anchor != nil {
		anchor := *d.anchor
		out.anchor = &anchor
	}
	return out
}

func (d *bufferDoc) line(i int) string {
	if i < 0 || i >= len(d.lines) {
		return ""
	}
	return d.lines[i]
}

func (d *bufferDoc) lineRunes(i int) []rune {
	if i < 0 || i >= len(d.lines) {
		return nil
	}
	return []rune(d.lines[i])
}

func (d *bufferDoc) setLine(i int, text string) {
	if i >= 0 && i < len(d.lines) {
		d.lines[i] = text
	}
}

// lineLen is the cursor-addressable length of a line: the number of runes.
func lineLen(line string) int { return len([]rune(line)) }

// clampColumn applies a mode's cursor convention to a column index.
func clampColumn(column, length int, policy ColumnPolicy) int {
	max := length - 1
	if policy == ColumnAllowEnd {
		max = length
	}
	if max < 0 {
		return 0
	}
	if column > max {
		return max
	}
	if column < 0 {
		return 0
	}
	return column
}

// clampPosition keeps a cursor inside the document.
func (d *bufferDoc) clampPosition(c Cursor, policy ColumnPolicy) Cursor {
	if len(d.lines) == 0 {
		return Cursor{}
	}
	if c.Line < 0 {
		c.Line = 0
	}
	if c.Line >= len(d.lines) {
		c.Line = len(d.lines) - 1
	}
	c.Column = clampColumn(c.Column, len(d.lineRunes(c.Line)), policy)
	return c
}

// selection returns the selection range, ordered, or false when nothing is
// selected.
func (d *bufferDoc) selection() (start, end Cursor, ok bool) {
	if d.anchor == nil {
		return Cursor{}, Cursor{}, false
	}
	start, end = *d.anchor, d.cursor
	if start.Line > end.Line || (start.Line == end.Line && start.Column > end.Column) {
		start, end = end, start
	}
	return start, end, true
}

func (d *bufferDoc) hasSelection() bool {
	_, _, ok := d.selection()
	return ok
}

// deleteRange removes the runes between start and end, ordered, and returns the
// cursor position left behind.
func (d *bufferDoc) deleteRange(start, end Cursor) Cursor {
	if len(d.lines) == 0 {
		return Cursor{}
	}
	start = d.clampPosition(start, ColumnAllowEnd)
	end = d.clampPosition(end, ColumnAllowEnd)
	switch {
	case start.Line == end.Line:
		line := d.lineRunes(start.Line)
		line = append(line[:start.Column:start.Column], line[end.Column:]...)
		d.setLine(start.Line, string(line))
		return start
	case end.Line-start.Line <= maxJoinableLines:
		head := d.lineRunes(start.Line)
		tail := d.lineRunes(end.Line)
		merged := make([]rune, 0, len(head)+len(tail))
		merged = append(merged, head[:start.Column]...)
		merged = append(merged, tail[end.Column:]...)
		d.setLine(start.Line, string(merged))
		d.lines = append(d.lines[:start.Line+1], d.lines[end.Line+1:]...)
		return start
	default:
		// A very large deletion is cheaper to apply by keeping the head line and
		// the tail of the last line than by rebuilding every skipped line.
		head := d.lineRunes(start.Line)
		tail := d.lineRunes(end.Line)
		merged := make([]rune, 0, len(head)+len(tail))
		merged = append(merged, head[:start.Column]...)
		merged = append(merged, tail[end.Column:]...)
		d.setLine(start.Line, string(merged))
		d.lines = append(d.lines[:start.Line+1], d.lines[end.Line:]...)
		return start
	}
}

// maxJoinableLines bounds how many lines one in-place splice keeps in the
// intermediate slice; beyond it the deletion splices from the tail instead.
const maxJoinableLines = 4096

// insertText inserts text at the cursor, splitting lines on newlines, and
// returns the cursor after the inserted text.
func (d *bufferDoc) insertText(c Cursor, text string) Cursor {
	if len(d.lines) == 0 {
		d.lines = []string{""}
	}
	c = d.clampPosition(c, ColumnAllowEnd)
	parts := strings.Split(text, "\n")
	line := d.lineRunes(c.Line)
	head := append([]rune(nil), line[:c.Column]...)
	tail := append([]rune(nil), line[c.Column:]...)
	if len(parts) == 1 {
		inserted := append(head, []rune(parts[0])...)
		inserted = append(inserted, tail...)
		d.setLine(c.Line, string(inserted))
		return Cursor{Line: c.Line, Column: c.Column + len([]rune(parts[0]))}
	}
	first := string(append(head, []rune(parts[0])...))
	last := string(append([]rune(parts[len(parts)-1]), tail...))
	middle := append([]string{first}, parts[1:len(parts)-1]...)
	middle = append(middle, last)
	d.lines = append(d.lines[:c.Line], append(middle, d.lines[c.Line+1:]...)...)
	return Cursor{Line: c.Line + len(parts) - 1, Column: len([]rune(parts[len(parts)-1]))}
}

// deleteLine removes the cursor's line and leaves the cursor on the same index,
// clamped to the last remaining line.
func (d *bufferDoc) deleteLine(c Cursor, policy ColumnPolicy) Cursor {
	if d.single || len(d.lines) <= 1 {
		d.lines = []string{""}
		return Cursor{}
	}
	if c.Line >= len(d.lines) {
		c.Line = len(d.lines) - 1
	}
	d.lines = append(d.lines[:c.Line], d.lines[c.Line+1:]...)
	if c.Line >= len(d.lines) {
		c.Line = len(d.lines) - 1
	}
	return d.clampPosition(Cursor{Line: c.Line, Column: 0}, policy)
}

// joinLine merges the cursor's line with the following line.
func (d *bufferDoc) joinLine(c Cursor, policy ColumnPolicy) (Cursor, bool) {
	if d.single || c.Line >= len(d.lines)-1 {
		return c, false
	}
	head := d.lineRunes(c.Line)
	tail := d.lineRunes(c.Line + 1)
	joined := string(append(append([]rune(nil), head...), tail...))
	d.lines = append(d.lines[:c.Line], append([]string{joined}, d.lines[c.Line+2:]...)...)
	return d.clampPosition(Cursor{Line: c.Line, Column: len(head)}, policy), true
}

// selectionText returns the selected text with newlines between lines.
func (d *bufferDoc) selectionText() (string, bool) {
	start, end, ok := d.selection()
	if !ok {
		return "", false
	}
	if start.Line == end.Line {
		line := d.lineRunes(start.Line)
		return string(line[start.Column:end.Column]), true
	}
	parts := make([]string, 0, end.Line-start.Line+1)
	parts = append(parts, string(d.lineRunes(start.Line)[start.Column:]))
	for i := start.Line + 1; i < end.Line; i++ {
		parts = append(parts, d.line(i))
	}
	parts = append(parts, string(d.lineRunes(end.Line)[:end.Column]))
	return strings.Join(parts, "\n"), true
}

// ---------------------------------------------------------------------------
// Movement
// ---------------------------------------------------------------------------

// move applies a cursor target and count to the document.
func (d *bufferDoc) move(target MoveTarget, count int, policy ColumnPolicy, pageRows int) Cursor {
	c := d.cursor
	switch target {
	case MoveLeft:
		for i := 0; i < count; i++ {
			if c.Column > 0 {
				c.Column--
			}
		}
	case MoveRight:
		for i := 0; i < count; i++ {
			if c.Column < lineLen(d.line(c.Line)) {
				c.Column++
			}
		}
	case MoveUp:
		c.Line -= count
	case MoveDown:
		c.Line += count
	case MoveWordForward:
		for i := 0; i < count; i++ {
			c = advanceWord(d, c, 1)
		}
	case MoveWordBackward:
		for i := 0; i < count; i++ {
			c = advanceWord(d, c, -1)
		}
	case MoveLineStart:
		c.Column = 0
	case MoveLineEnd:
		c.Column = lineLen(d.line(c.Line))
	case MoveDocStart:
		return Cursor{}
	case MoveDocEnd:
		c.Line = len(d.lines) - 1
		c.Column = lineLen(d.line(c.Line))
	case MovePageUp:
		c.Line -= count * pageRows
	case MovePageDown:
		c.Line += count * pageRows
	case MoveLineNumber:
		c.Line = count - 1
	}
	return d.clampPosition(c, policy)
}

// advanceWord moves one word forward or backward. A word is a run of
// non-whitespace runes. Forward movement leaves the current word, skips the
// whitespace after it, and lands on the first character of the next word, or on
// the next line that has one. Backward movement lands on the first character of
// the previous word. Both cross line boundaries, which needs no pattern
// language and no regular expressions.
func advanceWord(d *bufferDoc, c Cursor, direction int) Cursor {
	line, column := c.Line, c.Column
	runes := d.lineRunes(line)
	if direction > 0 {
		column = min(column+1, len(runes))
		for column < len(runes) && isWordRune(runes[column]) {
			column++
		}
		for column < len(runes) && !isWordRune(runes[column]) {
			column++
		}
		if column < len(runes) {
			return Cursor{Line: line, Column: column}
		}
		if line < len(d.lines)-1 {
			return advanceWord(d, Cursor{Line: line + 1}, direction)
		}
		return Cursor{Line: line, Column: len(runes)}
	}
	column = min(column, len(runes)) - 1
	if column < 0 {
		if line == 0 {
			return Cursor{}
		}
		return advanceWord(d, Cursor{Line: line - 1}, direction)
	}
	for column > 0 && !isWordRune(runes[column]) {
		column--
	}
	for column > 0 && isWordRune(runes[column-1]) {
		column--
	}
	return Cursor{Line: line, Column: column}
}

func isWordRune(r rune) bool { return !unicode.IsSpace(r) }

// ---------------------------------------------------------------------------
// Bounds
// ---------------------------------------------------------------------------

// checkDocBounds enforces the buffer bounds of a document.
func (d bufferDoc) checkDocBounds(limits Limits) error {
	if exceeded(int64(len(d.lines)), int64(limits.MaxLines)) {
		return limitExceeded("max_lines", int64(len(d.lines)), int64(limits.MaxLines))
	}
	total := 0
	for _, line := range d.lines {
		total += len(line)
		if exceeded(int64(len(line)), int64(limits.MaxLineBytes)) {
			return limitExceeded("max_line_bytes", int64(len(line)), int64(limits.MaxLineBytes))
		}
	}
	if exceeded(int64(total), int64(limits.MaxBufferBytes)) {
		return limitExceeded("max_buffer_bytes", int64(total), int64(limits.MaxBufferBytes))
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// checkTableBounds enforces the table bounds.
func checkTableBounds(table Table, limits Limits) error {
	if exceeded(int64(len(table.Rows)), int64(limits.MaxRows)) {
		return limitExceeded("max_rows", int64(len(table.Rows)), int64(limits.MaxRows))
	}
	for r, row := range table.Rows {
		for c, cell := range row {
			if exceeded(int64(len(cell)), int64(limits.MaxCellBytes)) {
				return domain.NewLimitError(domain.CodeOutputTooLarge,
					"table cell exceeds the cell limit",
					map[string]string{
						"limit":   "max_cell_bytes",
						"row":     strconv.Itoa(r),
						"column":  strconv.Itoa(c),
						"allowed": strconv.Itoa(limits.MaxCellBytes),
					})
			}
		}
	}
	return nil
}
