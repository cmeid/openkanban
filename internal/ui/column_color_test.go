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
// column color (which drives the column header and active-column border) must
// never equal m.colors.warning, the amber used for the "viewed in another TUI"
// border — otherwise those surfaces would collide as they originally did.
// Note: the selected-card border is now a static white (ANSI 15) and is no
// longer driven by columnColor, so the per-column color no longer affects it.
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

	// Invariant: in_progress column color (drives column header + active-column
	// border) must differ from the viewed-elsewhere border (warning). The
	// selected-card border is now static white and is no longer driven by
	// columnColor, but this invariant still holds for the surfaces that are.
	if m.columnColor(board.StatusInProgress) == m.colors.warning {
		t.Errorf("in_progress column color must not equal warning (the viewed-elsewhere border) — collision regressed")
	}
}
