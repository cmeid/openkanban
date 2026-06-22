package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

// waitingAt returns a *time.Time with the monotonic clock stripped, so
// Before() comparisons and any future round-trip assertions behave
// deterministically (see [[reference_openkanban_monotonic_time_fixture_trap]]).
func waitingAt(base time.Time, d time.Duration) *time.Time {
	t := base.Add(d).Round(0)
	return &t
}

// autoTicket builds a ticket carrying an AgentStatus + StatusChangedAt.
// oldestWaitingPeer reads these straight off the columnTickets pointer
// (the same object the poll mutates), so the fixture does not need a
// populated globalStore.
func autoTicket(id board.TicketID, status board.AgentStatus, changed *time.Time) *board.Ticket {
	return &board.Ticket{ID: id, AgentStatus: status, StatusChangedAt: changed}
}

// newAutoTestModel mirrors newCycleTestModel but lets each ticket carry a
// real AgentStatus/StatusChangedAt and lets the caller pick which IDs get
// a live (Unattached) pane. Tickets without a pane, or with a Detached
// pane, are deliberately not candidates.
func newAutoTestModel(cols [][]*board.Ticket, livePaneIDs []board.TicketID) *Model {
	panes := map[board.TicketID]*daemonclient.PaneView{}
	for _, id := range livePaneIDs {
		info := &daemon.SessionInfo{
			SessionID: "sid-" + string(id),
			TicketID:  string(id),
			Running:   true, // flips fresh PaneView Detached -> Unattached
			Cols:      80,
			Rows:      24,
		}
		panes[id] = daemonclient.NewPaneView(nil, string(id), info.SessionID, info)
	}
	return &Model{
		globalStore:   project.NewGlobalTicketStore(nil),
		panes:         panes,
		columnTickets: cols,
		columnOffsets: []int{0, 0, 0, 0},
		mode:          ModeAgentView,
		width:         120,
		height:        40,
		config:        &config.Config{Agents: map[string]config.AgentConfig{}},
	}
}

