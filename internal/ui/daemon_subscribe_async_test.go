package ui

import (
	"errors"
	"testing"

	"github.com/techdufus/openkanban/internal/daemon"
)

// TestSubscribeDaemonEventsCmd_NilClientFailsFast guards the startup-hang
// fix: the Subscribe handshake is armed as a bounded async cmd, and a
// missing client must resolve to a failure message immediately rather than
// hang. (The wedged-daemon deadline case is covered by the bounded context
// in subscribeDaemonEventsCmd + client.Subscribe honoring it.)
func TestSubscribeDaemonEventsCmd_NilClientFailsFast(t *testing.T) {
	msg := subscribeDaemonEventsCmd(nil)()

	failed, ok := msg.(daemonSubscribeFailedMsg)
	if !ok {
		t.Fatalf("got %T, want daemonSubscribeFailedMsg", msg)
	}
	if !errors.Is(failed.Err, errDaemonClientNil) {
		t.Errorf("Err = %v, want errDaemonClientNil", failed.Err)
	}
}

// TestHandleDaemonSubscribeReady_InstallsChannelAndArmsReader pins the
// deferred counterpart to what NewModel used to do synchronously: install
// the push channel + cancel func, flip daemonConnected, and return the
// readNextDaemonEvent re-arm cmd.
func TestHandleDaemonSubscribeReady_InstallsChannelAndArmsReader(t *testing.T) {
	m := &Model{}
	ch := make(chan daemon.SessionEvent, 1)
	unsubCalled := false
	unsub := func() { unsubCalled = true }

	_, cmd := m.handleDaemonSubscribeReady(daemonSubscribeReadyMsg{events: ch, unsub: unsub})

	if m.daemonEvents == nil {
		t.Error("daemonEvents not installed")
	}
	if !m.daemonConnected.Load() {
		t.Error("daemonConnected should be true after a ready handshake")
	}
	if m.daemonUnsub == nil {
		t.Fatal("daemonUnsub not installed")
	}
	if cmd == nil {
		t.Fatal("expected a re-arm cmd (readNextDaemonEvent), got nil")
	}
	// Confirm the stored cancel func is the one we passed.
	m.daemonUnsub()
	if !unsubCalled {
		t.Error("stored daemonUnsub is not the func passed in the ready msg")
	}
}
