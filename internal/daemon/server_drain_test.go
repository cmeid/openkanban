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

	srv.sessionsMu.Lock()
	srv.sessions[sess.ID()] = sess
	srv.sessionsMu.Unlock()
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
		srv.sessionsMu.RLock()
		_, alive := srv.sessions[sess.ID()]
		srv.sessionsMu.RUnlock()
		if !alive {
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
