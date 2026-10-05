package screen

import (
	"errors"
	"fmt"
)

// Size is a terminal viewport in cells.
type Size struct {
	Cols int
	Rows int
}

// Valid reports whether the size can address a cell.
func (s Size) Valid() bool { return s.Cols > 0 && s.Rows > 0 }

// ErrInvalidSize reports a terminal size that cannot address a cell.
var ErrInvalidSize = errors.New("invalid terminal size")

// ErrInvalidOptions reports unusable model options.
var ErrInvalidOptions = errors.New("invalid screen options")

// DefaultTabWidth is the column interval between tab stops, as in a plain
// xterm.
const DefaultTabWidth = 8

// Options configures the scrollback policy and the tab stops.
type Options struct {
	// ScrollbackLines is how many lines the primary screen keeps after they
	// scroll off the top. Zero keeps none, which is the right policy for a
	// full-screen view that owns its whole screen.
	ScrollbackLines int
	// TabWidth is the tab stop interval in columns; zero selects
	// DefaultTabWidth.
	TabWidth int
}

// Cell is one screen cell. A wide grapheme occupies two cells: the leading
// cell carries Width 2 and the grapheme, and the following cell carries an
// empty grapheme with Width 0.
type Cell struct {
	// Grapheme holds the base rune together with any combining marks that
	// follow it, so a rendered cell is always one displayable unit.
	Grapheme string
	// Width is the number of columns the cell occupies: 2 for the leading cell
	// of a wide grapheme, 1 for an ordinary cell, and 0 for the trailing cell
	// of a wide grapheme.
	Width int
	// Style is the cell's appearance.
	Style Style
}

// blankCell is the value of an unwritten cell.
var blankCell = Cell{Width: 1}

// continuationCell is the trailing column of a grapheme two columns wide. It
// carries no glyph of its own.
var continuationCell = Cell{Width: 0}

// Cursor is the model's cursor position in cell coordinates. Row and Col are
// zero based, like the view coordinates the renderer receives.
type Cursor struct {
	Row     int
	Col     int
	Visible bool
}

// line is one row of the grid. Its cells always number exactly one row of the
// viewport, so a cell's index is its column.
type line struct {
	cells []Cell
	// wrapped reports that the row's content continues on the next row because
	// it filled the viewport width.
	wrapped bool
}

func newLine(cols int) line {
	cells := make([]Cell, cols)
	for i := range cells {
		cells[i] = blankCell
	}
	return line{cells: cells}
}

// clear blanks every cell and drops the wrap flag, which is what switching
// screens or clearing a row requires.
func (ln *line) clear() {
	for i := range ln.cells {
		ln.cells[i] = blankCell
	}
	ln.wrapped = false
}

// put writes a cell of the given width at a column, blanking whatever it
// covers. A grapheme that needs more columns than the row has left keeps the
// width it fits, because a cell can never claim a column outside the row. A
// grapheme two columns wide also leaves a width-zero cell behind it, which is
// how a row records that the following column belongs to the same grapheme.
func (ln *line) put(col, width int, cell Cell) {
	if col < 0 || col >= len(ln.cells) {
		return
	}
	if col+width > len(ln.cells) {
		width = len(ln.cells) - col
		cell.Width = width
	}
	ln.clearSpan(col, width)
	ln.cells[col] = cell
	if width == 2 {
		ln.cells[col+1] = continuationCell
	}
}

// clearSpan blanks the cells covering a column span and repairs the wide cells
// that the span cuts in half: a lead inside the span takes its trailing cell
// with it, and a lead in front of the span shrinks to the one column it still
// covers. Without that repair a row would describe one column twice.
func (ln *line) clearSpan(col, width int) {
	end := min(col+width, len(ln.cells))
	if col > 0 && ln.cells[col-1].Width == 2 {
		ln.cells[col-1].Width = 1
	}
	if col < len(ln.cells) && ln.cells[col].Width == 2 && col+1 < len(ln.cells) {
		ln.cells[col+1] = blankCell
	}
	for i := col; i < end; i++ {
		if i > col && ln.cells[i].Width == 2 && i+1 < len(ln.cells) {
			ln.cells[i+1] = blankCell
		}
		ln.cells[i] = blankCell
	}
}

// attach adds a zero-width mark to the cell that precedes a column, so a
// combining mark joins the grapheme in front of it.
func (ln *line) attach(col int, mark rune) bool {
	for i := col - 1; i >= 0; i-- {
		cell := &ln.cells[i]
		if cell.Width == 0 {
			continue // the trailing half of a wide cell
		}
		if cell.Grapheme == "" {
			return false
		}
		cell.Grapheme += string(mark)
		return true
	}
	return false
}

// text returns the row's content without the trailing halves of wide cells.
func (ln *line) text() string {
	var out []rune
	for _, cell := range ln.cells {
		if cell.Width == 0 {
			continue
		}
		if cell.Grapheme == "" {
			out = append(out, ' ')
			continue
		}
		out = append(out, []rune(cell.Grapheme)...)
	}
	return string(out)
}

