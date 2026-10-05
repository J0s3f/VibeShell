// Package screen owns VibeShell's terminal screen model (PLAN 6.1): cells,
// cursor, style, viewport, scrollback policy, and alternate-screen state.
//
// The model interprets data text with the cursor-motion controls CR, LF, TAB,
// and BS only. Every other control byte is discarded while writing, so a cell
// can never hold bytes that a terminal would read as a control sequence. The
// renderer emits control sequences; the model moves the cursor and stores
// cells, which is what keeps text shown as data from activating clipboard,
// hyperlink, or title-change controls.
package screen

// ColorKind distinguishes how a Color's components are read.
type ColorKind int

const (
	// ColorDefault is the terminal's own default color for the attribute.
	ColorDefault ColorKind = iota
	// ColorPalette is one of the 256 palette entries.
	ColorPalette
	// ColorRGB is a direct 24-bit color.
	ColorRGB
)

// Color is a foreground or background color. The zero value is the terminal
// default, which is also what a cell holds when the view sets no color: a
// frame that only ever resets to default keeps a plain terminal usable.
type Color struct {
	kind  ColorKind
	index uint8 // palette entry for ColorPalette
	r     uint8 // red for ColorRGB
	g     uint8 // green for ColorRGB
	b     uint8 // blue for ColorRGB
}

// DefaultColor returns the terminal's default color.
func DefaultColor() Color { return Color{} }

// PaletteColor returns one of the 256 terminal palette entries.
func PaletteColor(index uint8) Color { return Color{kind: ColorPalette, index: index} }

// RGBColor returns a direct color.
func RGBColor(r, g, b uint8) Color { return Color{kind: ColorRGB, r: r, g: g, b: b} }

// Kind returns how the color's components are read.
func (c Color) Kind() ColorKind { return c.kind }

// IsDefault reports whether the color leaves the terminal's own choice in
// place.
func (c Color) IsDefault() bool { return c.kind == ColorDefault }

// Style is the set of attributes one cell carries.
type Style struct {
	FG Color
	BG Color
	// Bold, Faint, Italic, Underline, Blink, and Reverse mirror the SGR
	// attributes a text terminal supports. Strikethrough and underline styles
	// are deliberately absent: they need extra SGR parameters and no approved
	// view needs them.
	Bold      bool
	Faint     bool
	Italic    bool
	Underline bool
	Blink     bool
	Reverse   bool
}

// DefaultStyle returns the default text style.
func DefaultStyle() Style { return Style{} }

// IsDefault reports whether the style sets no attribute and no color, which is
// the style a reset sequence restores.
func (s Style) IsDefault() bool {
	return s == DefaultStyle()
}
