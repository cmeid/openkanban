package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/lipgloss"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

// TestEmpiricalRenderHeights prints lipgloss.Height for short / long /
// joined cards so the packing-loop math can be locked to measured numbers
// rather than theoretical ones. Kept as a regression guard.
func TestEmpiricalRenderHeights(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	m := &Model{
		globalStore:    globalStore,
		panes:          map[board.TicketID]*daemonclient.PaneView{},
		daemonViewing: map[board.TicketID]int{},
		columns:        board.DefaultColumns(),
		spinner:        sp,
		width:          120,
		height:         40,
		config:         &config.Config{Agents: map[string]config.AgentConfig{}},
		colors:         newUIColors(config.DefaultConfig().GetTheme()),
	}

	mk := func(title, desc string, labels []string) *board.Ticket {
		tk := &board.Ticket{
			ID:          board.NewTicketID(),
			Title:       title,
			Description: desc,
			Status:      board.StatusBacklog,
			Priority:    3,
			Labels:      labels,
			AgentStatus: board.AgentIdle,
			ProjectID:   proj.ID,
		}
		_ = globalStore.Add(tk)
		return tk
	}

	const cardWidth = 36 // inside a column ~40 wide (column passes width-4)

	short := mk("short title", "a brief description", []string{"bug"})
	long := mk(strings.Repeat("verylongword ", 12), "a brief description", []string{"bug"})
	huge := mk(strings.Repeat("x", 100), "a brief description", []string{"bug"})

	shortView := m.renderTicket(short, false, false, cardWidth, m.colors.primary, 1, 1)
	longView := m.renderTicket(long, false, false, cardWidth, m.colors.primary, 1, 1)
	hugeView := m.renderTicket(huge, false, false, cardWidth, m.colors.primary, 1, 1)

	t.Logf("(a) short-title card height = %d", lipgloss.Height(shortView))
	t.Logf("(b) long-title card height  = %d", lipgloss.Height(longView))
	t.Logf("(b') 100-rune card height   = %d", lipgloss.Height(hugeView))

	c1 := m.renderTicket(mk("c1", "d", nil), false, false, cardWidth, m.colors.primary, 1, 3)
	c2 := m.renderTicket(mk("c2", "d", nil), false, false, cardWidth, m.colors.primary, 2, 3)
	c3 := m.renderTicket(mk("c3", "d", nil), false, false, cardWidth, m.colors.primary, 3, 3)
	sum := lipgloss.Height(c1) + lipgloss.Height(c2) + lipgloss.Height(c3)
	joined := strings.Join([]string{c1, c2, c3}, "\n")
	t.Logf("(c) sum=%d  joined=%d  delta=%d", sum, lipgloss.Height(joined), lipgloss.Height(joined)-sum)

	// Mixed: 2 short + 1 long
	lc := m.renderTicket(mk(strings.Repeat("y", 80), "d", nil), false, false, cardWidth, m.colors.primary, 2, 3)
	mixedSum := lipgloss.Height(c1) + lipgloss.Height(lc) + lipgloss.Height(c2)
	mixedJoined := strings.Join([]string{c1, lc, c2}, "\n")
	t.Logf("(d) mixed: short=%d long=%d short=%d sum=%d joined=%d",
		lipgloss.Height(c1), lipgloss.Height(lc), lipgloss.Height(c2), mixedSum, lipgloss.Height(mixedJoined))
}
