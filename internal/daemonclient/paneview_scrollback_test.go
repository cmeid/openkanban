package daemonclient

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/terminal"
)

// TestPaneView_ApplyOutput_PopulatesScrollback verifies that live output
// populates scrollback at all. Scrollback history is the emulator's own
// native buffer (vt.ScrollbackLen), surfaced by the public ScrollbackLen
// accessor; if it stayed empty, scrollUpLocked would clamp to 0 and
// trackpad wheel-up would no-op in agent sessions.
//
// Setup writes more lines than fit on the 24-row screen, then asserts
// the scrollback has at least one line.
func TestPaneView_ApplyOutput_PopulatesScrollback(t *testing.T) {
	pv := &PaneView{width: 80, height: 24}
	pv.mu.Lock()
	pv.initEmulatorLocked()
	pv.mu.Unlock()
	t.Cleanup(func() {
		pv.mu.Lock()
		pv.teardownEmulatorLocked()
		pv.mu.Unlock()
	})

	for i := 0; i < 50; i++ {
		pv.applyOutput([]byte(fmt.Sprintf("line %d\r\n", i)))
	}

	if got := pv.ScrollbackLen(); got == 0 {
		t.Fatalf("ScrollbackLen() = 0 after 50 lines on 24-row screen, want > 0")
	}
}

// TestPaneView_ApplyOutput_MultiLineChunk_CapturesEveryScrolledLine is a
// regression test for the scrollback under-capture bug. The live attach
// path feeds each daemon PTY frame to applyOutput whole. The original
// row-0 snapshot producer captured at most ONE scrolled-off row per
// write, so a burst that scrolled the screen by N rows lost N-1 lines —
// trackpad scroll-up showed roughly one line per burst with large chunks
// missing. Reading scrollback from the emulator's native buffer captures
// every scrolled row.
//
// Unlike TestPaneView_ApplyOutput_PopulatesScrollback (which calls
// applyOutput once per line and so masks the bug), here all 50 lines
// arrive in a SINGLE applyOutput call.
func TestPaneView_ApplyOutput_MultiLineChunk_CapturesEveryScrolledLine(t *testing.T) {
	pv := &PaneView{width: 80, height: 24}
	pv.mu.Lock()
	pv.initEmulatorLocked()
	pv.mu.Unlock()
	t.Cleanup(func() {
		pv.mu.Lock()
		pv.teardownEmulatorLocked()
		pv.mu.Unlock()
	})

	// 50 newline-terminated lines delivered as ONE frame.
	var chunk bytes.Buffer
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&chunk, "line %d\r\n", i)
	}
	pv.applyOutput(chunk.Bytes())

	// 50 lines on a 24-row screen scroll ~26 rows off the top. The buggy
	// single-capture path pushed at most 1. Require a strong lower bound.
	lines := dumpNativeScrollback(pv)
	if len(lines) < 20 {
		t.Fatalf("native scrollback has %d lines after a 50-line chunk on a 24-row screen, want >= 20 (single-capture bug yielded <= 1)", len(lines))
	}

	// Contiguity: the oldest captured lines must be "line 0", "line 1", ...
	// in order with no gaps. A gap is exactly the user-visible "chunks
	// missing" symptom.
	for i, got := range lines {
		want := fmt.Sprintf("line %d", i)
		if got != want {
			t.Fatalf("scrollback[%d] = %q, want %q (gap indicates dropped lines)", i, got, want)
		}
	}
}

