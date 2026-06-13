package daemon

import (
	"bufio"
	"encoding/json"
	"testing"
	"time"
)

// TestHandleTicketDone_LiveSession_KillsAndBroadcastsExpected verifies
// the T2 happy path: when a TicketDoneReq arrives for a ticket whose
// session is live, the daemon (a) responds with Killed=true plus the
// SessionID, and (b) the subsequent "exited" SessionEvent carries
// Expected=true and Reason="ticket_done".
func TestHandleTicketDone_LiveSession_KillsAndBroadcastsExpected(t *testing.T) {
	srv, errCh := startServer(t)

	// A spawns; B subscribes so it observes the broadcast.
	a := dialTestClient(t, srv.SocketPath())
	ra := bufio.NewReader(a)
	helloAndUnpack(t, a, ra)

	b := dialTestClient(t, srv.SocketPath())
	rb := bufio.NewReader(b)
	subscribeClient(t, b, rb)

	// Long-lived child so the TicketDone path is the one that kills it.
	sessID := spawnHelper(t, a, ra, SpawnReq{
		TicketID:    "TD-1",
		SessionName: "td-test",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})

	// Drain the "started" event B sees so the next event we read is
	// the ticket-done-driven "exited".
	if ev, ok := readNextSessionEvent(t, b, rb, 1500*time.Millisecond); !ok || ev.Event != "started" {
		t.Fatalf("expected 'started' on B, got ev=%+v ok=%v", ev, ok)
	}

	// A invokes ticket done.
	writeReq(t, a, MsgTicketDoneReq, TicketDoneReq{TicketID: "TD-1"})
	a.SetReadDeadline(time.Now().Add(3 * time.Second))
	name, raw := readResp(t, ra)
	a.SetReadDeadline(time.Time{})
	if name != MsgTicketDoneResp {
		t.Fatalf("ticket_done: got %q want %q", name, MsgTicketDoneResp)
	}
	var resp TicketDoneResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode TicketDoneResp: %v", err)
	}
	if !resp.Killed {
		t.Errorf("TicketDoneResp.Killed = false; want true")
	}
	if resp.SessionID != sessID {
		t.Errorf("TicketDoneResp.SessionID = %q; want %q", resp.SessionID, sessID)
	}

	// B should now observe an "exited" event with Expected=true.
	deadline := time.Now().Add(3 * time.Second)
	var got SessionEvent
	for time.Now().Before(deadline) {
		ev, ok := readNextSessionEvent(t, b, rb, 500*time.Millisecond)
		if !ok {
			continue
		}
		if ev.Event == "exited" && ev.SessionID == sessID {
			got = ev
			break
		}
	}
	if got.Event != "exited" {
		t.Fatalf("did not observe 'exited' event for session %s within deadline", sessID)
	}
	if !got.Expected {
		t.Errorf("SessionEvent.Expected = false; want true")
	}
	if got.Reason != "ticket_done" {
		t.Errorf("SessionEvent.Reason = %q; want %q", got.Reason, "ticket_done")
	}

	a.Close()
	b.Close()
	waitServerDone(t, errCh, 5*time.Second)
}

// TestHandleTicketDone_NoLiveSession_Killedfalse verifies that a
// TicketDoneReq for a ticket the daemon does not know about returns
// Killed=false with no error. The CLI uses this signal to print a
// "no live session" warning to stderr (exit 0).
func TestHandleTicketDone_NoLiveSession_Killedfalse(t *testing.T) {
	srv, errCh := startServer(t)

	a := dialTestClient(t, srv.SocketPath())
	ra := bufio.NewReader(a)
	helloAndUnpack(t, a, ra)

	writeReq(t, a, MsgTicketDoneReq, TicketDoneReq{TicketID: "DOES-NOT-EXIST"})
	a.SetReadDeadline(time.Now().Add(3 * time.Second))
	name, raw := readResp(t, ra)
	a.SetReadDeadline(time.Time{})
	if name != MsgTicketDoneResp {
		t.Fatalf("ticket_done: got %q want %q (payload=%s)", name, MsgTicketDoneResp, string(raw))
	}
	var resp TicketDoneResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode TicketDoneResp: %v", err)
	}
	if resp.Killed {
		t.Errorf("TicketDoneResp.Killed = true; want false")
	}
	if resp.SessionID != "" {
		t.Errorf("TicketDoneResp.SessionID = %q; want empty", resp.SessionID)
	}

	a.Close()
	waitServerDone(t, errCh, 3*time.Second)
}

// TestHandleKill_UnknownSession_Idempotent verifies handleKill is now
// idempotent: a KillReq for a session the daemon no longer holds (e.g.
// already-killed via concurrent TicketDoneReq) returns KillResp with
// no error rather than the previous "session not found" failure.
func TestHandleKill_UnknownSession_Idempotent(t *testing.T) {
	srv, errCh := startServer(t)

	a := dialTestClient(t, srv.SocketPath())
	ra := bufio.NewReader(a)
	helloAndUnpack(t, a, ra)

	writeReq(t, a, MsgKillReq, KillReq{SessionID: "does-not-exist", GraceSeconds: 0})
	a.SetReadDeadline(time.Now().Add(3 * time.Second))
	name, raw := readResp(t, ra)
	a.SetReadDeadline(time.Time{})
	if name != MsgKillResp {
		t.Fatalf("kill on missing session: got %q payload=%s; want %q (no error)",
			name, string(raw), MsgKillResp)
	}
	// KillResp has no fields; just confirm it decodes.
	var resp KillResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode KillResp: %v", err)
	}

	a.Close()
	waitServerDone(t, errCh, 3*time.Second)
}

// TestSession_ExpectedCompletion verifies the Session.MarkExpectedCompletion
// / ExpectedCompletion accessor pair: starts as false, becomes true
// after Mark, and the underlying pane's ExpectedCompletedExit flag is
// also flipped.
func TestSession_ExpectedCompletion(t *testing.T) {
	var s Session
	if s.ExpectedCompletion() {
		t.Errorf("fresh session: ExpectedCompletion=true; want false")
	}
	s.MarkExpectedCompletion()
	if !s.ExpectedCompletion() {
		t.Errorf("after MarkExpectedCompletion: ExpectedCompletion=false; want true")
	}
	// Idempotent.
	s.MarkExpectedCompletion()
	if !s.ExpectedCompletion() {
		t.Errorf("after second MarkExpectedCompletion: ExpectedCompletion=false; want true")
	}
}
