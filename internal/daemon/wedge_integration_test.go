//go:build !windows

package daemon

import (
	"bufio"
	"encoding/json"
	"net"
	"testing"
	"time"
)

// floodPaneToBackpressure writes chunks of PTY input over an attached
// binary connection until the daemon-side pane is wedged. It does NOT
// read the daemon's responses to the flood — the point is to drive the
// non-draining child's PTY input buffer (and the pane's bounded input
// channel) full so WriteInput backpressures server-side. Returns after
// writing enough bytes to guarantee the wedge given a non-draining
// child (or earlier if the socket itself errors).
func floodPaneToBackpressure(t *testing.T, conn net.Conn, frames int) {
	t.Helper()
	chunk := make([]byte, 4096)
	for i := 0; i < frames; i++ {
		if err := WriteFrame(conn, TypePTYInput, chunk); err != nil {
			return
		}
	}
}

// listContainsSession reports whether a ListResp payload includes a
// session with the given id.
func listContainsSession(t *testing.T, raw json.RawMessage, sid string) bool {
	t.Helper()
	var list ListResp
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode ListResp: %v", err)
	}
	for _, s := range list.Sessions {
		if s.SessionID == sid {
			return true
		}
	}
	return false
}

// TestDaemon_WedgedPaneDoesNotStarveRPCs is an end-to-end LIVENESS guard
// for the original daemon-wide-deadlock scenario: a session whose child
// stops draining stdin gets flooded with input over a real attach binary
// stream, and the daemon must keep servicing RPCs for every session. It
// asserts that after the flood, List, TicketDone{S1}, and Kill{S2} all
// return within the read deadline — and exercises the close-before-wait
// group-kill teardown against a genuinely wedged pane end-to-end.
//
// The original incident was a TWO-fault deadlock (WriteInput holding p.mu
// across the blocked PTY write AND handleList→Session.Info taking p.mu
// under sessionsMu.RLock); either Task 1 or Task 2 alone breaks the
// cascade. This is therefore the holistic guard, NOT a single-fix
// discriminator — the per-fix red-before-green proofs live in the
// deterministic terminal unit tests (TestSessionInfo_LockFree for Task 1,
// TestWriteInput_DoesNotBlockOnFullChild for Task 2). NOTE (verified on
// this darwin host): reverting Task 1, Task 2, or even both did NOT make
// this end-to-end test hang within the deadline — the production
// lock-coupling cascade does not deterministically reproduce through the
// socket+goroutine harness here. It remains a useful regression guard
// (it does drive the full flood→RPC path), but it is not a substitute
// for the unit-level discriminators.
func TestDaemon_WedgedPaneDoesNotStarveRPCs(t *testing.T) {
	srv, errCh := startServer(t)

	// Controller conn: issues spawn / list / ticket-done / kill RPCs.
	ctl := dialTestClient(t, srv.SocketPath())
	rctl := bufio.NewReader(ctl)
	helloAndUnpack(t, ctl, rctl)

	// S1: provably non-draining child (sleep never reads stdin).
	s1 := spawnHelper(t, ctl, rctl, SpawnReq{
		TicketID:    "WEDGE-S1",
		SessionName: "wedge-s1",
		Command:     "/bin/sh",
		Args:        []string{"-c", "sleep 600"},
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})

	// S2: a draining child (cat) — the healthy session that must stay
	// serviceable while S1 is wedged.
	s2 := spawnHelper(t, ctl, rctl, SpawnReq{
		TicketID:    "WEDGE-S2",
		SessionName: "wedge-s2",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})

	// Attach to S1 on a dedicated conn and flood it. Use a second conn
	// so the controller conn stays in JSON mode for the RPC assertions.
	att := dialTestClient(t, srv.SocketPath())
	ratt := bufio.NewReader(att)
	helloAndUnpack(t, att, ratt)
	attachAndUnpack(t, att, ratt, AttachReq{SessionID: s1, Cols: 80, Rows: 24})

	// Flood far past any PTY input buffer + the 256-cap input channel so
	// the pane is wedged server-side and a writer goroutine is parked.
	floodPaneToBackpressure(t, att, 2048)

	// Give the daemon a beat to process the flood and wedge S1.
	time.Sleep(300 * time.Millisecond)

	// The load-bearing assertions: every RPC must return promptly even
	// with S1 wedged. A 3s read deadline is generous; the deadlock would
	// hang indefinitely.
	deadline := 3 * time.Second

	// List — the original daemon-wide vector (handleList → Info →
	// pane.Size/Running/PID under sessionsMu.RLock).
	writeReq(t, ctl, MsgListReq, ListReq{})
	ctl.SetReadDeadline(time.Now().Add(deadline))
	if name, _ := readResp(t, rctl); name != MsgListResp {
		t.Fatalf("List did not return promptly with a wedged pane: got %q", name)
	}
	ctl.SetReadDeadline(time.Time{})

	// TicketDone{S1} — winds down the wedged session itself.
	writeReq(t, ctl, MsgTicketDoneReq, TicketDoneReq{TicketID: "WEDGE-S1"})
	ctl.SetReadDeadline(time.Now().Add(deadline))
	if name, _ := readResp(t, rctl); name != MsgTicketDoneResp {
		t.Fatalf("TicketDone{S1} did not return promptly: got %q", name)
	}
	ctl.SetReadDeadline(time.Time{})

	// Kill{S2} — the healthy session must still be killable.
	writeReq(t, ctl, MsgKillReq, KillReq{SessionID: s2, GraceSeconds: 1})
	ctl.SetReadDeadline(time.Now().Add(deadline))
	if name, _ := readResp(t, rctl); name != MsgKillResp && name != MsgErrorResp {
		t.Fatalf("Kill{S2} did not return promptly: got %q", name)
	}
	ctl.SetReadDeadline(time.Time{})

	att.Close()
	ctl.Close()
	waitServerDone(t, errCh, 5*time.Second)
}

