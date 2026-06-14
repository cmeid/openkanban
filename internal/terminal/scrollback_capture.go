package terminal

import (
	xvt "github.com/charmbracelet/x/vt"
)

// CaptureTopRow returns a snapshot of vt's row 0 for later comparison
// against the post-write state. Returns nil when alt-screen is active
// or vt has no columns — both cases skip scrollback population.
//
// The caller is expected to invoke this BEFORE feeding bytes to
// vt.Write, then pair with PushScrolledLine AFTER the write. The
// SafeEmulator's internal mutex serializes CellAt access; callers
// holding a higher-level lock around vt.Write keep the
// before/after pair atomic with respect to other operations.
func CaptureTopRow(vt *xvt.SafeEmulator, altScreenActive bool) []Glyph {
	if vt == nil || altScreenActive {
		return nil
	}
	cols := vt.Width()
	if cols <= 0 {
		return nil
	}
	row := make([]Glyph, cols)
	for col := 0; col < cols; col++ {
		row[col] = CellToGlyph(vt.CellAt(col, 0))
	}
	return row
}

// PushScrolledLine pushes lastTopRow into sb when vt's current row 0
// differs from lastTopRow AND the original content isn't visible
// anywhere else on screen (i.e. the line actually scrolled off,
// rather than being redrawn in place). No-op when any argument is
// nil, when alt-screen is active, or when vt's width has changed
// between the snapshot and the post-write check.
func PushScrolledLine(vt *xvt.SafeEmulator, altScreenActive bool, lastTopRow []Glyph, sb *ScrollbackBuffer) {
	if vt == nil || altScreenActive || lastTopRow == nil || sb == nil {
		return
	}
	cols := vt.Width()
	if cols <= 0 || cols != len(lastTopRow) {
		return
	}
	changed := false
	for col := 0; col < cols; col++ {
		if CellToGlyph(vt.CellAt(col, 0)) != lastTopRow[col] {
			changed = true
			break
		}
	}
	if !changed {
		return
	}
	if lineVisibleOnScreen(vt, lastTopRow) {
		return
	}
	sb.Push(lastTopRow)
}

// lineVisibleOnScreen reports whether `line` matches any row in
// vt's current screen. Used to distinguish a redraw-in-place
// (line still on screen, don't push) from a true scroll-off (line
// gone, push to scrollback).
func lineVisibleOnScreen(vt *xvt.SafeEmulator, line []Glyph) bool {
	cols := vt.Width()
	rows := vt.Height()
	if len(line) != cols {
		return false
	}
	for row := 0; row < rows; row++ {
		match := true
		for col := 0; col < cols; col++ {
			if CellToGlyph(vt.CellAt(col, row)) != line[col] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
