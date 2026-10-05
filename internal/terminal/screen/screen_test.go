package screen

import (
	"errors"
	"strings"
	"testing"
)

func newTestScreen(t *testing.T, size Size, opts Options) *Screen {
	t.Helper()
	model, err := New(size, opts)
	if err != nil {
		t.Fatalf("New(%dx%d): %v", size.Cols, size.Rows, err)
	}
	return model
}

// rows returns every row's text, with trailing blanks trimmed, so a failure
// shows the visible screen.
func rows(model *Screen) []string {
	var out []string
	for row := range model.Size().Rows {
		text, _ := model.RowText(row)
		out = append(out, strings.TrimRight(text, " "))
	}
	return out
}

func assertRows(t *testing.T, model *Screen, want ...string) {
	t.Helper()
	got := rows(model)
	for len(got) < len(want) {
		got = append(got, "")
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %q, want %q (screen: %q)", i, got[i], want[i], got)
		}
	}
}

func TestNewRejectsUnusableSizesAndOptions(t *testing.T) {
	for _, size := range []Size{{}, {Cols: 0, Rows: 5}, {Cols: 5, Rows: 0}, {Cols: -1, Rows: 5}} {
		if _, err := New(size, Options{}); !errors.Is(err, ErrInvalidSize) {
			t.Fatalf("New(%v) error = %v, want ErrInvalidSize", size, err)
		}
	}
	if _, err := New(Size{Cols: 4, Rows: 2}, Options{ScrollbackLines: -1}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatal("a negative scrollback limit must be refused")
	}
	if _, err := New(Size{Cols: 4, Rows: 2}, Options{TabWidth: -2}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatal("a negative tab width must be refused")
	}
}

func TestWritePlacesTextAndAdvancesTheCursor(t *testing.T) {
	model := newTestScreen(t, Size{Cols: 10, Rows: 3}, Options{})
	model.Write("hello")
	assertRows(t, model, "hello")
	if cursor := model.Cursor(); cursor.Row != 0 || cursor.Col != 5 {
		t.Fatalf("cursor = %d,%d, want 0,5", cursor.Row, cursor.Col)
	}
}

func TestWriteDropsEveryControlThatIsNotCursorMotion(t *testing.T) {
	model := newTestScreen(t, Size{Cols: 40, Rows: 2}, Options{})
	// A colour change, a title, a clipboard write, and a hyperlink: none of them
	// may reach the cells.
	model.Write("\x1b[31mred\x1b[0m\x1b]0;title\x07\x1b]52;c;cGF5bG9hZA==\x07\x1b]8;;http://example.test\x07link\x1b]8;;\x07")
	assertRows(t, model, "[31mred[0m]0;title]52;c;cGF5bG9hZA==]8;;",
		"http://example.testlink]8;;")
	// The C1 forms of the introducers are dropped as well.
	model.Write("\u009b1m\u009dtitle\u009c")
	assertRows(t, model, "[31mred[0m]0;title]52;c;cGF5bG9hZA==]8;;",
		"http://example.testlink]8;;1mtitle")
}

func TestWideGraphemeOccupiesTwoColumns(t *testing.T) {
	model := newTestScreen(t, Size{Cols: 6, Rows: 1}, Options{})
	model.Write("世")
	cells, _ := model.Row(0)
	if cells[0].Grapheme != "世" || cells[0].Width != 2 {
		t.Fatalf("cell 0 = %+v, want the wide grapheme with width 2", cells[0])
	}
	if cells[1].Width != 0 || cells[1].Grapheme != "" {
		t.Fatalf("cell 1 = %+v, want the trailing half of the wide cell", cells[1])
	}
	if cursor := model.Cursor(); cursor.Col != 2 {
		t.Fatalf("cursor column = %d, want 2", cursor.Col)
	}
}

func TestWideGraphemeWrapsBeforeItWouldOverflow(t *testing.T) {
	// One column is left, which is not enough for a grapheme two columns wide.
	model := newTestScreen(t, Size{Cols: 2, Rows: 2}, Options{})
	model.Write("a世")
	assertRows(t, model, "a", "世")
	if model.lines[0].wrapped {
		t.Fatal("the first row must not be marked as continuing on the next one")
	}
}

