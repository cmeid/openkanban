package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

// --- Scaffolding -----------------------------------------------------------
//
// DeleteProject reaches into BOTH the project registry (config dir) and the
// daemon (Unix socket). To exercise it in a unit test we need to override
// both seams via env vars before any code looks them up:
//
//   - OPENKANBAN_CONFIG_DIR  → project.LoadRegistry / project.LoadTicketStore
//   - OPENKANBAN_DAEMON_SOCK → daemonclient.NewNoAutostart dial target
//   - OPENKANBAN_DAEMON_PID  → daemon.NewServer requires a pid file path
//   - OPENKANBAN_DAEMON_LOG  → daemon writes a log file alongside
//   - OPENKANBAN_DAEMON_BINARY → guard against any autostart fallback
//
// We stay in t.TempDir() for the config dir and use /tmp for the daemon
// socket (macOS AF_UNIX paths cap at 104 bytes — t.TempDir on macOS is
// often too long). Same pattern as cmd/ticket_daemon_test.go.

type deleteProjectEnv struct {
	t         *testing.T
	configDir string
	sock      string
}

func newDeleteProjectEnv(t *testing.T) *deleteProjectEnv {
	t.Helper()

	configDir := t.TempDir()
	t.Setenv("OPENKANBAN_CONFIG_DIR", configDir)

	// Daemon transport bits live in /tmp to dodge the 104-byte AF_UNIX
	// path cap on macOS — t.TempDir() can blow past it.
	sockDir, err := os.MkdirTemp("/tmp", "okapp-")
	if err != nil {
		t.Fatalf("mkdir sockdir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })

	sock := filepath.Join(sockDir, "d.sock")
	pid := filepath.Join(sockDir, "d.pid")
	log := filepath.Join(sockDir, "d.log")
	t.Setenv("OPENKANBAN_DAEMON_SOCK", sock)
	t.Setenv("OPENKANBAN_DAEMON_PID", pid)
	t.Setenv("OPENKANBAN_DAEMON_LOG", log)
	// Belt-and-braces against accidental autostart in any code path
	// that doesn't go through NewNoAutostart.
	t.Setenv("OPENKANBAN_DAEMON_BINARY", "/usr/bin/true")

	return &deleteProjectEnv{t: t, configDir: configDir, sock: sock}
}

// startTestDaemon spins up an in-process daemon.Server on env.sock and
// returns a long-lived control client. The client is held open via
// t.Cleanup so the server doesn't shut itself down between RPCs — its
// last-client-disconnect path is wired up to kill all live sessions.
func (env *deleteProjectEnv) startTestDaemon() *daemonclient.Client {
	env.t.Helper()

	pid := os.Getenv("OPENKANBAN_DAEMON_PID")
	srv, err := daemon.NewServer(env.sock, pid)
	if err != nil {
		env.t.Fatalf("daemon.NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ctx) }()
	env.t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
			env.t.Logf("daemon server did not exit within 3s")
		}
	})

	// Hold one client open for the entire test so the daemon doesn't
	// shut itself down between RPCs.
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	holder, err := daemonclient.New(dialCtx)
	if err != nil {
		env.t.Fatalf("hold daemon open: %v", err)
	}
	env.t.Cleanup(func() { holder.Close() })
	return holder
}

// spawnSessionForTicket asks the daemon to start a long-lived /bin/cat
// session bound to ticketID. Returns the daemon-side SessionID.
func (env *deleteProjectEnv) spawnSessionForTicket(ticketID string) string {
	env.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := daemonclient.New(ctx)
	if err != nil {
		env.t.Fatalf("daemonclient.New: %v", err)
	}
	env.t.Cleanup(func() { c.Close() })
	resp, err := c.Spawn(ctx, daemon.SpawnReq{
		TicketID:    ticketID,
		SessionName: "delete-project-test",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})
	if err != nil {
		env.t.Fatalf("Spawn ticket=%s: %v", ticketID, err)
	}
	return resp.SessionID
}

// createProjectWithTickets writes a project + N tickets to disk so that
// project.LoadTicketStore picks them up. Returns the project and the
// list of ticket IDs (in creation order).
func (env *deleteProjectEnv) createProjectWithTickets(name string, n int) (*project.Project, []board.TicketID) {
	env.t.Helper()
	repoDir := filepath.Join(env.configDir, "repo-"+name)
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		env.t.Fatalf("mkdir repo: %v", err)
	}

	reg, err := project.LoadRegistry()
	if err != nil {
		env.t.Fatalf("LoadRegistry: %v", err)
	}
	p := project.NewProject(name, repoDir)
	if err := reg.Add(p); err != nil {
		env.t.Fatalf("registry.Add: %v", err)
	}

	store, err := project.LoadTicketStore(p)
	if err != nil {
		env.t.Fatalf("LoadTicketStore: %v", err)
	}
	ids := make([]board.TicketID, 0, n)
	for i := 0; i < n; i++ {
		tk := board.NewTicket("ticket", p.ID)
		if err := store.SaveTicket(tk); err != nil {
			env.t.Fatalf("SaveTicket: %v", err)
		}
		ids = append(ids, tk.ID)
	}
	return p, ids
}

// listDaemonSessions returns the daemon's current session set via the
// List RPC. Fails the test on error.
func (env *deleteProjectEnv) listDaemonSessions() []daemon.SessionInfo {
	env.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := daemonclient.New(ctx)
	if err != nil {
		env.t.Fatalf("daemonclient.New: %v", err)
	}
	defer c.Close()
	list, err := c.List(ctx)
	if err != nil {
		env.t.Fatalf("List: %v", err)
	}
	return list.Sessions
}

