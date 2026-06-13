package daemon

import (
	"bufio"
	"encoding/json"
	"net"
	"testing"
	"time"
)

// readNextSessionEvent reads frames from r until it sees a JSONPush
// envelope carrying MsgSessionEvent, then decodes and returns it.
// Times out after the given deadline. Other JSONPush types and any
// JSONResp frames are skipped.
func readNextSessionEvent(t *testing.T, conn net.Conn, r *bufio.Reader, deadline time.Duration) (SessionEvent, bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		typ, payload, err := ReadFrame(r)
		conn.SetReadDeadline(time.Time{})
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			t.Logf("readNextSessionEvent: ReadFrame: %v", err)
			return SessionEvent{}, false
		}
		if typ != TypeJSONPush {
			continue
		}
		name, raw, derr := DecodeEnvelope(payload)
		if derr != nil {
			t.Logf("readNextSessionEvent: DecodeEnvelope: %v", derr)
			continue
		}
		if name != MsgSessionEvent {
			continue
		}
		var ev SessionEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Logf("readNextSessionEvent: unmarshal SessionEvent: %v", err)
			continue
		}
		return ev, true
	}
	return SessionEvent{}, false
}

// subscribeClient performs the Hello + Subscribe handshake on conn
// and returns once the server has acknowledged with SubscribeResp.
func subscribeClient(t *testing.T, conn net.Conn, r *bufio.Reader) {
	t.Helper()
	helloAndUnpack(t, conn, r)
	writeReq(t, conn, MsgSubscribeReq, SubscribeReq{})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	name, _ := readResp(t, r)
	if name != MsgSubscribeResp {
		t.Fatalf("subscribe: got %q want %q", name, MsgSubscribeResp)
	}
}

// TestSubscribe_TwoClients_BroadcastStarted verifies that when client A
// spawns a session, client B (separately subscribed) receives a
// "started" SessionEvent. A intentionally does NOT subscribe so its
// RPC response demux is unambiguous; the realistic cross-TUI scenario
// is that the spawning client and the observing client are different
// processes anyway.
func TestSubscribe_TwoClients_BroadcastStarted(t *testing.T) {
	srv, errCh := startServer(t)

	a := dialTestClient(t, srv.SocketPath())
	ra := bufio.NewReader(a)
	helloAndUnpack(t, a, ra)

	b := dialTestClient(t, srv.SocketPath())
	rb := bufio.NewReader(b)
	subscribeClient(t, b, rb)

	// A spawns. B should observe the "started" push.
	sessID := spawnHelper(t, a, ra, SpawnReq{
		TicketID:    "BROADCAST-1",
		SessionName: "broadcast-started",
		Command:     "/bin/sleep",
		Args:        []string{"10"},
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})

	ev, ok := readNextSessionEvent(t, b, rb, 1500*time.Millisecond)
	if !ok {
		t.Fatalf("client B did not receive any SessionEvent within deadline")
	}
	if ev.Event != "started" {
		t.Errorf("ev.Event: got %q want %q", ev.Event, "started")
	}
	if ev.SessionID != sessID {
		t.Errorf("ev.SessionID: got %q want %q", ev.SessionID, sessID)
	}
	if ev.TicketID != "BROADCAST-1" {
		t.Errorf("ev.TicketID: got %q want %q", ev.TicketID, "BROADCAST-1")
	}

	// Clean up: A kills the session, then both disconnect.
	writeReq(t, a, MsgKillReq, KillReq{SessionID: sessID, GraceSeconds: 0})
	a.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = readResp(t, ra)
	a.SetReadDeadline(time.Time{})

	a.Close()
	b.Close()
	waitServerDone(t, errCh, 5*time.Second)
}

// TestSubscribe_PaneExit_BroadcastsExited spawns a short-lived process
// (/bin/true) on client A and asserts subscriber B receives both
// "started" and "exited" events. The "exited" event comes from the
// daemon's watchSessionExit goroutine observing the pane's terminal
// ExitEvent — NOT from an explicit Kill RPC.
func TestSubscribe_PaneExit_BroadcastsExited(t *testing.T) {
	srv, errCh := startServer(t)

	a := dialTestClient(t, srv.SocketPath())
	ra := bufio.NewReader(a)
	helloAndUnpack(t, a, ra)

	b := dialTestClient(t, srv.SocketPath())
	rb := bufio.NewReader(b)
	subscribeClient(t, b, rb)

	// Use /bin/echo (a "spawn → emit one frame → exit" workload) as the
	// short-lived child. /bin/true is /usr/bin/true on macOS; using
	// /bin/echo keeps the test portable across the platforms that ship
	// /bin/.
	sessID := spawnHelper(t, a, ra, SpawnReq{
		TicketID:    "EXIT-1",
		SessionName: "exit-test",
		Command:     "/bin/echo",
		Args:        []string{"done"},
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})

	// First event should be "started"; second (or later) "exited".
	// Loop reading events until we see "exited" or hit the deadline.
	deadline := time.Now().Add(3 * time.Second)
	sawStarted := false
	sawExited := false
	for time.Now().Before(deadline) && (!sawStarted || !sawExited) {
		ev, ok := readNextSessionEvent(t, b, rb, 500*time.Millisecond)
		if !ok {
			continue
		}
		if ev.SessionID != sessID {
			t.Errorf("event for unexpected SessionID %q (want %q)", ev.SessionID, sessID)
			continue
		}
		switch ev.Event {
		case "started":
			sawStarted = true
		case "exited":
			sawExited = true
		}
	}
	if !sawStarted {
		t.Errorf("did not observe 'started' event for session %s", sessID)
	}
	if !sawExited {
		t.Errorf("did not observe 'exited' event for session %s", sessID)
	}

	a.Close()
	b.Close()
	waitServerDone(t, errCh, 5*time.Second)
}

