package daemon

import (
	"bufio"
	"encoding/json"
	"testing"
	"time"
)

// TestSession_SetViewing_CounterAndChangedFlag exercises the per-
// session viewer set in isolation. Verifies:
//
//   - SetViewing(c, true) on a new viewer returns changed=true and
//     count=1.
//   - SetViewing(c, true) again from the SAME client is idempotent —
//     changed=false, count stays 1. (This is the test that proves the
//     daemon doesn't have to gate against repeat calls from a
//     misbehaving client.)
//   - A second client's SetViewing(true) bumps to count=2 (multiple
//     viewers per session are first-class).
//   - SetViewing(c, false) decrements and returns changed=true.
//   - RemoveViewer scrubs an unknown client safely (no panic, returns
//     false).
//   - ViewerCount returns the live count under contention.
func TestSession_SetViewing_CounterAndChangedFlag(t *testing.T) {
	s := &Session{viewers: map[uint16]struct{}{}}

	if count, changed := s.SetViewing(7, true); count != 1 || !changed {
		t.Errorf("first viewing: count=%d changed=%v, want 1 true", count, changed)
	}
	if count, changed := s.SetViewing(7, true); count != 1 || changed {
		t.Errorf("idempotent viewing: count=%d changed=%v, want 1 false", count, changed)
	}
	if count, changed := s.SetViewing(8, true); count != 2 || !changed {
		t.Errorf("second viewer: count=%d changed=%v, want 2 true", count, changed)
	}
	if got := s.ViewerCount(); got != 2 {
		t.Errorf("ViewerCount() = %d, want 2", got)
	}

	if count, changed := s.SetViewing(7, false); count != 1 || !changed {
		t.Errorf("first unviewing: count=%d changed=%v, want 1 true", count, changed)
	}
	if count, changed := s.SetViewing(7, false); count != 1 || changed {
		t.Errorf("idempotent unviewing: count=%d changed=%v, want 1 false", count, changed)
	}

	if removed := s.RemoveViewer(8); !removed {
		t.Errorf("RemoveViewer(8) = false, want true (8 was viewing)")
	}
	if removed := s.RemoveViewer(8); removed {
		t.Errorf("RemoveViewer(8) twice = true, want false (idempotent on absence)")
	}
	if got := s.ViewerCount(); got != 0 {
		t.Errorf("ViewerCount() after all removals = %d, want 0", got)
	}
}

// TestSubscribe_SetViewing_Broadcasts asserts the end-to-end RPC:
// client A's SetViewing(true) produces a "viewing" SessionEvent that
// observer B sees, SetViewing(false) produces "unviewing", and a
// duplicate-true call from A is suppressed (no second "viewing" event).
func TestSubscribe_SetViewing_Broadcasts(t *testing.T) {
	srv, errCh := startServer(t)

	a := dialTestClient(t, srv.SocketPath())
	ra := bufio.NewReader(a)
	helloAndUnpack(t, a, ra)

	b := dialTestClient(t, srv.SocketPath())
	rb := bufio.NewReader(b)
	subscribeClient(t, b, rb)

	sessID := spawnHelper(t, a, ra, SpawnReq{
		TicketID:    "VIEW-1",
		SessionName: "view-test",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})

	// Drain the "started" event B sees.
	if ev, ok := readNextSessionEvent(t, b, rb, 1500*time.Millisecond); !ok || ev.Event != "started" {
		t.Fatalf("expected 'started' on B, got ev=%+v ok=%v", ev, ok)
	}

	// A → SetViewing(true). B should see "viewing".
	writeReq(t, a, MsgSetViewingReq, SetViewingReq{SessionID: sessID, Viewing: true})
	a.SetReadDeadline(time.Now().Add(2 * time.Second))
	name, raw := readResp(t, ra)
	a.SetReadDeadline(time.Time{})
	if name != MsgSetViewingResp {
		t.Fatalf("SetViewing resp type = %q, want %q", name, MsgSetViewingResp)
	}
	var resp SetViewingResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode SetViewingResp: %v", err)
	}
	if resp.ViewerCount != 1 {
		t.Errorf("ViewerCount after first SetViewing(true) = %d, want 1", resp.ViewerCount)
	}
	ev, ok := readNextSessionEvent(t, b, rb, 1500*time.Millisecond)
	if !ok || ev.Event != "viewing" || ev.SessionID != sessID {
		t.Errorf("expected 'viewing' SessionID=%s, got ev=%+v ok=%v", sessID, ev, ok)
	}

	// A → SetViewing(true) again. Idempotent — no second "viewing"
	// event, ViewerCount still 1.
	writeReq(t, a, MsgSetViewingReq, SetViewingReq{SessionID: sessID, Viewing: true})
	a.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, raw = readResp(t, ra)
	a.SetReadDeadline(time.Time{})
	_ = json.Unmarshal(raw, &resp)
	if resp.ViewerCount != 1 {
		t.Errorf("ViewerCount after duplicate SetViewing(true) = %d, want 1", resp.ViewerCount)
	}
	// A "viewing" event arriving here would mean the daemon didn't
	// suppress the duplicate.
	if ev, ok := readNextSessionEvent(t, b, rb, 300*time.Millisecond); ok {
		t.Errorf("expected no event on duplicate viewing, got %+v", ev)
	}

	// A → SetViewing(false). B should see "unviewing".
	writeReq(t, a, MsgSetViewingReq, SetViewingReq{SessionID: sessID, Viewing: false})
	a.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, raw = readResp(t, ra)
	a.SetReadDeadline(time.Time{})
	_ = json.Unmarshal(raw, &resp)
	if resp.ViewerCount != 0 {
		t.Errorf("ViewerCount after SetViewing(false) = %d, want 0", resp.ViewerCount)
	}
	ev, ok = readNextSessionEvent(t, b, rb, 1500*time.Millisecond)
	if !ok || ev.Event != "unviewing" {
		t.Errorf("expected 'unviewing', got ev=%+v ok=%v", ev, ok)
	}

	// Cleanup.
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

