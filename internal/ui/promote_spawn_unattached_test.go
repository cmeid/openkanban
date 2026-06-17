package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

// columnIndexOfStatus returns the column index for a status in the default
// board layout, or -1.
func columnIndexOfStatus(cols []board.Column, status board.TicketStatus) int {
	for i, c := range cols {
		if c.Status == status {
			return i
		}
	}
	return -1
}

// TestNewUnattachedPane_RunningTrueIsUnattached is the load-bearing unit test
// for the ctrl+space spawn path. The inline construction site inside the
// prepareSpawnWith closure sits after the daemonClient-nil guard and is
// unreachable from any daemonless test, so this helper is the ONLY place the
// Running:true invariant can be exercised in isolation.
//
// Non-vacuity: NewPaneView only flips to PaneViewUnattached when info.Running
// is true. The negative control proves the discriminator — drop Running:true
// from newUnattachedPane and the positive case collapses to PaneViewDetached,
// failing this test.
func TestNewUnattachedPane_RunningTrueIsUnattached(t *testing.T) {
	pv := newUnattachedPane(nil, "T-1", "sid-1", "name-1", "/wd", 120, 40)
	if got := pv.State(); got != daemonclient.PaneViewUnattached {
		t.Errorf("newUnattachedPane state = %v, want PaneViewUnattached", got)
	}
	if got := pv.SessionID(); got != "sid-1" {
		t.Errorf("SessionID = %q, want %q", got, "sid-1")
	}

	// Negative control: an otherwise-identical pane with Running:false must
	// land in PaneViewDetached. This is what makes the positive assertion
	// non-vacuous — it pins the exact field the helper must set.
	notRunning := daemonclient.NewPaneView(nil, "T-1", "sid-1", &daemon.SessionInfo{
		SessionID: "sid-1", Running: false,
	})
	if got := notRunning.State(); got != daemonclient.PaneViewDetached {
		t.Errorf("Running:false pane state = %v, want PaneViewDetached", got)
	}
}

// TestSpawnUnattachedReadyMsg_StaysOnBoard pins the completion handler: it
// registers the Unattached pane and stamps the ticket WITHOUT switching to
// ModeAgentView, setting focusedPane, or starting a listener.
//
// Non-vacuity: m.mode == ModeNormal is the zero-value default (vacuous alone),
// so it is paired with (a) panes+daemonOwned presence — which discriminate an
// absent/no-op handler — and (b) focusedPane == "" — which discriminates the
// accidental ATTACHED path (the attached spawnReadyMsg handler sets BOTH
// mode=ModeAgentView and focusedPane, so checking mode alone is incomplete).
func TestSpawnUnattachedReadyMsg_StaysOnBoard(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	ticket := &board.Ticket{ID: "T-bg-1", Title: "bg", ProjectID: "test", Status: board.StatusInProgress}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	m := &Model{
		globalStore: globalStore,
		panes:       map[board.TicketID]*daemonclient.PaneView{},
		daemonOwned: map[board.TicketID]struct{}{},
		columns:     board.DefaultColumns(),
		mode:        ModeNormal,
		focusedPane: "",
		width:       120,
		height:      40,
		config:      &config.Config{Agents: map[string]config.AgentConfig{}},
	}

	pane := newUnattachedPane(nil, ticket.ID, "sid-bg", "name", "/wd", 120, 40)
	_, _ = m.Update(spawnUnattachedReadyMsg{ticketID: ticket.ID, pane: pane})

	got, ok := m.panes[ticket.ID]
	if !ok {
		t.Fatalf("panes[%s] missing — handler did not register the pane", ticket.ID)
	}
	if got.State() != daemonclient.PaneViewUnattached {
		t.Errorf("registered pane state = %v, want PaneViewUnattached", got.State())
	}
	if _, owned := m.daemonOwned[ticket.ID]; !owned {
		t.Errorf("daemonOwned[%s] missing — handler did not register daemon ownership", ticket.ID)
	}
	if m.mode != ModeNormal {
		t.Errorf("mode = %v, want ModeNormal (unattached spawn must NOT switch to agent view)", m.mode)
	}
	if m.focusedPane != "" {
		t.Errorf("focusedPane = %q, want \"\" (unattached spawn must not focus the pane — that's the attached path)", m.focusedPane)
	}
	if ticket.AgentSpawnedAt == nil {
		t.Errorf("AgentSpawnedAt not stamped")
	}
}

// TestPromoteAndSpawnUnattached_MovesToInProgress pins the synchronous move
// half of the ctrl+space keybinding, driven through the real key path.
//
// Non-vacuity: the ticket starts in BACKLOG (not the target state), so the
// status assertion is meaningful; asserting it lands in the in_progress COLUMN
// (not just the status field) proves refreshColumnTickets ran. The returned
// spawn Cmd is intentionally NOT invoked — the move is synchronous in the
// Update thread, which is the property under test.
func TestPromoteAndSpawnUnattached_MovesToInProgress(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	// WorktreePath preset so the move skips git worktree/branch setup (which
	// would touch a non-repo TempDir); the move logic is what we're pinning.
	ticket := &board.Ticket{
		ID: "T-promote", Title: "promote", ProjectID: "test",
		Status: board.StatusBacklog, WorktreePath: "/already/set",
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	cols := board.DefaultColumns()
	backlogIdx := columnIndexOfStatus(cols, board.StatusBacklog)
	inProgressIdx := columnIndexOfStatus(cols, board.StatusInProgress)

	m := &Model{
		globalStore:   globalStore,
		panes:         map[board.TicketID]*daemonclient.PaneView{},
		daemonOwned:   map[board.TicketID]struct{}{},
		columns:       cols,
		columnTickets: make([][]*board.Ticket, len(cols)),
		columnOffsets: make([]int, len(cols)),
		mode:          ModeNormal,
		width:         120,
		height:        40,
		config: &config.Config{
			Agents:   map[string]config.AgentConfig{"claude": {Command: "claude"}},
			Defaults: config.BoardSettings{DefaultAgent: "claude"},
		},
		selectedProject: proj,
	}
	m.refreshColumnTickets()
	m.activeColumn = backlogIdx
	m.activeTicket = 0

	if m.selectedTicket() == nil || m.selectedTicket().ID != ticket.ID {
		t.Fatalf("fixture: backlog ticket not selected")
	}

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlAt}) // ctrl+space

	if ticket.Status != board.StatusInProgress {
		t.Errorf("ticket.Status = %q, want in_progress", ticket.Status)
	}
	found := false
	for _, tk := range m.columnTickets[inProgressIdx] {
		if tk.ID == ticket.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ticket not found in in_progress column %d after ctrl+space — refreshColumnTickets did not run", inProgressIdx)
	}

	// The resolved default agent type is stamped on the ticket (parity with
	// the attached spawn path) so status detection isn't left with "".
	if ticket.AgentType != "claude" {
		t.Errorf("ticket.AgentType = %q, want %q (resolved default must be persisted)", ticket.AgentType, "claude")
	}
}

