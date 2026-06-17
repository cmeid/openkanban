package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

// stuckModalFixture builds a Model with a single AgentStuck ticket
// selected on the in-progress column, no daemon wired (so handleQuit's
// global arm resolves to tea.Quit deterministically).
func stuckModalFixture(t *testing.T) (*Model, *board.Ticket) {
	t.Helper()
	proj := &project.Project{ID: "stuck-proj", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	ticket := &board.Ticket{
		ID:          "T-STUCK-1",
		Title:       "wedged session",
		ProjectID:   proj.ID,
		Status:      board.StatusInProgress,
		AgentType:   "claude",
		AgentStatus: board.AgentStuck,
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	m := &Model{
		globalStore:   globalStore,
		panes:         map[board.TicketID]*daemonclient.PaneView{},
		columnTickets: [][]*board.Ticket{{}, {ticket}, {}, {}},
		columnOffsets: []int{0, 0, 0, 0},
		mode:          ModeNormal,
		activeColumn:  1,
		activeTicket:  0,
		width:         120,
		height:        40,
		config: &config.Config{
			Defaults: config.BoardSettings{DefaultAgent: "claude"},
			Agents:   map[string]config.AgentConfig{"claude": {Command: "claude"}},
		},
	}
	return m, ticket
}

// TestStuckModal_KeyRouting pins the stuck-action modal's key routing:
//
//   - 'r' on a stuck card opens the modal (no-op on non-stuck cards).
//   - With the modal open, the GLOBAL key arms still run FIRST (PR #70
//     routing): q / ctrl+c reach handleQuit (→ tea.Quit with no daemon),
//     not swallowed by the modal.
//   - esc dismisses the modal.
//   - 'd' (destroy) dismisses the modal and takes the destroy path.
//   - 'r' (recover) dismisses the modal and takes the recover path.
//   - an unhandled key is swallowed while the modal is open (never
//     reaches a board binding).
func TestStuckModal_KeyRouting(t *testing.T) {
	t.Run("r opens the modal on a stuck card", func(t *testing.T) {
		m, _ := stuckModalFixture(t)
		if _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")}); !m.stuckActionPrompt {
			t.Fatalf("stuckActionPrompt = false, want true after pressing r on a stuck card")
		}
		if m.stuckActionTicket != "T-STUCK-1" {
			t.Errorf("stuckActionTicket = %q, want T-STUCK-1", m.stuckActionTicket)
		}
	})

	t.Run("r is inert on a non-stuck card", func(t *testing.T) {
		m, ticket := stuckModalFixture(t)
		ticket.AgentStatus = board.AgentWorking
		if _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")}); m.stuckActionPrompt {
			t.Fatalf("stuckActionPrompt = true on a non-stuck card; r must be inert")
		}
	})

	t.Run("global quit arm runs before the modal (q)", func(t *testing.T) {
		m, _ := stuckModalFixture(t)
		if _, _ = m.openStuckActionModal(); !m.stuckActionPrompt {
			t.Fatalf("precondition: modal not open")
		}
		// q must reach handleQuit (global arm) — with no daemon it
		// resolves to tea.Quit — NOT be swallowed by the modal.
		_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
		if !isQuitCmd(cmd) {
			t.Fatalf("q while modal open did not reach handleQuit (no tea.Quit) — global arm was swallowed")
		}
	})

	t.Run("global quit arm runs before the modal (ctrl+c)", func(t *testing.T) {
		m, _ := stuckModalFixture(t)
		if _, _ = m.openStuckActionModal(); !m.stuckActionPrompt {
			t.Fatalf("precondition: modal not open")
		}
		_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
		if !isQuitCmd(cmd) {
			t.Fatalf("ctrl+c while modal open did not reach handleQuit — global arm was swallowed")
		}
	})

	t.Run("esc dismisses the modal", func(t *testing.T) {
		m, _ := stuckModalFixture(t)
		if _, _ = m.openStuckActionModal(); !m.stuckActionPrompt {
			t.Fatalf("precondition: modal not open")
		}
		if _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc}); m.stuckActionPrompt {
			t.Errorf("after esc: stuckActionPrompt = true, want false")
		}
		if m.stuckActionTicket != "" {
			t.Errorf("after esc: stuckActionTicket = %q, want empty", m.stuckActionTicket)
		}
	})

	t.Run("d (destroy) dismisses the modal", func(t *testing.T) {
		m, _ := stuckModalFixture(t)
		if _, _ = m.openStuckActionModal(); !m.stuckActionPrompt {
			t.Fatalf("precondition: modal not open")
		}
		// No pane is registered, so destroy is a no-op Kill but must
		// still close the modal.
		if _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")}); m.stuckActionPrompt {
			t.Errorf("after d: stuckActionPrompt = true, want false (destroy must dismiss)")
		}
	})

	t.Run("r (recover) dismisses the modal", func(t *testing.T) {
		m, _ := stuckModalFixture(t)
		if _, _ = m.openStuckActionModal(); !m.stuckActionPrompt {
			t.Fatalf("precondition: modal not open")
		}
		// Recover routes through spawnAgent; with no existing pane it
		// proceeds down the spawn path, but the modal must be dismissed
		// regardless.
		if _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")}); m.stuckActionPrompt {
			t.Errorf("after r: stuckActionPrompt = true, want false (recover must dismiss)")
		}
	})

	t.Run("unhandled key is swallowed while modal open", func(t *testing.T) {
		m, _ := stuckModalFixture(t)
		if _, _ = m.openStuckActionModal(); !m.stuckActionPrompt {
			t.Fatalf("precondition: modal not open")
		}
		// 'j' is a board navigation key; while the modal is open it must
		// be swallowed (modal stays open, no navigation side effect).
		startCol, startTicket := m.activeColumn, m.activeTicket
		if _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")}); !m.stuckActionPrompt {
			t.Errorf("after j: modal closed; an unhandled key must be swallowed, not dismiss")
		}
		if m.activeColumn != startCol || m.activeTicket != startTicket {
			t.Errorf("after j: selection moved (%d,%d→%d,%d); modal must swallow board keys",
				startCol, startTicket, m.activeColumn, m.activeTicket)
		}
	})
}
