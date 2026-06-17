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

// TestDropTicket_FocusFollowsMovedTicket pins the drag-drop focus
// contract: after dropping a ticket onto another column the cursor must
// land on THAT ticket, not on index 0 of the target column. The target
// column is pre-seeded with a higher-priority resident so the moved
// ticket sorts to index 1 — index 0 would be a false pass under the old
// `activeTicket = 0` hardcode.
func TestDropTicket_FocusFollowsMovedTicket(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	// resident sits in the DONE column with the highest priority so it
	// owns index 0 after a priority sort. dragged starts in BACKLOG with
	// a lower priority and must land at index 1 of DONE once moved.
	resident := &board.Ticket{ID: "T-resident", Title: "resident", ProjectID: "test", Status: board.StatusDone, Priority: 1}
	dragged := &board.Ticket{ID: "T-dragged", Title: "dragged", ProjectID: "test", Status: board.StatusBacklog, Priority: 3}
	for _, tk := range []*board.Ticket{resident, dragged} {
		if err := globalStore.Add(tk); err != nil {
			t.Fatalf("Add %q: %v", tk.Title, err)
		}
	}

	cols := board.DefaultColumns()
	backlogCol, doneCol := -1, -1
	for i, c := range cols {
		switch c.Status {
		case board.StatusBacklog:
			backlogCol = i
		case board.StatusDone:
			doneCol = i
		}
	}

	m := &Model{
		globalStore:     globalStore,
		panes:           map[board.TicketID]*daemonclient.PaneView{},
		columns:         cols,
		columnTickets:   make([][]*board.Ticket, len(cols)),
		columnOffsets:   make([]int, len(cols)),
		mode:            ModeNormal,
		sortMode:        SortPriority,
		width:           120,
		height:          40,
		config:          &config.Config{Agents: map[string]config.AgentConfig{}},
		selectedProject: proj,
	}
	m.refreshColumnTickets()

	// Drag `dragged` (sole backlog card, index 0) onto the done column.
	m.activeColumn = backlogCol
	m.activeTicket = 0
	m.dragSourceColumn = backlogCol
	m.dragSourceTicket = 0
	m.dragTargetColumn = doneCol
	m.dragging = true

	_, cmd := m.dropTicket()
	if cmd != nil {
		_ = cmd() // no-op here (not leaving in_progress) but drive it for parity
	}

	if m.columns[m.activeColumn].Status != board.StatusDone {
		t.Fatalf("activeColumn status = %q, want done", m.columns[m.activeColumn].Status)
	}
	if m.activeTicket == 0 {
		t.Errorf("activeTicket = 0 — focus fell back to top of column instead of following the dropped ticket")
	}
	got := m.columnTickets[m.activeColumn][m.activeTicket]
	if got.ID != dragged.ID {
		t.Errorf("focused ticket = %q (%s), want %q (%s)", got.Title, got.ID, dragged.Title, dragged.ID)
	}
}

// TestEditTicket_FocusFollowsReorderedTicket pins the edit focus
// contract: changing a ticket's priority re-sorts its column, and the
// cursor must follow the edited ticket to its new position rather than
// staying at the now-stale index. The edited ticket is moved from the
// top of the column to the bottom, so a missing selectTicketByID leaves
// focus on a different card.
func TestEditTicket_FocusFollowsReorderedTicket(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	// Distinct priorities → deterministic priority-sort order a, b, c.
	a := &board.Ticket{ID: "T-a", Title: "alpha", ProjectID: "test", Status: board.StatusBacklog, Priority: 1}
	b := &board.Ticket{ID: "T-b", Title: "bravo", ProjectID: "test", Status: board.StatusBacklog, Priority: 2}
	c := &board.Ticket{ID: "T-c", Title: "charlie", ProjectID: "test", Status: board.StatusBacklog, Priority: 3}
	for _, tk := range []*board.Ticket{a, b, c} {
		if err := globalStore.Add(tk); err != nil {
			t.Fatalf("Add %q: %v", tk.Title, err)
		}
	}

	ti := textinput.New()
	di := textarea.New()
	bi := textinput.New()
	li := textinput.New()

	cols := board.DefaultColumns()
	m := &Model{
		globalStore:      globalStore,
		panes:            map[board.TicketID]*daemonclient.PaneView{},
		columns:          cols,
		columnTickets:    make([][]*board.Ticket, len(cols)),
		columnOffsets:    make([]int, len(cols)),
		mode:             ModeEditTicket,
		sortMode:         SortPriority,
		activeColumn:     0,
		activeTicket:     0,
		width:            120,
		height:           40,
		config:           &config.Config{Agents: map[string]config.AgentConfig{}},
		selectedProject:  proj,
		titleInput:       ti,
		descInput:        di,
		branchInput:      bi,
		labelsInput:      li,
		selectedBlockers: map[board.TicketID]bool{},
	}
	m.refreshColumnTickets()

	// `a` (priority 1) sits at index 0 of the backlog column. Confirm the
	// precondition so the assertion below is meaningful.
	preIdx := -1
	for i, tk := range m.columnTickets[0] {
		if tk.ID == a.ID {
			preIdx = i
		}
	}
	if preIdx != 0 {
		t.Fatalf("precondition: edited ticket should start at index 0, got %d", preIdx)
	}

	// Edit `a`: drop it to the lowest priority so the column reorders to
	// b, c, a and `a` lands at index 2.
	m.editingTicketID = a.ID
	m.titleInput.SetValue(a.Title)
	m.branchInput.SetValue("feat-alpha") // non-empty → skip branch generation
	m.ticketPriority = 5

	if _, _ = m.saveTicketForm(true); m.editingTicketID != "" {
		t.Fatalf("editingTicketID should be cleared after save, got %q", m.editingTicketID)
	}

	if m.activeColumn != 0 {
		t.Fatalf("activeColumn = %d, want 0 (backlog)", m.activeColumn)
	}
	if m.activeTicket == preIdx {
		t.Errorf("activeTicket still %d — focus did not follow the reordered ticket", m.activeTicket)
	}
	if m.activeTicket >= len(m.columnTickets[m.activeColumn]) {
		t.Fatalf("activeTicket = %d out of range (len=%d)", m.activeTicket, len(m.columnTickets[m.activeColumn]))
	}
	got := m.columnTickets[m.activeColumn][m.activeTicket]
	if got.ID != a.ID {
		t.Errorf("focused ticket = %q (%s), want %q (%s)", got.Title, got.ID, a.Title, a.ID)
	}
}
