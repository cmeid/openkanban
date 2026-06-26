package ui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
)

// TestTicketBorderColor pins the card border-color precedence:
// stuck > selected > viewed-in-another-TUI > hovered > default. In
// particular the amber "viewed elsewhere" border must never hide a stuck
// wedge or the user's current selection.
func TestTicketBorderColor(t *testing.T) {
	m := &Model{colors: newUIColors(config.DefaultConfig().GetTheme())}
	col := lipgloss.Color("99") // a distinct stand-in for the column color

	tests := []struct {
		name            string
		status          board.AgentStatus
		isSelected      bool
		isHovered       bool
		viewedElsewhere bool
		want            lipgloss.Color
	}{
		{"plain idle", board.AgentIdle, false, false, false, m.colors.surface},
		{"hovered only", board.AgentIdle, false, true, false, m.colors.overlay},
		{"viewed elsewhere", board.AgentIdle, false, false, true, m.colors.warning},
		{"viewed beats hover", board.AgentIdle, false, true, true, m.colors.warning},
		{"selected beats viewed", board.AgentIdle, true, false, true, lipgloss.Color("15")},
		{"selected only", board.AgentIdle, true, false, false, lipgloss.Color("15")},
		{"stuck beats selected+viewed", board.AgentStuck, true, true, true, m.colors.err},
		{"stuck only", board.AgentStuck, false, false, false, m.colors.err},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := m.ticketBorderColor(tt.status, tt.isSelected, tt.isHovered, tt.viewedElsewhere, col)
			if got != tt.want {
				t.Errorf("ticketBorderColor(%v, sel=%v, hov=%v, viewed=%v) = %v, want %v",
					tt.status, tt.isSelected, tt.isHovered, tt.viewedElsewhere, got, tt.want)
			}
		})
	}
}
