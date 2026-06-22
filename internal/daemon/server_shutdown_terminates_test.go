package daemon

import (
	"bufio"
	"testing"
	"time"
)

// TestServerLifecycle_ShutdownTerminatesWithAttachedClient pins the fix
// for the zombie-daemon bug. A persistent daemon that initiates shutdown
// (e.g. binary-stale self-restart) while a client stays connected must
// still EXIT.
//
// Before the fix, initiateShutdown closed the listener — which on a Go
// UnixListener unlinks the socket file, making the daemon invisible to
// new clients (daemon list → ENOENT) — and then Serve blocked on
// wg.Wait() forever, because a persistent-mode TUI's handleConn
// goroutine is parked in ReadFrame and never disconnects on its own.
// The observed field symptom: socket file gone, process alive, still
// serving the attached TUI (which reports live sessions), and the
// launchd respawn-on-new-binary never happens.
//
// The fix force-closes every registered client conn on shutdown so their
// read loops return and wg.Wait() completes. We connect a client and
// deliberately leave it connected — exactly the persistent TUI — then
// trigger shutdown and assert Serve returns promptly.
func TestServerLifecycle_ShutdownTerminatesWithAttachedClient(t *testing.T) {
	srv, errCh := startServerWithOptions(t, Options{Persistent: true})

	// Connect and stay connected. After the handshake this client's
	// handleConn goroutine parks in ReadFrame and never returns on its
	// own — the never-disconnecting persistent TUI.
	conn := dialTestClient(t, srv.SocketPath())
	r := bufio.NewReader(conn)
	helloAndUnpack(t, conn, r)

	// Same path the binary-stale self-restart takes.
	srv.initiateShutdown("test: binary updated on disk")

	// Serve must return promptly. Without the fix it hangs in wg.Wait()
	// until this timeout trips.
	waitServerDone(t, errCh, 3*time.Second)
}
