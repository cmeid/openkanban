package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

// TestSpawnEnterConsolidated pins the contract that "s" and Enter are
// interchangeable from the board view: both should spawn-or-attach the
// ticket's agent without cross-instructing the user to press the other
// key. Prior behavior was that pressing the "wrong" key for the current
// state ("Enter" with no pane, "s" with an attached pane) bounced the
// user with a "press 's' to spawn" / "press Enter to attach" notice
// instead of doing the obvious thing.
func TestSpawnEnterConsolidated(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	mkModel := func(t *testing.T, ticket *board.Ticket, columnTickets [][]*board.Ticket, activeCol, activeIdx int) *Model {
		t.Helper()
		proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
		globalStore := project.NewGlobalTicketStore(nil)
		globalStore.AddProject(proj)
		if ticket != nil {
			ticket.ProjectID = proj.ID
			if err := globalStore.Add(ticket); err != nil {
				t.Fatalf("Add ticket: %v", err)
			}
		}
		return &Model{
			globalStore:   globalStore,
			panes:         map[board.TicketID]*daemonclient.PaneView{},
			columnTickets: columnTickets,
			columnOffsets: []int{0, 0, 0, 0},
			mode:          ModeNormal,
			activeColumn:  activeCol,
			activeTicket:  activeIdx,
			width:         120,
			height:        40,
			config:        &config.Config{Agents: map[string]config.AgentConfig{}},
		}
	}

	// keyMsg returns the tea.KeyMsg the Update loop sees for a single
	// physical key — same wire representation handleNormalMode switches on.
	enterKey := tea.KeyMsg{Type: tea.KeyEnter}
	sKey := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}}

	// Both keys, dispatched against an identical fresh model, must end
	// in the same observable state. Asserting equivalence (rather than
	// re-asserting each branch) is what locks the consolidation in.
	type keyCase struct {
		name string
		key  tea.KeyMsg
	}
	cases := []keyCase{
		{"enter", enterKey},
		{"s", sKey},
	}

	t.Run("no ticket selected — both keys notify the same way", func(t *testing.T) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				m := mkModel(t, nil, [][]*board.Ticket{{}, {}, {}, {}}, 0, 0)
				if _, _ = m.Update(tc.key); m.notification != "No ticket selected" {
					t.Errorf("notification = %q, want \"No ticket selected\"", m.notification)
				}
				if m.mode != ModeNormal {
					t.Errorf("mode = %v, want ModeNormal", m.mode)
				}
			})
		}
	})

	t.Run("backlog ticket — both keys notify Press Space", func(t *testing.T) {
		// Acceptance: the gate that says "this ticket isn't ready yet"
		// fires on Enter too, not just on s. Pre-consolidation, Enter
		// stopped at "No agent running — press 's' to spawn", which is
		// a redirect, not the actual gate the user needs to clear.
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				ticket := &board.Ticket{
					ID:     "T-1",
					Title:  "Backlog ticket",
					Status: board.StatusBacklog,
				}
				m := mkModel(t, ticket, [][]*board.Ticket{{ticket}, {}, {}, {}}, 0, 0)
				if _, _ = m.Update(tc.key); m.notification != "Press Space to move to In Progress first" {
					t.Errorf("notification = %q, want \"Press Space to move to In Progress first\"", m.notification)
				}
				if m.mode != ModeNormal {
					t.Errorf("mode = %v, want ModeNormal", m.mode)
				}
			})
		}
	})

	t.Run("cross-key redirect notifications are gone", func(t *testing.T) {
		// Negative assertion: the old "press OTHER KEY to do the thing"
		// nudges have to disappear, otherwise the consolidation is
		// only cosmetic and the user still sees the bounce.
		ticket := &board.Ticket{
			ID:     "T-2",
			Title:  "In-progress ticket, no pane",
			Status: board.StatusInProgress,
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				m := mkModel(t, ticket, [][]*board.Ticket{{}, {ticket}, {}, {}}, 1, 0)
				_, _ = m.Update(tc.key)
				if got := m.notification; got == "No agent running — press 's' to spawn" ||
					got == "Agent already running — press Enter to attach" {
					t.Errorf("got cross-key redirect notification %q — should be consolidated away", got)
				}
			})
		}
	})
}
