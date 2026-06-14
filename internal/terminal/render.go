package terminal

import (
	"strings"

	xvt "github.com/charmbracelet/x/vt"
)

// renderVT is the unexported alias retained so existing intra-package
// call sites (Pane.View) read unchanged after RenderVT was exported in
// PR7.
var renderVT = RenderVT

// RenderVT is the top-level render dispatch. It returns the cached
// view for a pane's current state without touching any pane mutable
// state: callers must hold whatever lock guards the inputs while this
// runs (the Pane.View call site holds p.mu).
//
// Returns "Terminal not initialized" if vt is nil, "" if the emulator
// has a zero-sized viewport. When viewportOffset > 0 and scrollback
// is non-nil, renders a mixed scrollback + live view; otherwise just
// the live screen.
//
// Exported (PR7) so daemonclient.PaneView can render its locally-
// maintained emulator the same way Pane.View does. The lowercase
// renderVT alias below keeps the existing intra-package call sites
// readable.
func RenderVT(vt *xvt.SafeEmulator, scrollback *ScrollbackBuffer, viewportOffset int, cursorVisible bool, selection *SelectionState) string {
	if vt == nil {
		return "Terminal not initialized"
	}

	cols := vt.Width()
	rows := vt.Height()
	if cols <= 0 || rows <= 0 {
		return ""
	}

	if viewportOffset > 0 && scrollback != nil {
		return renderScrolledView(vt, scrollback, viewportOffset, cursorVisible, selection, cols, rows)
	}

	return renderLiveScreen(vt, cursorVisible, selection, cols, rows)
}

// renderScrolledView renders a viewport that includes scrollback history
// at the top and live content below. viewportOffset is the number of
// lines we've scrolled back from the live view.
func renderScrolledView(vt *xvt.SafeEmulator, scrollback *ScrollbackBuffer, viewportOffset int, cursorVisible bool, selection *SelectionState, cols, rows int) string {
	scrollbackLen := scrollback.Len()
	offset := viewportOffset
	if offset > scrollbackLen {
		offset = scrollbackLen
	}

	var result strings.Builder
	result.Grow(rows * cols * 2)

	// Number of scrollback lines visible at top of viewport
	scrollbackRowsVisible := offset
	if scrollbackRowsVisible > rows {
		scrollbackRowsVisible = rows
	}

	// Starting scrollback index (from the end of scrollback)
	scrollbackStart := scrollbackLen - offset

	for viewRow := 0; viewRow < rows; viewRow++ {
		if viewRow > 0 {
			result.WriteByte('\n')
		}

		if viewRow < scrollbackRowsVisible {
			// Render from scrollback. Logical row is negative, counting
			// up from -scrollbackLen at the oldest line to -1 at the
			// newest. This is what selection uses for hit-testing.
			scrollbackIdx := scrollbackStart + viewRow
			line := scrollback.Get(scrollbackIdx)
			logicalRow := scrollbackIdx - scrollbackLen
			result.WriteString(renderGlyphLine(line, cols, logicalRow, selection))
		} else {
			liveRow := viewRow - scrollbackRowsVisible
			result.WriteString(renderLiveRow(vt, cursorVisible, selection, cols, liveRow, liveRow))
		}
	}

	return result.String()
}

// renderGlyphLine renders one line of scrollback Glyphs with ANSI
// styling, batching runs of same-style cells for compactness.
// logicalRow is used for selection hit-testing.
func renderGlyphLine(line []Glyph, cols int, logicalRow int, selection *SelectionState) string {
	var result strings.Builder
	var currentStyle Glyph
	var batch strings.Builder
	firstCell := true
	inSelection := false

	flushBatch := func() {
		if batch.Len() == 0 {
			return
		}
		if inSelection {
			result.WriteString("\x1b[7m") // Reverse video for selection
		} else {
			result.WriteString(glyphANSI(currentStyle))
		}
		result.WriteString(batch.String())
		result.WriteString("\x1b[0m")
		batch.Reset()
	}

	for col := 0; col < cols; col++ {
		var glyph Glyph
		if col < len(line) {
			glyph = line[col]
		}
		// Skip continuation cells of wide glyphs. Emitting a space
		// here would visually shift everything after a wide char by
		// one column on the host terminal.
		if glyph.Width == 0 && col < len(line) {
			continue
		}
		ch := glyph.Char
		if ch == 0 {
			ch = ' '
		}

		cellSelected := selection != nil && selection.Contains(Position{Row: logicalRow, Col: col})

		if !firstCell && (!glyph.styleEqual(currentStyle) || cellSelected != inSelection) {
			flushBatch()
		}

		currentStyle = glyph
		inSelection = cellSelected
		firstCell = false

		batch.WriteRune(ch)
	}
	flushBatch()

	return result.String()
}

// renderLiveRow renders a single row from the live terminal screen,
// reading cells directly from the emulator. Cursor is rendered with
// reverse video and takes priority over selection highlighting.
// logicalRow is what selection uses for hit-testing; row is the
// emulator row index.
func renderLiveRow(vt *xvt.SafeEmulator, cursorVisible bool, selection *SelectionState, cols, row, logicalRow int) string {
	var result strings.Builder
	var currentStyle Glyph
	var batch strings.Builder
	firstCell := true
	inSelection := false

	flushBatch := func() {
		if batch.Len() == 0 {
			return
		}
		if inSelection {
			result.WriteString("\x1b[7m") // Reverse video for selection
		} else {
			result.WriteString(glyphANSI(currentStyle))
		}
		result.WriteString(batch.String())
		result.WriteString("\x1b[0m")
		batch.Reset()
	}

	cursor := vt.CursorPosition()

	for col := 0; col < cols; col++ {
		glyph := cellToGlyph(vt.CellAt(col, row))
		// Skip continuation cells of wide glyphs. Emitting a space
		// here would visually shift everything after a wide char by
		// one column on the host terminal. The cursor case is handled
		// separately so the cursor cell is still rendered if it
		// happens to land on a continuation column.
		isCursor := cursorVisible && col == cursor.X && row == cursor.Y
		if glyph.Width == 0 && !isCursor {
			continue
		}
		ch := glyph.Char
		if ch == 0 {
			ch = ' '
		}

		cellSelected := selection != nil && selection.Contains(Position{Row: logicalRow, Col: col})

		if !firstCell && (!glyph.styleEqual(currentStyle) || isCursor || cellSelected != inSelection) {
			flushBatch()
		}

		if isCursor {
			result.WriteString("\x1b[7m") // Reverse
			result.WriteRune(ch)
			result.WriteString("\x1b[27m") // Un-reverse
			firstCell = true
			inSelection = false
			continue
		}

		currentStyle = glyph
		inSelection = cellSelected
		firstCell = false

		batch.WriteRune(ch)
	}
	flushBatch()

	return result.String()
}

// renderLiveScreen renders the full live emulator screen. Unlike
// renderScrolledView, no scrollback is mixed in; logical row equals
// emulator row for selection hit-testing.
func renderLiveScreen(vt *xvt.SafeEmulator, cursorVisible bool, selection *SelectionState, cols, rows int) string {
	var result strings.Builder
	result.Grow(rows * cols * 2)

	for row := 0; row < rows; row++ {
		if row > 0 {
			result.WriteByte('\n')
		}
		result.WriteString(renderLiveRow(vt, cursorVisible, selection, cols, row, row))
	}

	return result.String()
}