func TestOldestWaitingPeer(t *testing.T) {
	base := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	old := waitingAt(base, 0)             // entered waiting first (oldest)
	mid := waitingAt(base, 5*time.Minute) // entered waiting later
	newer := waitingAt(base, 9*time.Minute)

	tests := []struct {
		name         string
		cols         [][]*board.Ticket
		livePaneIDs  []board.TicketID
		focused      board.TicketID
		attachedElse map[board.TicketID]bool
		want         board.TicketID
		wantOK       bool
	}{
		{
			// Anti-vacuous: the WRONG answers sit at board index 0 and 1.
			// T-work (a working session) is first, T-new (newer waiter) is
			// second, T-old (oldest waiter) is LAST. A naive "first
			// candidate in board order" returns T-new; only true FIFO-by-
			// timestamp returns T-old. Asserting T-old proves the ranking.
			name: "picks oldest waiter not first-in-board-order",
			cols: [][]*board.Ticket{
				{autoTicket("T-work", board.AgentWorking, waitingAt(base, -time.Hour))},
				{autoTicket("T-new", board.AgentWaiting, newer)},
				{autoTicket("T-old", board.AgentWaiting, old)},
				{},
			},
			livePaneIDs: []board.TicketID{"T-work", "T-new", "T-old"},
			want:        "T-old",
			wantOK:      true,
		},
		{
			name: "excludes the focused (just-left) session even if oldest",
			cols: [][]*board.Ticket{
				{autoTicket("T-old", board.AgentWaiting, old)},
				{autoTicket("T-mid", board.AgentWaiting, mid)},
				{},
				{},
			},
			livePaneIDs: []board.TicketID{"T-old", "T-mid"},
			focused:     "T-old",
			want:        "T-mid",
			wantOK:      true,
		},
		{
			name: "skips sessions attached by another TUI",
			cols: [][]*board.Ticket{
				{autoTicket("T-old", board.AgentWaiting, old)},
				{autoTicket("T-mid", board.AgentWaiting, mid)},
				{},
				{},
			},
			livePaneIDs:  []board.TicketID{"T-old", "T-mid"},
			attachedElse: map[board.TicketID]bool{"T-old": true},
			want:         "T-mid",
			wantOK:       true,
		},
		{
			name: "ignores working/none and nil-StatusChangedAt sessions",
			cols: [][]*board.Ticket{
				{autoTicket("T-work", board.AgentWorking, old), autoTicket("T-none", board.AgentNone, old)}, // not needs-attention
				{autoTicket("T-nilts", board.AgentWaiting, nil)},                                            // waiting but no timestamp
				{autoTicket("T-real", board.AgentIdle, mid)},                                                // the only valid needs-attention session
				{},
			},
			livePaneIDs: []board.TicketID{"T-work", "T-none", "T-nilts", "T-real"},
			want:        "T-real",
			wantOK:      true,
		},
		{
			name: "idle counts as needs-attention (agent at rest, not working)",
			cols: [][]*board.Ticket{
				{autoTicket("T-work", board.AgentWorking, old)}, // excluded (active)
				{autoTicket("T-idle", board.AgentIdle, mid)},    // the only needs-attention session
				{},
				{},
			},
			livePaneIDs: []board.TicketID{"T-work", "T-idle"},
			want:        "T-idle",
			wantOK:      true,
		},
		{
			// idle and waiting are pooled and ranked together by StatusChangedAt:
			// the OLDER idle beats the NEWER waiting. Anti-vacuous — if the code
			// still preferred waiting, or didn't pool idle, this would return the
			// waiter (or nothing).
			name: "idle and waiting ranked together, oldest wins",
			cols: [][]*board.Ticket{
				{autoTicket("T-wait-new", board.AgentWaiting, newer)}, // newer waiter
				{autoTicket("T-idle-old", board.AgentIdle, old)},      // older idle -> FIFO winner
				{},
				{},
			},
			livePaneIDs: []board.TicketID{"T-wait-new", "T-idle-old"},
			want:        "T-idle-old",
			wantOK:      true,
		},
		{
			name: "stuck counts as needs-attention (daemon-wedged pane)",
			cols: [][]*board.Ticket{
				{autoTicket("T-work", board.AgentWorking, old)}, // excluded (active)
				{autoTicket("T-stuck", board.AgentStuck, mid)},  // wedged -> needs attention
				{},
				{},
			},
			livePaneIDs: []board.TicketID{"T-work", "T-stuck"},
			want:        "T-stuck",
			wantOK:      true,
		},
		{
			name: "requires a live pane (waiter with no pane is skipped)",
			cols: [][]*board.Ticket{
				{autoTicket("T-nopane", board.AgentWaiting, old)}, // older but no pane
				{autoTicket("T-pane", board.AgentWaiting, mid)},
				{},
				{},
			},
			livePaneIDs: []board.TicketID{"T-pane"},
			want:        "T-pane",
			wantOK:      true,
		},
		{
			name: "no needs-attention sessions -> not found (off-ramp to board)",
			cols: [][]*board.Ticket{
				{autoTicket("T-work", board.AgentWorking, old)},
				{autoTicket("T-none", board.AgentNone, mid)},
				{},
				{},
			},
			livePaneIDs: []board.TicketID{"T-work", "T-none"},
			wantOK:      false,
		},
		{
			// Equal StatusChangedAt -> board order decides. T-second
			// appears in a later column, so even though its timestamp ties
			// T-first, the board-order-first one wins. columnTickets is
			// seeded directly (no implicit refresh) so the order is exact.
			name: "tie on timestamp resolves to board order",
			cols: [][]*board.Ticket{
				{autoTicket("T-first", board.AgentWaiting, mid)},
				{autoTicket("T-second", board.AgentWaiting, mid)},
				{},
				{},
			},
			livePaneIDs: []board.TicketID{"T-first", "T-second"},
			want:        "T-first",
			wantOK:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newAutoTestModel(tt.cols, tt.livePaneIDs)
			m.focusedPane = tt.focused
			got, ok := m.oldestWaitingPeer(tt.attachedElse)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (got id %q)", ok, tt.wantOK, got)
			}
			if ok && got != tt.want {
				t.Errorf("oldestWaitingPeer = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAutoAttach_CtrlGBranch drives the real Ctrl+G path: the keystroke
// goes through pane.HandleKey (which returns ExitFocusMsg) into
// handleAgentViewMode's branch, rather than seeding the post-branch state
// directly (see [[feedback_test_must_traverse_propagation_path]]).
func TestAutoAttach_CtrlGBranch(t *testing.T) {
	base := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)

	build := func(autoOn bool, withWaiter bool) *Model {
		cur := autoTicket("T-cur", board.AgentWorking, waitingAt(base, time.Minute))
		cols := [][]*board.Ticket{{cur}, {}, {}, {}}
		live := []board.TicketID{"T-cur"}
		if withWaiter {
			cols[1] = []*board.Ticket{autoTicket("T-wait", board.AgentWaiting, waitingAt(base, 0))}
			live = append(live, "T-wait")
		}
		m := newAutoTestModel(cols, live)
		m.focusedPane = "T-cur"
		m.autoAttach = autoOn
		return m
	}

	ctrlG := tea.KeyMsg{Type: tea.KeyCtrlG}

	t.Run("auto on with waiter jumps and attaches directly", func(t *testing.T) {
		m := build(true, true)
		model, _ := m.handleAgentViewMode(ctrlG)
		got := model.(*Model)
		if got.mode != ModeAgentView {
			t.Errorf("mode = %v, want ModeAgentView (should have jumped, not returned to board)", got.mode)
		}
		if got.focusedPane != "T-wait" {
			t.Errorf("focusedPane = %q, want \"T-wait\"", got.focusedPane)
		}
		// Direct attach (no preview modal): cycleAttachPrompt must NOT be set.
		if got.cycleAttachPrompt {
			t.Errorf("cycleAttachPrompt = true, want false (Auto attaches directly, no modal)")
		}
	})

	t.Run("auto on but no waiter falls through to board", func(t *testing.T) {
		m := build(true, false)
		model, _ := m.handleAgentViewMode(ctrlG)
		got := model.(*Model)
		if got.mode != ModeNormal {
			t.Errorf("mode = %v, want ModeNormal (off-ramp when nothing waits)", got.mode)
		}
		if got.focusedPane != "" {
			t.Errorf("focusedPane = %q, want \"\"", got.focusedPane)
		}
	})

	t.Run("auto off returns to board even with a waiter", func(t *testing.T) {
		m := build(false, true)
		model, _ := m.handleAgentViewMode(ctrlG)
		got := model.(*Model)
		if got.mode != ModeNormal {
			t.Errorf("mode = %v, want ModeNormal (Auto off = unchanged behavior)", got.mode)
		}
		if got.focusedPane != "" {
			t.Errorf("focusedPane = %q, want \"\"", got.focusedPane)
		}
	})
}

// TestToggleAutoAttach confirms the board toggle flips the flag and
// notifies, and that the agent-view Ctrl+G path reflects the flag.
func TestToggleAutoAttach(t *testing.T) {
	m := newAutoTestModel([][]*board.Ticket{{}, {}, {}, {}}, nil)
	m.mode = ModeNormal

	if m.autoAttach {
		t.Fatal("autoAttach should default to false")
	}
	model, _ := m.toggleAutoAttach()
	got := model.(*Model)
	if !got.autoAttach {
		t.Errorf("autoAttach = false after toggle, want true")
	}
	if got.notification != "Auto-attach: on" {
		t.Errorf("notification = %q, want \"Auto-attach: on\"", got.notification)
	}
	model, _ = got.toggleAutoAttach()
	got = model.(*Model)
	if got.autoAttach {
		t.Errorf("autoAttach = true after second toggle, want false")
	}
}

// TestAttachedElsewhereSet covers the pure filter Auto mode uses to skip
// sessions a sibling TUI holds: only sessions whose AttachedClient is set AND
// differs from our own client ID count as "elsewhere".
func TestAttachedElsewhereSet(t *testing.T) {
	const me uint16 = 7
	sessions := []daemon.SessionInfo{
		{TicketID: "T-free", AttachedClient: 0},    // nobody attached
		{TicketID: "T-mine", AttachedClient: me},   // this TUI
		{TicketID: "T-other", AttachedClient: 42},  // a sibling TUI
		{TicketID: "T-other2", AttachedClient: 99}, // another sibling
	}
	got := attachedElsewhereSet(sessions, me)

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (T-other, T-other2); got %v", len(got), got)
	}
	for _, id := range []board.TicketID{"T-other", "T-other2"} {
		if !got[id] {
			t.Errorf("%q should be in the elsewhere set", id)
		}
	}
	for _, id := range []board.TicketID{"T-free", "T-mine"} {
		if got[id] {
			t.Errorf("%q must NOT be in the elsewhere set", id)
		}
	}

	if s := attachedElsewhereSet(nil, me); len(s) != 0 {
		t.Errorf("nil sessions -> empty set, got %v", s)
	}
}

// TestRenderAgentView_AutoBadge covers the in-session Auto indicator. The agent
// view does NOT render contextualHints (the footer), so the Auto state must be
// surfaced in renderAgentView's own chrome: an AUTO badge + a Ctrl+g hint that
// reads "Next waiter" when armed, "Board" when not.
func TestRenderAgentView_AutoBadge(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	build := func(autoOn bool) *Model {
		proj := &project.Project{ID: "test-proj", Name: "TestProj", RepoPath: t.TempDir()}
		gs := project.NewGlobalTicketStore(nil)
		gs.AddProject(proj)
		tk := &board.Ticket{ID: "T-focus", Title: "FocusTicket", Status: board.StatusInProgress, ProjectID: proj.ID}
		if err := gs.Add(tk); err != nil {
			t.Fatalf("Add: %v", err)
		}
		info := &daemon.SessionInfo{SessionID: "sid-T-focus", TicketID: "T-focus", Running: true, Cols: 80, Rows: 24}
		pv := daemonclient.NewPaneView(nil, "T-focus", info.SessionID, info)
		return &Model{
			globalStore:   gs,
			panes:         map[board.TicketID]*daemonclient.PaneView{"T-focus": pv},
			daemonViewing: map[board.TicketID]int{},
			columns:       board.DefaultColumns(),
			columnTickets: [][]*board.Ticket{{tk}, {}, {}, {}},
			columnOffsets: []int{0, 0, 0, 0},
			width:         120,
			height:        40,
			mode:          ModeAgentView,
			focusedPane:   "T-focus",
			autoAttach:    autoOn,
			config:        &config.Config{Agents: map[string]config.AgentConfig{}},
			colors:        newUIColors(config.DefaultConfig().GetTheme()),
		}
	}

	t.Run("armed: AUTO badge + 'Next waiter' hint", func(t *testing.T) {
		out := build(true).renderAgentView()
		if !strings.Contains(out, "AUTO") {
			t.Errorf("expected AUTO badge in agent view when armed; got:\n%s", out)
		}
		if !strings.Contains(out, "Next waiter") {
			t.Errorf("expected Ctrl+g hint 'Next waiter' when armed; got:\n%s", out)
		}
	})

	t.Run("off: no AUTO badge, Ctrl+g reads 'Board'", func(t *testing.T) {
		out := build(false).renderAgentView()
		if strings.Contains(out, "AUTO") {
			t.Errorf("AUTO badge must not appear when off; got:\n%s", out)
		}
		if !strings.Contains(out, "Board") {
			t.Errorf("expected Ctrl+g hint 'Board' when off; got:\n%s", out)
		}
	})
}