// TestDisconnect_CleansViewers asserts that when a client viewing one
// or more sessions disconnects without an explicit SetViewing(false),
// the daemon scrubs that client from every viewers set and broadcasts
// "unviewing" SessionEvents. Without this, a crashed TUI would leave
// stale viewer counts on sibling boards.
func TestDisconnect_CleansViewers(t *testing.T) {
	srv, errCh := startServer(t)

	a := dialTestClient(t, srv.SocketPath())
	ra := bufio.NewReader(a)
	helloAndUnpack(t, a, ra)

	b := dialTestClient(t, srv.SocketPath())
	rb := bufio.NewReader(b)
	subscribeClient(t, b, rb)

	sessID := spawnHelper(t, a, ra, SpawnReq{
		TicketID:    "VIEW-DC",
		SessionName: "view-dc-test",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})

	// Drain the "started" event.
	if ev, ok := readNextSessionEvent(t, b, rb, 1500*time.Millisecond); !ok || ev.Event != "started" {
		t.Fatalf("expected 'started', got ev=%+v ok=%v", ev, ok)
	}

	// A views the session.
	writeReq(t, a, MsgSetViewingReq, SetViewingReq{SessionID: sessID, Viewing: true})
	a.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = readResp(t, ra)
	a.SetReadDeadline(time.Time{})

	// Drain the "viewing" event.
	if ev, ok := readNextSessionEvent(t, b, rb, 1500*time.Millisecond); !ok || ev.Event != "viewing" {
		t.Fatalf("expected 'viewing', got ev=%+v ok=%v", ev, ok)
	}

	// A disconnects abruptly (no SetViewing(false)).
	a.Close()

	// B should observe an automatic "unviewing" event.
	ev, ok := readNextSessionEvent(t, b, rb, 2*time.Second)
	if !ok {
		t.Fatalf("no unviewing event after abrupt client disconnect (zombie viewer count would persist)")
	}
	if ev.Event != "unviewing" {
		t.Errorf("expected 'unviewing' on disconnect, got %q", ev.Event)
	}
	if ev.SessionID != sessID {
		t.Errorf("ev.SessionID = %q, want %q", ev.SessionID, sessID)
	}

	// Cleanup.
	c := dialTestClient(t, srv.SocketPath())
	rc := bufio.NewReader(c)
	helloAndUnpack(t, c, rc)
	writeReq(t, c, MsgKillReq, KillReq{SessionID: sessID, GraceSeconds: 0})
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = readResp(t, rc)
	c.SetReadDeadline(time.Time{})
	c.Close()

	b.Close()
	waitServerDone(t, errCh, 5*time.Second)
}
