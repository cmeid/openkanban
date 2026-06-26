package terminal

import (
	"strings"

	xvt "github.com/charmbracelet/x/vt"
)

// RenderVTNativeScrollback renders like RenderVT but uses the emulator's
// OWN scrollback buffer (vt.ScrollbackLen / vt.ScrollbackCellAt) as the
// history source instead of a separate ScrollbackBuffer.
//
// This matters for fidelity: the emulator performs line wrapping, so its
// scrollback already contains every row that scrolled off the top —
// including the wrapped rows produced by an over-long line with no
// newline. The ScrollbackBuffer producer (CaptureTopRow/PushScrolledLine)
// snapshots only one row per write and so cannot capture those, which left
// gaps when scrolling back through agent output. Only the visible window
// is read per frame, so this stays cheap regardless of history depth.
//
// daemonclient.PaneView uses this; the daemon-side terminal.Pane still
// uses RenderVT with its own ScrollbackBuffer.
func RenderVTNativeScrollback(vt *xvt.SafeEmulator, viewportOffset int, cursorVisible bool, selection *SelectionState) string {
	if vt == nil {
		return "Terminal not initialized"
	}

	cols := vt.Width()
	rows := vt.Height()
	if cols <= 0 || rows <= 0 {
		return ""
	}

	if viewportOffset > 0 {
		return renderScrolledViewNative(vt, viewportOffset, cursorVisible, selection, cols, rows)
	}

	return renderLiveScreen(vt, cursorVisible, selection, cols, rows)
}

// renderScrolledViewNative mirrors renderScrolledView but reads scrollback
// rows from the emulator's native scrollback. The row/logical-row math is
// identical so selection hit-testing stays consistent with the live path.
func renderScrolledViewNative(vt *xvt.SafeEmulator, viewportOffset int, cursorVisible bool, selection *SelectionState, cols, rows int) string {
	scrollbackLen := vt.ScrollbackLen()
	offset := viewportOffset
	if offset > scrollbackLen {
		offset = scrollbackLen
	}

	var result strings.Builder
	result.Grow(rows * cols * 2)

	scrollbackRowsVisible := offset
	if scrollbackRowsVisible > rows {
		scrollbackRowsVisible = rows
	}

	scrollbackStart := scrollbackLen - offset

	for viewRow := 0; viewRow < rows; viewRow++ {
		if viewRow > 0 {
			result.WriteByte('\n')
		}

		if viewRow < scrollbackRowsVisible {
			scrollbackIdx := scrollbackStart + viewRow
			logicalRow := scrollbackIdx - scrollbackLen
			result.WriteString(renderNativeScrollbackLine(vt, scrollbackIdx, cols, logicalRow, selection))
		} else {
			liveRow := viewRow - scrollbackRowsVisible
			result.WriteString(renderLiveRow(vt, cursorVisible, selection, cols, liveRow, liveRow))
		}
	}

	return result.String()
}

// renderNativeScrollbackLine materializes scrollback line `idx` (0 =
// oldest) from the emulator's native scrollback into Glyphs and renders it
// via the shared renderGlyphLine path. Out-of-range columns read back as
// nil cells, which CellToGlyph maps to blanks.
func renderNativeScrollbackLine(vt *xvt.SafeEmulator, idx, cols, logicalRow int, selection *SelectionState) string {
	line := make([]Glyph, cols)
	for col := 0; col < cols; col++ {
		line[col] = CellToGlyph(vt.ScrollbackCellAt(col, idx))
	}
	return renderGlyphLine(line, cols, logicalRow, selection)
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