func TestWideGraphemeFillsTheRowExactly(t *testing.T) {
	model := newTestScreen(t, Size{Cols: 3, Rows: 2}, Options{})
	model.Write("a世b")
	assertRows(t, model, "a世", "b")
}

func TestCombiningMarkJoinsTheCellInFrontOfIt(t *testing.T) {
	model := newTestScreen(t, Size{Cols: 6, Rows: 1}, Options{})
	model.Write("e\u0301x")
	cells, _ := model.Row(0)
	if cells[0].Grapheme != "e\u0301" || cells[0].Width != 1 {
		t.Fatalf("cell 0 = %+v, want the base rune with its mark", cells[0])
	}
	if cells[1].Grapheme != "x" {
		t.Fatalf("cell 1 = %+v, want x", cells[1])
	}
	if cursor := model.Cursor(); cursor.Col != 2 {
		t.Fatalf("cursor column = %d, want 2: a combining mark takes no column", cursor.Col)
	}
}

func TestCombiningMarkWithNothingToJoinBecomesACell(t *testing.T) {
	model := newTestScreen(t, Size{Cols: 4, Rows: 1}, Options{})
	model.Write("\u0301a")
	cells, _ := model.Row(0)
	if cells[0].Grapheme != "\u0301" || cells[0].Width != 1 {
		t.Fatalf("cell 0 = %+v, want the mark visible in its own cell", cells[0])
	}
	if text, _ := model.RowText(0); text != "\u0301a  " {
		t.Fatalf("row text = %q", text)
	}
}

func TestOverwritingPartOfAWideCellRepairsTheGrid(t *testing.T) {
	model := newTestScreen(t, Size{Cols: 6, Rows: 1}, Options{})
	model.Write("世界ab")
	assertRows(t, model, "世界ab")

	// Overwriting the trailing half of a wide cell shrinks the cell to the one
	// column it still covers, so the row never describes a column twice.
	model.SetCursor(0, 1)
	model.WriteRune('x')
	cells, _ := model.Row(0)
	if cells[0].Width != 1 || cells[1].Grapheme != "x" {
		t.Fatalf("cells = %+v, want a repaired lead and x in its place", cells[:2])
	}
	assertRows(t, model, "世x界ab")

	// Overwriting the leading half leaves the ordinary cell that replaced it.
	model.SetCursor(0, 0)
	model.WriteRune('y')
	assertRows(t, model, "yx界ab")
}

func TestOverwritingASingleCellClearsTheWideCellBehindIt(t *testing.T) {
	model := newTestScreen(t, Size{Cols: 6, Rows: 1}, Options{})
	model.Write("世界ab")
	model.SetCursor(0, 1)
	model.Write("A")
	cells, _ := model.Row(0)
	if cells[0].Width != 1 || cells[0].Grapheme != "世" {
		t.Fatalf("cell 0 = %+v, want the wide grapheme reduced to one column", cells[0])
	}
	if cells[2].Grapheme != "界" || cells[2].Width != 2 || cells[3].Width != 0 {
		t.Fatalf("cells 2 and 3 = %+v, want the second wide grapheme intact", cells[2:4])
	}
	assertRows(t, model, "世A界ab")
}

func TestTabAdvancesToTheNextStop(t *testing.T) {
	model := newTestScreen(t, Size{Cols: 20, Rows: 1}, Options{})
	model.Write("a\tb\tc")
	assertRows(t, model, "a       b       c")
	model = newTestScreen(t, Size{Cols: 20, Rows: 1}, Options{TabWidth: 4})
	model.Write("a\tb")
	assertRows(t, model, "a   b")
}

func TestTabWrapsAtTheRightMargin(t *testing.T) {
	model := newTestScreen(t, Size{Cols: 4, Rows: 2}, Options{TabWidth: 4})
	model.Write("ab\tc")
	assertRows(t, model, "ab", "c")
}

