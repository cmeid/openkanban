package daemonclient

import (
	"bytes"
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

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

	pv.mu.Lock()
	got := pv.scrollback.Len()
	pv.mu.Unlock()
	if got != 0 {
		t.Fatalf("scrollback.Len() = %d while on alt-screen, want 0", got)
	}
}