// Screen is the owned terminal screen model.
type Screen struct {
	cols   int
	rows   int
	tab    int
	limit  int
	style  Style
	lines  []line
	cursor Cursor
	// pendingWrap records that the last character filled the right margin, so
	// the next character starts a new row.
	pendingWrap bool
	scrollback  [][]Cell
	saved       *savedState
}

// savedState is the primary screen preserved while the alternate screen is
// active, including its scrollback: leaving the alternate screen must restore
// exactly what was there.
type savedState struct {
	lines       []line
	cursor      Cursor
	pendingWrap bool
	scrollback  [][]Cell
}

// New returns a screen of the given size filled with blank cells.
func New(size Size, opts Options) (*Screen, error) {
	if !size.Valid() {
		return nil, fmt.Errorf("%w: %dx%d", ErrInvalidSize, size.Cols, size.Rows)
	}
	if opts.ScrollbackLines < 0 {
		return nil, fmt.Errorf("%w: negative scrollback limit %d", ErrInvalidOptions, opts.ScrollbackLines)
	}
	tab := opts.TabWidth
	if tab == 0 {
		tab = DefaultTabWidth
	}
	if tab < 1 {
		return nil, fmt.Errorf("%w: tab width %d", ErrInvalidOptions, tab)
	}
	lines := make([]line, size.Rows)
	for i := range lines {
		lines[i] = newLine(size.Cols)
	}
	return &Screen{
		cols:   size.Cols,
		rows:   size.Rows,
		tab:    tab,
		limit:  opts.ScrollbackLines,
		lines:  lines,
		cursor: Cursor{Visible: true},
	}, nil
}

// Size returns the viewport size.
func (s *Screen) Size() Size { return Size{Cols: s.cols, Rows: s.rows} }

// Cursor returns the current cursor state.
func (s *Screen) Cursor() Cursor { return s.cursor }

// Row returns a copy of the cells in a row, indexed by column. The copy keeps
// a renderer from modifying the grid.
func (s *Screen) Row(row int) ([]Cell, bool) {
	if row < 0 || row >= len(s.lines) {
		return nil, false
	}
	cells := make([]Cell, len(s.lines[row].cells))
	copy(cells, s.lines[row].cells)
	return cells, true
}

// RowText returns a row's content as text, for transcripts and for tests. It
// reports false for a row outside the viewport.
func (s *Screen) RowText(row int) (string, bool) {
	if row < 0 || row >= len(s.lines) {
		return "", false
	}
	return s.lines[row].text(), true
}

// Scrollback returns the retained lines, oldest first. The caller must not
// modify the rows.
func (s *Screen) Scrollback() [][]Cell { return s.scrollback }

// ClearScrollback drops every retained line.
func (s *Screen) ClearScrollback() { s.scrollback = nil }

// InAlternateScreen reports whether the alternate screen is active.
func (s *Screen) InAlternateScreen() bool { return s.saved != nil }

// EnterAlternateScreen switches to the alternate screen, saving the primary
// screen and its scrollback. The alternate screen never scrolls into
// scrollback: a full-screen view that redraws must not push the shell's own
// output off the record, and on leaving the primary screen is restored exactly.
func (s *Screen) EnterAlternateScreen() {
	if s.saved != nil {
		return
	}
	s.saved = &savedState{
		lines:       s.lines,
		cursor:      s.cursor,
		pendingWrap: s.pendingWrap,
		scrollback:  s.scrollback,
	}
	s.lines = make([]line, s.rows)
	for i := range s.lines {
		s.lines[i] = newLine(s.cols)
	}
	s.scrollback = nil
	s.pendingWrap = false
	s.clampCursor()
}

// LeaveAlternateScreen restores the primary screen. It does nothing when the
// alternate screen is not active, so a session can end a full-screen
// interaction without knowing how it began.
func (s *Screen) LeaveAlternateScreen() {
	if s.saved == nil {
		return
	}
	s.lines = s.saved.lines
	s.cursor = s.saved.cursor
	s.pendingWrap = s.saved.pendingWrap
	s.scrollback = s.saved.scrollback
	s.saved = nil
	s.clampCursor()
}

// Resize changes the viewport size. Rows added or removed come off the bottom,
// as in a terminal that keeps its oldest output in view, and a width change does
// not reflow stored text: rows keep their cells, columns beyond the new width
// are dropped, and a row's wrap flag is cleared, because after a change of width
// the next row is no longer a continuation of it. Full-screen interaction
// redraws on resize, so reflowing the previous frame would only produce a
// different wrong picture; see the open question in the package documentation.
func (s *Screen) Resize(size Size) error {
	if !size.Valid() {
		return fmt.Errorf("%w: %dx%d", ErrInvalidSize, size.Cols, size.Rows)
	}
	for len(s.lines) < size.Rows {
		s.lines = append(s.lines, newLine(size.Cols))
	}
	if len(s.lines) > size.Rows {
		s.lines = s.lines[:size.Rows]
	}
	s.cols, s.rows = size.Cols, size.Rows
	for i := range s.lines {
		s.lines[i].resize(size.Cols)
	}
	s.clampCursor()
	return nil
}