// waitForNoSessionsForTickets polls the daemon's List until none of the
// given ticket IDs has a live session, or the deadline elapses. The
// daemon's session-exit reap path runs asynchronously after TicketDone
// returns Killed=true, so a tight follow-up List can briefly see the
// dying session.
func (env *deleteProjectEnv) waitForNoSessionsForTickets(ids []board.TicketID, timeout time.Duration) {
	env.t.Helper()
	owned := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		owned[string(id)] = struct{}{}
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sessions := env.listDaemonSessions()
		stillThere := 0
		for _, s := range sessions {
			if _, ok := owned[s.TicketID]; ok {
				stillThere++
			}
		}
		if stillThere == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	env.t.Fatalf("daemon still reports sessions for tickets %v after %s", ids, timeout)
}

// --- Tests -----------------------------------------------------------------

func TestDeleteProject_KillsDaemonSessions(t *testing.T) {
	env := newDeleteProjectEnv(t)
	env.startTestDaemon()

	p, ids := env.createProjectWithTickets("kill-test", 2)
	for _, id := range ids {
		env.spawnSessionForTicket(string(id))
	}

	// Sanity: daemon sees both sessions.
	if got := len(env.listDaemonSessions()); got != 2 {
		t.Fatalf("pre-delete daemon sessions = %d; want 2", got)
	}

	if err := DeleteProject(p.Name); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	env.waitForNoSessionsForTickets(ids, 3*time.Second)

	// Registry no longer carries the project.
	reg, err := project.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if _, err := reg.Get(p.ID); err == nil {
		t.Errorf("registry still has project %s after DeleteProject", p.ID)
	}
}

func TestDeleteProject_LeavesOtherProjectsSessions(t *testing.T) {
	env := newDeleteProjectEnv(t)
	env.startTestDaemon()

	p, pIDs := env.createProjectWithTickets("victim", 1)
	q, qIDs := env.createProjectWithTickets("survivor", 1)
	env.spawnSessionForTicket(string(pIDs[0]))
	qSession := env.spawnSessionForTicket(string(qIDs[0]))

	if got := len(env.listDaemonSessions()); got != 2 {
		t.Fatalf("pre-delete daemon sessions = %d; want 2", got)
	}

	if err := DeleteProject(p.Name); err != nil {
		t.Fatalf("DeleteProject(victim): %v", err)
	}

	env.waitForNoSessionsForTickets(pIDs, 3*time.Second)

	// Q's session survives.
	sessions := env.listDaemonSessions()
	if len(sessions) != 1 {
		t.Fatalf("post-delete daemon sessions = %d; want 1", len(sessions))
	}
	if sessions[0].SessionID != qSession {
		t.Errorf("surviving session = %s; want %s", sessions[0].SessionID, qSession)
	}
	if sessions[0].TicketID != string(qIDs[0]) {
		t.Errorf("surviving TicketID = %s; want %s", sessions[0].TicketID, qIDs[0])
	}

	// Q itself is still registered.
	reg, err := project.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if _, err := reg.Get(q.ID); err != nil {
		t.Errorf("survivor project %s missing from registry: %v", q.ID, err)
	}
}

func TestDeleteProject_NoDaemonRunning(t *testing.T) {
	env := newDeleteProjectEnv(t)
	// Deliberately NOT starting a daemon. NewNoAutostart should bounce
	// off with ErrDaemonUnavailable, and the cleanup helper must
	// swallow it and proceed.

	p, _ := env.createProjectWithTickets("no-daemon", 1)

	if err := DeleteProject(p.Name); err != nil {
		t.Fatalf("DeleteProject without daemon: %v", err)
	}

	reg, err := project.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if _, err := reg.Get(p.ID); err == nil {
		t.Errorf("registry still has project %s after DeleteProject", p.ID)
	}
}

func TestDeleteProject_TicketStoreLoadFailed(t *testing.T) {
	env := newDeleteProjectEnv(t)
	env.startTestDaemon()

	// Register the project (so DeleteProject can find it) but build a
	// trap for LoadTicketStore: replace the project's ticket directory
	// with an UNREADABLE file. LoadTicketStore calls os.MkdirAll on the
	// ticket dir; if a non-directory file already exists at that path,
	// MkdirAll returns ENOTDIR and LoadTicketStore returns "create
	// project ticket dir: ...". That's the load-failure branch.
	repoDir := filepath.Join(env.configDir, "repo-corrupt")
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	reg, err := project.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	p := project.NewProject("corrupt", repoDir)
	if err := reg.Add(p); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	// Plant a regular file where LoadTicketStore wants a directory.
	ticketsRoot := filepath.Join(env.configDir, "tickets")
	if err := os.MkdirAll(ticketsRoot, 0o755); err != nil {
		t.Fatalf("mkdir tickets root: %v", err)
	}
	trapPath := filepath.Join(ticketsRoot, p.ID)
	if err := os.WriteFile(trapPath, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("plant trap file: %v", err)
	}

	// Sanity: confirm LoadTicketStore really does fail for this layout.
	// We don't want a silent green test from a load that happened to
	// succeed (e.g. if LoadTicketStore grew tolerance for this layout).
	if _, err := project.LoadTicketStore(p); err == nil {
		t.Fatalf("expected LoadTicketStore to fail for trap layout; got nil")
	}

	if err := DeleteProject(p.Name); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	reg, err = project.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if _, err := reg.Get(p.ID); err == nil {
		t.Errorf("registry still has corrupted project %s after DeleteProject", p.ID)
	}
}