func TestCarriageReturnAndLineFeedMoveTheCursor(t *testing.T) {
	model := newTestScreen(t, Size{Cols: 6, Rows: 3}, Options{})
	model.Write("abc\rX")
	assertRows(t, model, "Xbc", "")
	model = newTestScreen(t, Size{Cols: 6, Rows: 3}, Options{})
	model.Write("a\nb")
	assertRows(t, model, "a", "b")
	model = newTestScreen(t, Size{Cols: 6, Rows: 3}, Options{})
	model.Write("abc\r\nd")
	assertRows(t, model, "abc", "d")
}

func TestWritingTheLastColumnDefersTheWrap(t *testing.T) {
	model := newTestScreen(t, Size{Cols: 3, Rows: 2}, Options{})
	model.Write("abc")
	if cursor := model.Cursor(); cursor.Row != 0 || cursor.Col != 2 {
		t.Fatalf("cursor = %d,%d, want 0,2 after filling the row", cursor.Row, cursor.Col)
	}
	model.Write("d")
	assertRows(t, model, "abc", "d")
}

func TestNewlineAfterTheLastColumnDoesNotAddABlankRow(t *testing.T) {
	model := newTestScreen(t, Size{Cols: 3, Rows: 3}, Options{})
	model.Write("abc\n")
	assertRows(t, model, "abc")
	if cursor := model.Cursor(); cursor.Row != 1 || cursor.Col != 0 {
		t.Fatalf("cursor = %d,%d, want 1,0", cursor.Row, cursor.Col)
	}
}

func TestBackspaceMovesTheCursorAndOverwrites(t *testing.T) {
	model := newTestScreen(t, Size{Cols: 6, Rows: 1}, Options{})
	model.Write("abc\b\bX")
	assertRows(t, model, "aXc")
}

func TestScrollbackKeepsScrolledLinesWithinItsLimit(t *testing.T) {
	// A four-column viewport wraps "three", so the scrollback holds the rows
	// the screen actually showed.
	model := newTestScreen(t, Size{Cols: 4, Rows: 2}, Options{ScrollbackLines: 3})
	model.Write("one\ntwo\nthree\nfour\n")
	assertRows(t, model, "four", "")
	scrollback := model.Scrollback()
	if len(scrollback) != 3 {
		t.Fatalf("scrollback holds %d lines, want the limit of 3", len(scrollback))
	}
	var texts []string
	for _, cells := range scrollback {
		var b strings.Builder
		for _, cell := range cells {
			if cell.Width > 0 {
				b.WriteString(cell.Grapheme)
			}
		}
		texts = append(texts, strings.TrimRight(b.String(), " "))
	}
	if want := []string{"two", "thre", "e"}; strings.Join(texts, ",") != strings.Join(want, ",") {
		t.Fatalf("scrollback = %q, want %q: the oldest line is dropped first", texts, want)
	}
	model.ClearScrollback()
	if len(model.Scrollback()) != 0 {
		t.Fatal("ClearScrollback left lines behind")
	}
}

func TestNoScrollbackKeepsNothingByDefault(t *testing.T) {
	model := newTestScreen(t, Size{Cols: 4, Rows: 1}, Options{})
	model.Write("one\ntwo")
	if len(model.Scrollback()) != 0 {
		t.Fatalf("scrollback holds %d lines, want none", len(model.Scrollback()))
	}
	assertRows(t, model, "two")
}

func TestATrailingNewlineScrollsAFullViewport(t *testing.T) {
	// One row holds one line, so a line ending in a newline leaves the screen
	// scrolled, which is what a terminal does and what makes the prompt appear
	// on a clear row.
	model := newTestScreen(t, Size{Cols: 4, Rows: 1}, Options{})
	model.Write("one\n")
	assertRows(t, model, "")
	if cursor := model.Cursor(); cursor.Row != 0 || cursor.Col != 0 {
		t.Fatalf("cursor = %d,%d, want 0,0", cursor.Row, cursor.Col)
	}
}

