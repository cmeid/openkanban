package ui

import (
	"testing"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

// TestSpawnReadyMsg_RegistersTicketInDaemonOwned pins the invariant
// that spawnReadyMsg writes both m.panes AND m.daemonOwned. Without
// the daemonOwned write, the periodic 30s resync was the only thing
// catching up — meaning the "always show working" (W) bypass and the
// 'w' session filter both missed freshly-spawned tickets for up to
// 30 seconds, and any session spawned between the disconnect-sweep
// and the next resync was likewise invisible to those filters.
func TestSpawnReadyMsg_RegistersTicketInDaemonOwned(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	ticket := &board.Ticket{
		ID:        "T-SPAWN-DO-1",
		Title:     "spawn-target",
		ProjectID: proj.ID,
		Status:    board.StatusInProgress,
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	pane := daemonclient.NewPaneView(nil, string(ticket.ID), "sid-test", nil)

	m := &Model{
		globalStore:      globalStore,
		panes:            map[board.TicketID]*daemonclient.PaneView{},
		daemonOwned:      map[board.TicketID]struct{}{},
		daemonViewing:    map[board.TicketID]int{},
		columns:          board.DefaultColumns(),
		columnTickets:    make([][]*board.Ticket, len(board.DefaultColumns())),
		columnOffsets:    make([]int, len(board.DefaultColumns())),
		mode:             ModeSpawning,
		spawningTicketID: ticket.ID,
		spawningAgent:    "claude",
		width:            120,
		height:           40,
		config:           &config.Config{Agents: map[string]config.AgentConfig{}},
	}

	if _, ok := m.daemonOwned[ticket.ID]; ok {
		t.Fatalf("pre-condition: m.daemonOwned must NOT contain ticket before spawn")
	}

	msg := spawnReadyMsg{
		ticketID: ticket.ID,
		pane:     pane,
	}
	m.Update(msg)

	// Post-condition: m.panes has the ticket (existing invariant)
	if _, ok := m.panes[ticket.ID]; !ok {
		t.Errorf("m.panes missing ticket after spawnReadyMsg")
	}
	// Post-condition (the fix): m.daemonOwned ALSO has the ticket. The
	// spawn just succeeded — there genuinely IS a daemon-side session
	// for this ticket — so the daemonOwned invariant must hold WITHOUT
	// waiting for the next 30s resync.
	if _, ok := m.daemonOwned[ticket.ID]; !ok {
		t.Errorf("m.daemonOwned missing ticket after spawnReadyMsg (regression: W toggle / session filter miss this session until next resync)")
	}
}

// TestSpawnReadyMsg_AlwaysShowWorking_SurfacesAcrossProjects is the
// integration shape of the user-reported bug: a project-filtered board
// with W=on must surface a freshly-spawned cross-project session
// IMMEDIATELY, not 30s later. Exercises the full refreshColumnTickets
// → ticketMatchesFilter path post-spawn.
func TestSpawnReadyMsg_AlwaysShowWorking_SurfacesAcrossProjects(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	fooProj := &project.Project{ID: "foo", RepoPath: t.TempDir()}
	barProj := &project.Project{ID: "bar", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(fooProj)
	globalStore.AddProject(barProj)

	// Ticket in project "bar" — the one Chris would expect to surface
	// despite the foo-narrowed project filter, because it has a fresh
	// session.
	barTicket := &board.Ticket{
		ID:        "T-BAR-1",
		Title:     "cross-project work",
		ProjectID: barProj.ID,
		Status:    board.StatusInProgress,
	}
	if err := globalStore.Add(barTicket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	pane := daemonclient.NewPaneView(nil, string(barTicket.ID), "sid-bar", nil)

	m := &Model{
		globalStore:       globalStore,
		panes:             map[board.TicketID]*daemonclient.PaneView{},
		daemonOwned:       map[board.TicketID]struct{}{},
		daemonViewing:     map[board.TicketID]int{},
		columns:           board.DefaultColumns(),
		columnTickets:     make([][]*board.Ticket, len(board.DefaultColumns())),
		columnOffsets:     make([]int, len(board.DefaultColumns())),
		filterProjectIDs:  map[string]bool{"foo": true},
		alwaysShowWorking: true,
		mode:              ModeSpawning,
		spawningTicketID:  barTicket.ID,
		spawningAgent:     "claude",
		width:             120,
		height:            40,
		config:            &config.Config{Agents: map[string]config.AgentConfig{}},
	}

	// Baseline: pre-spawn refresh — bar ticket is hidden by project filter.
	m.refreshColumnTickets()
	inProgressCol := indexOfStatus(m.columns, board.StatusInProgress)
	if inProgressCol < 0 {
		t.Fatalf("no in_progress column in DefaultColumns()")
	}
	if got := len(m.columnTickets[inProgressCol]); got != 0 {
		t.Fatalf("baseline: in_progress has %d tickets, want 0 (filtered to foo)", got)
	}

	// Dispatch spawnReadyMsg for the bar ticket.
	msg := spawnReadyMsg{
		ticketID: barTicket.ID,
		pane:     pane,
	}
	m.Update(msg)

	// After spawn the bar ticket should now be in daemonOwned, so a
	// refresh with alwaysShowWorking=true should surface it despite
	// the foo-only project filter.
	m.refreshColumnTickets()
	if got := len(m.columnTickets[inProgressCol]); got != 1 {
		t.Errorf("after spawn with W=on: in_progress has %d tickets, want 1 (cross-project session must surface)", got)
	}
}

func indexOfStatus(cols []board.Column, status board.TicketStatus) int {
	for i, c := range cols {
		if c.Status == status {
			return i
		}
	}
	return -1
}
