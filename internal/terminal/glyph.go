package terminal

import (
	"fmt"
	"image/color"
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// Glyph is openkanban's internal terminal cell representation. The
// underlying emulator (charm/x/vt) uses *uv.Cell; we translate at the
// pane boundary so the rest of the package (scrollback, selection,
// rendering) deals with a stable, value-typed struct that we control.
//
// Char is the first rune of the cell's grapheme cluster. This drops
// combining marks and treats wide cells as a single rune — see Width
// for the original cell width when needed.
type Glyph struct {
	Char      rune
	Bold      bool
	Italic    bool
	Underline bool
	Reverse   bool
	Blink     bool
	FG        color.Color // nil = terminal default
	BG        color.Color // nil = terminal default
	Width     int         // mono-spaced display width (1 for normal, 2 for CJK)
}

// IsBlank reports whether the glyph is effectively empty.
func (g Glyph) IsBlank() bool {
	return g.Char == 0 || g.Char == ' '
}

// styleEqual reports whether two glyphs would render with the same
// SGR attributes. Used by the renderer to batch runs of identical
// styling. color.Color comparison via == works because all the
// concrete types charm/uv uses (ansi.BasicColor, ansi.IndexedColor,
// color.RGBA, nil) are comparable.
func (g Glyph) styleEqual(o Glyph) bool {
	return g.Bold == o.Bold &&
		g.Italic == o.Italic &&
		g.Underline == o.Underline &&
		g.Reverse == o.Reverse &&
		g.Blink == o.Blink &&
		g.FG == o.FG &&
		g.BG == o.BG
}

// cellToGlyph translates a charm/x/vt cell into our internal Glyph.
// A nil cell maps to a zero-value Glyph (rendered as a space). The
// cell's Content is a grapheme cluster; we take only the first rune.
// Combining marks and ZWJ sequences will not round-trip — known
// limitation, called out at the boundary.
func cellToGlyph(c *uv.Cell) Glyph {
	if c == nil {
		return Glyph{Width: 1}
	}
	var ch rune
	for _, r := range c.Content {
		ch = r
		break
	}
	width := c.Width
	if width <= 0 {
		width = 1
	}
	return Glyph{
		Char:      ch,
		Bold:      c.Style.Attrs&uv.AttrBold != 0,
		Italic:    c.Style.Attrs&uv.AttrItalic != 0,
		Underline: c.Style.Underline != uv.UnderlineNone,
		Reverse:   c.Style.Attrs&uv.AttrReverse != 0,
		Blink:     c.Style.Attrs&uv.AttrBlink != 0,
		FG:        c.Style.Fg,
		BG:        c.Style.Bg,
		Width:     width,
	}
}

// glyphANSI emits an ANSI SGR sequence that sets the terminal state
// to render glyph g. It always emits a full reset+set rather than a
// diff because the renderer wraps each run with \x1b[0m anyway.
// Returns "" if g has no styling beyond defaults.
func glyphANSI(g Glyph) string {
	var parts []string

	if code := colorANSI(g.FG, true); code != "" {
		parts = append(parts, code)
	}
	if code := colorANSI(g.BG, false); code != "" {
		parts = append(parts, code)
	}
	if g.Bold {
		parts = append(parts, "1")
	}
	if g.Italic {
		parts = append(parts, "3")
	}
	if g.Underline {
		parts = append(parts, "4")
	}
	if g.Reverse {
		parts = append(parts, "7")
	}
	if g.Blink {
		parts = append(parts, "5")
	}

	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("\x1b[%sm", strings.Join(parts, ";"))
}

// colorANSI returns the SGR parameter (without the leading CSI or
// trailing m) for a single color slot. Returns "" for nil (default).
// Encodes via 256-color palette when the underlying type is an indexed
// color, and as 24-bit RGB otherwise.
func colorANSI(c color.Color, isFG bool) string {
	if c == nil {
		return ""
	}

	base := 38
	if !isFG {
		base = 48
	}

	// Indexed colors (ANSI 16 + 256-color palette) round-trip best
	// as 5;<idx>. ansi.BasicColor is uint8 in [0,15]; IndexedColor is
	// uint8 in [0,255]. RGBA() returns scaled values that lose the
	// index, so detect the concrete type.
	switch v := c.(type) {
	case ansi.BasicColor:
		return fmt.Sprintf("%d;5;%d", base, uint8(v))
	case ansi.IndexedColor:
		return fmt.Sprintf("%d;5;%d", base, uint8(v))
	}

	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("%d;2;%d;%d;%d", base, r>>8, g>>8, b>>8)
}
