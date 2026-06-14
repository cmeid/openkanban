package daemonclient

import (
	"fmt"
	"testing"
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
