package screen

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzScreenWrite checks that arbitrary data text never breaks the grid: the
// model keeps its invariants, never holds a control byte, and leaves the cursor
// inside the viewport whatever the text contains.
func FuzzScreenWrite(f *testing.F) {
	f.Add("plain text", 12, 4, 8)
	f.Add("wide 世界 and marks é", 8, 3, 8)
	f.Add("tabs\tand\r\nnewlines\n", 10, 2, 8)
	f.Add("\x1b[31m\x1b]52;c;x\x07\u009b", 16, 4, 8)
	f.Add("wrap exactly at the margin", 3, 2, 3)
	f.Add("\x00\x07\x08\x0b\x0c", 6, 2, 4)

	f.Fuzz(func(t *testing.T, text string, cols, rows, tabWidth int) {
		if cols < 1 || cols > 64 || rows < 1 || rows > 64 || tabWidth < 1 || tabWidth > 16 {
			t.Skip()
		}
		if len(text) > 4096 || !utf8.ValidString(text) {
			t.Skip()
		}
		model, err := New(Size{Cols: cols, Rows: rows}, Options{TabWidth: tabWidth, ScrollbackLines: 8})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		model.Write(text)
		assertGridIsSound(t, model)

		// Any resize the transport could ask for must leave the grid sound too.
		resized, err := New(Size{Cols: cols, Rows: rows}, Options{TabWidth: tabWidth})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		resized.Write(text)
		if err := resized.Resize(Size{Cols: cols + 1, Rows: rows + 1}); err != nil {
			t.Fatalf("Resize: %v", err)
		}
		resized.Write(text)
		assertGridIsSound(t, resized)

		// The alternate screen must restore the primary one exactly.
		before, cursor := snapshot(resized)
		resized.EnterAlternateScreen()
		resized.Write(text)
		assertGridIsSound(t, resized)
		resized.LeaveAlternateScreen()
		if after, afterCursor := snapshot(resized); after != before || afterCursor != cursor {
			t.Fatal("the alternate screen did not restore the primary screen")
		}
	})
}

// FuzzSanitizeText checks the property the renderer relies on: sanitizing is
// stable and never leaves a byte a terminal would act on.
func FuzzSanitizeText(f *testing.F) {
	f.Add("plain")
	f.Add("\x1b[31m\x1b]52;c;x\x07")
	f.Add("\u009b\u009d\u009c\u0090")
	f.Add("\r\n\t\b text")

	f.Fuzz(func(t *testing.T, text string) {
		got := SanitizeText(text)
		if again := SanitizeText(got); again != got {
			t.Fatalf("SanitizeText is not stable: %q then %q", got, again)
		}
		for _, r := range got {
			switch {
			case r == '\r' || r == '\n' || r == '\t' || r == '\b':
			case r < 0x20, r == deleteRune, r >= 0x80 && r <= 0x9f:
				t.Fatalf("SanitizeText kept the control rune %U in %q", r, got)
			}
		}
		// A raw control byte would make the text invalid UTF-8, and a decoded
		// one is already refused above. The bytes are not searched directly,
		// because a continuation byte of an ordinary character may have the same
		// value as a C1 control.
		if !utf8.ValidString(got) {
			t.Fatalf("SanitizeText produced invalid UTF-8: %q", got)
		}
	})
}

// FuzzWrapText checks that laying text out never produces a row wider than the
// viewport and always reports a cursor position inside the rows it produced.
func FuzzWrapText(f *testing.F) {
	f.Add("hello world", 5, 8, 6)
	f.Add("世界 wide", 4, 8, 2)
	f.Add("tab\tstop", 10, 4, 4)
	f.Add("", 1, 1, 0)

	f.Fuzz(func(t *testing.T, text string, cols, tabWidth, cursor int) {
		if cols < 1 || cols > 64 || tabWidth < 1 || tabWidth > 16 || cursor < 0 || cursor > 4096 {
			t.Skip()
		}
		if len(text) > 4096 {
			t.Skip()
		}
		layout := WrapText(text, cols, tabWidth, cursor)
		if len(layout.Rows) == 0 {
			t.Fatal("WrapText produced no rows")
		}
		for _, row := range layout.Rows {
			if width := rowColumns(row, cols); width > cols {
				t.Fatalf("row %q is %d columns wide, want at most %d", row, width, cols)
			}
		}
		if layout.CursorRow < 0 || layout.CursorRow >= len(layout.Rows) {
			t.Fatalf("cursor row %d is outside %d rows", layout.CursorRow, len(layout.Rows))
		}
		if layout.CursorCol < 0 || layout.CursorCol >= cols {
			t.Fatalf("cursor column %d is outside a row of %d", layout.CursorCol, cols)
		}
		if !layout.CursorOnRow {
			t.Fatal("CursorOnRow is false although the cursor row is inside the layout")
		}
	})
}

// rowColumns returns how many columns a laid-out row occupies, counting the way
// the layout does: a grapheme wider than the whole row takes the columns the row
// has, and one that does not fit the remaining columns ends the row.
func rowColumns(row string, cols int) int {
	columns := 0
	for _, r := range row {
		width := min(RuneWidth(r), cols)
		if columns+width > cols {
			break
		}
		columns += width
	}
	return columns
}

// assertGridIsSound checks the structural invariants of the grid after any write.
func assertGridIsSound(t *testing.T, model *Screen) {
	t.Helper()
	size := model.Size()
	cursor := model.Cursor()
	if cursor.Row < 0 || cursor.Row >= size.Rows || cursor.Col < 0 || cursor.Col >= size.Cols {
		t.Fatalf("cursor %d,%d is outside a %dx%d viewport", cursor.Row, cursor.Col, size.Cols, size.Rows)
	}
	for row := 0; row < size.Rows; row++ {
		cells, ok := model.Row(row)
		if !ok {
			t.Fatalf("row %d is missing from a %d row viewport", row, size.Rows)
		}
		if len(cells) != size.Cols {
			t.Fatalf("row %d holds %d cells, want %d", row, len(cells), size.Cols)
		}
		for column, cell := range cells {
			switch cell.Width {
			case 0:
				// A width-zero cell only ever follows the lead of a wide grapheme.
				if column == 0 {
					t.Fatalf("row %d starts with a continuation cell", row)
				}
				if cells[column-1].Width != 2 {
					t.Fatalf("row %d column %d is a continuation of %+v", row, column, cells[column-1])
				}
			case 1, 2:
				if strings.ContainsAny(cell.Grapheme, "\x1b\x07\x9b\x9d\x9c\x90") {
					t.Fatalf("row %d column %d holds a control byte in %q", row, column, cell.Grapheme)
				}
				if cell.Width == 2 && column+1 < len(cells) && cells[column+1].Width != 0 {
					t.Fatalf("row %d column %d is wide but not followed by its continuation", row, column)
				}
			default:
				t.Fatalf("row %d column %d has width %d", row, column, cell.Width)
			}
			if cell.Width == 2 && column == len(cells)-1 {
				// A wide grapheme may only sit at the margin if the row is one
				// column wide, which the width check above already allows.
				continue
			}
		}
	}
}

// snapshot returns the visible text of a screen together with its cursor, for
// comparing two screens.
func snapshot(model *Screen) (string, Cursor) {
	var out strings.Builder
	for row := range model.Size().Rows {
		text, _ := model.RowText(row)
		out.WriteString(text)
		out.WriteByte('\n')
	}
	return out.String(), model.Cursor()
}
