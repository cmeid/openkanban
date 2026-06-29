package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/daemonclient"
)

// agentViewModel returns a minimal Model in ModeAgentView with a Detached pane
// (nil info → Detached; HandleKey and ScrollLines are safe no-ops with nil vt).
func agentViewModel(id board.TicketID) *Model {
	pane := daemonclient.NewPaneView(nil, string(id), "", nil)
	return &Model{
		mode:        ModeAgentView,
		focusedPane: id,
		panes:       map[board.TicketID]*daemonclient.PaneView{id: pane},
	}
}

func keyUp() tea.KeyMsg   { return tea.KeyMsg{Type: tea.KeyUp} }
func keyDown() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyDown} }

// TestAgentViewScroll_UpReturnsNilCmd verifies that an up arrow in agent view
// scrolls the pane and returns a nil cmd — no deferred Tick, no arrow forwarded
// to the agent after a delay.
func TestAgentViewScroll_UpReturnsNilCmd(t *testing.T) {
	m := agentViewModel("S1")
	_, cmd := m.handleAgentViewMode(keyUp())
	if cmd != nil {
		t.Error("up: want nil cmd (no deferred arrow forwarding to agent)")
	}
}

// TestAgentViewScroll_DownReturnsNilCmd is the same invariant for down.
func TestAgentViewScroll_DownReturnsNilCmd(t *testing.T) {
	m := agentViewModel("S2")
	_, cmd := m.handleAgentViewMode(keyDown())
	if cmd != nil {
		t.Error("down: want nil cmd (no deferred arrow forwarding to agent)")
	}
}

// TestAgentViewScroll_RepeatedArrowsAllNilCmd is the regression guard: every
// up/down in a rapid sequence must return nil cmd. Under the old classifier,
// isolated arrows returned a non-nil Tick cmd and were later forwarded to Claude.
func TestAgentViewScroll_RepeatedArrowsAllNilCmd(t *testing.T) {
	m := agentViewModel("S3")
	for i := 0; i < 10; i++ {
		msg := keyUp()
		if i%2 == 1 {
			msg = keyDown()
		}
		_, cmd := m.handleAgentViewMode(msg)
		if cmd != nil {
			t.Errorf("arrow[%d]: want nil cmd, got non-nil (arrow leaked to agent)", i)
		}
	}
}

func TestReconcileMouseMode(t *testing.T) {
	tests := []struct {
		name     string
		prevMode Mode
		currMode Mode
		wantNil  bool
	}{
		{"normal→agent: cmd emitted", ModeNormal, ModeAgentView, false},
		{"agent→normal: cmd emitted", ModeAgentView, ModeNormal, false},
		{"normal→normal: no-op", ModeNormal, ModeNormal, true},
		{"agent→agent: no-op", ModeAgentView, ModeAgentView, true},
		{"agent→spawning: cmd emitted", ModeAgentView, ModeSpawning, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Model{mode: tt.currMode}
			cmd := m.reconcileMouseMode(tt.prevMode)
			if (cmd == nil) != tt.wantNil {
				t.Errorf("cmd nil=%v, want nil=%v", cmd == nil, tt.wantNil)
			}
		})
	}
}
