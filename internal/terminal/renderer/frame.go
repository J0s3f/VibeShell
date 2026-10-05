package renderer

import (
	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/terminal/screen"
)

// statusStyle is how a status line is shown: the same reverse-video treatment a
// text terminal gives a status row, which also keeps it readable on a terminal
// with an unreadable foreground and background pair.
var statusStyle = screen.Style{Reverse: true}

// buildFrame renders a plan and its content into the bytes sent to the client.
//
// The frame is a full repaint rather than a diff: the client is an ordinary
// terminal with its own screen, so positioning the cursor on every row and
// erasing the rest of the line is both simpler and more robust than tracking
// what changed. Every sequence here comes from the screen package's table, and
// every glyph in the frame comes from the screen model, which drops control
// bytes while writing. No part of the view reaches a sequence.
func buildFrame(p plan, content string) ([]byte, error) {
	model, err := screen.New(p.size, screen.Options{})
	if err != nil {
		// The plan was validated before this point, so a size error here would
		// be an invariant violation in the renderer rather than bad input.
		return nil, domain.NewInternalError(
			domain.CodeInvariantViolation, "cannot build a screen for a validated viewport", err,
		)
	}
	model.Write(content)
	model.SetCursor(p.cursorRow, p.cursorCol)
	if p.status != "" {
		// The status row is written last, from the top of the last row, so a
		// longer status line cannot push content off the screen.
		model.SetCursor(p.size.Rows-1, 0)
		model.SetStyle(statusStyle)
		model.Write(p.status)
		model.SetStyle(screen.DefaultStyle())
	}

	frame := make([]byte, 0, p.size.Rows*(p.size.Cols*2+32))
	if p.altScreen {
		frame = append(frame, screen.EnterAlternateScreen...)
	} else {
		// A view that stays on the primary screen must also release the
		// alternate screen, so a full-screen interaction that ends hands the
		// terminal back before the shell's own output continues.
		frame = append(frame, screen.LeaveAlternateScreen...)
	}
	frame = append(frame, screen.ResetStyle...)
	frame = append(frame, screen.CursorHome...)

	style := screen.DefaultStyle()
	for row := range p.size.Rows {
		cells, ok := model.Row(row)
		if !ok {
			break
		}
		frame = screen.AppendCursorPosition(frame, row, 0)
		frame = appendRow(frame, cells, &style)
		// The rest of the row is cleared in the default style, so a styled
		// frame never bleeds its background into the terminal's own.
		frame = append(frame, screen.ResetStyle...)
		frame = append(frame, screen.EraseLine...)
	}
	frame = screen.AppendCursorPosition(frame, p.cursorRow, p.cursorCol)
	if model.Cursor().Visible {
		frame = append(frame, screen.ShowCursor...)
	} else {
		frame = append(frame, screen.HideCursor...)
	}
	return frame, nil
}

// appendRow writes one row's cells. Cells that only complete a wide grapheme
// carry no glyph, and cells that were never written are emitted as spaces, so
// the client's cursor always lands on the same column the model holds.
func appendRow(dst []byte, cells []screen.Cell, style *screen.Style) []byte {
	for _, cell := range cells {
		if cell.Width == 0 {
			continue
		}
		dst = screen.AppendSGR(dst, *style, cell.Style)
		*style = cell.Style
		if cell.Grapheme == "" {
			dst = append(dst, ' ')
			continue
		}
		dst = append(dst, cell.Grapheme...)
	}
	return dst
}
