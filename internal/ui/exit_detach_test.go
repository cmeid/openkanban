package ui

import (
	"testing"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/daemonclient"
)

// TestExitToBoard_ReleasesFocusedPane pins the #1 fix: leaving a session's
// agent view must release the daemon attach slot (Detach the focused pane), so
// a backgrounded TUI no longer holds the session's single attach slot hostage.
// It also guards the structural change to exitToBoard — clearing agent-view
// state and detaching with a focused pane present must not panic, and Detach on
// an unattached pane is a safe no-op leaving it Unattached.
func TestExitToBoard_ReleasesFocusedPane(t *testing.T) {
	m := newTakeoverTestModel(t)
	id := board.TicketID("T-EXIT")
	pv := unattachedPane(id)
	m.panes[id] = pv
	m.focusedPane = id
	m.cycleAttachPrompt = true

	m.exitToBoard()

	if m.mode != ModeNormal {
		t.Errorf("mode = %v, want ModeNormal", m.mode)
	}
	if m.focusedPane != "" {
		t.Errorf("focusedPane = %q, want empty", m.focusedPane)
	}
	if m.cycleAttachPrompt {
		t.Error("cycleAttachPrompt = true, want false")
	}
	if got := pv.State(); got != daemonclient.PaneViewUnattached {
		t.Errorf("pane state = %v, want PaneViewUnattached after exitToBoard", got)
	}
}