// TestPaneView_ApplyOutput_OverLongLine_CapturesWrappedRows is a
// regression test for the residual under-capture: a single physical line
// longer than the terminal width with NO newline soft-wraps to multiple
// rows. One vt.Write scrolls the screen by many rows, but the old row-0
// snapshot producer (and a per-\r\n splitter) treated the whole thing as
// one unit and captured at most one scrolled row. The emulator performs
// the wrapping, so reading scrollback from its native buffer captures
// every wrapped row.
func TestPaneView_ApplyOutput_OverLongLine_CapturesWrappedRows(t *testing.T) {
	const cols, rows = 80, 24
	pv := &PaneView{width: cols, height: rows}
	pv.mu.Lock()
	pv.initEmulatorLocked()
	pv.mu.Unlock()
	t.Cleanup(func() {
		pv.mu.Lock()
		pv.teardownEmulatorLocked()
		pv.mu.Unlock()
	})

	// One physical line (no CRLF until the very end) built from 100
	// distinct 80-char blocks so it wraps to 100 rows. Block i begins with
	// an "L%03d|" marker, the rest padded with '-', so we can assert the
	// wrapped rows landed in scrollback contiguously.
	const blocks = 100
	var sb strings.Builder
	for i := 0; i < blocks; i++ {
		marker := fmt.Sprintf("L%03d|", i)
		sb.WriteString(marker)
		for j := len(marker); j < cols; j++ {
			sb.WriteByte('-')
		}
	}
	sb.WriteString("\r\n")
	pv.applyOutput([]byte(sb.String()))

	lines := dumpNativeScrollback(pv)
	// 100 wrapped rows on a 24-row screen scroll ~76 off the top. The
	// single-capture path captured at most 1.
	if len(lines) < 50 {
		t.Fatalf("native scrollback has %d wrapped rows after an over-long line, want >= 50 (single-capture bug yielded <= 1)", len(lines))
	}
	// Contiguity: scrollback[i] is wrapped row i = block i, prefix "L%03d|".
	for i, got := range lines {
		want := fmt.Sprintf("L%03d|", i)
		if !strings.HasPrefix(got, want) {
			t.Fatalf("scrollback[%d] = %q, want prefix %q (wrapped-row gap)", i, got, want)
		}
	}
}

// TestPaneView_View_ScrolledBack_RendersHistoryContiguously closes the
// loop end-to-end: after a multi-line burst, scrolling the viewport to the
// top and rendering must show the OLDEST history lines, in order, with
// none missing. This guards the consumer side (View ->
// RenderVTNativeScrollback reading the emulator's native scrollback) — the
// capture-side tests above would still pass if render regressed back to
// the lossy ring, but this one would not.
func TestPaneView_View_ScrolledBack_RendersHistoryContiguously(t *testing.T) {
	const cols, rows = 80, 24
	pv := &PaneView{width: cols, height: rows}
	pv.mu.Lock()
	pv.initEmulatorLocked()
	pv.mu.Unlock()
	t.Cleanup(func() {
		pv.mu.Lock()
		pv.teardownEmulatorLocked()
		pv.mu.Unlock()
	})

	var chunk bytes.Buffer
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&chunk, "line %d\r\n", i)
	}
	pv.applyOutput(chunk.Bytes())

	// Scroll all the way up (same effect as shift+home).
	pv.mu.Lock()
	pv.viewportOffset = pv.vt.ScrollbackLen()
	pv.dirty = true
	pv.mu.Unlock()

	view := pv.View()
	// The top of a fully-scrolled-up viewport shows the oldest history.
	// On the old lossy-ring render these lines were never captured, so the
	// scrolled view showed only the live tail (line 26+).
	for i := 0; i < 10; i++ {
		marker := fmt.Sprintf("line %d", i)
		if !strings.Contains(view, marker) {
			t.Fatalf("scrolled-up View() missing %q (history chunk dropped)\n---\n%s", marker, view)
		}
	}
	if strings.Index(view, "line 0") > strings.Index(view, "line 9") {
		t.Fatalf("scrolled-up View() renders history out of order")
	}
}

// glyphLineText reconstructs the plain text of a scrollback glyph row,
// skipping wide-glyph continuation cells and trimming trailing blanks.
func glyphLineText(line []terminal.Glyph) string {
	var b strings.Builder
	for _, g := range line {
		if g.Width == 0 {
			continue
		}
		ch := g.Char
		if ch == 0 {
			ch = ' '
		}
		b.WriteRune(ch)
	}
	return strings.TrimRight(b.String(), " ")
}

