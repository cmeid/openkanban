package daemonclient

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// trackConn wraps net.Conn to count Close() calls. The underlying Conn may
// be nil (stub-only mode) — in that case Close() is a no-op beyond counting.
type trackConn struct {
	net.Conn
	closes atomic.Int32
}

func (c *trackConn) Close() error {
	c.closes.Add(1)
	if c.Conn != nil {
		return c.Conn.Close()
	}
	return nil
}

// TestPaneView_StaleLoopExitDoesNotClobberNewAttach guards the double-Enter
// regression: when Detach() closes conn1 but the old attachLoop hasn't exited
// yet, a concurrent attach() may set p.attachConn = conn2. If the old loop's
// handleAttachExit grabs p.attachConn (conn2) and closes it, the new
// attachLoop dies immediately with frames=0, and the TUI bounces back to the
// board on the first Enter.
//
// Fix: handleAttachExit receives ownConn (the conn it was started with) and
// bails out silently when p.attachConn != ownConn — leaving the new attach's
// state intact.
func TestPaneView_StaleLoopExitDoesNotClobberNewAttach(t *testing.T) {
	c1 := &trackConn{} // old (stale) conn — stub-only, no underlying pipe
	c2, r2 := net.Pipe()
	defer c2.Close()
	defer r2.Close()

	pv := NewPaneViewForTest("ticket-abc")

	// Simulate the state AFTER a new attach() has replaced the old conn:
	// p.attachConn = c2, state = Attached.
	pv.mu.Lock()
	pv.attachConn = c2
	pv.state = PaneViewAttached
	pv.mu.Unlock()

	// The OLD attachLoop exits (ownConn = c1, which != p.attachConn c2).
	pv.handleAttachExit(c1, nil, true)

	// p.attachConn must still be c2 and state still Attached.
	pv.mu.Lock()
	stillConn := pv.attachConn == c2
	stillAttached := pv.state == PaneViewAttached
	pv.mu.Unlock()

	if !stillConn {
		t.Error("handleAttachExit for stale conn clobbered p.attachConn (new conn lost)")
	}
	if !stillAttached {
		t.Error("handleAttachExit for stale conn reset state to Unattached (new attach killed)")
	}

	// The stale conn must have been closed exactly once.
	if n := c1.closes.Load(); n != 1 {
		t.Errorf("stale conn Close() called %d times, want 1", n)
	}

	// No PaneDetachedMsg should appear in the channel.
	timeout := time.NewTimer(20 * time.Millisecond)
	defer timeout.Stop()
	for {
		select {
		case msg := <-pv.teaMsgs:
			if _, ok := msg.(PaneDetachedMsg); ok {
				t.Error("handleAttachExit for stale conn emitted PaneDetachedMsg, want none")
				return
			}
		case <-timeout.C:
			return // no PaneDetachedMsg — correct
		}
	}
}

// TestPaneView_StalePaneDetachedMsgSkippedOnAttachDrain guards the second
// double-Enter regression class: after exitToBoard() calls Detach(), the
// detach()'s WG goroutine emits PaneDetachedMsg into teaMsgs once the old
// attachLoop drains. If the board is not actively consuming the channel,
// that message sits buffered. On the next Enter, doAttach (model.go) calls
// attach() then drains one message from TeaMessages(). Without the fix that
// message is the stale PaneDetachedMsg, which causes exitToBoard() to fire
// and immediately undo the fresh attach — the flash-and-bounce the user sees.
//
// Red-before-green: remove the `continue` line in the drain loop below and
// re-run — the drain returns PaneDetachedMsg instead of PaneAttachedMsg,
// demonstrating the channel-level bug. The same `continue` in doAttach
// (model.go) is what prevents it in production.
func TestPaneView_StalePaneDetachedMsgSkippedOnAttachDrain(t *testing.T) {
	const id = "ticket-stale-drain"
	pv := NewPaneViewForTest(id)

	// Seed the channel exactly as it appears after Detach()+attach():
	// stale PaneDetachedMsg (from detach()'s WG goroutine) then the fresh
	// PaneAttachedMsg (from the new attach()).
	pv.teaMsgs <- PaneDetachedMsg{PaneID: id}
	pv.teaMsgs <- PaneAttachedMsg{PaneID: id}

	// Run the skip-detach drain from doAttach. Must return PaneAttachedMsg,
	// not the stale PaneDetachedMsg.
	timeout := time.NewTimer(50 * time.Millisecond)
	defer timeout.Stop()
	var got tea.Msg
loop:
	for {
		select {
		case msg, ok := <-pv.TeaMessages():
			if !ok {
				t.Fatal("teaMsgs closed unexpectedly")
			}
			if _, isDetach := msg.(PaneDetachedMsg); isDetach {
				continue // skip stale — mirrors fix in doAttach
			}
			got = msg
			break loop
		case <-timeout.C:
			t.Fatal("drain timed out: no non-PaneDetachedMsg within 50ms")
		}
	}

	if _, ok := got.(PaneAttachedMsg); !ok {
		t.Errorf("attach drain returned %T, want PaneAttachedMsg "+
			"(stale PaneDetachedMsg was not skipped — would cause board bounce)", got)
	}
}

// TestPaneView_MatchingLoopExitEmitsDetachedMsg confirms that when the exiting
// loop owns the current conn, the normal cleanup path fires: state →
// Unattached, PaneDetachedMsg emitted.
func TestPaneView_MatchingLoopExitEmitsDetachedMsg(t *testing.T) {
	c, r := net.Pipe()
	r.Close()

	pv := NewPaneViewForTest("ticket-def")

	pv.mu.Lock()
	pv.initEmulatorLocked()
	pv.attachConn = c
	pv.state = PaneViewAttached
	pv.mu.Unlock()

	pv.handleAttachExit(c, nil, true)

	pv.mu.Lock()
	state := pv.state
	conn := pv.attachConn
	pv.mu.Unlock()

	if state != PaneViewUnattached {
		t.Errorf("state = %v, want PaneViewUnattached", state)
	}
	if conn != nil {
		t.Error("p.attachConn was not cleared after matching exit")
	}

	// PaneDetachedMsg must appear promptly.
	var got tea.Msg
	select {
	case got = <-pv.teaMsgs:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("no message in teaMsgs within 100ms after matching exit")
	}
	if _, ok := got.(PaneDetachedMsg); !ok {
		t.Errorf("got %T, want PaneDetachedMsg", got)
	}
}
