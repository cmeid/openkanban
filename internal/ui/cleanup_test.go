package ui

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
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

// ticketDoneRecorderAPI is a daemonGuardAPI fake purpose-built for the
// performTicketCleanup tests. It records every TicketDone invocation so
// tests can assert that the cleanup path fires the RPC unconditionally
// (the rescue path for daemon-owned sessions the resync hasn't yet
// imported into m.panes — see B3 in tickets/investigate-whether-there-might-ever-be.md).
type ticketDoneRecorderAPI struct {
	calls atomic.Int32
	last  atomic.Value // stores string — the last ticketID passed to TicketDone
}

func (s *ticketDoneRecorderAPI) PrepareExit(_ context.Context) (daemon.PrepareExitResp, error) {
	return daemon.PrepareExitResp{}, nil
}
func (s *ticketDoneRecorderAPI) CancelExit(_ context.Context) error { return nil }
func (s *ticketDoneRecorderAPI) Kill(_ context.Context, _ string, _ time.Duration) error {
	return nil
}
func (s *ticketDoneRecorderAPI) ClientID() uint16 { return 1 }
func (s *ticketDoneRecorderAPI) Owns(_ context.Context, _ string) (daemon.OwnsResp, error) {
	return daemon.OwnsResp{Owned: false}, nil
}
func (s *ticketDoneRecorderAPI) TicketDone(_ context.Context, ticketID string) (daemon.TicketDoneResp, error) {
	s.calls.Add(1)
	s.last.Store(ticketID)
	return daemon.TicketDoneResp{Killed: false}, nil
}
func (s *ticketDoneRecorderAPI) List(_ context.Context) (daemon.ListResp, error) {
	return daemon.ListResp{}, nil
}
func (s *ticketDoneRecorderAPI) Spawn(_ context.Context, _ daemon.SpawnReq) (daemon.SpawnResp, error) {
	return daemon.SpawnResp{}, nil
}

func (s *ticketDoneRecorderAPI) lastTicketID() string {
	v := s.last.Load()
	if v == nil {
		return ""
	}
	return v.(string)
}

// TestPerformTicketCleanup_NotifiesDaemonEvenWithoutLocalPane pins B3:
// when the user deletes a ticket whose daemon-owned session has NOT yet
// been imported into m.panes by the 30s resync (sibling-TUI window), the
// cleanup path must still send TicketDone to the daemon so the orphan
// session is reaped. The pre-fix code only called the daemon when a
// local pane existed, which left the daemon-side session pointing at a
// now-deleted ticket.
func TestPerformTicketCleanup_NotifiesDaemonEvenWithoutLocalPane(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	ticket := &board.Ticket{
		ID:        "T-CLEAN-NOPANE",
		Title:     "no local pane",
		ProjectID: proj.ID,
		Status:    board.StatusInProgress,
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	api := &ticketDoneRecorderAPI{}
	m := &Model{
		globalStore:   globalStore,
		panes:         map[board.TicketID]*daemonclient.PaneView{}, // intentionally empty
		columnTickets: [][]*board.Ticket{{}, {ticket}, {}, {}},
		columnOffsets: []int{0, 0, 0, 0},
		mode:          ModeNormal,
		activeColumn:  1,
		activeTicket:  0,
		width:         120,
		height:        40,
		config:        &config.Config{Agents: map[string]config.AgentConfig{}},
		guardAPI:      api,
	}

	m.performTicketCleanup(ticket)

	if got := api.calls.Load(); got != 1 {
		t.Errorf("TicketDone called %d times; want exactly 1 (rescue path must fire even with no local pane)", got)
	}
	if got := api.lastTicketID(); got != string(ticket.ID) {
		t.Errorf("TicketDone called with ticketID=%q; want %q", got, ticket.ID)
	}
}

// TestPerformTicketCleanup_NotifiesDaemonWhenLocalPaneExists confirms the
// notify is UNCONDITIONAL: even when the local pane already exists (and
// pane.Stop has handled the writer-side teardown), the daemon RPC still
// fires. On the daemon side this becomes a no-op (handleTicketDone
// returns Killed=false on miss when pane.Stop's Kill already removed
// the session), but firing it unconditionally is what closes the
// sibling-TUI orphan window — we don't gate on local state.
func TestPerformTicketCleanup_NotifiesDaemonWhenLocalPaneExists(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	ticket := &board.Ticket{
		ID:        "T-CLEAN-PANE",
		Title:     "with local pane",
		ProjectID: proj.ID,
		Status:    board.StatusInProgress,
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	// Detached pane with sessionID="" — Stop is a no-op so we don't
	// need a live daemon (see makeDetachedPane / PaneView.stop's
	// early return for state==Detached || sessionID=="").
	pane := makeDetachedPane("")

	api := &ticketDoneRecorderAPI{}
	m := &Model{
		globalStore:   globalStore,
		panes:         map[board.TicketID]*daemonclient.PaneView{ticket.ID: pane},
		columnTickets: [][]*board.Ticket{{}, {ticket}, {}, {}},
		columnOffsets: []int{0, 0, 0, 0},
		mode:          ModeNormal,
		activeColumn:  1,
		activeTicket:  0,
		width:         120,
		height:        40,
		config:        &config.Config{Agents: map[string]config.AgentConfig{}},
		guardAPI:      api,
	}

	m.performTicketCleanup(ticket)

	if got := api.calls.Load(); got != 1 {
		t.Errorf("TicketDone called %d times; want exactly 1 (notify must fire unconditionally, not gated on local pane existence)", got)
	}
	if got := api.lastTicketID(); got != string(ticket.ID) {
		t.Errorf("TicketDone called with ticketID=%q; want %q", got, ticket.ID)
	}
	// And the local pane was removed as part of cleanup.
	if _, ok := m.panes[ticket.ID]; ok {
		t.Errorf("panes[%s] still present after performTicketCleanup; want deleted", ticket.ID)
	}
}
