package ui

import (
	"testing"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
)

// TestColumnColorScheme pins the column→color mapping after the
// "in_progress border must differ from the viewed-elsewhere border" recolor.
// in_progress is green (success), backlog is the quiet neutral (overlay), and
// done is grey (muted). The key invariant is the last case: the in_progress
// column color (which becomes the selected-card border) must never again equal
// m.colors.warning, the amber used for the "viewed in another TUI" border —
// otherwise the two borders collide as they originally did.
func TestColumnColorScheme(t *testing.T) {
	m := &Model{colors: newUIColors(config.DefaultConfig().GetTheme())}

	tests := []struct {
		name   string
		status board.TicketStatus
		want   string // field name on uiColors, for the error message
		got    func() bool
	}{
		{"backlog → overlay", board.StatusBacklog, "overlay",
			func() bool { return m.columnColor(board.StatusBacklog) == m.colors.overlay }},
		{"next → info", board.StatusNext, "info",
			func() bool { return m.columnColor(board.StatusNext) == m.colors.info }},
		{"in_progress → success", board.StatusInProgress, "success",
			func() bool { return m.columnColor(board.StatusInProgress) == m.colors.success }},
		{"in_review → secondary", board.StatusInReview, "secondary",
			func() bool { return m.columnColor(board.StatusInReview) == m.colors.secondary }},
		{"done → muted", board.StatusDone, "muted",
			func() bool { return m.columnColor(board.StatusDone) == m.colors.muted }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.got() {
				t.Errorf("columnColor(%q) = %v, want %s", tt.status, m.columnColor(tt.status), tt.want)
			}
		})
	}

	// Invariant: in_progress (selected-card border) must differ from the
	// viewed-elsewhere border (warning). This is the ticket's acceptance
	// criterion expressed as a regression guard.
	if m.columnColor(board.StatusInProgress) == m.colors.warning {
		t.Errorf("in_progress column color must not equal warning (the viewed-elsewhere border) — collision regressed")
	}
}
