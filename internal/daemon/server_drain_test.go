package daemon

import (
	"bufio"
	"log"
	"net"
	"strings"
	"testing"
	"time"
)

// TestServerLifecycle_DefaultDefersShutdownUntilSessionsDrain is the
// regression guard for "default-mode daemon kills live sessions on
// exit-guard bypass". A default-mode daemon whose last client
// disconnects while a session is still live must NOT force-kill that
// session (the old behavior: WARN + initiateShutdown → cleanup kills
// everything). Instead it defers: the session survives and the daemon
// stays listening until the session drains naturally, after which it
// exits cleanly so it doesn't linger as an orphan.
//
// This is the inverse of TestServerLifecycle_PersistentSurvivesLastDisconnect:
// persistent mode stays up indefinitely; default mode stays up only
// until the registry drains, then shuts down.
func TestServerLifecycle_DefaultDefersShutdownUntilSessionsDrain(t *testing.T) {
	var logBuf syncBuffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prev) })

	srv, errCh := startServer(t) // default (non-persistent) mode

	// Plant a long-running session directly into the registry (the
	// remove_session_invariant_test pattern). /bin/cat blocks on stdin,
	// so it stays live until we kill it.
	sess, err := NewSession(SpawnReq{
		TicketID:    "DRAIN-1",
		SessionName: "drain-sess",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Kill(0) })

	srv.reg.store(sess.ID(), sess)
	srv.watchSessionExit(sess)

	// Connect a TUI client, then disconnect it. This is the
	// last-client-disconnect with live == 1 that previously triggered
	// the destructive shutdown.
	c1 := dialTestClient(t, srv.SocketPath())
	r1 := bufio.NewReader(c1)
	helloWithName(t, c1, r1, ClientNameTUI)
	c1.Close()

	// The session must SURVIVE. Poll across more than one drain tick to
	// be sure the watcher isn't tearing it down. The old code deleted it
	// in cleanup() almost immediately.
	deadline := time.Now().Add(drainPollInterval*2 + 500*time.Millisecond)
	for time.Now().Before(deadline) {
		if _, alive := srv.reg.get(sess.ID()); !alive {
			t.Fatalf("live session was removed after last client disconnected; deferral failed")
		}
		// Daemon must also still be running.
		select {
		case err := <-errCh:
			t.Fatalf("daemon exited while a session was still live: %v", err)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Daemon must still be listening for a fresh connection.
	c2, err := net.Dial("unix", srv.SocketPath())
	if err != nil {
		t.Fatalf("post-disconnect dial: default daemon should still listen while a session is live, got: %v", err)
	}
	c2.Close()

	// The deferral must be logged, and the destructive cleanup path must
	// NOT have fired.
	logs := logBuf.String()
	if !strings.Contains(logs, "deferring shutdown until they exit") {
		t.Errorf("expected deferral log line, got:\n%s", logs)
	}
	if strings.Contains(logs, "shutdown-cleanup killing session") {
		t.Errorf("destructive cleanup ran on a live session; log:\n%s", logs)
	}

	// Now let the session drain naturally. The daemon must shut down on
	// its own (no client connected) within a couple of drain ticks.
	if err := sess.Kill(0); err != nil {
		t.Fatalf("sess.Kill: %v", err)
	}
	waitServerDone(t, errCh, drainPollInterval*3+2*time.Second)

	if !strings.Contains(logBuf.String(), "live sessions drained") {
		t.Errorf("expected drained-shutdown log line, got:\n%s", logBuf.String())
	}

	// Socket should be gone after a clean shutdown (cleanup removes it).
	if _, err := net.Dial("unix", srv.SocketPath()); err == nil {
		t.Errorf("daemon still listening after drain; expected shutdown")
	}
}

// TestServerLifecycle_DeferralIsSingleInFlightAcrossReattach covers the
// re-attach-during-drain branch that the linear test above does not: a
// client disconnects with a live session (drain starts), a SECOND client
// then re-attaches while the watcher is waiting and disconnects again.
// The single-in-flight guard (`start := !s.drainPending` in
// handleLastClientDisconnect) must ensure only ONE awaitSessionDrain
// watcher is ever spawned — the second disconnect must be a no-op while
// drainPending is still set. We assert that by counting the deferral log
// line: it must appear exactly once across both disconnects. After the
// session finally drains, the daemon must shut down exactly once.
//
// Red proof (manual): set `start := true` at server.go's
// handleLastClientDisconnect and this test fails — the deferral line is
// logged twice (two watchers) instead of once.
func TestServerLifecycle_DeferralIsSingleInFlightAcrossReattach(t *testing.T) {
	var logBuf syncBuffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prev) })

	srv, errCh := startServer(t) // default (non-persistent) mode

	// One long-running session so live stays > 0 for the whole test;
	// /bin/cat blocks on stdin until we kill it.
	sess, err := NewSession(SpawnReq{
		TicketID:    "DRAIN-REATTACH-1",
		SessionName: "drain-reattach-sess",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Kill(0) })

	srv.reg.store(sess.ID(), sess)
	srv.watchSessionExit(sess)

	const deferLine = "deferring shutdown until they exit"
	const disconnectLine = "remaining=0"

	// Disconnect #1: drain must start. Wait until the deferral is logged
	// (proves disconnect #1 was fully processed and drainPending is set)
	// BEFORE re-attaching — otherwise we'd be testing attach-before-drain,
	// not re-attach-during-drain.
	c1 := dialTestClient(t, srv.SocketPath())
	r1 := bufio.NewReader(c1)
	helloWithName(t, c1, r1, ClientNameTUI)
	c1.Close()
	waitForLog(t, &logBuf, func(s string) bool {
		return strings.Contains(s, deferLine)
	}, 2*time.Second, "drain to start after first disconnect")

	// Re-attach #2 while the watcher is waiting, then disconnect again.
	c2 := dialTestClient(t, srv.SocketPath())
	r2 := bufio.NewReader(c2)
	helloWithName(t, c2, r2, ClientNameTUI)
	c2.Close()
	// Wait until the SECOND last-client-disconnect has been handled
	// (two "remaining=0" log lines). handleLastClientDisconnect runs
	// synchronously in handleConn's defer right after that line.
	waitForLog(t, &logBuf, func(s string) bool {
		return strings.Count(s, disconnectLine) >= 2
	}, 2*time.Second, "second disconnect to be processed")

	// THE ASSERTION: the deferral fired exactly once — a single watcher,
	// not one per disconnect.
	if got := strings.Count(logBuf.String(), deferLine); got != 1 {
		t.Fatalf("deferral logged %d times, want exactly 1 (single-in-flight watcher); log:\n%s", got, logBuf.String())
	}

	// Session must still be alive and the daemon still listening.
	_, alive := srv.reg.get(sess.ID())
	if !alive {
		t.Fatalf("live session was removed during re-attach churn; deferral failed")
	}
	select {
	case err := <-errCh:
		t.Fatalf("daemon exited while a session was still live: %v", err)
	default:
	}
	probe, err := net.Dial("unix", srv.SocketPath())
	if err != nil {
		t.Fatalf("daemon should still listen while a session is live, got: %v", err)
	}
	probe.Close()

	// Drain the session: the single watcher must shut the daemon down
	// exactly once.
	if err := sess.Kill(0); err != nil {
		t.Fatalf("sess.Kill: %v", err)
	}
	waitServerDone(t, errCh, drainPollInterval*3+2*time.Second)
	// Match the watcher's own line only — "deferred shutdown ...". Note
	// the broader "live sessions drained" substring also appears in
	// initiateShutdown's reason line ("shutdown initiated (live sessions
	// drained)"), so counting that would double-count one shutdown.
	if got := strings.Count(logBuf.String(), "deferred shutdown"); got != 1 {
		t.Errorf("deferred-shutdown logged %d times, want exactly 1; log:\n%s", got, logBuf.String())
	}
	if _, err := net.Dial("unix", srv.SocketPath()); err == nil {
		t.Errorf("daemon still listening after drain; expected shutdown")
	}
}

// waitForLog polls the captured log buffer until pred is satisfied or the
// timeout elapses, failing the test with desc on timeout. Used to order
// async server-side disconnect handling deterministically without sleeps.
func waitForLog(t *testing.T, buf *syncBuffer, pred func(string) bool, timeout time.Duration, desc string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred(buf.String()) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; log:\n%s", desc, buf.String())
}