// resize keeps the leading cells of a row and blanks the ones a narrower
// viewport no longer has.
func (ln *line) resize(cols int) {
	if cols == len(ln.cells) {
		return
	}
	cells := make([]Cell, cols)
	for i := range cells {
		if i < len(ln.cells) {
			cells[i] = ln.cells[i]
		} else {
			cells[i] = blankCell
		}
	}
	ln.cells = cells
	if cols > 0 && ln.wrapped {
		// The row filled a width that no longer exists, so it no longer wraps.
		ln.wrapped = false
	}
}

// SetStyle sets the style used for cells written next.
func (s *Screen) SetStyle(style Style) { s.style = style }

// Style returns the style used for cells written next.
func (s *Screen) Style() Style { return s.style }

// SetCursor moves the cursor to a cell, ignoring a position outside the
// viewport, and clears a pending wrap because an explicit move cancels it.
func (s *Screen) SetCursor(row, col int) {
	if row < 0 || row >= s.rows || col < 0 || col >= s.cols {
		return
	}
	s.cursor.Row, s.cursor.Col = row, col
	s.pendingWrap = false
}

// SetCursorVisible shows or hides the cursor. A renderer that emits the cursor
// sequences reads this back from Cursor.
func (s *Screen) SetCursorVisible(visible bool) { s.cursor.Visible = visible }

// Write applies data text to the screen. Only CR, LF, TAB, and BS move the
// cursor; every other control byte is dropped, so data text can never produce
// a terminal control sequence.
func (s *Screen) Write(text string) {
	for _, r := range text {
		s.WriteRune(r)
	}
}

// WriteRune applies one rune of data text.
func (s *Screen) WriteRune(r rune) {
	switch classifyDataRune(r) {
	case dataMotion:
		s.move(r)
	case dataDropped:
	default:
		s.print(r)
	}
}

func (s *Screen) move(r rune) {
	switch r {
	case '\r':
		s.cursor.Col = 0
		s.pendingWrap = false
	case '\n':
		// A newline after the right margin continues on the next row. Treating
		// it that way, instead of leaving the pending wrap set, keeps text that
		// ends in a newline from producing a spurious blank row.
		s.nextRow()
	case '\t':
		// Advance to the next tab stop. Reaching the right margin wraps to the
		// next row, and a wrap ends the tab because no stop follows it.
		for {
			if s.pendingWrap || s.cursor.Col >= s.cols {
				s.nextRow()
				continue
			}
			s.print(' ')
			if s.pendingWrap || s.cursor.Col%s.tab == 0 {
				break
			}
		}
	case '\b':
		s.pendingWrap = false
		if s.cursor.Col > 0 {
			s.cursor.Col--
		}
	}
}

// print writes one printable rune, wrapping as needed.
func (s *Screen) print(r rune) {
	width := RuneWidth(r)
	if width == 0 {
		if s.lines[s.cursor.Row].attach(s.cursor.Col, r) {
			// The mark joined the grapheme in front of the cursor, which means
			// it takes no column of its own.
			s.pendingWrap = false
			return
		}
		// With nothing to join it to, the mark becomes a cell of its own so the
		// data stays visible instead of disappearing.
		width = 1
	}
	if s.pendingWrap || s.cursor.Col+width > s.cols {
		s.nextRow()
	}
	s.lines[s.cursor.Row].put(s.cursor.Col, width, Cell{Grapheme: string(r), Width: width, Style: s.style})
	if s.cursor.Col+width >= s.cols {
		s.cursor.Col = s.cols - 1
		s.pendingWrap = true
		return
	}
	s.cursor.Col += width
}

// nextRow moves to the first column of the row below, scrolling when the
// viewport is full.
func (s *Screen) nextRow() {
	s.pendingWrap = false
	s.cursor.Col = 0
	if s.cursor.Row+1 < s.rows {
		s.cursor.Row++
		return
	}
	s.scrollUp()
	s.cursor.Row = s.rows - 1
}

// scrollUp drops the top row of the viewport. On the primary screen the row
// joins the scrollback, bounded by the configured limit; on the alternate
// screen it is discarded, because a full-screen view owns its whole screen.
func (s *Screen) scrollUp() {
	top := s.lines[0]
	if s.limit > 0 && s.saved == nil {
		cells := make([]Cell, len(top.cells))
		copy(cells, top.cells)
		s.scrollback = append(s.scrollback, cells)
		if overflow := len(s.scrollback) - s.limit; overflow > 0 {
			s.scrollback = append([][]Cell(nil), s.scrollback[overflow:]...)
		}
	}
	last := len(s.lines) - 1
	copy(s.lines, s.lines[1:])
	s.lines[last] = top
	s.lines[last].clear()
}

func (s *Screen) clampCursor() {
	s.cursor.Row = min(max(s.cursor.Row, 0), s.rows-1)
	s.cursor.Col = min(max(s.cursor.Col, 0), s.cols-1)
}
