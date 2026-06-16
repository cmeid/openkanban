package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

// newCycleTestModel constructs a minimal Model with the supplied
// tickets laid out across columns and a parallel set of PaneViews —
// one per ticket whose ID appears in unattachedIDs, each in
// PaneViewUnattached state (the only state cycleUnattachedSession
// considers). Tickets whose IDs do not appear in unattachedIDs are
// rendered into columnTickets but get no pane, which mirrors the
// real-world board case where most tickets have no live session.
func newCycleTestModel(t *testing.T, columns [][]*board.Ticket, unattachedIDs []board.TicketID) *Model {
	t.Helper()
	panes := map[board.TicketID]*daemonclient.PaneView{}
	for _, id := range unattachedIDs {
		// info.Running=true is the daemonclient signal that flips a
		// fresh PaneView from Detached to Unattached without needing
		// a real network attach. See NewPaneView in
		// internal/daemonclient/paneview.go for the exact branch.
		info := &daemon.SessionInfo{
			SessionID: "sid-" + string(id),
			TicketID:  string(id),
			Running:   true,
			Cols:      80,
			Rows:      24,
		}
		panes[id] = daemonclient.NewPaneView(nil, string(id), info.SessionID, info)
	}
	return &Model{
		globalStore:   project.NewGlobalTicketStore(nil),
		panes:         panes,
		columnTickets: columns,
		columnOffsets: []int{0, 0, 0, 0},
		mode:          ModeAgentView,
		width:         120,
		height:        40,
		config:        &config.Config{Agents: map[string]config.AgentConfig{}},
	}
}

func TestCycleUnattachedSession_Empty(t *testing.T) {
	// With no panes at all, cycling notifies and is a no-op for
	// focusedPane / cycleAttachPrompt. This is the "you're the only
	// session in town" case.
	m := newCycleTestModel(t, [][]*board.Ticket{{}, {}, {}, {}}, nil)
	m.focusedPane = "T-attached"

	model, _ := m.cycleUnattachedSession(1)
	got := model.(*Model)

	if got.notification != "No other open sessions" {
		t.Errorf("notification = %q, want \"No other open sessions\"", got.notification)
	}
	if got.cycleAttachPrompt {
		t.Errorf("cycleAttachPrompt = true, want false on empty set")
	}
	if got.focusedPane != "T-attached" {
		t.Errorf("focusedPane = %q, want unchanged \"T-attached\"", got.focusedPane)
	}
}

func TestCycleUnattachedSession_Single(t *testing.T) {
	// One Unattached pane B; current focus is on A which has no pane
	// (so A is not in the candidate set). Either delta should land on B.
	tA := &board.Ticket{ID: "T-A"}
	tB := &board.Ticket{ID: "T-B"}
	for _, delta := range []int{1, -1} {
		m := newCycleTestModel(t,
			[][]*board.Ticket{{tA, tB}, {}, {}, {}},
			[]board.TicketID{"T-B"})
		m.focusedPane = "T-A"

		model, _ := m.cycleUnattachedSession(delta)
		got := model.(*Model)

		if got.focusedPane != "T-B" {
			t.Errorf("delta=%d: focusedPane = %q, want \"T-B\"", delta, got.focusedPane)
		}
		if !got.cycleAttachPrompt {
			t.Errorf("delta=%d: cycleAttachPrompt = false, want true after successful cycle", delta)
		}
	}
}

func TestCycleUnattachedSession_BoardOrderForward(t *testing.T) {
	// Three Unattached panes laid out across columns. Cycle order must
	// follow columnTickets order (column 0 first, then column 1, etc.)
	// — the same order the user reads on the board.
	tA := &board.Ticket{ID: "T-A"}
	tB := &board.Ticket{ID: "T-B"}
	tC := &board.Ticket{ID: "T-C"}
	cols := [][]*board.Ticket{{tA}, {tB}, {tC}, {}}
	ids := []board.TicketID{"T-A", "T-B", "T-C"}

	cases := []struct {
		from board.TicketID
		want board.TicketID
	}{
		{"", "T-A"},        // no focus → first candidate
		{"T-X", "T-A"},     // focus outside set → first candidate
		{"T-A", "T-B"},     // forward step
		{"T-B", "T-C"},     // forward step
		{"T-C", "T-A"},     // wrap
	}
	for _, c := range cases {
		t.Run(string(c.from)+"->"+string(c.want), func(t *testing.T) {
			m := newCycleTestModel(t, cols, ids)
			m.focusedPane = c.from
			model, _ := m.cycleUnattachedSession(1)
			if got := model.(*Model).focusedPane; got != c.want {
				t.Errorf("focusedPane = %q, want %q", got, c.want)
			}
		})
	}
}

