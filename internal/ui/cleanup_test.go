package ui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
)

// TestCleanup_LeavesDaemonSessionsAlive is the regression test for the
// daemon-shutdown bug: model.Cleanup() used to call pane.StopGraceful(),
// which sent a daemon Kill RPC for every attached pane. That terminated
// sessions other TUIs were still watching when this TUI quit — the
// daemon's whole point is to outlive any single TUI.
//
// After the fix, Cleanup() detaches via pane.Close() and leaves the
// daemon-side session alone. The daemon's own last-client-disconnect
// handler is the only place sessions die on TUI quit, and that only
// fires when the actual last connection drops.
func TestCleanup_LeavesDaemonSessionsAlive(t *testing.T) {
	// Per-test daemon socket / pid / log under /tmp (macOS AF_UNIX path
	// length budget). Same pattern as cmd/ticket_daemon_test.go.
	dir, err := os.MkdirTemp("/tmp", "okbcln-")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sock := filepath.Join(dir, "d.sock")
	pidPath := filepath.Join(dir, "d.pid")
	logPath := filepath.Join(dir, "d.log")
	t.Setenv("OPENKANBAN_DAEMON_SOCK", sock)
	t.Setenv("OPENKANBAN_DAEMON_PID", pidPath)
	t.Setenv("OPENKANBAN_DAEMON_LOG", logPath)
	// Disable the autostart fork: the in-process server below owns the
	// socket. /usr/bin/true is POSIX-portable; /bin/true is missing on
	// macOS.
	t.Setenv("OPENKANBAN_DAEMON_BINARY", "/usr/bin/true")

	srv, err := daemon.NewServer(sock, pidPath)
	if err != nil {
		t.Fatalf("daemon.NewServer: %v", err)
	}
	srvCtx, cancelSrv := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(srvCtx) }()
	t.Cleanup(func() {
		cancelSrv()
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
			t.Logf("daemon did not exit within 3s")
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := daemonclient.New(ctx)
	if err != nil {
		t.Fatalf("daemonclient.New: %v", err)
	}
	defer c.Close()

	resp, err := c.Spawn(ctx, daemon.SpawnReq{
		TicketID:    "T-CLN-1",
		SessionName: "cleanup-test",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	list, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var info daemon.SessionInfo
	for _, s := range list.Sessions {
		if s.SessionID == resp.SessionID {
			info = s
		}
	}

	pv := daemonclient.NewPaneView(c, "T-CLN-1", resp.SessionID, &info)
	pv.SetSize(80, 24)
	if err := pv.Attach(ctx); err != nil {
		t.Fatalf("PaneView.Attach: %v", err)
	}
	if !pv.Running() {
		t.Fatalf("pre-Cleanup: Running()=false, want true")
	}

	m := &Model{
		panes: map[board.TicketID]*daemonclient.PaneView{
			"T-CLN-1": pv,
		},
	}

	m.Cleanup()

	// The daemon must still report the session as live: Cleanup detaches
	// the pane but does not kill the underlying agent.
	list, err = c.List(ctx)
	if err != nil {
		t.Fatalf("post-Cleanup List: %v", err)
	}
	for _, s := range list.Sessions {
		if s.SessionID == resp.SessionID {
			return
		}
	}
	t.Errorf("session %s missing from daemon after Cleanup; the daemon-side session must outlive a single TUI's exit", resp.SessionID)
}
