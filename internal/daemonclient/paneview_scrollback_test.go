package daemonclient

import (
	"fmt"
	"testing"

	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/terminal"
)

// TestPaneView_ApplyOutput_PopulatesScrollback verifies the local
// scrollback producer on the client side. The daemon-aware refactor
// (PR7) lifted the scrollback CONSUMER code (scrollUpLocked,
// RenderVT viewportOffset, shift+home) into PaneView but omitted
// the PRODUCER: terminal.Pane.captureScrollbackBeforeWrite /
// captureScrollbackAfterWrite around vt.Write. Symptom: trackpad
// wheel-up does nothing in agent sessions because the local
// scrollback ring is always empty; scrollUpLocked clamps to
// p.scrollback.Len() == 0 → no-op.
//
// Setup writes more lines than fit on the 24-row screen, then
// asserts the scrollback ring has at least one captured line.
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

	pv.mu.Lock()
	got := pv.scrollback.Len()
	pv.mu.Unlock()
	if got == 0 {
		t.Fatalf("scrollback.Len() = 0 after 50 lines on 24-row screen, want > 0")
	}
}

// TestPaneView_SnapshotApply_PopulatesScrollback verifies that the
// snapshot apply path drives local scrollback capture: bytes produced
// by SerializeScrollback should land in the client's scrollback ring
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

	pv.mu.Lock()
	got := pv.scrollback.Len()
	pv.mu.Unlock()
	if got < 6 {
		t.Fatalf("scrollback.Len() = %d after snapshot apply, want >= 6", got)
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

	// Enter alt-screen, then push enough content via \r\n so it
	// would otherwise have scrolled into the primary ring. Because
	// altScreenActive is set, CaptureTopRow returns nil and nothing
	// is pushed.
	pv.applySnapshotChunk([]byte("\x1b[?1049h"))
	for i := 0; i < 10; i++ {
		pv.applySnapshotChunk([]byte(fmt.Sprintf("alt %d\r\n", i)))
	}

	pv.mu.Lock()
	got := pv.scrollback.Len()
	pv.mu.Unlock()
	if got != 0 {
		t.Fatalf("scrollback.Len() = %d during alt-screen apply, want 0", got)
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

	pv.mu.Lock()
	got := pv.scrollback.Len()
	pv.mu.Unlock()
	if got != 0 {
		t.Fatalf("scrollback.Len() = %d while on alt-screen, want 0", got)
	}
}
