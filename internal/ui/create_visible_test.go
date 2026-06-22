package ui

import (
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

// newCreateModel builds a Model wired for the interactive "n" create
// path (saveTicketForm(false)). The form inputs, selected project, and
// blockers map are all populated so saveTicketForm runs end-to-end.
func newCreateModel(t *testing.T, proj *project.Project, gs *project.GlobalTicketStore) *Model {
	t.Helper()
	cols := board.DefaultColumns()
	return &Model{
		globalStore:      gs,
		panes:            map[board.TicketID]*daemonclient.PaneView{},
		daemonOwned:      map[board.TicketID]struct{}{},
		columns:          cols,
		columnTickets:    make([][]*board.Ticket, len(cols)),
		columnOffsets:    make([]int, len(cols)),
		mode:             ModeCreateTicket,
		activeColumn:     0, // backlog
		activeTicket:     0,
		width:            120,
		height:           40,
		sortMode:         SortPriority,
		config:           &config.Config{Agents: map[string]config.AgentConfig{}},
		selectedProject:  proj,
		titleInput:       textinput.New(),
		descInput:        textarea.New(),
		branchInput:      textinput.New(),
		labelsInput:      textinput.New(),
		filterInput:      textinput.New(),
		ticketPriority:   3,
		selectedBlockers: map[board.TicketID]bool{},
		filterProjectIDs: map[string]bool{},
	}
}

func findByTitle(gs *project.GlobalTicketStore, title string) *board.Ticket {
	for _, tk := range gs.All() {
		if tk.Title == title {
			return tk
		}
	}
	return nil
}

// TestCreateTicket_VisibleDespiteOpenOnlyFilter is the core regression:
// with the open-only session filter active, a freshly created ticket
// (which has no daemon session) would be filtered out of the board, so
// selectTicketByID has nothing to land on and the new ticket is neither
// highlighted nor visible. Creating must reveal it.
//
// Non-vacuous: a daemon-owned resident with higher priority occupies
// index 0 of the backlog column, so the new ticket must land at index 1.
// An assertion of "selected card == created" plus activeTicket != 0
// cannot pass by the cursor happening to sit at index 0.
func TestCreateTicket_VisibleDespiteOpenOnlyFilter(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	gs := project.NewGlobalTicketStore(nil)
	gs.AddProject(proj)

	// Resident: daemon-owned (survives open-only), priority 1 → index 0.
	resident := &board.Ticket{ID: "T-resident", Title: "resident", ProjectID: "test", Status: board.StatusBacklog, Priority: 1}
	if err := gs.Add(resident); err != nil {
		t.Fatalf("add resident: %v", err)
	}

	m := newCreateModel(t, proj, gs)
	m.daemonOwned[resident.ID] = struct{}{}
	m.sessionFilter = SessionFilterOpen
	m.refreshColumnTickets()

	const title = "new-card"
	m.titleInput.SetValue(title)
	if _, _ = m.saveTicketForm(false); m.editingTicketID != "" {
		t.Fatalf("editingTicketID should be cleared after save")
	}

	created := findByTitle(gs, title)
	if created == nil {
		t.Fatalf("created ticket %q not found in store", title)
	}

	// Filter relaxed so the no-session ticket can show.
	if m.sessionFilter != SessionFilterAll {
		t.Errorf("sessionFilter = %q, want %q (open-only should be cleared on create)", m.sessionFilter, SessionFilterAll)
	}
	// Selection points at the created ticket, in the backlog column...
	if m.activeColumn != 0 {
		t.Fatalf("activeColumn = %d, want 0 (backlog)", m.activeColumn)
	}
	if m.activeTicket >= len(m.columnTickets[m.activeColumn]) {
		t.Fatalf("activeTicket %d out of range (len=%d)", m.activeTicket, len(m.columnTickets[m.activeColumn]))
	}
	if m.activeTicket == 0 {
		t.Errorf("activeTicket = 0 — selection sits on the resident, not the created ticket")
	}
	got := m.columnTickets[m.activeColumn][m.activeTicket]
	if got.ID != created.ID {
		t.Errorf("selected ticket = %q (%s), want created %q (%s)", got.Title, got.ID, created.Title, created.ID)
	}
}

// TestCreateTicket_VisibleDespiteSearchQuery: an active search the new
// title doesn't match must be cleared so the created ticket shows.
func TestCreateTicket_VisibleDespiteSearchQuery(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	gs := project.NewGlobalTicketStore(nil)
	gs.AddProject(proj)

	m := newCreateModel(t, proj, gs)
	m.filterQuery = "zzz-no-match"
	m.filterInput.SetValue(m.filterQuery)
	m.refreshColumnTickets()

	const title = "alpha-feature"
	m.titleInput.SetValue(title)
	m.saveTicketForm(false)

	created := findByTitle(gs, title)
	if created == nil {
		t.Fatalf("created ticket %q not found", title)
	}
	if m.filterQuery != "" {
		t.Errorf("filterQuery = %q, want cleared", m.filterQuery)
	}
	assertSelected(t, m, created)
}

// TestCreateTicket_PreservesMatchingFilter guards against over-clearing:
// when the new ticket already passes the active filter, the filter must
// be left untouched (revealThroughFilters is a no-op).
func TestCreateTicket_PreservesMatchingFilter(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	gs := project.NewGlobalTicketStore(nil)
	gs.AddProject(proj)

	m := newCreateModel(t, proj, gs)
	m.filterQuery = "keep"
	m.filterInput.SetValue(m.filterQuery)
	m.refreshColumnTickets()

	const title = "keep-this-card" // contains the active query → matches
	m.titleInput.SetValue(title)
	m.saveTicketForm(false)

	created := findByTitle(gs, title)
	if created == nil {
		t.Fatalf("created ticket %q not found", title)
	}
	if m.filterQuery != "keep" {
		t.Errorf("filterQuery = %q, want preserved \"keep\" (ticket matched, no need to clear)", m.filterQuery)
	}
	assertSelected(t, m, created)
}

// assertSelected fails cleanly (no index panic) when the active
// selection does not resolve to want — important for the red-before-fix
// case where the filter leaves the column empty.
func assertSelected(t *testing.T, m *Model, want *board.Ticket) {
	t.Helper()
	if m.activeColumn < 0 || m.activeColumn >= len(m.columnTickets) {
		t.Fatalf("activeColumn %d out of range", m.activeColumn)
	}
	col := m.columnTickets[m.activeColumn]
	if m.activeTicket < 0 || m.activeTicket >= len(col) {
		t.Fatalf("activeTicket %d out of range (len=%d) — created ticket not visible/selected", m.activeTicket, len(col))
	}
	if got := col[m.activeTicket]; got.ID != want.ID {
		t.Errorf("selected = %q (%s), want created %q (%s)", got.Title, got.ID, want.Title, want.ID)
	}
}