func TestCycleUnattachedSession_BoardOrderBackward(t *testing.T) {
	// Same three-pane fixture but cycling with delta=-1. From "no
	// focus" the user lands on the LAST candidate (mirror of the
	// forward "first candidate" case).
	tA := &board.Ticket{ID: "T-A"}
	tB := &board.Ticket{ID: "T-B"}
	tC := &board.Ticket{ID: "T-C"}
	cols := [][]*board.Ticket{{tA}, {tB}, {tC}, {}}
	ids := []board.TicketID{"T-A", "T-B", "T-C"}

	cases := []struct {
		from board.TicketID
		want board.TicketID
	}{
		{"", "T-C"},        // no focus, backward → last candidate
		{"T-X", "T-C"},     // focus outside set, backward → last candidate
		{"T-C", "T-B"},     // backward step
		{"T-B", "T-A"},     // backward step
		{"T-A", "T-C"},     // wrap
	}
	for _, c := range cases {
		t.Run(string(c.from)+"->"+string(c.want), func(t *testing.T) {
			m := newCycleTestModel(t, cols, ids)
			m.focusedPane = c.from
			model, _ := m.cycleUnattachedSession(-1)
			if got := model.(*Model).focusedPane; got != c.want {
				t.Errorf("focusedPane = %q, want %q", got, c.want)
			}
		})
	}
}

func TestCycleUnattachedSession_SkipsNonUnattached(t *testing.T) {
	// A pane in PaneViewDetached state (constructed with info=nil)
	// MUST be excluded from the candidate set. This stands in for the
	// PaneViewAttached case as well — both states fall through the
	// filter, and what matters is "only Unattached is included".
	tA := &board.Ticket{ID: "T-A"}
	tB := &board.Ticket{ID: "T-B"}
	tC := &board.Ticket{ID: "T-C"}
	cols := [][]*board.Ticket{{tA, tB, tC}, {}, {}, {}}

	// Build the model by hand so we can mix states.
	panes := map[board.TicketID]*daemonclient.PaneView{
		"T-A": daemonclient.NewPaneView(nil, "T-A", "sid-A", &daemon.SessionInfo{
			SessionID: "sid-A", TicketID: "T-A", Running: true, Cols: 80, Rows: 24,
		}),
		// T-B: constructed with info=nil → PaneViewDetached, excluded.
		"T-B": daemonclient.NewPaneView(nil, "T-B", "sid-B", nil),
		"T-C": daemonclient.NewPaneView(nil, "T-C", "sid-C", &daemon.SessionInfo{
			SessionID: "sid-C", TicketID: "T-C", Running: true, Cols: 80, Rows: 24,
		}),
	}
	m := &Model{
		globalStore:   project.NewGlobalTicketStore(nil),
		panes:         panes,
		columnTickets: cols,
		mode:          ModeAgentView,
		width:         120,
		height:        40,
		config:        &config.Config{Agents: map[string]config.AgentConfig{}},
		focusedPane:   "T-A",
	}

	model, _ := m.cycleUnattachedSession(1)
	// Candidate set is {T-A, T-C}; cycling forward from T-A → T-C.
	if got := model.(*Model).focusedPane; got != "T-C" {
		t.Errorf("focusedPane = %q, want \"T-C\" (Detached pane T-B should have been skipped)", got)
	}
}

