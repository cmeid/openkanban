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

// TestNewTicketStatusFromActiveColumn pins where a new ticket lands when
// the user presses "n" from each of the default columns. The rule:
// in_review and done route to in_progress; backlog, next, and in_progress
// keep the user's focused column. Authored as a guard against accidentally
// creating tickets in "outbound" columns (in_review, done) where they
// would be invisible to the normal create-then-work flow. Column indices
// are resolved by status (not literals) so the test survives column reorder.
func TestNewTicketStatusFromActiveColumn(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	cases := []struct {
		name         string
		fromStatus   board.TicketStatus
		wantStatus   board.TicketStatus
		focusFollows bool // for in_review/done, focus should land on in_progress column
	}{
		{name: "from backlog stays in backlog", fromStatus: board.StatusBacklog, wantStatus: board.StatusBacklog},
		{name: "from next stays in next", fromStatus: board.StatusNext, wantStatus: board.StatusNext},
		{name: "from in_progress stays in in_progress", fromStatus: board.StatusInProgress, wantStatus: board.StatusInProgress},
		{name: "from in_review routes to in_progress", fromStatus: board.StatusInReview, wantStatus: board.StatusInProgress, focusFollows: true},
		{name: "from done routes to in_progress", fromStatus: board.StatusDone, wantStatus: board.StatusInProgress, focusFollows: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
			globalStore := project.NewGlobalTicketStore(nil)
			globalStore.AddProject(proj)

			ti := textinput.New()
			di := textarea.New()
			bi := textinput.New()
			li := textinput.New()

			cols := board.DefaultColumns()
			m := &Model{
				globalStore:       globalStore,
				panes:             map[board.TicketID]*daemonclient.PaneView{},
				columns:           cols,
				columnTickets:     make([][]*board.Ticket, len(cols)),
				columnOffsets:     make([]int, len(cols)),
				mode:              ModeCreateTicket,
				activeColumn:      columnIndexOfStatus(cols, tc.fromStatus),
				activeTicket:      0,
				width:             120,
				height:            40,
				config:            &config.Config{Agents: map[string]config.AgentConfig{}},
				selectedProject:   proj,
				titleInput:        ti,
				descInput:         di,
				branchInput:       bi,
				labelsInput:       li,
				ticketPriority:    3,
				ticketUseWorktree: true,
				selectedBlockers:  map[board.TicketID]bool{},
			}
			m.refreshColumnTickets()

			title := "new-ticket-" + tc.name
			m.titleInput.SetValue(title)

			if _, _ = m.saveTicketForm(false); m.editingTicketID != "" {
				t.Fatalf("editingTicketID should be cleared after save, got %q", m.editingTicketID)
			}

			var created *board.Ticket
			for _, tk := range globalStore.All() {
				if tk.Title == title {
					created = tk
					break
				}
			}
			if created == nil {
				t.Fatalf("expected a ticket titled %q in store; not found", title)
			}
			if created.Status != tc.wantStatus {
				t.Errorf("ticket status: got %q, want %q", created.Status, tc.wantStatus)
			}

			if tc.focusFollows {
				if got := m.columns[m.activeColumn].Status; got != board.StatusInProgress {
					t.Errorf("focus should follow ticket into in_progress; activeColumn status=%q", got)
				}
			}
		})
	}
}
