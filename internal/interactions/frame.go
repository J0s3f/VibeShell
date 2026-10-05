package interactions

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"j0s.at/vibeshell/internal/domain"
)

// replacementRune substitutes every rune that could activate a terminal
// control when it appears as data. It is a visible placeholder, not a
// silent drop, so a user can tell that content was sanitized.
const replacementRune = '�'

// SpanStyle is a semantic style the renderer maps onto terminal attributes.
// The primitives never name a terminal sequence; they name a role.
type SpanStyle uint8

const (
	// StyleDefault is unstyled content.
	StyleDefault SpanStyle = iota
	// StyleSelection marks a selected range.
	StyleSelection
	// StyleCursorLine marks the line the cursor is on.
	StyleCursorLine
	// StyleSelectedRow marks the selected row of a table.
	StyleSelectedRow
	// StyleHeader marks column titles.
	StyleHeader
	// StyleGutter marks line numbers.
	StyleGutter
)

// Span is a styled run of runes on one frame row, addressed in rune columns.
// An empty span carries no style, so a renderer can ignore it.
type Span struct {
	Row   int
	Start int
	End   int
	Style SpanStyle
}

// Frame is the plain-data projection of an interaction: the exact text of each
// row, where the cursor is, and which runs carry which semantic style. It
// contains no escape bytes at all; the trusted renderer decides the sequences.
type Frame struct {
	Rows   []string
	Cursor *Cursor
	Spans  []Span
	Status string
	View   domain.AppView
}

// Text renders the frame rows joined by newlines, for assertions and logs.
func (f Frame) Text() string { return strings.Join(f.Rows, "\n") }

// isDataSafe reports whether a rune may be shown as data without the renderer
// having to escape it. TAB is allowed because the renderer expands it as part of
// ordinary layout; every other C0 control, DEL, and every C1 control (which is
// where eight-bit escape sequences live) is not.
func isDataSafe(r rune) bool {
	switch {
	case r == '\t':
		return true
	case r < 0x20:
		return false
	case r == 0x7f:
		return false
	case r >= 0x80 && r <= 0x9f:
		return false
	default:
		return true
	}
}

