package daemon

import (
	"log"
	"strings"
	"testing"
)

// newStepServer builds a Server WITHOUT running Serve, so no background
// goroutines exist — stalenessStep can be driven synchronously and the
// staleCheck seam can be reassigned without racing watchBinaryStaleness.
// Returns the server and a syncBuffer capturing log output for the test.
func newStepServer(t *testing.T, opts Options) (*Server, *syncBuffer) {
	t.Helper()
	sock, pid := testEnv(t)
	srv, err := NewServerWithOptions(sock, pid, opts)
	if err != nil {
		t.Fatalf("NewServerWithOptions: %v", err)
	}
	t.Cleanup(func() {
		if srv.ln != nil {
			_ = srv.ln.Close()
		}
		if srv.pidlock != nil {
			srv.pidlock.Release()
		}
	})

	var logBuf syncBuffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return srv, &logBuf
}

// shutdownClosed reports whether initiateShutdown has fired (the shutdown
// channel is closed) without blocking.
func shutdownClosed(s *Server) bool {
	select {
	case <-s.shutdown:
		return true
	default:
		return false
	}
}

// TestStalenessStep_PersistentRestartsOnDrain is the load-bearing
// regression guard for the gap this change closes: a persistent (launchd)
// daemon that detects a newer binary while sessions are live used to go
// inert and NEVER restart. It must instead keep polling and restart the
// moment the registry drains to zero, so launchd respawns it on the new
// binary. (Revert `return !s.persistent` to `return true` in stalenessStep
// and this test's first assertion fails — the red-before-green proof.)
func TestStalenessStep_PersistentRestartsOnDrain(t *testing.T) {
	srv, logBuf := newStepServer(t, Options{Persistent: true})
	srv.staleCheck = func() bool { return true }
	srv.reg.store("x", &Session{id: "x", ticketID: "t"})

	// First tick: stale + 1 live session → keep polling, do NOT restart
	// (never orphan in-progress work).
	if stop := srv.stalenessStep(); stop {
		t.Fatal("first step returned stop=true with a live session; persistent mode must keep polling")
	}
	if shutdownClosed(srv) {
		t.Fatal("shutdown initiated while a session was still live")
	}

	// Session drains naturally.
	srv.reg.delete("x")

	// Next tick: stale + 0 sessions → restart so launchd respawns new.
	if stop := srv.stalenessStep(); !stop {
		t.Fatal("step returned stop=false after sessions drained; persistent daemon must restart")
	}
	if !shutdownClosed(srv) {
		t.Fatal("shutdown not initiated after sessions drained on a stale persistent daemon")
	}
	if got := logBuf.String(); !strings.Contains(got, "restart once sessions drain") {
		t.Errorf("missing persistent restart-on-drain WARN; log=%q", got)
	}
}

// TestStalenessStep_ImmediateWhenZeroSessions is a behavior guard (passes
// under the revert): with no live work at risk, a stale daemon shuts down
// at once so the next launch / respawn is on the new binary — in both
// modes.
func TestStalenessStep_ImmediateWhenZeroSessions(t *testing.T) {
	for _, persistent := range []bool{true, false} {
		name := "default"
		if persistent {
			name = "persistent"
		}
		t.Run(name, func(t *testing.T) {
			srv, _ := newStepServer(t, Options{Persistent: persistent})
			srv.staleCheck = func() bool { return true }

			if stop := srv.stalenessStep(); !stop {
				t.Fatal("step returned stop=false with zero sessions; must shut down immediately")
			}
			if !shutdownClosed(srv) {
				t.Fatal("shutdown not initiated with zero sessions on a stale daemon")
			}
		})
	}
}

// TestStalenessStep_DefaultHandsOffUnderLiveSessions is a regression guard
// (passes under the revert) pinning the deliberate decision NOT to extend
// drain-restart to default mode: a default-mode daemon must not bounce
// under an idle-but-attached TUI just because its agent finished. Its exit
// stays owned by the last-client-disconnect path (awaitSessionDrain).
func TestStalenessStep_DefaultHandsOffUnderLiveSessions(t *testing.T) {
	srv, _ := newStepServer(t, Options{}) // default mode
	srv.staleCheck = func() bool { return true }
	srv.reg.store("x", &Session{id: "x", ticketID: "t"})

	if stop := srv.stalenessStep(); !stop {
		t.Fatal("default mode step returned stop=false; it must hand off to the last-client path")
	}
	if shutdownClosed(srv) {
		t.Fatal("default mode self-initiated shutdown under a live session; the last-client path must own the exit")
	}
}

// TestStalenessStep_NoOpWhenFresh is a guard: a non-stale binary must not
// set pendingRestart or initiate shutdown, and the watcher keeps polling.
func TestStalenessStep_NoOpWhenFresh(t *testing.T) {
	srv, _ := newStepServer(t, Options{Persistent: true})
	srv.staleCheck = func() bool { return false }
	srv.reg.store("x", &Session{id: "x", ticketID: "t"})

	if stop := srv.stalenessStep(); stop {
		t.Fatal("fresh binary returned stop=true; should keep watching quietly")
	}
	srv.stalenessMu.Lock()
	pr := srv.pendingRestart
	srv.stalenessMu.Unlock()
	if pr {
		t.Fatal("pendingRestart set despite a fresh binary")
	}
	if shutdownClosed(srv) {
		t.Fatal("shutdown initiated despite a fresh binary")
	}
}