func TestAlternateScreenRestoresThePrimaryScreen(t *testing.T) {
	model := newTestScreen(t, Size{Cols: 6, Rows: 2}, Options{ScrollbackLines: 4})
	model.Write("shell\noutput")
	before, _ := model.Row(0)
	beforeCursor := model.Cursor()
	beforeScrollback := len(model.Scrollback())

	model.EnterAlternateScreen()
	if !model.InAlternateScreen() {
		t.Fatal("EnterAlternateScreen did not switch screens")
	}
	if text, _ := model.RowText(0); text != "      " {
		t.Fatalf("the alternate screen started with %q, want a blank screen", text)
	}
	model.Write("pager\n")
	if len(model.Scrollback()) != 0 {
		t.Fatal("the alternate screen added to the scrollback")
	}

	model.LeaveAlternateScreen()
	if model.InAlternateScreen() {
		t.Fatal("LeaveAlternateScreen did not switch back")
	}
	after, _ := model.Row(0)
	if len(after) != len(before) {
		t.Fatalf("row length changed: %d, want %d", len(after), len(before))
	}
	for i := range before {
		if after[i] != before[i] {
			t.Fatalf("cell %d = %+v, want %+v", i, after[i], before[i])
		}
	}
	if model.Cursor() != beforeCursor {
		t.Fatalf("cursor = %+v, want %+v", model.Cursor(), beforeCursor)
	}
	if len(model.Scrollback()) != beforeScrollback {
		t.Fatalf("scrollback holds %d lines, want %d", len(model.Scrollback()), beforeScrollback)
	}
	// Both switches are idempotent, so a session may end an interaction without
	// knowing how it began.
	model.LeaveAlternateScreen()
	model.EnterAlternateScreen()
	model.EnterAlternateScreen()
	if text, _ := model.RowText(0); text != "      " {
		t.Fatalf("a repeated switch produced %q", text)
	}
}

func TestScrollingOnTheAlternateScreenDiscardsTheTopRow(t *testing.T) {
	model := newTestScreen(t, Size{Cols: 4, Rows: 2}, Options{ScrollbackLines: 4})
	model.Write("a\nb")
	model.EnterAlternateScreen()
	model.Write("1\n2\n3\n")
	assertRows(t, model, "3")
	if len(model.Scrollback()) != 0 {
		t.Fatalf("the alternate screen kept %d scrollback lines", len(model.Scrollback()))
	}
}

func TestResizeKeepsContentAndClampsTheCursor(t *testing.T) {
	model := newTestScreen(t, Size{Cols: 8, Rows: 3}, Options{ScrollbackLines: 4})
	model.Write("one\ntwo\nthree")
	if err := model.Resize(Size{Cols: 8, Rows: 5}); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	assertRows(t, model, "one", "two", "three", "", "")
	// Rows come off the bottom, as in a terminal that keeps its oldest output
	// in view, and the scrollback is untouched by a viewport change.
	if err := model.Resize(Size{Cols: 8, Rows: 2}); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	assertRows(t, model, "one", "two")
	if len(model.Scrollback()) != 0 {
		t.Fatalf("a viewport change added %d scrollback lines", len(model.Scrollback()))
	}
	// A narrower viewport drops the columns it no longer has.
	if err := model.Resize(Size{Cols: 2, Rows: 1}); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	assertRows(t, model, "on")
	if err := model.Resize(Size{Cols: 0, Rows: 1}); !errors.Is(err, ErrInvalidSize) {
		t.Fatalf("Resize error = %v, want ErrInvalidSize", err)
	}
	if size := model.Size(); size.Cols != 2 || size.Rows != 1 {
		t.Fatalf("Size = %v after a refused resize, want the previous size", size)
	}
	if cursor := model.Cursor(); cursor.Row != 0 || cursor.Col > 1 {
		t.Fatalf("cursor = %d,%d after a resize, want it inside the viewport", cursor.Row, cursor.Col)
	}
}

func TestResizeDropsCellsBeyondTheNewWidth(t *testing.T) {
	model := newTestScreen(t, Size{Cols: 8, Rows: 1}, Options{})
	model.Write("abcdefgh")
	if err := model.Resize(Size{Cols: 3, Rows: 1}); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	assertRows(t, model, "abc")
	cells, _ := model.Row(0)
	if len(cells) != 3 {
		t.Fatalf("row has %d cells, want 3", len(cells))
	}
}

