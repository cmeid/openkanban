package daemon

import (
	"strings"

	xvt "github.com/charmbracelet/x/vt"

	"github.com/techdufus/openkanban/internal/terminal"
)

// SerializeRedraw produces a synthetic ANSI redraw stream that, when
// fed into a fresh xvt.SafeEmulator of the same dimensions, produces
// a screen state semantically equivalent to the source emulator's
// current state.
//
// Emitted in order:
//  1. RIS (\x1bc)                 — full reset
//  2. ED (\x1b[2J) + CUP 1;1      — defensive clear/home
//  3. Mode bits:
//       alt-screen entry (\x1b[?1049h) if altScreen
//       mouse modes (\x1b[?1000h + \x1b[?1006h) if mouseEnabled
//  4. Title (OSC 2;<title> BEL)   if non-empty
//  5. Row-by-row: CUP <row+1>;1 then SGR-batched glyph runs
//     (same batching pattern as terminal/render.go: emit \x1b[<sgr>m,
//     write the run, then \x1b[0m to reset before the next run).
//  6. Cursor parking: CUP <cy+1>;<cx+1>
//  7. Cursor visibility (\x1b[?25l) if !cursorVisible
//
// The function is deliberately stateless: it reads from vt via the
// SafeEmulator's locked accessors and never mutates it. Callers must
// supply the modal booleans the Pane tracks alongside vt (mouseEnabled,
// altScreen, cursorVisible) and the OSC-2 paneTitle string. We can't
// pull those from SafeEmulator alone — IsAltScreen is exposed but
// cursor-visibility and mouse mode are not.
func SerializeRedraw(vt *xvt.SafeEmulator, cursorVisible, mouseEnabled, altScreen bool, title string) []byte {
	if vt == nil {
		return nil
	}

	cols := vt.Width()
	rows := vt.Height()
	if cols <= 0 || rows <= 0 {
		return nil
	}

	var b strings.Builder
	// Rough sizing: ~3 bytes per cell + a few hundred for control
	// prologue/epilogue. Avoids dozens of small grows for medium screens.
	b.Grow(rows*cols*3 + 256)

	// 1. RIS — hard reset. Clears every mode bit (mouse, alt-screen,
	//    cursor visibility, SGR) so the rest of the stream lands on a
	//    known baseline.
	b.WriteString("\x1bc")

	// 2. ED 2 + CUP 1;1 — defensive clear & home. RIS already does
	//    this on a faithful emulator but we belt-and-suspender it so a
	//    less-strict reader still ends up in the right place.
	b.WriteString("\x1b[2J")
	b.WriteString("\x1b[H")

	// 3. Mode bits BEFORE the cell dump so subsequent writes land on
	//    the alt screen / pick up the right mouse mode.
	if altScreen {
		b.WriteString("\x1b[?1049h")
	}
	if mouseEnabled {
		// Match what detectMouseModeChanges treats as "mouse enabled":
		// X10 button reports + SGR extended coordinates. This is the
		// minimum to round-trip the boolean — finer-grained mouse mode
		// (1002/1003) is not preserved across reconnect in this PR.
		b.WriteString("\x1b[?1000h")
		b.WriteString("\x1b[?1006h")
	}

	// 4. OSC 2 title — set after modes so it shows up in the host
	//    window/tab regardless of which screen we're on. Use BEL
	//    terminator (\x07) for broadest compatibility.
	if title != "" {
		b.WriteString("\x1b]2;")
		b.WriteString(title)
		b.WriteByte('\x07')
	}

	// 5. Row-by-row dump. We park the cursor at the start of each row
	//    via CUP rather than relying on natural cursor wrap, because
	//    terminal autowrap behavior diverges across emulators on the
	//    final column.
	for row := 0; row < rows; row++ {
		// CUP is 1-indexed.
		writeCUP(&b, row+1, 1)
		writeRow(&b, vt, cols, row)
	}

	// 6. Park cursor at its actual logical position.
	cur := vt.CursorPosition()
	cx := cur.X
	cy := cur.Y
	if cx < 0 {
		cx = 0
	}
	if cy < 0 {
		cy = 0
	}
	writeCUP(&b, cy+1, cx+1)

	// 7. Cursor visibility last, so emulators that toggle it as part
	//    of CUP don't undo our intent.
	if !cursorVisible {
		b.WriteString("\x1b[?25l")
	}

	return []byte(b.String())
}

