package ui

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

// writeLiveClaudeJSONL seeds a transcript agent.FindClaudeSession will
// accept: one assistant event with real, user-visible text (the
// jsonlHasRealAssistantContent "alive" scan) under the fake home's
// claude bucket for `worktree`.
func writeLiveClaudeJSONL(t *testing.T, homeDir, worktree, uuid string) string {
	t.Helper()
	dir := filepath.Join(homeDir, ".claude", "projects", encodeWorktreeForTest(worktree))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, uuid+".jsonl")
	ev := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role":    "assistant",
			"content": "Real assistant work happened here.",
		},
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPollBackfillsSessionIDForNonRunningPane pins the UUID back-fill
// against a pane whose cached Running() flag is false.
//
// AgentSessionID is the durable resume key — the thing that makes a
// session reachable again after a status round-trip — and `p.running`
// is only the pane's *cached* lastInfo.Running, which goes stale for an
// unattached or resynced pane (internal/ui/CLAUDE.md: "count by m.panes
// membership, NOT pane.Running()"). Before the hoist, the back-fill sat
// below the `if !p.running { continue }` early-return, so exactly the
// panes most likely to need the key never got one.
//
// The early-continue itself must survive: a non-running pane still
// reports AgentNone ("I don't know"), which the agentStatusResultMsg
// handler treats as "preserve whatever is set".
//
// RED-BEFORE-GREEN: move the back-fill block back below the
// `!p.running` continue and AgentSessionID stays empty.
func TestPollBackfillsSessionIDForNonRunningPane(t *testing.T) {
	tk := board.NewTicket("backfill-non-running", "proj-bf")
	tk.AgentType = "claude"
	gs := backfillTestEnv(t, "proj-bf", tk)

	// backfillTestEnv points HOME at its own config dir; re-point it at a
	// home we control so agent.FindClaudeSession reads our fixture bucket.
	home := t.TempDir()
	t.Setenv("HOME", home)
	worktree := filepath.Join(home, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	const uuid = "abcdefab-cdef-4abc-8def-abcdefabcdef"
	writeLiveClaudeJSONL(t, home, worktree, uuid)

	stored, _ := gs.Get(tk.ID)
	if stored == nil {
		t.Fatal("fixture: ticket not in store")
	}
	stored.WorktreePath = worktree
	if stored.AgentSessionID != "" {
		t.Fatalf("fixture: AgentSessionID = %q, want empty (the back-fill only fires on an empty field)", stored.AgentSessionID)
	}

	// A pane built with info == nil lands in the Detached state, so
	// Running() is false — the stale-cache shape this test is about.
	pv := daemonclient.NewPaneView(nil, string(tk.ID), "sid-bf", nil)
	if pv.Running() {
		t.Fatal("fixture: pane Running() = true, want false (the early-continue must be exercised)")
	}
	m := &Model{
		globalStore:     gs,
		panes:           map[board.TicketID]*daemonclient.PaneView{tk.ID: pv},
		lastPTYActivity: map[board.TicketID]time.Time{},
	}

	msg, ok := m.pollAgentStatusesAsync()().(agentStatusResultMsg)
	if !ok {
		t.Fatalf("poll returned %T, want agentStatusResultMsg", msg)
	}

	reloaded, _ := gs.Get(tk.ID)
	if reloaded.AgentSessionID != uuid {
		t.Errorf("AgentSessionID = %q, want %q — the resume key must be back-filled even when the pane's cached Running() is false",
			reloaded.AgentSessionID, uuid)
	}
	// The early-continue must still be in place: a non-running pane has
	// no status to report, and AgentNone is "unknown", not a transition.
	if got := msg[tk.ID]; got != board.AgentNone {
		t.Errorf("results[%s] = %q, want %q (the !running early-continue must survive the hoist)",
			tk.ID, got, board.AgentNone)
	}
}

// TestExitedUnexpected_KeepsCompletedBadge covers the fallout of never
// sending TicketDone on a status change: the daemon no longer learns
// that a completion was expected, so an agent that finishes its work
// and exits on its own now delivers Expected=false.
//
// Before session preservation this was invisible — the session was
// already killed when the card left in_progress, so nothing was alive
// to exit later. Now a session lives on an in_review/done card, and
// demoting its AgentCompleted badge to AgentNone on that exit would
// erase the "✓ done" marker from a genuinely finished ticket.
//
// RED-BEFORE-GREEN: drop the `case ticket.AgentStatus ==
// board.AgentCompleted` arm in daemon_subscribe.go's exited handler and
// the badge falls back to AgentNone.
func TestExitedUnexpected_KeepsCompletedBadge(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	const tid board.TicketID = "exit-keeps-completed"
	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	now := time.Now()
	const originalUUID = "uuid-completed-card"
	ticket := &board.Ticket{
		ID:             tid,
		Title:          "finished, then the agent exited",
		ProjectID:      "test",
		Status:         board.StatusDone,
		AgentStatus:    board.AgentCompleted,
		AgentSessionID: originalUUID,
		AgentSpawnedAt: &now,
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	pv := daemonclient.NewPaneView(nil, string(tid), "sid-done", &daemon.SessionInfo{Running: true})
	m := &Model{
		globalStore:     globalStore,
		daemonOwned:     map[board.TicketID]struct{}{tid: {}},
		daemonViewing:   map[board.TicketID]int{},
		lastPTYActivity: map[board.TicketID]time.Time{},
		panes:           map[board.TicketID]*daemonclient.PaneView{tid: pv},
	}

	_, _ = m.handleDaemonSessionEvent(daemonSessionEventMsg{
		Event: daemon.SessionEvent{
			Event:    "exited",
			TicketID: string(tid),
			Expected: false,
		},
	})

	if ticket.AgentStatus != board.AgentCompleted {
		t.Errorf("AgentStatus = %q, want %q — a finished ticket must keep its badge when its preserved session later exits",
			ticket.AgentStatus, board.AgentCompleted)
	}
	if ticket.AgentSessionID != originalUUID {
		t.Errorf("AgentSessionID = %q, want %q (the resume link outlives the PTY)", ticket.AgentSessionID, originalUUID)
	}
	if ticket.AgentSpawnedAt == nil {
		t.Error("AgentSpawnedAt = nil, want non-nil")
	}
	// A genuinely exited session SHOULD drop its pane — preservation is
	// about status changes, not about pretending a dead process is live.
	if _, ok := m.panes[tid]; ok {
		t.Error("panes still holds the exited session; an actual exit must drop the pane")
	}
}

// startTestDaemonClient brings up an in-process openkanbankd on a
// per-test socket and returns a connected client. Same harness as
// TestCleanup_LeavesDaemonSessionsAlive (cleanup_test.go): the socket
// lives under /tmp for the macOS AF_UNIX path-length budget, and
// OPENKANBAN_DAEMON_BINARY points at /usr/bin/true so the autostart
// fork can never race the in-process server for the socket.
func startTestDaemonClient(t *testing.T) (*daemonclient.Client, context.Context) {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "okbrt-")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sock := filepath.Join(dir, "d.sock")
	pidPath := filepath.Join(dir, "d.pid")
	t.Setenv("OPENKANBAN_DAEMON_SOCK", sock)
	t.Setenv("OPENKANBAN_DAEMON_PID", pidPath)
	t.Setenv("OPENKANBAN_DAEMON_LOG", filepath.Join(dir, "d.log"))
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

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	c, err := daemonclient.New(ctx)
	if err != nil {
		t.Fatalf("daemonclient.New: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	return c, ctx
}

// daemonSessionInfo returns the daemon's current view of sessionID, or
// nil when the daemon no longer has it.
func daemonSessionInfo(t *testing.T, ctx context.Context, c *daemonclient.Client, sessionID string) *daemon.SessionInfo {
	t.Helper()
	list, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, s := range list.Sessions {
		if s.SessionID == sessionID {
			info := s
			return &info
		}
	}
	return nil
}

// waitForContent polls a pane until its rendered content contains want.
// Output arrives over the binary stream asynchronously, so a bare read
// right after a write is a race.
func waitForContent(t *testing.T, pv *daemonclient.PaneView, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(pv.GetContent(), want) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("pane content never contained %q within %s; got:\n%s", want, timeout, pv.GetContent())
}

// runCmd drains a tea.Cmd inline, recursing one level into tea.Batch so
// the batched children actually execute (calling a Batch only yields the
// BatchMsg — the child Cmds inside it are never run by the caller).
func runCmd(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, child := range batch {
			if child != nil {
				child()
			}
		}
	}
}

// cmdOf runs a (tea.Model, tea.Cmd)-returning method and hands back just
// the Cmd, so a move can be driven in a single expression.
func cmdOf(fn func() (tea.Model, tea.Cmd)) tea.Cmd {
	_, cmd := fn()
	return cmd
}

// TestSessionSurvivesStatusRoundTrip is the end-to-end statement of the
// whole ticket, against a real in-process daemon and a real PTY:
//
//	"Even if I move a ticket to done and then have to pull it back to
//	 backlog and promote to in progress again, any time i enter the
//	 ticket, the session should still be there. No resets, no new
//	 sessions, nada."
//
// A /bin/cat session is spawned, a marker is typed into it, and the card
// is then walked all the way right (in_progress → in_review → done) and
// all the way left (→ in_review → in_progress → next → backlog) and back
// again. After every single leg the SAME daemon session must still be
// there, under the SAME SessionID.
//
// VACUITY GUARDS:
//   - The session is asserted present BEFORE the first move, so "still
//     there" afterwards is a real observation rather than a statement
//     about a session that never started.
//   - Every leg asserts pane.SessionID() == sessionID0, not just m.panes
//     membership: a RECREATED session satisfies map membership while
//     having lost the process and the scrollback.
//   - The cold-start subtest empties m.panes and rebuilds the pane from
//     the daemon's own List, which is what proves the scrollback came
//     from the daemon rather than from a local emulator that happened to
//     still hold it.
func TestSessionSurvivesStatusRoundTrip(t *testing.T) {
	c, ctx := startTestDaemonClient(t)
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	const tid board.TicketID = "T-RT-1"
	const marker = "MARKER-42"

	resp, err := c.Spawn(ctx, daemon.SpawnReq{
		TicketID:    string(tid),
		SessionName: "roundtrip-test",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	sessionID0 := resp.SessionID

	info := daemonSessionInfo(t, ctx, c, sessionID0)
	if info == nil {
		t.Fatal("fixture: daemon does not list the session it just spawned")
	}

	pv := daemonclient.NewPaneView(c, string(tid), sessionID0, info)
	pv.SetSize(80, 24)
	if err := pv.Attach(ctx); err != nil {
		t.Fatalf("PaneView.Attach: %v", err)
	}

	// Type into the live PTY. /bin/cat echoes it straight back, so the
	// marker lands in the daemon-side scrollback — the "where I was"
	// this whole change is about.
	pv.WriteRaw([]byte(marker + "\n"))
	waitForContent(t, pv, marker, 5*time.Second)

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	now := time.Now()
	ticket := &board.Ticket{
		ID:             tid,
		Title:          "round-trip target",
		ProjectID:      "test",
		Status:         board.StatusInProgress,
		AgentStatus:    board.AgentWorking,
		AgentType:      "claude",
		AgentSessionID: "deadbeef-0002",
		AgentSpawnedAt: &now,
		// Non-empty so a move BACK to in_progress doesn't detour into
		// setupWorktree (no worktree manager is wired here).
		WorktreePath: t.TempDir(),
		UseWorktree:  true,
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	m := &Model{
		globalStore:     globalStore,
		daemon:          c,
		daemonClient:    c,
		panes:           map[board.TicketID]*daemonclient.PaneView{tid: pv},
		daemonOwned:     map[board.TicketID]struct{}{tid: {}},
		daemonViewing:   map[board.TicketID]int{},
		lastPTYActivity: map[board.TicketID]time.Time{tid: now},
		columns:         board.DefaultColumns(),
		config:          &config.Config{Agents: map[string]config.AgentConfig{}},
		width:           80,
		height:          26,
	}
	m.refreshColumnTickets()
	m.selectTicketByID(tid)

	// NON-VACUITY: the session must be live before the first move, or
	// "still there" after each move would prove nothing.
	if daemonSessionInfo(t, ctx, c, sessionID0) == nil {
		t.Fatal("fixture: daemon lost the session before the first move")
	}

	legs := []struct {
		name    string
		forward bool
		want    board.TicketStatus
	}{
		{"in_progress→in_review", true, board.StatusInReview},
		{"in_review→done", true, board.StatusDone},
		{"done→in_review", false, board.StatusInReview},
		{"in_review→in_progress", false, board.StatusInProgress},
		{"in_progress→next", false, board.StatusNext},
		{"next→backlog", false, board.StatusBacklog},
		{"backlog→next", true, board.StatusNext},
		{"next→in_progress", true, board.StatusInProgress},
	}

	for _, leg := range legs {
		if leg.forward {
			runCmd(cmdOf(m.quickMoveTicket))
		} else {
			runCmd(cmdOf(m.quickMoveTicketBackward))
		}

		if ticket.Status != leg.want {
			t.Fatalf("%s: Status = %q, want %q", leg.name, ticket.Status, leg.want)
		}
		if daemonSessionInfo(t, ctx, c, sessionID0) == nil {
			t.Fatalf("%s: the daemon session is gone — a status change must never end a session", leg.name)
		}
		got, ok := m.panes[tid]
		if !ok {
			t.Fatalf("%s: pane was dropped from m.panes; Enter would have nothing to re-attach to", leg.name)
		}
		if got.SessionID() != sessionID0 {
			t.Fatalf("%s: pane SessionID = %q, want %q — this is a NEW session, not the preserved one",
				leg.name, got.SessionID(), sessionID0)
		}
	}

	// The pane the user never left still shows the marker.
	if !strings.Contains(pv.GetContent(), marker) {
		t.Errorf("after the round trip the attached pane lost %q:\n%s", marker, pv.GetContent())
	}

	t.Run("cold start re-enters the same session with its scrollback", func(t *testing.T) {
		// Park the card in done, the column a user is most likely to
		// pull back from.
		runCmd(cmdOf(m.quickMoveTicket))
		runCmd(cmdOf(m.quickMoveTicket))
		if ticket.Status != board.StatusDone {
			t.Fatalf("setup: Status = %q, want done", ticket.Status)
		}

		// Simulate a TUI restart: drop every local pane and release the
		// daemon's single attach slot. Everything about the session now
		// has to come back from the daemon.
		if err := pv.Close(); err != nil {
			t.Fatalf("pv.Close: %v", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			info := daemonSessionInfo(t, ctx, c, sessionID0)
			if info == nil {
				t.Fatal("closing the pane killed the daemon session")
			}
			if info.AttachedClient == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("daemon still reports client %d attached 5s after Close", info.AttachedClient)
			}
			time.Sleep(25 * time.Millisecond)
		}
		m.panes = map[board.TicketID]*daemonclient.PaneView{}
		m.focusedPane = ""
		m.mode = ModeNormal

		// Pass 1 of the periodic resync is what adopts a daemon-owned
		// session on a cold board — with no status filter, which is why
		// a done card is adoptable at all.
		_, tickCmd := m.handleDaemonResyncTick()
		if tickCmd == nil {
			t.Fatal("resync tick returned no Cmd")
		}
		resync, ok := tickCmd().(daemonResyncMsg)
		if !ok {
			t.Fatalf("resync tick returned %T, want daemonResyncMsg", resync)
		}
		if resync.err != nil {
			t.Fatalf("resync: %v", resync.err)
		}
		m.handleDaemonResyncMsg(resync)

		adopted, ok := m.panes[tid]
		if !ok {
			t.Fatal("resync did not adopt a pane for the daemon-owned done ticket")
		}
		if adopted == pv {
			t.Fatal("resync reused the closed pane; the cold-start path is not being exercised")
		}
		if adopted.SessionID() != sessionID0 {
			t.Fatalf("adopted pane SessionID = %q, want %q", adopted.SessionID(), sessionID0)
		}

		// Enter on the done card. The pane is Unattached, so spawnAgent
		// re-attaches instead of spawning anything new.
		m.refreshColumnTickets()
		m.selectTicketByID(tid)
		_, cmd := m.spawnAgent()
		if m.mode != ModeAgentView {
			t.Fatalf("mode = %q after Enter on a done card, want %q (no new session should be spawned)", m.mode, ModeAgentView)
		}
		if m.focusedPane != tid {
			t.Fatalf("focusedPane = %q, want %q", m.focusedPane, tid)
		}
		runCmd(cmd)

		// A freshly built PaneView starts from freshEmulatorLocked ->
		// ClearScrollback, so the only place this marker can come from
		// is the daemon's own snapshot. This is the literal "right back
		// where I was".
		waitForContent(t, adopted, marker, 5*time.Second)

		if daemonSessionInfo(t, ctx, c, sessionID0) == nil {
			t.Fatal("the daemon session is gone after re-entry")
		}
	})
}

// TestNoTicketDoneFromStatusPaths is the cheapest permanent
// revert-detector for the preservation guarantee: it pins WHERE the
// session-ending RPC may be called from at all.
//
// TicketDone tears down the daemon's PTY. After this change the only
// sanctioned callers are the two delete paths (TUI ticket-delete via
// performTicketCleanup, CLI ticket-delete via notifyDaemonTicketDoneCLI)
// plus the project-delete sweep in internal/app. A status-mutation path
// — `cmd/ticket_move.go`, `cmd/ticket_done.go`, or the TUI's board-move
// helpers — must never reach it.
//
// Anchored on call syntax (`.TicketDone(`), so re-adding the RPC in any
// form trips it; the interface declaration in daemon_api.go and the
// client method itself are outside the scanned set.
func TestNoTicketDoneFromStatusPaths(t *testing.T) {
	// file → exact number of `.TicketDone(` CALL sites allowed.
	want := map[string]int{
		"model.go":      1, // performTicketCleanup — ticket delete
		"ticket.go":     1, // notifyDaemonTicketDoneCLI — CLI ticket delete
		"daemon_api.go": 0, // interface declaration, not a call
	}
	// Files that must not contain the literal at all. These are the
	// status-mutation paths the preservation guarantee is about.
	forbidden := map[string]bool{
		"ticket_move.go": true,
		"ticket_done.go": true,
	}

	got := map[string]int{}
	roots := []string{"./", "../../cmd"}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() {
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			calls := 0
			for i, line := range strings.Split(string(data), "\n") {
				if !strings.Contains(line, ".TicketDone(") {
					continue
				}
				calls++
				if forbidden[name] {
					t.Errorf("%s:%d calls TicketDone — status changes must never end a session:\n\t%s",
						path, i+1, strings.TrimSpace(line))
				}
			}
			if calls > 0 {
				got[name] += calls
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	for name, n := range want {
		if n == 0 {
			continue
		}
		if got[name] != n {
			t.Errorf("%s has %d TicketDone call(s), want %d — a new caller (or a removed one) "+
				"changes which paths can end a session; update this guard deliberately",
				name, got[name], n)
		}
	}
	for name, n := range got {
		if _, known := want[name]; !known {
			t.Errorf("unexpected TicketDone caller %s (%d call(s)): only ticket-delete paths may "+
				"end a session; if this is legitimate, add it to the want map with a reason",
				name, n)
		}
	}
}
