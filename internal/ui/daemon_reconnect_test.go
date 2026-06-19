package ui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
)

// newPipeClient builds a real, LIVE *daemonclient.Client over a net.Pipe
// with a goroutine that speaks just enough of the codec to satisfy the
// Hello handshake (mirrors internal/daemonclient/version_skew_test.go).
// The returned client's readLoop is running; the fake daemon keeps
// draining so the client doesn't trip a disconnect. Closed() is false.
func newPipeClient(t *testing.T) *daemonclient.Client {
	t.Helper()
	clientSide, daemonSide := net.Pipe()

	srvErr := make(chan error, 1)
	go func() {
		r := bufio.NewReader(daemonSide)
		typ, payload, err := daemon.ReadFrame(r)
		if err != nil {
			srvErr <- err
			return
		}
		if typ != daemon.TypeJSONReq {
			srvErr <- fmt.Errorf("frame type 0x%02x", typ)
			return
		}
		name, _, err := daemon.DecodeEnvelope(payload)
		if err != nil || name != daemon.MsgHelloReq {
			srvErr <- fmt.Errorf("envelope name=%q err=%v", name, err)
			return
		}
		resp := daemon.HelloResp{
			ProtocolVersion: daemon.ProtocolVersion,
			BinaryVersion:   "fake-daemon",
			ClientCount:     1,
			ClientID:        7,
		}
		out, err := daemon.EncodeMsg(daemon.MsgHelloResp, resp)
		if err != nil {
			srvErr <- err
			return
		}
		if err := daemon.WriteFrame(daemonSide, daemon.TypeJSONResp, out); err != nil {
			srvErr <- err
			return
		}
		srvErr <- nil
		// Keep draining so the client's readLoop stays alive (and the
		// client stays !Closed) until the test's cleanup closes the conn.
		for {
			if _, _, err := daemon.ReadFrame(r); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	c, err := daemonclient.NewWithConn(ctx, clientSide)
	if err != nil {
		t.Fatalf("NewWithConn: %v", err)
	}
	if err := <-srvErr; err != nil {
		t.Fatalf("fake daemon: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Close()
		_ = daemonSide.Close()
	})
	return c
}

// newClosedClient returns a real client that has been Close()d, so
// Closed() reports true — the terminal state a daemon restart leaves the
// TUI's control client in.
func newClosedClient(t *testing.T) *daemonclient.Client {
	t.Helper()
	c := newPipeClient(t)
	_ = c.Close()
	if !c.Closed() {
		t.Fatal("newClosedClient: Closed() = false after Close()")
	}
	return c
}

// TestResyncErrorTriggersReconnectWhenClientClosed is the core wiring
// guard: a failed resync whose control client is terminally Closed must
// kick a reconnect attempt. Reverting the Task-3 wiring in
// handleDaemonResyncMsg makes the positive case fail (daemonReconnecting
// stays false) — red-before-green.
func TestResyncErrorTriggersReconnectWhenClientClosed(t *testing.T) {
	m := makeReconcileTestModel(t, &listStubAPI{}, nil)
	m.daemonClient = newClosedClient(t)

	_, cmd := m.handleDaemonResyncMsg(daemonResyncMsg{err: errors.New("cannot reach openkanbankd")})

	if !m.daemonReconnecting {
		t.Fatal("daemonReconnecting = false; want true (closed client should trigger reconnect)")
	}
	if cmd == nil {
		t.Fatal("cmd = nil; want a batch (resync re-arm + reconnect)")
	}
}

// TestResyncErrorNoReconnectWhenClientLive is the non-vacuous control:
// the SAME failed-resync path must NOT reconnect when the control client
// is still live (a transient List error, not a dead daemon). If the
// gate were vacuous (reconnect on any error), this would fail.
func TestResyncErrorNoReconnectWhenClientLive(t *testing.T) {
	m := makeReconcileTestModel(t, &listStubAPI{}, nil)
	m.daemonClient = newPipeClient(t) // live, !Closed

	_, _ = m.handleDaemonResyncMsg(daemonResyncMsg{err: errors.New("transient")})

	if m.daemonReconnecting {
		t.Fatal("daemonReconnecting = true; want false (live client must not trigger reconnect)")
	}
}

// TestReconnectSkewIsTerminal pins the subtle correctness trap: a
// protocol-version-skew result must NOT retry and must NOT swap the
// client — a re-dial can't fix a version mismatch, so the handler stays
// degraded and tells the user to restart the daemon.
func TestReconnectSkewIsTerminal(t *testing.T) {
	sentinel := &listStubAPI{}
	m := makeReconcileTestModel(t, sentinel, nil)
	m.daemonClient = newClosedClient(t)
	m.daemonReconnecting = true // an attempt was in flight

	skew := fmt.Errorf("%w: client=1 daemon=2", daemonclient.ErrProtocolVersionSkew)
	_, cmd := m.handleDaemonReconnectedMsg(daemonReconnectedMsg{err: skew})

	if m.daemonReconnecting {
		t.Error("daemonReconnecting = true; want false (must clear the in-flight flag)")
	}
	if cmd != nil {
		t.Error("cmd != nil; want nil (skew is terminal — no re-arm/retry)")
	}
	if m.daemon != sentinel {
		t.Error("m.daemon was swapped on skew; want unchanged")
	}
}

// TestReconnectGenericErrorStopsAndDefersToResync: a non-skew failure
// clears the flag and returns no cmd (the still-running resync tick
// drives the next attempt). Non-vacuous vs the success case below.
func TestReconnectGenericErrorStopsAndDefersToResync(t *testing.T) {
	sentinel := &listStubAPI{}
	m := makeReconcileTestModel(t, sentinel, nil)
	m.daemonClient = newClosedClient(t)
	m.daemonReconnecting = true

	_, cmd := m.handleDaemonReconnectedMsg(daemonReconnectedMsg{err: errors.New("dial failed")})

	if m.daemonReconnecting {
		t.Error("daemonReconnecting = true; want false")
	}
	if cmd != nil {
		t.Error("cmd != nil; want nil (resync tick retries, not a self re-arm)")
	}
	if m.daemon != sentinel {
		t.Error("m.daemon was swapped on failure; want unchanged")
	}
}

// TestReconnectSuccessSwapsClient: a successful re-dial swaps BOTH
// m.daemonClient and m.daemon to the fresh client, clears the flag, and
// re-arms (non-nil cmd → the Subscribe re-arm). Reverting the swap in
// handleDaemonReconnectedMsg makes this fail.
func TestReconnectSuccessSwapsClient(t *testing.T) {
	sentinel := &listStubAPI{}
	m := makeReconcileTestModel(t, sentinel, nil)
	old := newClosedClient(t)
	m.daemonClient = old
	m.daemonReconnecting = true

	fresh := newPipeClient(t)
	_, cmd := m.handleDaemonReconnectedMsg(daemonReconnectedMsg{client: fresh})

	if m.daemonReconnecting {
		t.Error("daemonReconnecting = true; want false")
	}
	if m.daemonClient != fresh {
		t.Error("m.daemonClient not swapped to the fresh client")
	}
	if m.daemon != fresh {
		t.Error("m.daemon not swapped to the fresh client")
	}
	if cmd == nil {
		t.Error("cmd = nil; want the Subscribe re-arm")
	}
}

// TestMaybeReconnectDaemonGuard: the in-flight guard prevents the 30s
// resync tick from launching overlapping dials.
func TestMaybeReconnectDaemonGuard(t *testing.T) {
	m := makeReconcileTestModel(t, &listStubAPI{}, nil)

	if cmd := m.maybeReconnectDaemon(); cmd == nil {
		t.Fatal("first maybeReconnectDaemon returned nil; want a cmd")
	}
	if !m.daemonReconnecting {
		t.Fatal("daemonReconnecting = false after first call; want true")
	}
	if cmd := m.maybeReconnectDaemon(); cmd != nil {
		t.Fatal("second maybeReconnectDaemon returned a cmd; want nil (guarded)")
	}
}
