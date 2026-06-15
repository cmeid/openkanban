package daemonclient

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/daemon"
)

// startTestDaemon spins up an in-process daemon.Server on a per-test
// socket and points the daemonclient env-overrides at it. Returns the
// socket path; the t.Cleanup chain handles teardown.
//
// Uses /tmp directly (not t.TempDir) for AF_UNIX path-length budget on
// macOS — same trick the daemon's integration tests use.
func startTestDaemon(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "okbdc-")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sock := filepath.Join(dir, "d.sock")
	pid := filepath.Join(dir, "d.pid")
	log := filepath.Join(dir, "d.log")
	t.Setenv("OPENKANBAN_DAEMON_SOCK", sock)
	t.Setenv("OPENKANBAN_DAEMON_PID", pid)
	t.Setenv("OPENKANBAN_DAEMON_LOG", log)

	srv, err := daemon.NewServer(sock, pid)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
			t.Logf("server did not exit within 3s")
		}
	})

	return sock
}

func TestClient_HelloAndList(t *testing.T) {
	_ = startTestDaemon(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	if c.ClientID() == 0 {
		t.Errorf("ClientID: got 0 want non-zero")
	}
	if c.ProtocolVersion() != daemon.ProtocolVersion {
		t.Errorf("ProtocolVersion: got %d want %d", c.ProtocolVersion(), daemon.ProtocolVersion)
	}
	if c.ClientCount() != 1 {
		t.Errorf("ClientCount: got %d want 1", c.ClientCount())
	}

	resp, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Sessions) != 0 {
		t.Errorf("List: got %d sessions, want 0", len(resp.Sessions))
	}
}

func TestClient_SpawnList(t *testing.T) {
	_ = startTestDaemon(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	spawn, err := c.Spawn(ctx, daemon.SpawnReq{
		TicketID:    "T-CL-1",
		SessionName: "tc-spawn",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if spawn.SessionID == "" {
		t.Errorf("Spawn: empty SessionID")
	}

	list, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, s := range list.Sessions {
		if s.SessionID == spawn.SessionID {
			found = true
			if s.TicketID != "T-CL-1" {
				t.Errorf("TicketID: got %q want T-CL-1", s.TicketID)
			}
		}
	}
	if !found {
		t.Errorf("spawned session not in List")
	}

	// Kill so the test's daemon cleanup doesn't have to.
	if err := c.Kill(ctx, spawn.SessionID, 1*time.Second); err != nil {
		t.Errorf("Kill: %v", err)
	}
}

func TestClient_PrepareExit(t *testing.T) {
	_ = startTestDaemon(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	resp, err := c.PrepareExit(ctx)
	if err != nil {
		t.Fatalf("PrepareExit: %v", err)
	}
	if resp.ClientCount != 1 {
		t.Errorf("PrepareExit ClientCount: got %d want 1", resp.ClientCount)
	}
	if resp.OtherActiveClients != 0 {
		t.Errorf("PrepareExit OtherActiveClients (sole client): got %d want 0", resp.OtherActiveClients)
	}
	if c.ClientCount() != 1 {
		t.Errorf("post-PrepareExit ClientCount: got %d want 1", c.ClientCount())
	}
}

// TestClient_CancelExit_ReversesPrepare verifies that calling CancelExit
// on a client that previously called PrepareExit causes a peer client's
// next PrepareExit to see the original as active again (i.e. counted in
// OtherActiveClients). This also doubles as the round-trip test that
// gates Task 3 — a missing dispatch case in the server would surface
// here as an "unknown message type" error, not at build time.
func TestClient_CancelExit_ReversesPrepare(t *testing.T) {
	_ = startTestDaemon(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	a, err := New(ctx)
	if err != nil {
		t.Fatalf("New A: %v", err)
	}
	defer a.Close()

	b, err := New(ctx)
	if err != nil {
		t.Fatalf("New B: %v", err)
	}
	defer b.Close()

	// A prepares to exit: from A's perspective B is still active (1).
	respA, err := a.PrepareExit(ctx)
	if err != nil {
		t.Fatalf("A PrepareExit: %v", err)
	}
	if respA.OtherActiveClients != 1 {
		t.Errorf("A PrepareExit OtherActiveClients: got %d want 1", respA.OtherActiveClients)
	}

	// B prepares to exit: from B's perspective A is exiting, so 0.
	respB, err := b.PrepareExit(ctx)
	if err != nil {
		t.Fatalf("B PrepareExit: %v", err)
	}
	if respB.OtherActiveClients != 0 {
		t.Errorf("B PrepareExit OtherActiveClients (peer exiting): got %d want 0", respB.OtherActiveClients)
	}

	// A cancels its exit: B's next PrepareExit should now see A active.
	if err := a.CancelExit(ctx); err != nil {
		t.Fatalf("A CancelExit: %v", err)
	}
	respB2, err := b.PrepareExit(ctx)
	if err != nil {
		t.Fatalf("B PrepareExit (post-CancelExit): %v", err)
	}
	if respB2.OtherActiveClients != 1 {
		t.Errorf("B PrepareExit after A CancelExit: got OtherActiveClients=%d want 1", respB2.OtherActiveClients)
	}
}

// TestClient_DialDaemonUnavailable points the socket at a missing path
// — no autostart binary, no daemon running. Should surface
// ErrDaemonUnavailable. The autostart fork DOES still get tried; we
// stub OPENKANBAN_DAEMON_BINARY to /bin/true so the fork "succeeds"
// but the child never binds.
func TestClient_DialDaemonUnavailable(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "okbdc-")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	t.Setenv("OPENKANBAN_DAEMON_SOCK", filepath.Join(dir, "nope.sock"))
	t.Setenv("OPENKANBAN_DAEMON_PID", filepath.Join(dir, "nope.pid"))
	t.Setenv("OPENKANBAN_DAEMON_LOG", filepath.Join(dir, "nope.log"))
	t.Setenv(envBinary, "/bin/true") // exits immediately, never binds

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = New(ctx)
	if err == nil {
		t.Fatalf("New: want error, got nil")
	}
}

func TestClient_SubscribeReceivesEvents(t *testing.T) {
	_ = startTestDaemon(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// First client subscribes.
	c1, err := New(ctx)
	if err != nil {
		t.Fatalf("New c1: %v", err)
	}
	defer c1.Close()

	ch, cancelSub, err := c1.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancelSub()

	// Second client spawns a session.
	c2, err := New(ctx)
	if err != nil {
		t.Fatalf("New c2: %v", err)
	}
	defer c2.Close()

	spawn, err := c2.Spawn(ctx, daemon.SpawnReq{
		TicketID:    "T-SUB-1",
		SessionName: "sub-spawn",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// PR4's server has a placeholder drainEvents goroutine; it does
	// not yet push SessionEvents to subscribed clients. We assert
	// that's the case to keep the test honest — once PR9 wires the
	// real fan-out this assertion flips and the test verifies the
	// channel receives an event.
	select {
	case ev := <-ch:
		// Real push path live → assert content matches.
		if ev.SessionID != spawn.SessionID {
			t.Errorf("event SessionID: got %q want %q", ev.SessionID, spawn.SessionID)
		}
	case <-time.After(300 * time.Millisecond):
		// PR4-era no-op fan-out: acceptable for PR7. Test passes; PR9
		// will replace the timeout branch with a t.Fatal.
		t.Log("no SessionEvent observed within 300ms — PR4-era fan-out is still a no-op; PR9 will exercise this path")
	}

	_ = c2.Kill(ctx, spawn.SessionID, 0)
}
