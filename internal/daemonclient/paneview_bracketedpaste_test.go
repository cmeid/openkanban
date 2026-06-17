package daemonclient

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestPaneView_TranslateKey_BracketedPasteWrapsWhenActive verifies that a
// paste KeyMsg is forwarded wrapped in ESC[200~ … ESC[201~ ONLY when the child
// has enabled bracketed-paste mode (?2004h). Wrapping lets the child ingest a
// large paste atomically (one redraw) instead of per-rune — the flood that
// ballooned the TUI. Wrapping a child that didn't ask for it would render the
// literal markers, so the gate is load-bearing.
//
// Reverting the translateKey change (always returning raw runes) fails the
// "mode active" assertion — red-before-green.
func TestPaneView_TranslateKey_BracketedPasteWrapsWhenActive(t *testing.T) {
	pv := &PaneView{}
	runes := []rune("hello\nworld paste")
	pasteMsg := tea.KeyMsg{Type: tea.KeyRunes, Runes: runes, Paste: true}

	// Mode inactive → raw (back-compat; never emit markers unasked-for).
	if got := pv.translateKey(pasteMsg); string(got) != string(runes) {
		t.Fatalf("paste, mode inactive = %q, want raw %q", got, string(runes))
	}

	// Mode active → wrapped.
	pv.bracketedPasteActive.Store(true)
	want := "\x1b[200~" + string(runes) + "\x1b[201~"
	if got := pv.translateKey(pasteMsg); string(got) != want {
		t.Fatalf("paste, mode active = %q, want wrapped %q", got, want)
	}

	// Typed (non-paste) runes are never wrapped, even with mode active.
	typed := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ab")}
	if got := pv.translateKey(typed); string(got) != "ab" {
		t.Fatalf("typed runes, mode active = %q, want raw %q", got, "ab")
	}

	// Mode disabled again → raw.
	pv.bracketedPasteActive.Store(false)
	if got := pv.translateKey(pasteMsg); string(got) != string(runes) {
		t.Fatalf("paste, mode disabled = %q, want raw %q", got, string(runes))
	}
}

// TestPaneView_ApplyOutput_TracksBracketedPasteMode verifies the client mirrors
// the child's DECSET 2004 state from the byte stream, the way it already tracks
// alt-screen / mouse / DECCKM. Mirrors TestPaneView_CursorAppMode_TracksDECCKMBytes.
func TestPaneView_ApplyOutput_TracksBracketedPasteMode(t *testing.T) {
	pv := &PaneView{width: 80, height: 24}
	pv.mu.Lock()
	pv.initEmulatorLocked()
	pv.mu.Unlock()
	t.Cleanup(func() {
		pv.mu.Lock()
		pv.teardownEmulatorLocked()
		pv.mu.Unlock()
	})

	if pv.bracketedPasteActive.Load() {
		t.Fatal("bracketedPasteActive = true before any ?2004h, want false")
	}
	pv.applyOutput([]byte("\x1b[?2004h"))
	if !pv.bracketedPasteActive.Load() {
		t.Fatal("after ESC[?2004h, bracketedPasteActive = false, want true")
	}
	pv.applyOutput([]byte("\x1b[?2004l"))
	if pv.bracketedPasteActive.Load() {
		t.Fatal("after ESC[?2004l, bracketedPasteActive = true, want false")
	}
}
