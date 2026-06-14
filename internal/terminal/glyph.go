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

// CellToGlyph translates a charm/x/vt cell into our internal Glyph.
// A nil cell maps to a zero-value Glyph (rendered as a space). The
// cell's Content is a grapheme cluster; we take only the first rune.
// Combining marks and ZWJ sequences will not round-trip — known
// limitation, called out at the boundary.
//
// Width semantics:
//   - 1: normal single-cell glyph.
//   - 2: leading half of a wide (CJK / emoji) glyph; the cell to its
//     right is the continuation (Width == 0).
//   - 0: continuation cell of a preceding wide glyph. Callers iterating
//     columns and writing one rune per cell MUST skip these — emitting a
//     space for the continuation shifts everything after the wide glyph
//     one column right (see TestSerializeRedraw_RoundTrip/wide_chars_round_trip).
//
// Exported (PR5) so the daemon's redraw serializer can read cells
// from a SafeEmulator without duplicating the boundary translation.
func CellToGlyph(c *uv.Cell) Glyph {
	if c == nil {
		return Glyph{Width: 1}
	}
	var ch rune
	for _, r := range c.Content {
		ch = r
		break
	}
	// Preserve Width == 0 for the continuation half of a wide glyph;
	// only defend against negative (which ultraviolet does not produce
	// today).
	width := c.Width
	if width < 0 {
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

// GlyphANSI emits an ANSI SGR sequence that sets the terminal state
// to render glyph g. It always emits a full reset+set rather than a
// diff because the renderer wraps each run with \x1b[0m anyway.
// Returns "" if g has no styling beyond defaults.
//
// Exported (PR5) so the daemon's redraw serializer can emit SGR runs
// using the same encoding as render.go.
func GlyphANSI(g Glyph) string {
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

// cellToGlyph is an unexported alias for CellToGlyph retained so the
// existing pane.go / render.go call sites read naturally. New code
// outside this package should call CellToGlyph.
var cellToGlyph = CellToGlyph

// glyphANSI is an unexported alias for GlyphANSI.
var glyphANSI = GlyphANSI

// colorANSI returns the SGR parameter (without the leading CSI or
// trailing m) for a single color slot. Returns "" for nil (default).
//
// Encoding strategy is deliberate so a Glyph -> SGR -> emulator-parse
// round-trip preserves the concrete color type (needed by the daemon's
// SerializeRedraw round-trip test — and harmless for the regular
// renderer path, which only writes to a real terminal):
//
//   - ansi.BasicColor (0-15) → basic SGR codes 30-37 / 90-97 (FG)
//     or 40-47 / 100-107 (BG). The emulator parses these back into
//     BasicColor, not IndexedColor — so `==` equality survives.
//   - ansi.IndexedColor (0-255) → 38;5;<idx> / 48;5;<idx>.
//   - Anything else → 24-bit RGB.
func colorANSI(c color.Color, isFG bool) string {
	if c == nil {
		return ""
	}

	switch v := c.(type) {
	case ansi.BasicColor:
		return basicColorSGR(uint8(v), isFG)
	case ansi.IndexedColor:
		idx := uint8(v)
		// Even an IndexedColor in the low 16 round-trips most
		// faithfully as the 38;5;<idx> form: the emulator parses it
		// back as IndexedColor, preserving the concrete type.
		base := 38
		if !isFG {
			base = 48
		}
		return fmt.Sprintf("%d;5;%d", base, idx)
	}

	base := 38
	if !isFG {
		base = 48
	}
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("%d;2;%d;%d;%d", base, r>>8, g>>8, b>>8)
}

// basicColorSGR maps an ANSI BasicColor index (0-15) to a single-token
// SGR parameter. Values 0-7 are the standard 8 colors; 8-15 are the
// bright variants emitted as 90-97 / 100-107.
func basicColorSGR(idx uint8, isFG bool) string {
	if idx < 8 {
		base := 30
		if !isFG {
			base = 40
		}
		return fmt.Sprintf("%d", base+int(idx))
	}
	if idx < 16 {
		base := 90
		if !isFG {
			base = 100
		}
		return fmt.Sprintf("%d", base+int(idx-8))
	}
	// Out of band: fall back to indexed encoding.
	base := 38
	if !isFG {
		base = 48
	}
	return fmt.Sprintf("%d;5;%d", base, idx)
}