func TestCycleUnattachedSession_RejectsBadDelta(t *testing.T) {
	// Defensive: only +1 / -1 are meaningful. Anything else is a bug
	// in the caller and the helper bails without mutating state.
	tA := &board.Ticket{ID: "T-A"}
	m := newCycleTestModel(t,
		[][]*board.Ticket{{tA}, {}, {}, {}},
		[]board.TicketID{"T-A"})
	m.focusedPane = "starting"

	for _, bad := range []int{0, 2, -2, 99} {
		model, cmd := m.cycleUnattachedSession(bad)
		got := model.(*Model)
		if got.focusedPane != "starting" {
			t.Errorf("delta=%d: focusedPane mutated to %q", bad, got.focusedPane)
		}
		if got.cycleAttachPrompt {
			t.Errorf("delta=%d: cycleAttachPrompt = true, want false", bad)
		}
		if cmd != nil {
			t.Errorf("delta=%d: cmd = non-nil, want nil", bad)
		}
	}
}

func TestHandleCycleAttachPromptKey_EscReturnsToBoard(t *testing.T) {
	// While the modal is open, Esc must drop it AND return the user to
	// ModeNormal with no focused pane — the "cancel" semantics the
	// product picked over "stay on the snapshot" or "resume original".
	m := newCycleTestModel(t, [][]*board.Ticket{{}, {}, {}, {}}, nil)
	m.cycleAttachPrompt = true
	m.mode = ModeAgentView
	m.focusedPane = "T-modal"

	model, _ := m.handleCycleAttachPromptKey(tea.KeyMsg{Type: tea.KeyEsc})
	got := model.(*Model)

	if got.cycleAttachPrompt {
		t.Errorf("cycleAttachPrompt = true after Esc, want false")
	}
	if got.mode != ModeNormal {
		t.Errorf("mode = %v after Esc, want ModeNormal", got.mode)
	}
	if got.focusedPane != "" {
		t.Errorf("focusedPane = %q after Esc, want \"\"", got.focusedPane)
	}
}

func TestHandleCycleAttachPromptKey_SwallowsOtherKeys(t *testing.T) {
	// A stray keystroke (any printable character, or arrow keys, etc.)
	// while the modal is open must NOT bypass the modal. If it did, the
	// whole point of the modal — absorbing the user's intent so the
	// AttachFirstMsg handshake doesn't eat their first keystroke —
	// would be defeated.
	m := newCycleTestModel(t,
		[][]*board.Ticket{{&board.Ticket{ID: "T-X"}}, {}, {}, {}},
		[]board.TicketID{"T-X"})
	m.cycleAttachPrompt = true
	m.focusedPane = "T-X"

	model, cmd := m.handleCycleAttachPromptKey(
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	got := model.(*Model)

	if !got.cycleAttachPrompt {
		t.Errorf("cycleAttachPrompt cleared by stray key, want preserved")
	}
	if got.focusedPane != "T-X" {
		t.Errorf("focusedPane = %q, want unchanged \"T-X\"", got.focusedPane)
	}
	if cmd != nil {
		t.Errorf("cmd = non-nil for swallowed key, want nil")
	}
}

func TestHandleCycleAttachPromptKey_CycleAdvances(t *testing.T) {
	// Inside the modal, Ctrl+] continues the cycle without dropping the
	// modal — the user can walk all peers in sequence and then commit
	// (Enter) or cancel (Esc) at the right one.
	tA := &board.Ticket{ID: "T-A"}
	tB := &board.Ticket{ID: "T-B"}
	m := newCycleTestModel(t,
		[][]*board.Ticket{{tA, tB}, {}, {}, {}},
		[]board.TicketID{"T-A", "T-B"})
	m.cycleAttachPrompt = true
	m.focusedPane = "T-A"

	model, _ := m.handleCycleAttachPromptKey(tea.KeyMsg{Type: tea.KeyCtrlCloseBracket})
	got := model.(*Model)

	if got.focusedPane != "T-B" {
		t.Errorf("focusedPane = %q after Ctrl+] in modal, want \"T-B\"", got.focusedPane)
	}
	if !got.cycleAttachPrompt {
		t.Errorf("cycleAttachPrompt cleared by in-modal cycle, want preserved")
	}
}