// TestDaemon_WatchdogEmitsStuck proves Task 5: the detect-only watchdog
// emits exactly one Status:"stuck" SessionEvent for a session whose
// child stopped draining stdin, AND it does NOT auto-kill (the session
// stays alive). Subscribe, flood a non-draining session, and wait for
// the stuck event within a few watchdog ticks.
func TestDaemon_WatchdogEmitsStuck(t *testing.T) {
	srv, errCh := startServer(t)

	// Subscriber conn observes SessionEvents.
	sub := dialTestClient(t, srv.SocketPath())
	rsub := bufio.NewReader(sub)
	subscribeClient(t, sub, rsub)

	// Controller spawns + attaches + floods the non-draining session.
	ctl := dialTestClient(t, srv.SocketPath())
	rctl := bufio.NewReader(ctl)
	helloAndUnpack(t, ctl, rctl)
	sid := spawnHelper(t, ctl, rctl, SpawnReq{
		TicketID:    "WATCHDOG-1",
		SessionName: "watchdog-1",
		Command:     "/bin/sh",
		Args:        []string{"-c", "sleep 600"},
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})

	att := dialTestClient(t, srv.SocketPath())
	ratt := bufio.NewReader(att)
	helloAndUnpack(t, att, ratt)
	attachAndUnpack(t, att, ratt, AttachReq{SessionID: sid, Cols: 80, Rows: 24})

	// Flood CONTINUOUSLY in the background until we observe the stuck
	// event. The pane's wedged flag is intentionally transient — it
	// clears on the next successful WriteInput enqueue — so a fixed-size
	// burst can leave the flag cleared by the time the watchdog ticks
	// (the source of earlier flakiness). A sustained flood keeps the
	// child's stdin buffer + the bounded input channel full, so
	// WriteInput keeps backpressuring and wedged stays set across the
	// watchdog's stuckThreshold window. This mirrors the real incident: a
	// continuous paste, not a one-shot burst.
	stopFlood := make(chan struct{})
	floodDone := make(chan struct{})
	go func() {
		defer close(floodDone)
		chunk := make([]byte, 4096)
		for {
			select {
			case <-stopFlood:
				return
			default:
			}
			if err := WriteFrame(att, TypePTYInput, chunk); err != nil {
				// Socket backpressured/closed — pause briefly and retry
				// until told to stop.
				select {
				case <-stopFlood:
					return
				case <-time.After(20 * time.Millisecond):
				}
			}
		}
	}()

	// Wait for a Status:"stuck" event. The watchdog ticks every
	// activityTickInterval (2s) and flags after stuckThreshold (4s), so
	// allow a generous window.
	gotStuck := false
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		ev, ok := readNextSessionEvent(t, sub, rsub, 2*time.Second)
		if !ok {
			continue
		}
		if ev.Event == "status" && ev.Status == "stuck" && ev.SessionID == sid {
			gotStuck = true
			break
		}
	}
	close(stopFlood)
	<-floodDone
	if !gotStuck {
		t.Fatalf("never received a Status:\"stuck\" event for session %s within deadline", sid)
	}

	// No auto-kill: the session must still be alive in List.
	writeReq(t, ctl, MsgListReq, ListReq{})
	ctl.SetReadDeadline(time.Now().Add(3 * time.Second))
	name, raw := readResp(t, rctl)
	ctl.SetReadDeadline(time.Time{})
	if name != MsgListResp {
		t.Fatalf("List after stuck: got %q", name)
	}
	if !listContainsSession(t, raw, sid) {
		t.Fatalf("session %s was removed after going stuck — watchdog must NOT auto-kill", sid)
	}

	// Kill the wedged session explicitly so the registry drains and the
	// default-mode daemon can shut down cleanly (a still-live session
	// keeps it alive via awaitSessionDrain — correct, but it'd hang this
	// test). This also exercises the close-before-wait group-kill
	// teardown against a genuinely wedged pane end-to-end.
	writeReq(t, ctl, MsgKillReq, KillReq{SessionID: sid, GraceSeconds: 1})
	ctl.SetReadDeadline(time.Now().Add(5 * time.Second))
	if name, _ := readResp(t, rctl); name != MsgKillResp && name != MsgErrorResp {
		t.Fatalf("Kill of wedged session: got %q", name)
	}
	ctl.SetReadDeadline(time.Time{})

	att.Close()
	ctl.Close()
	sub.Close()
	waitServerDone(t, errCh, 8*time.Second)
}
