package ui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemonclient"
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

// TestResolveSpawnAgent pins the core guard: agent identity comes from the
// ticket (resume) or the project pin, with NO global fallback. An unpinned
// project yields errNoProjectAgent so the caller refuses the spawn.
func TestResolveSpawnAgent(t *testing.T) {
	m := &Model{}
	pinned := &project.Project{Settings: project.ProjectSettings{DefaultAgent: "claude-custom"}}
	unpinned := &project.Project{}

	if _, err := m.resolveSpawnAgent(&board.Ticket{}, unpinned); !errors.Is(err, errNoProjectAgent) {
		t.Errorf("unpinned project: got err %v, want errNoProjectAgent", err)
	}
	if got, err := m.resolveSpawnAgent(&board.Ticket{}, pinned); err != nil || got != "claude-custom" {
		t.Errorf("pinned project: got (%q, %v), want (claude-custom, nil)", got, err)
	}
	// A ticket already carrying AgentType wins (resume continuity) even over the pin.
	if got, err := m.resolveSpawnAgent(&board.Ticket{AgentType: "claude"}, pinned); err != nil || got != "claude" {
		t.Errorf("ticket with AgentType: got (%q, %v), want (claude, nil)", got, err)
	}
}

// TestPromoteAndSpawnUnattached_UnpinnedRefuses pins the require-pin guard on
// the ctrl+space path: an unpinned project refuses to spawn AND the refusal
// precedes promotion, so the ticket is not stamped, not paned, and not moved
// out of backlog. Reverting the refuse-before-promote ordering fails the
// status assertion; reverting the resolver fails the AgentType/pane assertions.
func TestPromoteAndSpawnUnattached_UnpinnedRefuses(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()} // unpinned: no DefaultAgent
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	ticket := &board.Ticket{
		ID: "T-unpinned", Title: "x", ProjectID: "test",
		Status: board.StatusBacklog, WorktreePath: "/already/set",
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	cols := board.DefaultColumns()
	backlogIdx := columnIndexOfStatus(cols, board.StatusBacklog)
	m := &Model{
		globalStore:   globalStore,
		panes:         map[board.TicketID]*daemonclient.PaneView{},
		daemonOwned:   map[board.TicketID]struct{}{},
		columns:       cols,
		columnTickets: make([][]*board.Ticket, len(cols)),
		columnOffsets: make([]int, len(cols)),
		mode:          ModeNormal,
		width:         120,
		height:        40,
		config: &config.Config{
			Agents: map[string]config.AgentConfig{"claude": {Command: "claude"}},
		},
		selectedProject: proj,
	}
	m.refreshColumnTickets()
	m.activeColumn = backlogIdx
	m.activeTicket = 0

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlAt}) // ctrl+space

	if ticket.AgentType != "" {
		t.Errorf("unpinned project stamped AgentType = %q, want empty", ticket.AgentType)
	}
	if ticket.Status != board.StatusBacklog {
		t.Errorf("unpinned ticket moved to %q, want backlog (refusal must precede promotion)", ticket.Status)
	}
	if _, ok := m.panes[ticket.ID]; ok {
		t.Errorf("unpinned project created a pane; spawn should have been refused")
	}
}

// TestSidebarPinLineOnlyWhenUnpinned pins the sidebar's render contract: a pinned
// project shows just its name row (the agent line is noise once configured — it's
// still reachable via the e editor and the g toast), while an unpinned project
// keeps the amber "↳ unpinned · g" hint (its spawn would refuse, so the warning
// earns its row). Reverting the change re-renders the pinned label and fails the
// "must NOT contain" assertion.
func TestSidebarPinLineOnlyWhenUnpinned(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	store := project.NewGlobalTicketStore(nil)
	store.AddProject(&project.Project{
		ID: "pinned", Name: "Pinned", RepoPath: t.TempDir(),
		Settings: project.ProjectSettings{DefaultAgent: "claude"},
	})
	store.AddProject(&project.Project{
		ID: "loose", Name: "Loose", RepoPath: t.TempDir(),
	})

	m := &Model{
		globalStore:    store,
		sidebarVisible: true,
		sidebarFocused: true,
		sidebarWidth:   30,
		width:          120,
		height:         40,
		colors:         newUIColors(config.DefaultConfig().GetTheme()),
		config: &config.Config{
			Agents: map[string]config.AgentConfig{
				"claude": {Command: "claude", Label: "Claude (Default)"},
			},
		},
	}

	out := m.renderSidebar()

	// Both project rows render.
	for _, want := range []string{"Pinned", "Loose"} {
		if !strings.Contains(out, want) {
			t.Errorf("sidebar missing project row %q\n---\n%s", want, out)
		}
	}
	// The unpinned project keeps its hint.
	if !strings.Contains(out, "unpinned · g") {
		t.Errorf("sidebar dropped the unpinned hint\n---\n%s", out)
	}
	// The pinned project's agent label is NOT echoed under it.
	if strings.Contains(out, "Claude (Default)") {
		t.Errorf("sidebar still renders the pinned agent label\n---\n%s", out)
	}
}