func TestRowReturnsACopyOfTheCells(t *testing.T) {
	model := newTestScreen(t, Size{Cols: 4, Rows: 1}, Options{})
	model.Write("ab")
	cells, ok := model.Row(0)
	if !ok {
		t.Fatal("Row(0) reported no row")
	}
	cells[0] = Cell{Grapheme: "X", Width: 1}
	again, _ := model.Row(0)
	if again[0].Grapheme != "a" {
		t.Fatalf("cell 0 = %+v after modifying the copy, want the original", again[0])
	}
	if _, ok := model.Row(5); ok {
		t.Fatal("Row(5) reported a row outside the viewport")
	}
}

func TestSanitizeTextKeepsMotionAndDropsSequences(t *testing.T) {
	got := SanitizeText("a\x1b[31mb\x1b]0;title\x07c\u009b\u009cd\x00e")
	if want := "a[31mb]0;titlecde"; got != want {
		t.Fatalf("SanitizeText = %q, want %q", got, want)
	}
	if got := SanitizeText("keep\tthese\r\n"); got != "keep\tthese\r\n" {
		t.Fatalf("SanitizeText = %q, want the cursor-motion controls kept", got)
	}
}

func TestSanitizeTextIsIdempotent(t *testing.T) {
	for _, text := range []string{"", "plain", "\x1b[1m\x1b[0m", "\u009b31m", "a\x00b"} {
		once := SanitizeText(text)
		if twice := SanitizeText(once); twice != once {
			t.Fatalf("SanitizeText(%q) = %q then %q, want a stable result", text, once, twice)
		}
	}
}

