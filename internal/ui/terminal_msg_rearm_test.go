package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/daemonclient"
)

// countTeaMsgReaders runs every leaf Cmd produced by handleTerminalMsg and
// counts how many return a PaneOutputMsg for paneID — i.e. how many readers of
// the pane's single-reader teaMsgs channel were armed. The pane's channel is
// pre-filled with sentinels by the caller so each reader returns immediately
// instead of blocking, making the count fully deterministic (no goroutines).
func countTeaMsgReaders(t *testing.T, cmd tea.Cmd, paneID string) int {
	t.Helper()
	count := 0
	var run func(c tea.Cmd)
	run = func(c tea.Cmd) {
		if c == nil {
			return
		}
		switch m := c().(type) {
		case tea.BatchMsg:
			for _, sub := range m {
				run(sub)
			}
		case daemonclient.PaneOutputMsg:
			if m.PaneID == paneID {
				count++
			}
		}
	}
	run(cmd)
	return count
}

// TestHandleTerminalMsg_PaneOutputArmsSingleReader pins the invariant that a
// PaneOutputMsg arms exactly ONE reader of the pane's teaMsgs channel.
//
// PaneView.Update already returns readNextMsg() for PaneOutputMsg (and
// PaneAttachedMsg). If handleTerminalMsg ALSO bridges in listenPaneMessages
// for the same pane, two goroutines compete for one single-reader channel;
// the loser parks forever (listenPaneMessages has no closeCh escape), leaking
// a parked reader — and its parent execBatchMsg WaitGroup waiter — per output
// event. A long-lived session accumulates thousands. Regression for the stall
// dump showing 181 parked listenPaneMessages + 1008 execBatchMsg waiters.
func TestHandleTerminalMsg_PaneOutputArmsSingleReader(t *testing.T) {
	const tid = "T-rearm"
	pv := daemonclient.NewPaneViewForTest(tid)
	// Pre-fill so any armed reader returns a sentinel immediately rather than
	// blocking on an empty channel.
	for i := 0; i < 8; i++ {
		pv.EmitForTest(daemonclient.PaneOutputMsg{PaneID: tid})
	}
	m := &Model{panes: map[board.TicketID]*daemonclient.PaneView{board.TicketID(tid): pv}}

	_, cmd := m.handleTerminalMsg(daemonclient.PaneOutputMsg{PaneID: tid})

	if got := countTeaMsgReaders(t, cmd, tid); got != 1 {
		t.Fatalf("PaneOutputMsg armed %d teaMsgs readers, want 1 "+
			"(double-arm leaks a permanently-parked reader per output event)", got)
	}
}

// TestHandleTerminalMsg_RenderTickStillRearms guards the other half: for
// messages where PaneView.Update returns nil (e.g. PaneRenderTickMsg), the
// listenPaneMessages bridge MUST still arm the reader, or the pane goes deaf
// after a render tick.
func TestHandleTerminalMsg_RenderTickStillRearms(t *testing.T) {
	const tid = "T-tick"
	pv := daemonclient.NewPaneViewForTest(tid)
	for i := 0; i < 8; i++ {
		pv.EmitForTest(daemonclient.PaneOutputMsg{PaneID: tid})
	}
	m := &Model{panes: map[board.TicketID]*daemonclient.PaneView{board.TicketID(tid): pv}}

	_, cmd := m.handleTerminalMsg(daemonclient.PaneRenderTickMsg{PaneID: tid})

	if got := countTeaMsgReaders(t, cmd, tid); got != 1 {
		t.Fatalf("PaneRenderTickMsg armed %d teaMsgs readers, want 1 "+
			"(bridge must re-arm when PaneView.Update returns nil)", got)
	}
}
