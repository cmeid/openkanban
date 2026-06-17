package daemon

import (
	"bufio"
	"encoding/json"
	"testing"
	"time"
)

// TestNaturalExit_PaneReclaimed verifies that when an agent session exits
// naturally (child process dies on its own — no Kill/TicketDone RPC),
// watchSessionExit's deferred cleanup calls pane.Stop() so the master fd,
// drain goroutine, and scrollback emulator are reclaimed.
//
// Observable: pane.Running() returns false after teardown
// (doTeardown stores false into runningAtomic). Without the fix
// pane.Running() stays true forever after a natural exit because the
// read-loop's publishExit path never clears runningAtomic — it only
// notifies subscribers.
//
// Determinism: runningAtomic is an atomic.Bool written exactly once by
// doTeardown and never cleared by any other path, so the transition
// false→true at start, true→false at teardown is a single clean edge
// with no races.
//
// Command choice: /bin/sleep 0.15 instead of /bin/true or /bin/echo.
// Ultra-fast commands (/usr/bin/true) can exit before watchSessionExit's
// Subscribe call wires up, causing the ExitEvent to be missed entirely —
// the PanicSafety test has the same comment. 0.15 s gives the watcher
// goroutine time to subscribe before the child exits; the post-exit
// assertions then poll for up to 3 s — more than enough.
func TestNaturalExit_PaneReclaimed(t *testing.T) {
	srv, errCh := startServer(t)

	conn := dialTestClient(t, srv.SocketPath())
	r := bufio.NewReader(conn)
	helloAndUnpack(t, conn, r)

	writeReq(t, conn, MsgSpawnReq, SpawnReq{
		TicketID:    "G8-NAT-1",
		SessionName: "natural-exit-reclaim",
		Command:     "/bin/sleep",
		Args:        []string{"0.15"},
		Cols:        80,
		Rows:        24,
		Scrollback:  100,
	})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	name, raw := readResp(t, r)
	if name != MsgSpawnResp {
		t.Fatalf("spawn: got %q, payload=%s", name, string(raw))
	}
	var spawn SpawnResp
	if err := json.Unmarshal(raw, &spawn); err != nil {
		t.Fatalf("decode SpawnResp: %v", err)
	}
	conn.SetReadDeadline(time.Time{})
	if spawn.SessionID == "" {
		t.Fatal("SpawnResp.SessionID empty")
	}

	// Fetch the Session pointer while it's still in the registry.
	// We need it to inspect pane.Running() after exit.
	sess, ok := srv.reg.get(spawn.SessionID)
	if !ok {
		t.Fatal("session not in registry immediately after spawn")
	}

	// Wait for the child to exit naturally and watchSessionExit to:
	//   1. receive the ExitEvent
	//   2. run removeSession (registry entry disappears)
	//   3. call pane.Stop() (runningAtomic → false)
	//
	// We poll for the registry removal first (same pattern as
	// TestWatchSessionExit_PanicSafety), then assert pane.Running().
	deadline := time.Now().Add(3 * time.Second)
	removed := false
	for time.Now().Before(deadline) {
		if _, stillThere := srv.reg.get(spawn.SessionID); !stillThere {
			removed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !removed {
		t.Fatalf("session %s still in registry after natural exit (3 s timeout)", spawn.SessionID)
	}

	// Give Stop() a brief moment to complete after removeSession returns.
	// removeSession and pane.Stop() are sequential in the same defer, so
	// by the time we observe registry removal the Stop() call is either
	// already done or about to be called. A short poll handles any
	// scheduling gap between the atomic registry swap and the pane write.
	paneReclaimedDeadline := time.Now().Add(500 * time.Millisecond)
	reclaimed := false
	for time.Now().Before(paneReclaimedDeadline) {
		if !sess.pane.Running() {
			reclaimed = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !reclaimed {
		t.Errorf("pane.Running() still true after natural exit — master fd + drain goroutine leaked (fix: watchSessionExit must call pane.Stop())")
	}

	conn.Close()
	waitServerDone(t, errCh, 3*time.Second)
}
