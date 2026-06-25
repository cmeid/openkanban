package daemonclient

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestPaneView_readNextMsg_ReturnsOnDetach is the regression guard for the
// poller-goroutine leak that degraded a long-running TUI into a perceived
// "freeze".
//
// Each agent-view attach arms a readNextMsg poll that parks on the pane's
// teaMsgs channel. Before the fix the poll's select watched only teaMsgs and
// the daemon-wide client.closeCh — so a *detach* (leaving the agent view),
// which neither closes teaMsgs nor fires closeCh, left that poll parked
// forever. Its bubbletea execBatchMsg parent then parked on its WaitGroup
// forever too. Every agent-view enter/exit cycle leaked one such pair; over a
// session this reached >1600 goroutines, and the GC/scheduler tax on those
// stacks manifested as the multi-second UI freeze.
//
// The poll must wake on detach. Red-before-green: without the
// `case <-detachCh` arm in readNextMsg, the poll never returns after detach and
// the second select below times out.
func TestPaneView_readNextMsg_ReturnsOnDetach(t *testing.T) {
	pv := &PaneView{
		id:       "L1",
		state:    PaneViewAttached,
		teaMsgs:  make(chan tea.Msg, 64),
		detachCh: make(chan struct{}),
		client:   &Client{closeCh: make(chan struct{})},
	}

	cmd := pv.readNextMsg()
	if cmd == nil {
		t.Fatal("readNextMsg returned nil Cmd while attached")
	}

	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()

	// Sanity: the poll parks while attached with nothing pending.
	select {
	case <-done:
		t.Fatal("poll returned before any message or detach")
	case <-time.After(100 * time.Millisecond):
	}

	// Simulate Detach(): close + swap detachCh and flip state under p.mu —
	// mirrors detach() (paneview.go) and handleAttachExit.
	pv.mu.Lock()
	close(pv.detachCh)
	pv.detachCh = make(chan struct{})
	pv.state = PaneViewUnattached
	pv.mu.Unlock()

	// The parked poll must wake promptly (returning a benign nil msg) rather
	// than leak until Close().
	select {
	case msg := <-done:
		if msg != nil {
			t.Errorf("poll returned %T on detach, want nil", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("poll did not return after detach — goroutine leaked")
	}

	// After detach, re-arming must not spawn a fresh forever-parked poll
	// (closes the race where a final buffered output msg re-arms readNextMsg
	// just after detach).
	if again := pv.readNextMsg(); again != nil {
		t.Error("readNextMsg armed a poll while detached — re-arm leak path open")
	}
}