// SanitizeData replaces every rune that could activate a terminal control with a
// visible placeholder. Buffer content, cell text, status text, labels, and event
// text all pass through it, so text displayed as data can never activate
// clipboard, hyperlink, title-change, or other unapproved terminal controls
// (PLAN 6.1).
func SanitizeData(s string) string {
	if isAllDataSafe(s) {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	for _, r := range s {
		if isDataSafe(r) {
			out.WriteRune(r)
			continue
		}
		out.WriteRune(replacementRune)
	}
	return out.String()
}

func isAllDataSafe(s string) bool {
	for _, r := range s {
		if !isDataSafe(r) {
			return false
		}
	}
	return true
}

// Frame projects the current state into rows the renderer can draw. It applies
// the viewport, the screen size, the column alignment the view declared, and the
// style spans for the selection and the cursor. Every bound of the frame size is
// enforced here, so an interaction can never ask for more than the screen holds.
func (m *Machine) Frame() (Frame, error) {
	view, err := m.view()
	if err != nil {
		return Frame{}, err
	}
	if m.screen.Rows > m.limits.MaxFrameRows {
		return Frame{}, limitExceeded("max_frame_rows", int64(m.screen.Rows), int64(m.limits.MaxFrameRows))
	}
	if m.screen.Columns > m.limits.MaxFrameColumns {
		return Frame{}, limitExceeded("max_frame_columns", int64(m.screen.Columns), int64(m.limits.MaxFrameColumns))
	}
	builder := frameBuilder{
		machine:  m,
		columns:  m.screen.Columns,
		maxRows:  m.pageRows(),
		viewport: m.state.Viewport,
	}
	switch m.spec.View.Mode {
	case domain.AppViewModeTable, domain.AppViewModeStatus:
		builder.buildTable()
	case domain.AppViewModeForm:
		builder.buildForm()
	default:
		builder.buildText()
	}
	frame := Frame{
		Rows:   builder.rows,
		Spans:  builder.spans,
		Cursor: builder.cursor,
		View:   view,
	}
	frame.Status = m.statusText()
	if err := frame.Validate(m.limits); err != nil {
		return Frame{}, err
	}
	return frame, nil
}

// frameBuilder accumulates rows and spans for one frame.
type frameBuilder struct {
	machine  *Machine
	viewport domain.Viewport
	columns  int
	maxRows  int
	rows     []string
	spans    []Span
	cursor   *Cursor
}

func (b *frameBuilder) addRow(text string) int {
	if len(b.rows) >= b.maxRows {
		return -1
	}
	b.rows = append(b.rows, b.clip(SanitizeData(text)))
	return len(b.rows) - 1
}

func (b *frameBuilder) addSpan(row, start, end int, style SpanStyle) {
	if row < 0 || start < 0 || end <= start || style == StyleDefault {
		return
	}
	b.spans = append(b.spans, Span{Row: row, Start: start, End: end, Style: style})
}

// clip cuts a row to the screen width in rune columns. Cell-width handling for
// wide characters and combining marks belongs to the renderer's screen model;
// this package only guarantees that no row exceeds the declared column count.
func (b *frameBuilder) clip(text string) string {
	runes := []rune(text)
	if len(runes) <= b.columns {
		return text
	}
	return string(runes[:b.columns])
}

// buildText lays out text, editor, and pager views.
func (b *frameBuilder) buildText() {
	m := b.machine
	doc := m.editDoc()
	gutter := m.spec.View.Gutter
	width := 0
	if gutter {
		width = len(strconv.Itoa(max(doc.cursor.Line, len(doc.lines)-1) + 1))
	}
	selectionStart, selectionEnd, hasSelection := doc.selection()
	cursorVisible := b.cursorVisible()
	cursorLine := doc.cursor.Line
	if cursorLine < b.viewport.Top || cursorLine >= b.viewport.Top+b.maxRows {
		// A scrolled-away cursor is not drawn; the status line still reports it.
		cursorVisible = false
	}
	for line := b.viewport.Top; line < len(doc.lines) && line < b.viewport.Top+b.maxRows; line++ {
		text := doc.line(line)
		prefix := ""
		offset := 0
		if gutter {
			prefix = padStart(strconv.Itoa(line+1), width) + " "
			offset = width + 1
		}
		row := b.addRow(prefix + text)
		if row < 0 {
			return
		}
		if gutter {
			b.addSpan(row, 0, offset, StyleGutter)
		}
		if line == cursorLine && hasSelection {
			b.addSpan(row, offset, offset+lineLen(text), StyleCursorLine)
		}
		if !hasSelection {
			continue
		}
		from, to := columnAt(selectionStart, line), columnAt(selectionEnd, line)
		if from == to {
			continue
		}
		b.addSpan(row, offset+from, offset+to, StyleSelection)
	}
	if !cursorVisible {
		return
	}
	screenRow := cursorLine - b.viewport.Top
	if screenRow < 0 || screenRow >= len(b.rows) {
		return
	}
	cursorColumn := doc.cursor.Column
	if gutter {
		cursorColumn += width + 1
	}
	b.cursor = &Cursor{Line: screenRow, Column: cursorColumn}
}

// cursorVisible reports whether this view mode draws a cursor. Editor, table,
// status, and form views always have a focus, so they draw one. A text view and a
// pager do not, unless the view declares otherwise.
func (b *frameBuilder) cursorVisible() bool {
	switch b.machine.spec.View.Mode {
	case domain.AppViewModeEditor, domain.AppViewModeTable, domain.AppViewModeStatus, domain.AppViewModeForm:
		return true
	default:
		return b.machine.spec.View.CursorVisible
	}
}

// columnAt is the selection column on one line; a range spanning other lines
// covers the whole line.
func columnAt(at Cursor, line int) int {
	switch {
	case line < at.Line:
		return 0
	case line > at.Line:
		return -1
	default:
		return at.Column
	}
}

// buildTable lays out table and status views: a column title row, the visible
// data rows, and an optional footer.
func (b *frameBuilder) buildTable() {
	m := b.machine
	columns := m.spec.View.Columns
	rows := m.state.Table.Rows
	widths := columnWidths(columns, rows)
	header := formatCells(columnTitles(columns), columns, widths)
	if row := b.addRow(header); row >= 0 {
		b.addSpan(row, 0, len([]rune(header)), StyleHeader)
	}
	anchor := m.state.Table.Anchor
	for line := b.viewport.Top; line < len(rows) && line < b.viewport.Top+b.maxRows; line++ {
		text := formatCells(rowAt(rows, line, len(columns)), columns, widths)
		row := b.addRow(text)
		if row < 0 {
			return
		}
		if anchor != nil && line == m.state.Table.Cursor.Line {
			b.addSpan(row, 0, len([]rune(text)), StyleSelectedRow)
		}
	}
	if m.spec.View.Footer != "" {
		b.addRow(m.spec.View.Footer)
	}
	screenRow := m.state.Table.Cursor.Line - b.viewport.Top
	if screenRow < 0 || screenRow >= len(b.rows) {
		return
	}
	// The first frame row is the column title row, so data starts one below it.
	b.cursor = &Cursor{Line: screenRow + 1, Column: m.state.Table.Cursor.Column}
}

// columnWidths is the widest cell per column, so columns stay aligned as the
// user scrolls and the screen resizes. A column is never narrower than its
// title.
func columnWidths(columns []Column, rows [][]string) []int {
	widths := make([]int, len(columns))
	for i, column := range columns {
		widths[i] = len([]rune(column.Title))
	}
	for _, row := range rows {
		for i := range widths {
			if i < len(row) {
				if width := len([]rune(row[i])); width > widths[i] {
					widths[i] = width
				}
			}
		}
	}
	return widths
}

// buildForm lays out a form: one row per field, label and value, with the
// focused field marked.
func (b *frameBuilder) buildForm() {
	m := b.machine
	width := 0
	for _, field := range m.spec.View.Fields {
		if len([]rune(fieldLabel(field))) > width {
			width = len([]rune(fieldLabel(field)))
		}
	}
	for index, field := range m.spec.View.Fields {
		value := ""
		if index < len(m.state.Fields) {
			value = m.state.Fields[index]
		}
		text := padEnd(fieldLabel(field), width) + ": " + value
		row := b.addRow(text)
		if row < 0 {
			return
		}
		if index == m.state.Focus && field.Editable {
			b.addSpan(row, width+2, width+2+lineLen(value), StyleSelectedRow)
		}
	}
	doc, err := m.fieldDoc()
	if err != nil {
		return
	}
	if m.state.Focus < 0 || m.state.Focus >= len(m.spec.View.Fields) {
		return
	}
	if !m.spec.View.Fields[m.state.Focus].Editable {
		return
	}
	// The focused field's row may be scrolled out of the window; the renderer
	// still receives the row only when it is visible.
	screenRow := m.state.Focus - b.viewport.Top
	if screenRow < 0 || screenRow >= len(b.rows) {
		return
	}
	b.cursor = &Cursor{Line: screenRow, Column: width + 2 + doc.cursor.Column}
}

func fieldLabel(field Field) string {
	if field.Label != "" {
		return field.Label
	}
	return field.Name
}

func columnTitles(columns []Column) []string {
	titles := make([]string, len(columns))
	for i, column := range columns {
		titles[i] = column.Title
	}
	return titles
}

// formatCells lays out one row with the declared column widths and alignments,
// padding short cells so a ragged data set still reads as a table.
func formatCells(cells []string, columns []Column, widths []int) string {
	parts := make([]string, len(columns))
	for i := range columns {
		cell := ""
		if i < len(cells) {
			cell = SanitizeData(cells[i])
		}
		switch columns[i].Align {
		case AlignRight:
			parts[i] = padStart(cell, widths[i])
		case AlignCenter:
			parts[i] = padCenter(cell, widths[i])
		default:
			parts[i] = padEnd(cell, widths[i])
		}
	}
	return strings.TrimRight(strings.Join(parts, " "), " ")
}

// rowAt returns a data row padded to the column count, so a short handler row
// cannot shift every later column.
func rowAt(rows [][]string, index, columns int) []string {
	row := rows[index]
	if len(row) == columns {
		return row
	}
	padded := make([]string, columns)
	copy(padded, row)
	return padded
}

func padStart(s string, width int) string {
	gap := width - len([]rune(s))
	if gap <= 0 {
		return s
	}
	return strings.Repeat(" ", gap) + s
}

func padEnd(s string, width int) string {
	gap := width - len([]rune(s))
	if gap <= 0 {
		return s
	}
	return s + strings.Repeat(" ", gap)
}

func padCenter(s string, width int) string {
	gap := width - len([]rune(s))
	if gap <= 0 {
		return s
	}
	left := gap / 2
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", gap-left)
}

// Validate checks that a frame is safe to hand to the trusted renderer: no row
// carries a control byte, no row exceeds the screen width, and the frame fits
// the configured bounds.
func (f Frame) Validate(limits Limits) error {
	if exceeded(int64(len(f.Rows)), int64(limits.MaxFrameRows)) {
		return limitExceeded("max_frame_rows", int64(len(f.Rows)), int64(limits.MaxFrameRows))
	}
	for index, row := range f.Rows {
		if !utf8.ValidString(row) {
			return domain.NewValidationError(domain.CodeInvalidAppView,
				"frame row is not valid UTF-8", map[string]string{"row": strconv.Itoa(index)})
		}
		for _, r := range row {
			if !isDataSafe(r) {
				return domain.NewValidationError(domain.CodeInvalidAppView,
					"frame row contains a control character",
					map[string]string{
						"row":  strconv.Itoa(index),
						"rune": fmt.Sprintf("U+%04X", r),
					})
			}
		}
	}
	for _, span := range f.Spans {
		if span.Row < 0 || span.Row >= len(f.Rows) {
			return domain.NewValidationError(domain.CodeInvalidAppView,
				"style span points outside the frame", map[string]string{"row": strconv.Itoa(span.Row)})
		}
		if span.End > len([]rune(f.Rows[span.Row])) {
			return domain.NewValidationError(domain.CodeInvalidAppView,
				"style span exceeds its row", map[string]string{"row": strconv.Itoa(span.Row)})
		}
	}
	return nil
}