// dumpNativeScrollback returns the emulator's native scrollback as
// plain-text lines, oldest first. NOTE: this inspects the source buffer
// the render path reads, not the rendered output itself (it reimplements
// text extraction). End-to-end render coverage is in
// TestPaneView_View_ScrolledBack_RendersHistoryContiguously.
func dumpNativeScrollback(pv *PaneView) []string {
	pv.mu.Lock()
	defer pv.mu.Unlock()
	if pv.vt == nil {
		return nil
	}
	n := pv.vt.ScrollbackLen()
	cols := pv.vt.Width()
	out := make([]string, n)
	for y := 0; y < n; y++ {
		row := make([]terminal.Glyph, cols)
		for x := 0; x < cols; x++ {
			row[x] = terminal.CellToGlyph(pv.vt.ScrollbackCellAt(x, y))
		}
		out[y] = glyphLineText(row)
	}
	return out
}

// TestPaneView_SnapshotApply_PopulatesScrollback verifies that the
// snapshot apply path drives scrollback capture: bytes produced by
// SerializeScrollback should land in the emulator's native scrollback
// after applySnapshotChunk runs.
func TestPaneView_SnapshotApply_PopulatesScrollback(t *testing.T) {
	pv := &PaneView{width: 20, height: 4}
	pv.mu.Lock()
	pv.initEmulatorLocked()
	pv.mu.Unlock()
	t.Cleanup(func() {
		pv.mu.Lock()
		pv.teardownEmulatorLocked()
		pv.mu.Unlock()
	})

	// Build 10 scrollback rows for a 4-row grid: at least 10-4=6
	// must spill into the ring.
	src := make([][]terminal.Glyph, 10)
	for i := 0; i < 10; i++ {
		s := fmt.Sprintf("hist %d", i)
		row := make([]terminal.Glyph, 20)
		for j, r := range s {
			row[j] = terminal.Glyph{Char: r, Width: 1}
		}
		for j := len(s); j < 20; j++ {
			row[j] = terminal.Glyph{Char: ' ', Width: 1}
		}
		src[i] = row
	}

	bytes := daemon.SerializeScrollback(src)
	pv.applySnapshotChunk(bytes)

	if got := pv.ScrollbackLen(); got < 6 {
		t.Fatalf("ScrollbackLen() = %d after snapshot apply, want >= 6", got)
	}
}

// TestPaneView_SnapshotApply_AltScreenRedrawSkipsScrollback verifies
// that when the snapshot redraw flips into alt-screen, subsequent
// content does NOT land in primary scrollback.
func TestPaneView_SnapshotApply_AltScreenRedrawSkipsScrollback(t *testing.T) {
	pv := &PaneView{width: 20, height: 4}
	pv.mu.Lock()
	pv.initEmulatorLocked()
	pv.mu.Unlock()
	t.Cleanup(func() {
		pv.mu.Lock()
		pv.teardownEmulatorLocked()
		pv.mu.Unlock()
	})

	// Enter alt-screen, then push enough content via \r\n so it would
	// otherwise have scrolled into the primary scrollback. Because the
	// content goes to the alternate screen, the primary scrollback (what
	// ScrollbackLen reports) stays empty.
	pv.applySnapshotChunk([]byte("\x1b[?1049h"))
	for i := 0; i < 10; i++ {
		pv.applySnapshotChunk([]byte(fmt.Sprintf("alt %d\r\n", i)))
	}

	if got := pv.ScrollbackLen(); got != 0 {
		t.Fatalf("ScrollbackLen() = %d during alt-screen apply, want 0", got)
	}
}

