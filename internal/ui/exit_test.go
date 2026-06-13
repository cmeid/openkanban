package ui

import (
	"testing"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/project"
	"github.com/techdufus/openkanban/internal/terminal"
)

// TestExitMsgSelectsTicket pins the contract that when a focused agent
// session exits, the board cursor jumps to the exited ticket so the user
// can press Space to mark it done without hunting across columns.
//
// Guardrail: a backgrounded session exit (the user wasn't watching) must
// NOT steal the cursor — that's the explicit scope boundary in
// /Users/cmeid/.claude/plans/when-exiting-an-openkanban-sharded-lantern.md.
func TestExitMsgSelectsTicket(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	otherTicket := &board.Ticket{
		ID:        "other-1",
		Title:     "Other",
		ProjectID: "test",
		Status:    board.StatusBacklog,
	}
	targetTicket := &board.Ticket{
		ID:        "target-1",
		Title:     "Target",
		ProjectID: "test",
		Status:    board.StatusInProgress,
	}
	if err := globalStore.Add(otherTicket); err != nil {
		t.Fatalf("Add otherTicket: %v", err)
	}
	if err := globalStore.Add(targetTicket); err != nil {
		t.Fatalf("Add targetTicket: %v", err)
	}

	// columnTickets layout: col 0 = Backlog (other), col 1 = In Progress (target).
	columnTickets := [][]*board.Ticket{
		{otherTicket},
		{targetTicket},
	}

	t.Run("focused exit jumps cursor to exited ticket", func(t *testing.T) {
		m := &Model{
			globalStore:   globalStore,
			panes:         map[board.TicketID]*terminal.Pane{targetTicket.ID: nil},
			columnTickets: columnTickets,
			columnOffsets: []int{0, 0},
			mode:          ModeAgentView,
			focusedPane:   targetTicket.ID,
			// Start on otherTicket so a successful jump is observable.
			activeColumn: 0,
			activeTicket: 0,
		}

		if _, _ = m.Update(terminal.ExitMsg{PaneID: string(targetTicket.ID)}); m.mode != ModeNormal {
			t.Errorf("mode = %v, want ModeNormal", m.mode)
		}
		if m.focusedPane != "" {
			t.Errorf("focusedPane = %q, want empty", m.focusedPane)
		}
		if m.activeColumn != 1 || m.activeTicket != 0 {
			t.Errorf("active = (col=%d, ticket=%d), want (col=1, ticket=0)",
				m.activeColumn, m.activeTicket)
		}
	})

	t.Run("backgrounded exit does not steal cursor", func(t *testing.T) {
		m := &Model{
			globalStore:   globalStore,
			panes:         map[board.TicketID]*terminal.Pane{targetTicket.ID: nil},
			columnTickets: columnTickets,
			columnOffsets: []int{0, 0},
			mode:          ModeNormal,
			// focusedPane intentionally empty: session is backgrounded, user
			// is looking at the board (or elsewhere). Exit must not move them.
			focusedPane:  "",
			activeColumn: 0,
			activeTicket: 0,
		}

		_, _ = m.Update(terminal.ExitMsg{PaneID: string(targetTicket.ID)})

		if m.activeColumn != 0 || m.activeTicket != 0 {
			t.Errorf("active = (col=%d, ticket=%d), want (col=0, ticket=0) unchanged",
				m.activeColumn, m.activeTicket)
		}
		if m.mode != ModeNormal {
			t.Errorf("mode = %v, want ModeNormal (unchanged)", m.mode)
		}
	})
}
