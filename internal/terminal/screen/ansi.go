package screen

// Control sequences the renderer emits. Keeping them in one place makes the
// renderer the only package that produces them, which is what lets data text
// be shown without any risk of activating a terminal control.
const (
	// EraseLine erases the whole current line.
	EraseLine = "\x1b[2K"
	// ClearScreen erases the whole display; pair it with CursorHome to redraw
	// from the top.
	ClearScreen = "\x1b[2J"
	// CursorHome moves the cursor to the first cell of the screen.
	CursorHome = "\x1b[H"
	// CarriageReturn returns the cursor to the start of its row.
	CarriageReturn = "\r"
	// LineFeed moves the cursor to the next row.
	LineFeed = "\n"
	// SelectGraphicRendition is the prefix of every style sequence.
	SelectGraphicRendition = "\x1b["
	// ResetStyle restores every attribute and color to the terminal default.
	ResetStyle = "\x1b[0m"
	// HideCursor and ShowCursor set cursor visibility.
	HideCursor = "\x1b[?25l"
	ShowCursor = "\x1b[?25h"
	// EnterAlternateScreen and LeaveAlternateScreen switch the screen buffer
	// and save or restore the primary screen with it.
	EnterAlternateScreen = "\x1b[?1049h"
	LeaveAlternateScreen = "\x1b[?1049l"
)

// SGR parameters for one attribute, as "reset;set" so both values come from
// one table entry.
type attributeParams struct {
	set   int
	reset int
}

var (
	boldParams      = attributeParams{set: 1, reset: 22}
	faintParams     = attributeParams{set: 2, reset: 22}
	italicParams    = attributeParams{set: 3, reset: 23}
	underlineParams = attributeParams{set: 4, reset: 24}
	blinkParams     = attributeParams{set: 5, reset: 25}
	reverseParams   = attributeParams{set: 7, reset: 27}
)

// AppendSGR appends the sequences that turn the style a terminal currently has
// into next. Emitting only the difference keeps a frame small and means a
// renderer never has to reset everything it does not use.
func AppendSGR(dst []byte, current, next Style) []byte {
	if current == next {
		return dst
	}
	params := make([]int, 0, 16)
	params = append(params, boolParams(current.Bold, next.Bold, boldParams)...)
	params = append(params, boolParams(current.Faint, next.Faint, faintParams)...)
	params = append(params, boolParams(current.Italic, next.Italic, italicParams)...)
	params = append(params, boolParams(current.Underline, next.Underline, underlineParams)...)
	params = append(params, boolParams(current.Blink, next.Blink, blinkParams)...)
	params = append(params, boolParams(current.Reverse, next.Reverse, reverseParams)...)
	params = append(params, colorParams(current.FG, next.FG, 39)...)
	params = append(params, colorParams(current.BG, next.BG, 49)...)
	if len(params) == 0 {
		return dst
	}
	dst = append(dst, SelectGraphicRendition...)
	dst = appendParams(dst, params)
	return append(dst, 'm')
}

func boolParams(current, next bool, params attributeParams) []int {
	switch {
	case next && !current:
		return []int{params.set}
	case current && !next:
		return []int{params.reset}
	}
	return nil
}

// colorParams maps a color change to SGR parameters, using defaultParameter
// when the color returns to the terminal default.
func colorParams(current, next Color, defaultParameter int) []int {
	if current == next {
		return nil
	}
	switch next.kind {
	case ColorDefault:
		return []int{defaultParameter}
	case ColorPalette:
		extended := 38
		if defaultParameter == 49 {
			extended = 48
		}
		return []int{extended, 5, int(next.index)}
	case ColorRGB:
		extended := 38
		if defaultParameter == 49 {
			extended = 48
		}
		return []int{extended, 2, int(next.r), int(next.g), int(next.b)}
	}
	return []int{defaultParameter}
}

func appendParams(dst []byte, params []int) []byte {
	for i, param := range params {
		if i > 0 {
			dst = append(dst, ';')
		}
		dst = appendInt(dst, param)
	}
	return dst
}

func appendInt(dst []byte, value int) []byte {
	if value == 0 {
		return append(dst, '0')
	}
	if value < 0 {
		dst = append(dst, '-')
		value = -value
	}
	var digits [8]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	return append(dst, digits[position:]...)
}

// AppendCursorPosition appends the sequence that moves the cursor to a cell.
func AppendCursorPosition(dst []byte, row, col int) []byte {
	dst = append(dst, SelectGraphicRendition...)
	dst = appendInt(dst, row+1)
	dst = append(dst, ';')
	dst = appendInt(dst, col+1)
	return append(dst, 'H')
}