// TestPromoteAndSpawnUnattached_AlreadyHasPane pins the don't-double-spawn
// guard: when a pane already exists for the focused ticket, ctrl+space notifies
// and leaves the pane untouched (no second spawn, no mode change).
func TestPromoteAndSpawnUnattached_AlreadyHasPane(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	ticket := &board.Ticket{ID: "T-live", Title: "live", ProjectID: "test", Status: board.StatusInProgress}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	existing := daemonclient.NewPaneView(nil, string(ticket.ID), "sid-live", nil)
	cols := board.DefaultColumns()
	inProgressIdx := columnIndexOfStatus(cols, board.StatusInProgress)

	m := &Model{
		globalStore:   globalStore,
		panes:         map[board.TicketID]*daemonclient.PaneView{ticket.ID: existing},
		daemonOwned:   map[board.TicketID]struct{}{},
		columns:       cols,
		columnTickets: make([][]*board.Ticket, len(cols)),
		columnOffsets: make([]int, len(cols)),
		mode:          ModeNormal,
		width:         120,
		height:        40,
		config:        &config.Config{Agents: map[string]config.AgentConfig{}},
		selectedProject: proj,
	}
	m.refreshColumnTickets()
	m.activeColumn = inProgressIdx
	m.activeTicket = 0

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlAt}) // ctrl+space

	if !strings.Contains(m.notification, "Agent already running") {
		t.Errorf("notification = %q, want it to mention 'Agent already running'", m.notification)
	}
	if got := m.panes[ticket.ID]; got != existing {
		t.Errorf("panes[%s] was replaced — guard should leave the live pane untouched", ticket.ID)
	}
	if m.mode != ModeNormal {
		t.Errorf("mode = %v, want ModeNormal (guard must not attach/switch)", m.mode)
	}
}

// TestContextualHints_CtrlSpaceSurfaced pins the footer doc surface: a selected
// backlog ticket (the fallback hint set, where ctrl+space is most novel) must
// advertise Ctrl+Space at a generous width.
//
// Non-vacuity: a generous budget guarantees the width-aware packer keeps the
// new hint, so the substring assert fails iff the hintSpec was never added.
func TestContextualHints_CtrlSpaceSurfaced(t *testing.T) {
	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)
	ticket := &board.Ticket{ID: "T-bk", Title: "bk", ProjectID: "test", Status: board.StatusBacklog}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	cols := board.DefaultColumns()
	m := &Model{
		globalStore:   globalStore,
		panes:         map[board.TicketID]*daemonclient.PaneView{},
		columns:       cols,
		columnTickets: make([][]*board.Ticket, len(cols)),
		columnOffsets: make([]int, len(cols)),
		mode:          ModeNormal,
	}
	m.refreshColumnTickets()
	m.activeColumn = columnIndexOfStatus(cols, board.StatusBacklog)
	m.activeTicket = 0
	if m.selectedTicket() == nil {
		t.Fatalf("fixture: backlog ticket not selected")
	}

	const budget = 1_000_000
	out := hintsAt(m, budget)
	if !strings.Contains(out, "Ctrl+Space") {
		t.Errorf("backlog footer missing Ctrl+Space hint: %q", out)
	}
	if !strings.Contains(out, "bg agent") {
		t.Errorf("backlog footer missing 'bg agent' label: %q", out)
	}
	if w := lipgloss.Width(out); w > budget {
		t.Errorf("rendered width %d exceeds budget %d", w, budget)
	}

	// The in_progress-no-pane branch (a distinct hint set) must also surface
	// the key — that's where ctrl+space spawns-in-place. Move the ticket and
	// re-select so selectedTicket() returns an in_progress, pane-less ticket.
	ticket.SetStatus(board.StatusInProgress)
	m.refreshColumnTickets()
	m.activeColumn = columnIndexOfStatus(cols, board.StatusInProgress)
	m.activeTicket = 0
	if st := m.selectedTicket(); st == nil || st.Status != board.StatusInProgress {
		t.Fatalf("fixture: in_progress ticket not selected")
	}
	if _, hasPane := m.panes[ticket.ID]; hasPane {
		t.Fatalf("fixture: ticket unexpectedly has a pane (would take the wrong hint branch)")
	}
	out = hintsAt(m, budget)
	if !strings.Contains(out, "Ctrl+Space") {
		t.Errorf("in_progress-no-pane footer missing Ctrl+Space hint: %q", out)
	}
}
