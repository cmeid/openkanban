package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

// writeAliveJSONL is the IsClaudeSessionDead-passing counterpart to
// writeDeadJSONL in dead_session_daemon_test.go: it emits a single
// assistant event with real, user-visible text so the alive-content
// scan in jsonlHasRealAssistantContent returns true. Used by the
// pull-back chooser tests, which need shouldCleanupDeadSession to
// fall through to offerChooser=true via the IsClaudeSessionDead arm
// (the Owns probe is skipped when AgentSessionID is empty).
func writeAliveJSONL(t *testing.T, homeDir, worktree, uuid string) string {
	t.Helper()
	dir := filepath.Join(homeDir, ".claude", "projects", encodeWorktreeForTest(worktree))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, uuid+".jsonl")
	events := []map[string]any{
		{
			"type": "assistant",
			"message": map[string]any{
				"role":    "assistant",
				"content": "Real assistant work happened here.",
			},
		},
	}
	var sb strings.Builder
	for _, ev := range events {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		sb.Write(b)
		sb.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// pulledBackFixture builds a Model + Ticket pair where the ticket has
// previously been spawned (AgentSpawnedAt set) and the user has since
// moved the ticket's status (StatusChangedAt set later) — the explicit
// pull-back gesture spawnAgent should now detect. Description is empty
// so PreviewBriefMerge reports wouldChange=false; the chooser must
// therefore fire on the pulledBack signal alone, not on a brief change.
func pulledBackFixture(t *testing.T, statusChangedDelta time.Duration) (*Model, *board.Ticket, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	worktree := filepath.Join(home, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	uuid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	writeAliveJSONL(t, home, worktree, uuid)

	proj := &project.Project{ID: "pb-proj", RepoPath: home}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	spawnedAt := time.Now().Add(-time.Hour).Round(time.Millisecond)
	statusChanged := spawnedAt.Add(statusChangedDelta).Round(time.Millisecond)

	ticket := &board.Ticket{
		ID:              "T-PB-1",
		Title:           "pull-back chooser fixture",
		ProjectID:       proj.ID,
		Status:          board.StatusInProgress,
		WorktreePath:    worktree,
		BranchName:      "task/pull-back-fixture",
		AgentType:       "claude",
		AgentSpawnedAt:  &spawnedAt,
		StatusChangedAt: &statusChanged,
		Description:     "", // empty → PreviewBriefMerge wouldChange=false
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
			Agents: map[string]config.AgentConfig{
				"claude": {Command: "claude"},
			},
		},
	}
	return m, ticket, home
}

// TestSpawnAgent_PulledBack_FiresChooser_EvenWithUnchangedBrief pins
// the new trigger condition: when the user pulls a ticket back into
// in_progress AFTER its prior session ran (StatusChangedAt >
// AgentSpawnedAt), the brief-chooser modal must fire — even if the
// brief itself hasn't changed. The chooser's "Resume prior session or
// start fresh?" question is exactly what the pull-back gesture is
// asking for.
//
// Negative-control coverage lives in
// TestSpawnAgent_NotPulledBack_NoChooserOnUnchangedBrief.
func TestSpawnAgent_PulledBack_FiresChooser_EvenWithUnchangedBrief(t *testing.T) {
	// StatusChangedAt = AgentSpawnedAt + 1h → strictly after → pulledBack=true
	m, _, _ := pulledBackFixture(t, time.Hour)

	_, _ = m.spawnAgent()

	if !m.showChoice {
		t.Fatalf("showChoice = false, want true (pulled-back ticket must fire the chooser)")
	}
	if !strings.Contains(m.choiceMsg, "pulled back") {
		t.Errorf("choiceMsg = %q, want it to mention \"pulled back\"", m.choiceMsg)
	}
	if len(m.choices) != 3 {
		t.Errorf("choices = %d, want 3 (d/u/n)", len(m.choices))
	}
}

// TestPullBackChooser_EscDismisses pins that Esc cancels the pull-back
// chooser modal. The modal is shown via m.showChoice while m.mode stays
// ModeNormal, so the global Esc arm in handleKey (which runs before the
// showChoice dispatch) must route to handleChoice rather than swallowing
// the keystroke and leaving the modal up. Regression guard: previously
// the ModeNormal Esc branch reset mode/help/confirm but never cleared
// showChoice, so Esc looked dead while the chooser was open.
func TestPullBackChooser_EscDismisses(t *testing.T) {
	m, _, _ := pulledBackFixture(t, time.Hour)

	if _, _ = m.spawnAgent(); !m.showChoice {
		t.Fatalf("precondition: showChoice = false, want true (chooser must be open before Esc)")
	}

	if _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc}); m.showChoice {
		t.Errorf("after Esc: showChoice = true, want false (Esc must dismiss the chooser)")
	}
	if m.choices != nil {
		t.Errorf("after Esc: choices = %v, want nil", m.choices)
	}
	if m.choiceMsg != "" {
		t.Errorf("after Esc: choiceMsg = %q, want empty", m.choiceMsg)
	}
}

// TestSpawnAgent_NotPulledBack_NoChooserOnUnchangedBrief pins the
// negative half: when the prior session was the most recent status
// transition (StatusChangedAt <= AgentSpawnedAt), the chooser must NOT
// fire on an empty brief. This is the routine re-attach case (the user
// Ctrl+g'd back to the board and is re-entering the same in_progress
// session) — an unsolicited modal here would be friction without
// signal.
func TestSpawnAgent_NotPulledBack_NoChooserOnUnchangedBrief(t *testing.T) {
	// StatusChangedAt = AgentSpawnedAt - 1h → strictly before → pulledBack=false
	m, _, _ := pulledBackFixture(t, -time.Hour)

	// Without a daemon stub the spawn path would attempt a real Spawn
	// RPC — to avoid that, we assert on m.showChoice immediately. The
	// chooser branch returns early when it fires, so if showChoice is
	// false we KNOW the code fell through to the prepareSpawnWith
	// closure (which spawns a goroutine but the test doesn't await it).
	_, _ = m.spawnAgent()

	if m.showChoice {
		t.Errorf("showChoice = true, want false (no pull-back, no brief change → chooser should not fire). msg=%q", m.choiceMsg)
	}
}