// TestSubscribe_AttachDetach_Broadcasts attaches from client A then
// detaches, and asserts the subscriber on client B sees "attached"
// followed by "detached".
func TestSubscribe_AttachDetach_Broadcasts(t *testing.T) {
	srv, errCh := startServer(t)

	// A is the actor: spawns and attaches. We don't Subscribe A so it
	// only sees the JSON responses to its own RPCs.
	a := dialTestClient(t, srv.SocketPath())
	ra := bufio.NewReader(a)
	helloAndUnpack(t, a, ra)

	// B is the observer.
	b := dialTestClient(t, srv.SocketPath())
	rb := bufio.NewReader(b)
	subscribeClient(t, b, rb)

	sessID := spawnHelper(t, a, ra, SpawnReq{
		TicketID:    "ATTACH-1",
		SessionName: "attach-test",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})

	// Drain the "started" event B sees.
	if ev, ok := readNextSessionEvent(t, b, rb, 1500*time.Millisecond); !ok || ev.Event != "started" {
		t.Fatalf("expected 'started' on B, got ev=%+v ok=%v", ev, ok)
	}

	// A attaches. Have to read the AttachResp + snapshot bytes off A's
	// conn so the daemon doesn't block writing into a buffer-full conn.
	resp, snapshot := attachAndUnpack(t, a, ra, AttachReq{
		SessionID: sessID,
		Cols:      80,
		Rows:      24,
	})
	if resp.ClientID == 0 {
		t.Errorf("AttachResp.ClientID = 0")
	}
	_ = snapshot

	// B should see "attached".
	ev, ok := readNextSessionEvent(t, b, rb, 1500*time.Millisecond)
	if !ok {
		t.Fatalf("client B did not receive 'attached' event within deadline")
	}
	if ev.Event != "attached" {
		t.Errorf("ev.Event: got %q want %q", ev.Event, "attached")
	}
	if ev.SessionID != sessID {
		t.Errorf("ev.SessionID: got %q want %q", ev.SessionID, sessID)
	}

	// A cleanly detaches.
	writeBinaryFrame(t, a, TypeDetach, nil)

	// B should see "detached".
	ev, ok = readNextSessionEvent(t, b, rb, 1500*time.Millisecond)
	if !ok {
		t.Fatalf("client B did not receive 'detached' event within deadline")
	}
	if ev.Event != "detached" {
		t.Errorf("ev.Event: got %q want %q", ev.Event, "detached")
	}

	// Clean up — kill the cat session and disconnect.
	c := dialTestClient(t, srv.SocketPath())
	rc := bufio.NewReader(c)
	helloAndUnpack(t, c, rc)
	writeReq(t, c, MsgKillReq, KillReq{SessionID: sessID, GraceSeconds: 0})
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = readResp(t, rc)
	c.SetReadDeadline(time.Time{})
	c.Close()

	a.Close()
	b.Close()
	waitServerDone(t, errCh, 5*time.Second)
}

// TestSubscribe_NonSubscribedClient_NoPush asserts that a client which
// did NOT call Subscribe does not receive push frames. We rely on the
// JSONResp demux on the client: a Subscribe-less client only receives
// frames it asked for, and would error/log unexpected JSONPush types.
func TestSubscribe_NonSubscribedClient_NoPush(t *testing.T) {
	srv, errCh := startServer(t)

	a := dialTestClient(t, srv.SocketPath())
	ra := bufio.NewReader(a)
	helloAndUnpack(t, a, ra) // no Subscribe

	b := dialTestClient(t, srv.SocketPath())
	rb := bufio.NewReader(b)
	helloAndUnpack(t, b, rb) // no Subscribe

	sessID := spawnHelper(t, a, ra, SpawnReq{
		TicketID:    "NOSUB-1",
		SessionName: "nosub-test",
		Command:     "/bin/sleep",
		Args:        []string{"10"},
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})

	// B should NOT receive any push frame within a brief window.
	if ev, ok := readNextSessionEvent(t, b, rb, 500*time.Millisecond); ok {
		t.Errorf("non-subscribed client B received unexpected SessionEvent: %+v", ev)
	}

	writeReq(t, a, MsgKillReq, KillReq{SessionID: sessID, GraceSeconds: 0})
	a.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = readResp(t, ra)
	a.SetReadDeadline(time.Time{})

	a.Close()
	b.Close()
	waitServerDone(t, errCh, 5*time.Second)
}

