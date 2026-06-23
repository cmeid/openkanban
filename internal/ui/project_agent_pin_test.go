package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/project"
)

// TestCycleProjectAgent_PersistsPin pins the sidebar 'g' affordance: it advances
// the focused project's DefaultAgent through the configured agents and persists
// the change to the registry on disk. This is the ONLY surface that chooses
// agent identity (the per-project pin), so it must actually save — otherwise the
// pin evaporates on restart and the guard silently degrades.
func TestCycleProjectAgent_PersistsPin(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := project.NewProject("Proj", t.TempDir())
	if proj.Settings.DefaultAgent != "" {
		t.Fatalf("fixture: new project should start unpinned, got %q", proj.Settings.DefaultAgent)
	}

	reg := &project.ProjectRegistry{Projects: map[string]*project.Project{proj.ID: proj}}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	m := &Model{
		globalStore:     globalStore,
		projectRegistry: reg,
		config: &config.Config{
			Agents: map[string]config.AgentConfig{
				"claude":        {Command: "claude", Label: "Claude (Default)"},
				"claude-custom": {Command: "claude", Label: "Claude (Custom)"},
			},
		},
		sidebarFocused: true,
		sidebarIndex:   1, // project rows start at index 1 (0 == "All")
	}

	// getAgentNames sorts: ["claude", "claude-custom"]. First press pins names[0].
	keyG := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")}
	m.handleSidebarNav(keyG)

	if proj.Settings.DefaultAgent != "claude" {
		t.Fatalf("after first cycle DefaultAgent = %q, want %q", proj.Settings.DefaultAgent, "claude")
	}
	// Persisted to disk: a fresh load sees the pin.
	reloaded, err := project.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if got := reloaded.Projects[proj.ID]; got == nil || got.Settings.DefaultAgent != "claude" {
		t.Fatalf("pin not persisted: reloaded DefaultAgent = %v", got)
	}

	// Second press advances to the next configured agent.
	m.handleSidebarNav(keyG)
	if proj.Settings.DefaultAgent != "claude-custom" {
		t.Fatalf("after second cycle DefaultAgent = %q, want %q", proj.Settings.DefaultAgent, "claude-custom")
	}
}
