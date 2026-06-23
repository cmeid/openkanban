package ui

import (
	"testing"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/project"
)

// editTextField simulates the real focus→edit→leave cycle for one text field:
// move the cursor there (seeding peInput), set a value, then write it back.
func editTextField(m *Model, field int, value string) {
	m.peField = field
	m.peSyncFromField()
	m.peInput.SetValue(value)
	m.peSyncToField()
}

// TestProjectEditForm_SavesAgentsAndPin pins the unified editor's persistence:
// editing an agent's env, toggling its enabled override, renaming the project,
// and setting the pin must persist to BOTH config.json (agents) and
// projects.json (project). A fresh registry load confirms the project bits hit
// disk. Non-vacuous: the default claude-custom env is ~/.claude-personal; the
// test changes it and asserts the new value.
func TestProjectEditForm_SavesAgentsAndPin(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	cfg := config.DefaultConfig() // seeds claude + claude-custom + others
	proj := project.NewProject("OldName", t.TempDir())
	reg := &project.ProjectRegistry{Projects: map[string]*project.Project{proj.ID: proj}}
	gs := project.NewGlobalTicketStore(nil)
	gs.AddProject(proj)

	cols := board.DefaultColumns()
	m := &Model{
		config:          cfg,
		projectRegistry: reg,
		globalStore:     gs,
		columns:         cols,
		columnTickets:   make([][]*board.Ticket, len(cols)),
		columnOffsets:   make([]int, len(cols)),
		width:           120,
		height:          40,
	}

	m.editProject(proj)

	idx := -1
	for i, r := range m.peAgents {
		if r.key == "claude-custom" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("claude-custom not present in editor rows")
	}
	base := 2 + idx*peFieldsPerAgent

	// Edit the custom agent's env (text field, via the real sync cycle).
	editTextField(m, base+peSubEnv, "CLAUDE_CONFIG_DIR=~/.claude-work")
	// Rename the project (field 0).
	editTextField(m, 0, "NewName")
	// Toggle the custom agent's enabled override to "off" (selector field).
	m.peField = base + peSubEnabled
	for m.peAgents[idx].enabled != "off" {
		m.peCycleSelector(1)
	}
	// Pin the project to the custom agent (selector field 1).
	m.peField = 1
	for m.peProjectAgent != "claude-custom" {
		m.peCycleSelector(1)
	}

	if _, cmd := m.saveProjectEditForm(); cmd != nil {
		// save returns nil cmd on success; a non-nil here would be unexpected
		_ = cmd
	}

	// config.json side: agent env + enabled override persisted in memory.
	cc := m.config.Agents["claude-custom"]
	if cc.Env["CLAUDE_CONFIG_DIR"] != "~/.claude-work" {
		t.Errorf("agent env not saved: got %v", cc.Env)
	}
	if cc.Enabled == nil || *cc.Enabled != false {
		t.Errorf("agent enabled override not saved: got %v", cc.Enabled)
	}

	// projects.json side: name + pin persisted to disk.
	reloaded, err := project.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	got := reloaded.Projects[proj.ID]
	if got == nil {
		t.Fatal("project missing after reload")
	}
	if got.Name != "NewName" {
		t.Errorf("project name not persisted: got %q", got.Name)
	}
	if got.Settings.DefaultAgent != "claude-custom" {
		t.Errorf("project pin not persisted: got %q", got.Settings.DefaultAgent)
	}
}

// TestEnabledAgentNames_FiltersByPath pins that the pin cycle only offers
// enabled agents: an explicit Enabled=&false hides one even though its command
// exists, and an Enabled=&true shows one whose command is absent.
func TestEnabledAgentNames_FiltersByPath(t *testing.T) {
	tr, fa := true, false
	m := &Model{config: &config.Config{Agents: map[string]config.AgentConfig{
		"sh-on":    {Command: "sh", Enabled: &tr},
		"sh-off":   {Command: "sh", Enabled: &fa},
		"ghost":    {Command: "definitely-not-a-real-binary-xyzzy"}, // auto → off
		"ghost-on": {Command: "definitely-not-a-real-binary-xyzzy", Enabled: &tr},
	}}}

	got := map[string]bool{}
	for _, n := range m.enabledAgentNames() {
		got[n] = true
	}
	if !got["sh-on"] || got["sh-off"] {
		t.Errorf("override not honored: %v", got)
	}
	if got["ghost"] {
		t.Errorf("auto-detect should hide a missing command: %v", got)
	}
	if !got["ghost-on"] {
		t.Errorf("force-on should show even a missing command: %v", got)
	}
}