func TestNormalizeLineFeedsReturnsToColumnZero(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no newline", "plain text", "plain text"},
		{"bare lf", "a\nb", "a\r\nb"},
		{"already crlf", "a\r\nb", "a\r\nb"},
		{"trailing lf", "a\n", "a\r\n"},
		{"leading lf", "\na", "\r\na"},
		{"multiple", "a\nb\nc", "a\r\nb\r\nc"},
		{"bare cr kept", "a\rb", "a\rb"},
		{"crlf then lf", "a\r\n\nb", "a\r\n\r\nb"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeLineFeeds(tc.in); got != tc.want {
				t.Fatalf("NormalizeLineFeeds(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRuneWidth(t *testing.T) {
	tests := []struct {
		r    rune
		want int
	}{
		{r: 'a', want: 1},
		{r: '世', want: 2},
		{r: '한', want: 2},
		{r: '\u0301', want: 0},
		{r: '\u200b', want: 0},
		{r: '\ufe0f', want: 0},
		{r: '\u1160', want: 0},
		{r: '\u3000', want: 2},
		{r: '\uff21', want: 2},
		{r: '\u00e9', want: 1},
	}
	for _, test := range tests {
		if got := RuneWidth(test.r); got != test.want {
			t.Fatalf("RuneWidth(%q) = %d, want %d", test.r, got, test.want)
		}
	}
}

func TestWrapTextLaysOutRowsAndTheCursor(t *testing.T) {
	layout := WrapText("hello world", 5, DefaultTabWidth, 5)
	if strings.Join(layout.Rows, "|") != "hello| worl|d" {
		t.Fatalf("rows = %v, want three wrapped rows", layout.Rows)
	}
	// Index 5 is the space after a full row, and a cursor that has filled the
	// right margin stays on its last column.
	if layout.CursorRow != 0 || layout.CursorCol != 4 || !layout.CursorOnRow {
		t.Fatalf("cursor = %d,%d (on row %t), want 0,4", layout.CursorRow, layout.CursorCol, layout.CursorOnRow)
	}
	layout = WrapText("hello world", 5, DefaultTabWidth, 7)
	if layout.CursorRow != 1 || layout.CursorCol != 2 {
		t.Fatalf("cursor = %d,%d, want 1,2", layout.CursorRow, layout.CursorCol)
	}
	layout = WrapText("ab\tcd", 10, DefaultTabWidth, 3)
	if layout.Rows[0] != "ab      cd" {
		t.Fatalf("row 0 = %q, want the tab expanded to the next stop", layout.Rows[0])
	}
	// A tab whose stop is the right margin fills the row and lets the next
	// character wrap.
	layout = WrapText("ab\tcd", 8, DefaultTabWidth, 2)
	if strings.Join(layout.Rows, "|") != "ab      |cd" {
		t.Fatalf("rows = %v, want the tab to fill the first row", layout.Rows)
	}
	layout = WrapText("世x", 3, DefaultTabWidth, 1)
	if layout.Rows[0] != "世x" || layout.CursorCol != 2 {
		t.Fatalf("rows = %v, cursor = %d, want the wide grapheme to take two columns", layout.Rows, layout.CursorCol)
	}
	// A wide grapheme that does not fit moves to the next row whole.
	layout = WrapText("世x", 2, DefaultTabWidth, 0)
	if strings.Join(layout.Rows, "|") != "世|x" {
		t.Fatalf("rows = %v, want the wide grapheme on its own row", layout.Rows)
	}
	layout = WrapText("", 4, DefaultTabWidth, 0)
	if len(layout.Rows) != 1 || layout.Rows[0] != "" || !layout.CursorOnRow {
		t.Fatalf("empty text laid out as %v", layout.Rows)
	}
	layout = WrapText("ab\ncd", 4, DefaultTabWidth, 0)
	if strings.Join(layout.Rows, "|") != "ab|cd" {
		t.Fatalf("rows = %v, want one row per line", layout.Rows)
	}
}

func TestAppendSGREmitsOnlyTheDifference(t *testing.T) {
	var out []byte
	out = AppendSGR(out, DefaultStyle(), Style{Bold: true, FG: PaletteColor(2)})
	if got, want := string(out), "\x1b[1;38;5;2m"; got != want {
		t.Fatalf("AppendSGR = %q, want %q", got, want)
	}
	out = AppendSGR(nil, Style{Bold: true, FG: PaletteColor(2)}, Style{Bold: true, FG: PaletteColor(2)})
	if len(out) != 0 {
		t.Fatalf("AppendSGR = %q for an unchanged style, want nothing", out)
	}
	out = AppendSGR(nil, Style{Bold: true, Underline: true, BG: RGBColor(1, 2, 3)}, DefaultStyle())
	if got, want := string(out), "\x1b[22;24;49m"; got != want {
		t.Fatalf("AppendSGR = %q, want %q", got, want)
	}
	out = AppendSGR(nil, DefaultStyle(), Style{BG: RGBColor(1, 2, 3)})
	if got, want := string(out), "\x1b[48;2;1;2;3m"; got != want {
		t.Fatalf("AppendSGR = %q, want %q", got, want)
	}
	out = AppendSGR(nil, Style{Italic: true, Reverse: true}, DefaultStyle())
	if got, want := string(out), "\x1b[23;27m"; got != want {
		t.Fatalf("AppendSGR = %q, want %q", got, want)
	}
}

func TestAppendCursorPosition(t *testing.T) {
	if got, want := string(AppendCursorPosition(nil, 0, 0)), "\x1b[1;1H"; got != want {
		t.Fatalf("AppendCursorPosition = %q, want %q", got, want)
	}
	if got, want := string(AppendCursorPosition(nil, 23, 79)), "\x1b[24;80H"; got != want {
		t.Fatalf("AppendCursorPosition = %q, want %q", got, want)
	}
}

func TestSetCursorIgnoresPositionsOutsideTheViewport(t *testing.T) {
	model := newTestScreen(t, Size{Cols: 4, Rows: 2}, Options{})
	model.SetCursor(9, 9)
	if cursor := model.Cursor(); cursor.Row != 0 || cursor.Col != 0 {
		t.Fatalf("cursor = %d,%d, want the origin", cursor.Row, cursor.Col)
	}
	model.SetCursor(1, 2)
	model.Write("x")
	assertRows(t, model, "", "  x")
}
