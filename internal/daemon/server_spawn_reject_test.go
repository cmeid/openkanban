package daemon

import (
	"strings"
	"testing"
)

// TestHandleSpawn_RejectsEmptyTicketID verifies that handleSpawn
// refuses anonymous spawns: a SpawnReq with TicketID="" returns a
// non-nil error whose message mentions "empty TicketID", and the
// session registry remains empty (no half-spawned PTY leaked).
//
// This is the structural enforcement of "no orphans by construction":
// without a TicketID, the daemon cannot dedup, route TicketDone, or
// reap on ticket deletion, so the only safe answer is to refuse at the
// entry. Driving handleSpawn directly (rather than over the wire)
// keeps the test deterministic and avoids spawning a child process.
func TestHandleSpawn_RejectsEmptyTicketID(t *testing.T) {
	srv, _ := startServer(t)

	// Synthetic clientConn — handleSpawn only reads c.id (for log
	// messages), so a zero-value is fine. We do NOT register it via
	// registerClient because that would also start the per-conn
	// goroutines.
	c := &clientConn{id: 1}

	resp, err := srv.handleSpawn(c, SpawnReq{
		TicketID:    "",
		SessionName: "anon-rejected",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})
	if err == nil {
		t.Fatalf("handleSpawn with empty TicketID: got nil error; want rejection")
	}
	if !strings.Contains(err.Error(), "empty TicketID") {
		t.Errorf("handleSpawn error=%q; want it to mention %q", err.Error(), "empty TicketID")
	}
	if resp.SessionID != "" || resp.PID != 0 {
		t.Errorf("handleSpawn returned non-zero response on reject: SessionID=%q PID=%d",
			resp.SessionID, resp.PID)
	}

	// Registry must be untouched — no NewSession should have run.
	srv.sessionsMu.RLock()
	got := len(srv.sessions)
	srv.sessionsMu.RUnlock()
	if got != 0 {
		t.Errorf("after rejected spawn: sessions count=%d want 0 (no half-spawned session)", got)
	}

	// Server shutdown is driven by startServer's t.Cleanup(cancel),
	// which fires after this function returns. Serve's goroutine
	// drains into the buffered errCh and exits without further work
	// from us — no client conn means no last-client-disconnect path
	// to trip, and we never dialed so there's nothing to close.
}

// TestHandleSpawn_AcceptsNonEmptyTicketID is the inverse defense
// against accidental over-rejection: the same call shape with a real
// TicketID still succeeds and lands one session in the registry.
func TestHandleSpawn_AcceptsNonEmptyTicketID(t *testing.T) {
	srv, _ := startServer(t)

	c := &clientConn{id: 2}

	resp, err := srv.handleSpawn(c, SpawnReq{
		TicketID:    "REJECT-TEST-1",
		SessionName: "non-empty-accepted",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})
	if err != nil {
		t.Fatalf("handleSpawn with non-empty TicketID: %v", err)
	}
	if resp.SessionID == "" {
		t.Fatalf("handleSpawn returned empty SessionID on success")
	}

	srv.sessionsMu.RLock()
	got := len(srv.sessions)
	sess, ok := srv.sessions[resp.SessionID]
	srv.sessionsMu.RUnlock()
	if got != 1 {
		t.Errorf("after accepted spawn: sessions count=%d want 1", got)
	}
	if !ok {
		t.Fatalf("spawned session %q not in registry", resp.SessionID)
	}

	// Cleanup: kill the live session so the daemon can shut down
	// without warning about a bypassed exit-guard. handleSpawn started
	// /bin/cat which would otherwise block reading stdin forever.
	if err := sess.Kill(0); err != nil {
		t.Errorf("cleanup Kill: %v", err)
	}

	// Serve goroutine teardown handled by startServer's t.Cleanup(cancel).
}
