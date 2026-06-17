package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/project"
)

// seedSidebarStore builds a GlobalTicketStore with two projects spanning all
// five statuses:
//
//	test  -> 2 backlog + 1 in_progress + 1 in_review (4 open) + 1 done + 1 archived  = 6 total
//	proj2 -> 1 backlog (1 open) + 1 done                                             = 2 total
//
// So: all = 8 total / 5 open; test = 6 total / 4 open.
func seedSidebarStore(t *testing.T) *project.GlobalTicketStore {
	t.Helper()
	store := project.NewGlobalTicketStore(nil)
	store.AddProject(&project.Project{ID: "test", Name: "Test", RepoPath: t.TempDir()})
	store.AddProject(&project.Project{ID: "proj2", Name: "Proj2", RepoPath: t.TempDir()})

	add := func(projectID string, status board.TicketStatus) {
		tk := &board.Ticket{
			ID:        board.NewTicketID(),
			Title:     "t",
			ProjectID: projectID,
			Status:    status,
		}
		if err := store.Add(tk); err != nil {
			t.Fatalf("Add ticket: %v", err)
		}
	}

	add("test", board.StatusBacklog)
	add("test", board.StatusBacklog)
	add("test", board.StatusInProgress)
	add("test", board.StatusInReview)
	add("test", board.StatusDone)
	add("test", board.StatusArchived)
	add("proj2", board.StatusBacklog)
	add("proj2", board.StatusDone)

	return store
}

func TestSidebarTicketCount(t *testing.T) {
	store := seedSidebarStore(t)

	tests := []struct {
		name      string
		projectID string
		openOnly  bool
		want      int
	}{
		{"all, all statuses", "", false, 8},
		{"all, open only", "", true, 5},
		{"test project, all statuses", "test", false, 6},
		{"test project, open only", "test", true, 4},
		{"proj2, all statuses", "proj2", false, 2},
		{"proj2, open only", "proj2", true, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Model{globalStore: store, sidebarOpenOnly: tt.openOnly}
			if got := m.sidebarTicketCount(tt.projectID); got != tt.want {
				t.Errorf("sidebarTicketCount(%q) with openOnly=%v = %d, want %d",
					tt.projectID, tt.openOnly, got, tt.want)
			}
		})
	}
}

// TestSidebarOpenOnlyToggle traverses the full propagation path: the "o" key
// flows through handleSidebarNav (the real handler), flipping the in-memory
// field, which renderSidebar then reads to filter the displayed counts.
func TestSidebarOpenOnlyToggle(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	m := &Model{
		globalStore:    seedSidebarStore(t),
		sidebarVisible: true,
		sidebarFocused: true,
		sidebarWidth:   30,
		width:          120,
		height:         40,
		colors:         newUIColors(config.DefaultConfig().GetTheme()),
	}

	// Before toggling: counts show all tickets, no "(open)" indicator.
	out := m.renderSidebar()
	for _, want := range []string{"All (8)", "Test (6)", "Proj2 (2)"} {
		if !strings.Contains(out, want) {
			t.Errorf("pre-toggle sidebar missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "Projects (open)") {
		t.Errorf("pre-toggle sidebar should not show open indicator\n---\n%s", out)
	}

	// Press "o" while the sidebar is focused.
	if _, _ = m.handleSidebarNav(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")}); !m.sidebarOpenOnly {
		t.Fatalf("sidebarOpenOnly = false after pressing 'o', want true")
	}

	// After toggling: counts exclude done+archived, indicator present.
	out = m.renderSidebar()
	for _, want := range []string{"Projects (open)", "All (5)", "Test (4)", "Proj2 (1)"} {
		if !strings.Contains(out, want) {
			t.Errorf("post-toggle sidebar missing %q\n---\n%s", want, out)
		}
	}

	// Pressing "o" again restores the all-tickets view.
	if _, _ = m.handleSidebarNav(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")}); m.sidebarOpenOnly {
		t.Fatalf("sidebarOpenOnly = true after second 'o', want false")
	}
}
