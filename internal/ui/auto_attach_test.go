package ui

import (
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
			name: "ignores non-waiting and nil-StatusChangedAt waiters",
			cols: [][]*board.Ticket{
				{autoTicket("T-work", board.AgentWorking, old)},  // not waiting
				{autoTicket("T-nilts", board.AgentWaiting, nil)}, // waiting but no timestamp
				{autoTicket("T-real", board.AgentWaiting, mid)},  // the only valid waiter
				{},
			},
			livePaneIDs: []board.TicketID{"T-work", "T-nilts", "T-real"},
			want:        "T-real",
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
			name: "no waiters -> not found (off-ramp to board)",
			cols: [][]*board.Ticket{
				{autoTicket("T-work", board.AgentWorking, old)},
				{autoTicket("T-idle", board.AgentIdle, mid)},
				{},
				{},
			},
			livePaneIDs: []board.TicketID{"T-work", "T-idle"},
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

	t.Run("auto on with waiter jumps instead of board", func(t *testing.T) {
		m := build(true, true)
		model, _ := m.handleAgentViewMode(ctrlG)
		got := model.(*Model)
		if got.mode != ModeAgentView {
			t.Errorf("mode = %v, want ModeAgentView (should have jumped, not returned to board)", got.mode)
		}
		if got.focusedPane != "T-wait" {
			t.Errorf("focusedPane = %q, want \"T-wait\"", got.focusedPane)
		}
		if !got.cycleAttachPrompt {
			t.Errorf("cycleAttachPrompt = false, want true after Auto jump")
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