// SerializeScrollback produces an ANSI byte stream that, when written
// to a vt emulator via vt.Write, reproduces the scrollback rows as
// lines that scroll off the top of the screen. Each row is emitted as
// SGR-batched glyph runs (matching writeRow's style), followed by
// "\r\n" + "\x1b[0m" to reset attributes before the next row.
//
// Width=0 continuation cells are skipped (wide-char invariant — see
// internal/terminal/CLAUDE.md).
//
// Returns nil if rows is empty.
func SerializeScrollback(rows [][]terminal.Glyph) []byte {
	if len(rows) == 0 {
		return nil
	}

	var b strings.Builder
	// Rough sizing: per row ~3 bytes per cell + a few bytes for the
	// terminator. Reasonable headroom prevents repeated grows.
	approxCols := 0
	for _, r := range rows {
		if len(r) > approxCols {
			approxCols = len(r)
		}
	}
	b.Grow(len(rows) * (approxCols*3 + 8))

	for _, row := range rows {
		writeGlyphRow(&b, row)
		b.WriteString("\r\n")
		b.WriteString("\x1b[0m")
	}

	return []byte(b.String())
}

// writeGlyphRow is the writeRow counterpart for raw []Glyph rows
// (rather than reading via vt.CellAt). Emits SGR-batched runs and
// skips Width=0 continuation cells.
func writeGlyphRow(b *strings.Builder, row []terminal.Glyph) {
	var currentStyle terminal.Glyph
	var batch strings.Builder
	firstCell := true

	flush := func() {
		if batch.Len() == 0 {
			return
		}
		if sgr := terminal.GlyphANSI(currentStyle); sgr != "" {
			b.WriteString(sgr)
		}
		b.WriteString(batch.String())
		b.WriteString("\x1b[0m")
		batch.Reset()
	}

	for _, g := range row {
		if g.Width == 0 {
			continue
		}
		ch := g.Char
		if ch == 0 {
			ch = ' '
		}
		if !firstCell && !sameStyle(g, currentStyle) {
			flush()
		}
		currentStyle = g
		firstCell = false
		batch.WriteRune(ch)
	}
	flush()
}

// writeCUP writes a 1-indexed Cursor Position Report (CSI <row>;<col>H).
// Inlined as a helper to keep the hot loop readable.
func writeCUP(b *strings.Builder, row, col int) {
	b.WriteString("\x1b[")
	b.WriteString(itoa(row))
	b.WriteByte(';')
	b.WriteString(itoa(col))
	b.WriteByte('H')
}

// writeRow emits one row's worth of glyphs to b, batching runs of
// identical SGR style into a single \x1b[<sgr>m ... \x1b[0m wrapper.
// Mirrors the batching loop in terminal/render.go's renderLiveRow,
// minus selection/cursor concerns (the snapshot doesn't carry them).
//
// We always emit \x1b[0m after each run so the next run starts from a
// known SGR baseline — matches GlyphANSI's "full set, not diff"
// contract.
func writeRow(b *strings.Builder, vt *xvt.SafeEmulator, cols, row int) {
	var currentStyle terminal.Glyph
	var batch strings.Builder
	firstCell := true

	flush := func() {
		if batch.Len() == 0 {
			return
		}
		if sgr := terminal.GlyphANSI(currentStyle); sgr != "" {
			b.WriteString(sgr)
		}
		b.WriteString(batch.String())
		// Reset after every run so the SGR state never leaks into
		// the next run's prologue. This is consistent with the
		// renderer's wrapping style.
		b.WriteString("\x1b[0m")
		batch.Reset()
	}

	for col := 0; col < cols; col++ {
		g := terminal.CellToGlyph(vt.CellAt(col, row))
		// Skip continuation cells of wide glyphs — the destination
		// emulator will allocate them when it consumes the leading
		// rune. Emitting a space here instead would shift every
		// glyph after the wide one one column right.
		if g.Width == 0 {
			continue
		}
		ch := g.Char
		if ch == 0 {
			ch = ' '
		}

		// Style boundary: flush the previous run before opening
		// the next one. styleEqual isn't exported, so we compare
		// the relevant fields here directly. This must stay in
		// sync with terminal/glyph.go's styleEqual.
		if !firstCell && !sameStyle(g, currentStyle) {
			flush()
		}

		currentStyle = g
		firstCell = false
		batch.WriteRune(ch)
	}
	flush()
}

// sameStyle reports whether two glyphs share an SGR state. Mirrors the
// unexported Glyph.styleEqual in terminal/glyph.go; duplicated here so
// redraw.go doesn't force an export of styleEqual just for this caller.
func sameStyle(a, b terminal.Glyph) bool {
	return a.Bold == b.Bold &&
		a.Italic == b.Italic &&
		a.Underline == b.Underline &&
		a.Reverse == b.Reverse &&
		a.Blink == b.Blink &&
		a.FG == b.FG &&
		a.BG == b.BG
}

// itoa is a tiny non-allocating int-to-string for the row/col CUP
// arguments. strconv.Itoa allocates a new string per call; we issue
// this enough times per snapshot (one per row, plus cursor) that a
// scratch helper is worth it. The values are always small positive
// ints (≤ a few hundred), so we can use a fixed-size buffer.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	if n < 0 {
		// CUP shouldn't ever be negative; treat as 0 to be safe.
		return "0"
	}
	var buf [11]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