// TestPaneView_TranslateKey_DECCKM verifies the client-side mirror of
// the application-cursor-keys branch. Arrow keys must encode as SS3
// (ESC O A/B/C/D) when DECCKM is set, CSI otherwise. Without this,
// Claude Code (which runs with DECCKM set) treats openkanban's arrows
// as generic cursor-up keystrokes in a text input, mutating its visible
// chat view instead of navigating prompt history.
func TestPaneView_TranslateKey_DECCKM(t *testing.T) {
	tests := []struct {
		name string
		msg  tea.KeyMsg
		on   []byte
		off  []byte
	}{
		{name: "Up", msg: tea.KeyMsg{Type: tea.KeyUp}, on: []byte("\x1bOA"), off: []byte("\x1b[A")},
		{name: "Down", msg: tea.KeyMsg{Type: tea.KeyDown}, on: []byte("\x1bOB"), off: []byte("\x1b[B")},
		{name: "Right", msg: tea.KeyMsg{Type: tea.KeyRight}, on: []byte("\x1bOC"), off: []byte("\x1b[C")},
		{name: "Left", msg: tea.KeyMsg{Type: tea.KeyLeft}, on: []byte("\x1bOD"), off: []byte("\x1b[D")},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/off", func(t *testing.T) {
			pv := &PaneView{}
			got := pv.translateKey(tt.msg)
			if !bytes.Equal(got, tt.off) {
				t.Errorf("DECCKM off: translateKey(%v) = %q, want %q", tt.msg, got, tt.off)
			}
		})
		t.Run(tt.name+"/on", func(t *testing.T) {
			pv := &PaneView{}
			pv.cursorAppMode.Store(true)
			got := pv.translateKey(tt.msg)
			if !bytes.Equal(got, tt.on) {
				t.Errorf("DECCKM on: translateKey(%v) = %q, want %q", tt.msg, got, tt.on)
			}
		})
	}
}

// TestPaneView_CursorAppMode_TracksDECCKMBytes exercises the full
// integration: feed ESC[?1h / ESC[?1l through the local emulator via
// applyOutput, verify the EnableMode/DisableMode callbacks fire and
// translateKey sees the updated flag. Mirrors
// internal/terminal.TestPaneCursorAppModeCallback for the client path.
func TestPaneView_CursorAppMode_TracksDECCKMBytes(t *testing.T) {
	pv := &PaneView{width: 80, height: 24}
	pv.mu.Lock()
	pv.initEmulatorLocked()
	pv.mu.Unlock()
	t.Cleanup(func() {
		pv.mu.Lock()
		pv.teardownEmulatorLocked()
		pv.mu.Unlock()
	})

	if pv.cursorAppMode.Load() {
		t.Fatalf("cursorAppMode before any DECCKM byte = true, want false")
	}
	if got := pv.translateKey(tea.KeyMsg{Type: tea.KeyUp}); !bytes.Equal(got, []byte("\x1b[A")) {
		t.Fatalf("baseline Up encoding = %q, want %q", got, "\x1b[A")
	}

	pv.applyOutput([]byte("\x1b[?1h"))
	if !pv.cursorAppMode.Load() {
		t.Fatalf("after ESC[?1h, cursorAppMode = false, want true")
	}
	if got := pv.translateKey(tea.KeyMsg{Type: tea.KeyUp}); !bytes.Equal(got, []byte("\x1bOA")) {
		t.Errorf("DECCKM-on Up encoding = %q, want %q", got, "\x1bOA")
	}

	pv.applyOutput([]byte("\x1b[?1l"))
	if pv.cursorAppMode.Load() {
		t.Fatalf("after ESC[?1l, cursorAppMode = true, want false")
	}
	if got := pv.translateKey(tea.KeyMsg{Type: tea.KeyUp}); !bytes.Equal(got, []byte("\x1b[A")) {
		t.Errorf("post-reset Up encoding = %q, want %q", got, "\x1b[A")
	}
}

// TestPaneView_ApplyOutput_AltScreenSkipsScrollback confirms that
// when the alt-screen is active (e.g. vim, less, full-TUI apps),
// scrolling within the alt-screen does NOT populate the primary
// scrollback ring — same contract as terminal.Pane.
func TestPaneView_ApplyOutput_AltScreenSkipsScrollback(t *testing.T) {
	pv := &PaneView{width: 80, height: 24}
	pv.mu.Lock()
	pv.initEmulatorLocked()
	pv.mu.Unlock()
	t.Cleanup(func() {
		pv.mu.Lock()
		pv.teardownEmulatorLocked()
		pv.mu.Unlock()
	})

	pv.applyOutput([]byte("\x1b[?1049h"))
	for i := 0; i < 50; i++ {
		pv.applyOutput([]byte(fmt.Sprintf("alt %d\r\n", i)))
	}

	if got := pv.ScrollbackLen(); got != 0 {
		t.Fatalf("ScrollbackLen() = %d while on alt-screen, want 0", got)
	}
}
