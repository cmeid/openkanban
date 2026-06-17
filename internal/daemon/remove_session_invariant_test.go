package daemon

import (
	"bufio"
	"log"
	"strings"
	"testing"
	"time"
)

// TestRemoveSession_LogsInvariantViolationOnDuplicate plants two
// sessions sharing a TicketID directly into srv.sessions (bypassing
// handleSpawn's dedup check — the only thing standing between this
// state and reality post-PR #34). When one of those sessions exits
// naturally, watchSessionExit's removeSession is the path exercised:
// it deletes the exiting session, then scans for any OTHER session
// whose TicketID matches. If one exists, that's an invariant
// violation under the PR #34 contract and the defensive sweep must
// log a WARN.
//
// We don't auto-kill the surviving duplicate; that would silently
// change cleanup semantics. The log line is the breadcrumb.
//
// Sanity baseline: TestRemoveSession_CleanExitNoWarn covers the
// negative direction — a single-session exit must NOT emit the WARN.
func TestRemoveSession_LogsInvariantViolationOnDuplicate(t *testing.T) {
	// Capture log output for the duration of the test. syncBuffer
	// (defined in integration_test.go) is goroutine-safe so the
	// daemon's own log writes can't race with the test reads.
	var logBuf syncBuffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prev) })

	srv, errCh := startServer(t)

	// Keep a client alive for the daemon's lifecycle (clients==0
	// triggers shutdown). We follow the helloAndUnpack pattern from
	// the other watch_session_exit / dedup tests.
	a := dialTestClient(t, srv.SocketPath())
	ra := bufio.NewReader(a)
	helloAndUnpack(t, a, ra)

	const sharedTicket = "INVARIANT-1"
	mkSess := func(name string) *Session {
		t.Helper()
		sess, err := NewSession(SpawnReq{
			TicketID:    sharedTicket,
			SessionName: name,
			Command:     "/bin/cat", // long-running; blocked on stdin
			Cols:        80,
			Rows:        24,
			Scrollback:  1000,
		})
		if err != nil {
			t.Fatalf("NewSession(%s): %v", name, err)
		}
		return sess
	}

	sess1 := mkSess("dup-1")
	sess2 := mkSess("dup-2")

	srv.reg.store(sess1.ID(), sess1)
	srv.reg.store(sess2.ID(), sess2)

	// Wire the watcher for sess1 so its natural-exit path runs
	// removeSession (with the new defensive sweep). sess2 is left
	// without a watcher so it survives sess1's death cleanly.
	srv.watchSessionExit(sess1)

	// Kill sess1's underlying PTY directly. Its watcher will receive
	// the ExitEvent and the deferred removeSession + emit will fire.
	// sess.Kill(0) does not touch the registry — removal is the
	// watcher's job, which is precisely what we want to exercise.
	if err := sess1.Kill(0); err != nil {
		t.Fatalf("sess1.Kill: %v", err)
	}

	// Wait for sess1 to clear out of the registry — the post-condition
	// that proves removeSession ran. sess2 must still be there.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, oneLives := srv.reg.get(sess1.ID())
		_, twoLives := srv.reg.get(sess2.ID())
		if !oneLives && twoLives {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, oneLives := srv.reg.get(sess1.ID())
	_, twoLives := srv.reg.get(sess2.ID())
	if oneLives {
		t.Fatalf("sess1 still in registry; removeSession did not run")
	}
	if !twoLives {
		t.Fatalf("sess2 was removed; the defensive sweep must NOT auto-kill")
	}

	// Give a beat for the WARN log to flush (the watcher's deferred
	// chain runs removeSession + emit; both log lines flow through
	// the same Writer).
	time.Sleep(50 * time.Millisecond)

	logOut := logBuf.String()
	if !strings.Contains(logOut, "WARN") || !strings.Contains(logOut, "invariant violation") {
		t.Errorf("expected WARN/invariant-violation log line; got:\n%s", logOut)
	}
	if !strings.Contains(logOut, sess1.ID()) {
		t.Errorf("WARN log missing exiting SessionID %s; got:\n%s", sess1.ID(), logOut)
	}
	if !strings.Contains(logOut, sess2.ID()) {
		t.Errorf("WARN log missing surviving SessionID %s; got:\n%s", sess2.ID(), logOut)
	}
	if !strings.Contains(logOut, sharedTicket) {
		t.Errorf("WARN log missing TicketID %s; got:\n%s", sharedTicket, logOut)
	}

	// Tear down sess2 so the daemon can shut cleanly. Direct kill
	// then registry delete — no watcher to do it for us.
	if err := sess2.Kill(0); err != nil {
		t.Fatalf("sess2.Kill: %v", err)
	}
	srv.reg.delete(sess2.ID())

	a.Close()
	waitServerDone(t, errCh, 5*time.Second)
}

// TestRemoveSession_CleanExitNoWarn is the negative complement: a
// single-session exit (no duplicate) must NOT emit the WARN line.
// Pins that the sweep is observability-only and fires exclusively on
// the invariant-violation path.
func TestRemoveSession_CleanExitNoWarn(t *testing.T) {
	var logBuf syncBuffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prev) })

	srv, errCh := startServer(t)
	a := dialTestClient(t, srv.SocketPath())
	ra := bufio.NewReader(a)
	helloAndUnpack(t, a, ra)

	const ticketID = "CLEAN-1"
	sess, err := NewSession(SpawnReq{
		TicketID:    ticketID,
		SessionName: "clean",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	srv.reg.store(sess.ID(), sess)
	srv.watchSessionExit(sess)

	if err := sess.Kill(0); err != nil {
		t.Fatalf("sess.Kill: %v", err)
	}

	// Wait for removal.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, present := srv.reg.get(sess.ID()); !present {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	time.Sleep(50 * time.Millisecond)
	logOut := logBuf.String()
	if strings.Contains(logOut, "invariant violation") {
		t.Errorf("clean single-session exit emitted invariant-violation WARN; got:\n%s", logOut)
	}

	// Wind-down must be deterministic. Under heavy full-suite load the
	// watcher's natural removeSession can lag past the poll above; with
	// the default-mode deferred-shutdown behavior (the daemon no longer
	// force-kills live sessions on last-client-disconnect — see
	// awaitSessionDrain), a still-registered session would make the
	// daemon defer shutdown and this waitServerDone would time out.
	// Mirror TestRemoveSession_LogsInvariantViolationOnDuplicate: ensure
	// the registry is empty before disconnecting so the last-client path
	// takes the immediate (live==0) shutdown branch. Idempotent with any
	// lagging watcher removal (the map delete is a no-op if already gone).
	srv.reg.delete(sess.ID())

	a.Close()
	waitServerDone(t, errCh, 5*time.Second)
}
