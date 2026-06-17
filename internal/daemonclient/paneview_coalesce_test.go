package daemonclient

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestPaneView_CoalescesRenderSignals is the regression guard for the
// large-paste render storm (tickets/build-handling-for-a-stuck-session.md).
// attachLoop calls signalRender once per 64KB PTY-output frame; a burst of
// frames between two model consumes must collapse to a SINGLE PaneOutputMsg.
// applyOutput already writes every frame's bytes to the emulator, so the
// model only needs one render signal to repaint the whole burst.
//
// Reverting the fix (making signalRender emit unconditionally) makes this
// assert 100 != 1 and fail — the red-before-green proof.
//
// Scoped to the coalescing state machine only (renderSignalPending + teaMsgs):
// no emulator is initialized, since signalRender/consumeRenderSignal never
// touch the vt.
func TestPaneView_CoalescesRenderSignals(t *testing.T) {
	pv := &PaneView{teaMsgs: make(chan tea.Msg, 64)}

	const burst = 100
	for i := 0; i < burst; i++ {
		pv.signalRender()
	}
	if got := len(pv.teaMsgs); got != 1 {
		t.Fatalf("emitted %d PaneOutputMsgs for a %d-frame burst, want 1 (coalesced)", got, burst)
	}

	// The model consuming the PaneOutputMsg clears the pending flag; the
	// next burst must then emit a fresh signal (output after a consume must
	// not render silently).
	<-pv.teaMsgs
	pv.consumeRenderSignal()

	pv.signalRender()
	if got := len(pv.teaMsgs); got != 1 {
		t.Fatalf("after consume, a new burst emitted %d signals, want 1", got)
	}
}

// TestPaneView_SignalRenderRearmsAfterConsume verifies the steady-state
// invariant: alternating signal/consume always yields exactly one queued
// signal per round, so normal (non-burst) output is never dropped.
func TestPaneView_SignalRenderRearmsAfterConsume(t *testing.T) {
	pv := &PaneView{teaMsgs: make(chan tea.Msg, 64)}

	for round := 0; round < 5; round++ {
		pv.signalRender()
		if got := len(pv.teaMsgs); got != 1 {
			t.Fatalf("round %d: queued %d signals before consume, want 1", round, got)
		}
		<-pv.teaMsgs
		pv.consumeRenderSignal()
	}
}
