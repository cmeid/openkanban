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
// brief-chooser tests, which need shouldCleanupDeadSession to
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

// oldSessionFixture builds a Model + Ticket pair for an already-spawned
// ticket (AgentSpawnedAt set) whose StatusChangedAt is offset from spawn
// by statusChangedDelta. A positive delta models the real world: any live
// session bumps StatusChangedAt past AgentSpawnedAt every time the status
// poll flips AgentStatus working↔waiting (SetAgentStatus stamps
// StatusChangedAt — board.go). Description is empty and no brief file is
// written, so PreviewBriefMerge reports wouldChange=false; the chooser
// must therefore stay closed regardless of the delta, since it now fires
// only on a genuine brief change.
func oldSessionFixture(t *testing.T, statusChangedDelta time.Duration) (*Model, *board.Ticket, string) {
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

// TestSpawnAgent_OldSession_StatusBumpedAfterSpawn_NoChooserWhenBriefUnchanged
// is the regression test for "old sessions always ask to update the
// brief." An old session's StatusChangedAt is pushed past AgentSpawnedAt
// by ordinary agent activity (SetAgentStatus stamps StatusChangedAt on
// every working↔waiting flip). The brief-chooser must NOT fire on that
// alone — only a genuine brief change may open it. Before the fix the
// gate also fired on StatusChangedAt > AgentSpawnedAt, so this case (the
// common one for any session that did work) popped the modal every
// re-spawn.
func TestSpawnAgent_OldSession_StatusBumpedAfterSpawn_NoChooserWhenBriefUnchanged(t *testing.T) {
	// StatusChangedAt = AgentSpawnedAt + 1h — the state SetAgentStatus
	// produces after any in-session status transition. Brief unchanged
	// (empty Description, no brief file) → wouldChange=false.
	m, _, _ := oldSessionFixture(t, time.Hour)

	_, _ = m.spawnAgent()

	if m.showChoice {
		t.Errorf("showChoice = true, want false (status bumped after spawn but brief unchanged → chooser must stay closed). msg=%q", m.choiceMsg)
	}
}

// TestSpawnAgent_BriefChanged_FiresChooser pins the positive trigger:
// when the card description has diverged from the on-disk brief
// (wouldChange=true), the chooser fires with the brief-change message —
// and must NOT mention "pulled back" (the removed signal).
func TestSpawnAgent_BriefChanged_FiresChooser(t *testing.T) {
	m, ticket, _ := oldSessionFixture(t, time.Hour)
	// Non-empty Description with no brief file on disk →
	// PreviewBriefMerge case (fileAbsent && desc != "") → wouldChange=true.
	ticket.Description = "the card description changed since this session started"

	_, _ = m.spawnAgent()

	if !m.showChoice {
		t.Fatalf("showChoice = false, want true (brief changed → chooser must fire)")
	}
	if !strings.Contains(m.choiceMsg, "Brief was updated") {
		t.Errorf("choiceMsg = %q, want it to mention \"Brief was updated\"", m.choiceMsg)
	}
	if strings.Contains(m.choiceMsg, "pulled back") {
		t.Errorf("choiceMsg = %q, must not mention \"pulled back\" (signal removed)", m.choiceMsg)
	}
	if len(m.choices) != 3 {
		t.Errorf("choices = %d, want 3 (d/u/n)", len(m.choices))
	}
}

// TestBriefChooser_EscDismisses pins that Esc cancels the brief-chooser
// modal. The modal is shown via m.showChoice while m.mode stays
// ModeNormal, so the global Esc arm in handleKey (which runs before the
// showChoice dispatch) must route to handleChoice rather than swallowing
// the keystroke and leaving the modal up. Regression guard: previously
// the ModeNormal Esc branch reset mode/help/confirm but never cleared
// showChoice, so Esc looked dead while the chooser was open. The chooser
// is opened here via a genuine brief change (wouldChange=true).
func TestBriefChooser_EscDismisses(t *testing.T) {
	m, ticket, _ := oldSessionFixture(t, time.Hour)
	ticket.Description = "brief diverged, open the chooser"

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

// TestSpawnAgent_UnchangedBrief_NoChooser pins the negative half from the
// other direction: a prior status transition BEFORE spawn, brief
// unchanged → no chooser. Together with the status-bumped-after-spawn
// case above, this documents that the StatusChangedAt/AgentSpawnedAt
// ordering no longer influences the gate at all — only wouldChange does.
func TestSpawnAgent_UnchangedBrief_NoChooser(t *testing.T) {
	m, _, _ := oldSessionFixture(t, -time.Hour)

	// Without a daemon stub the spawn path would attempt a real Spawn
	// RPC — to avoid that, we assert on m.showChoice immediately. The
	// chooser branch returns early when it fires, so if showChoice is
	// false we KNOW the code fell through to the prepareSpawnWith
	// closure (which spawns a goroutine but the test doesn't await it).
	_, _ = m.spawnAgent()

	if m.showChoice {
		t.Errorf("showChoice = true, want false (no brief change → chooser should not fire). msg=%q", m.choiceMsg)
	}
}

// TestBriefChooser_Discard_ClearsSessionLink pins the second half of the
// ForceFresh contract. Suppressing --resume at argv-build time is not
// enough: the ticket's AgentSessionID is the durable resume key, and the
// poll loop's back-fill only claims a UUID for a ticket that has none
// (backfillAgentSession is gated on an empty field). Leave the discarded
// UUID in place and the NEW session never gets linked — so the next
// spawn resumes the very conversation the user threw away, and option
// 'd' silently works exactly once.
//
// The closure is invoked directly; the tea.Cmd it returns (which is what
// performs the actual daemon Spawn) is deliberately not run.
//
// RED-BEFORE-GREEN: drop the ticketsvc.UnlinkSession call from the 'd'
// closure and AgentSessionID survives the discard.
func TestBriefChooser_Discard_ClearsSessionLink(t *testing.T) {
	m, ticket, _ := oldSessionFixture(t, time.Hour)
	ticket.Description = "brief diverged, open the chooser"
	const uuid = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	ticket.AgentSessionID = uuid

	if _, _ = m.spawnAgent(); !m.showChoice {
		t.Fatalf("precondition: chooser did not open, so the 'd' closure under test is unreachable")
	}
	var discard *choiceItem
	for i := range m.choices {
		if m.choices[i].Key == 'd' {
			discard = &m.choices[i]
			break
		}
	}
	if discard == nil {
		t.Fatalf("no 'd' (discard) choice among %d options", len(m.choices))
	}

	_ = discard.Fn()

	if ticket.AgentSessionID != "" {
		t.Errorf("AgentSessionID = %q, want empty — discarding a session must unlink it so the "+
			"poll loop can back-fill the new session's UUID", ticket.AgentSessionID)
	}
	if ticket.AgentSpawnedAt != nil {
		t.Errorf("AgentSpawnedAt = %v, want nil (the fresh-spawn branch is gated on it)", ticket.AgentSpawnedAt)
	}
}
