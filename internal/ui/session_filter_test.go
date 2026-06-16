package ui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

func TestNextSessionFilter(t *testing.T) {
	// Cycle must reach every filter and return to all — guards against
	// silently dropping a mode if the slice is reordered.
	cycle := []SessionFilter{
		SessionFilterAll,
		SessionFilterOpen,
		SessionFilterWaiting,
		SessionFilterAll,
	}
	cur := SessionFilterAll
	for i := 1; i < len(cycle); i++ {
		cur = nextSessionFilter(cur)
		if cur != cycle[i] {
			t.Fatalf("step %d: got %q, want %q", i, cur, cycle[i])
		}
	}
}

func TestSessionFilterLabel(t *testing.T) {
	tests := []struct {
		filter SessionFilter
		want   string
	}{
		{SessionFilterAll, "all"},
		{SessionFilterOpen, "open sessions"},
		{SessionFilterWaiting, "waiting sessions"},
	}
	for _, tt := range tests {
		if got := sessionFilterLabel(tt.filter); got != tt.want {
			t.Errorf("sessionFilterLabel(%q) = %q, want %q", tt.filter, got, tt.want)
		}
	}
}

func TestTicketMatchesFilter_SessionFilter(t *testing.T) {
	mk := func(id, title string, status board.AgentStatus) *board.Ticket {
		return &board.Ticket{
			ID:          board.TicketID(id),
			Title:       title,
			Status:      board.StatusBacklog,
			AgentStatus: status,
			CreatedAt:   time.Now(),
		}
	}

	// Three tickets:
	//   live-working — has a daemon session, status=working
	//   live-waiting — has a daemon session, status=waiting
	//   no-session   — no daemon entry, status=idle
	working := mk("t-working", "live-working", board.AgentWorking)
	waiting := mk("t-waiting", "live-waiting", board.AgentWaiting)
	dormant := mk("t-dormant", "no-session", board.AgentIdle)

	m := &Model{
		daemonOwned: map[board.TicketID]struct{}{
			working.ID: {},
			waiting.ID: {},
		},
	}

	t.Run("All matches everything", func(t *testing.T) {
		m.sessionFilter = SessionFilterAll
		for _, tk := range []*board.Ticket{working, waiting, dormant} {
			if !m.ticketMatchesFilter(tk) {
				t.Errorf("All: %s should match", tk.Title)
			}
		}
	})

	t.Run("Open matches daemon-owned tickets only", func(t *testing.T) {
		m.sessionFilter = SessionFilterOpen
		if !m.ticketMatchesFilter(working) {
			t.Error("Open: live-working should match")
		}
		if !m.ticketMatchesFilter(waiting) {
			t.Error("Open: live-waiting should match")
		}
		if m.ticketMatchesFilter(dormant) {
			t.Error("Open: no-session must NOT match")
		}
	})

	t.Run("Waiting matches daemon-owned + AgentWaiting only", func(t *testing.T) {
		m.sessionFilter = SessionFilterWaiting
		if !m.ticketMatchesFilter(waiting) {
			t.Error("Waiting: live-waiting should match")
		}
		if m.ticketMatchesFilter(working) {
			t.Error("Waiting: live-working must NOT match (status=working)")
		}
		if m.ticketMatchesFilter(dormant) {
			t.Error("Waiting: no-session must NOT match (no daemon entry)")
		}
	})

	t.Run("Waiting without daemon session does not match", func(t *testing.T) {
		// Defensive: AgentStatus may still read "waiting" on a ticket
		// whose session has already exited (stale on-disk status).
		// Without a live daemon entry, the filter must hide it — the
		// whole point of the filter is "what sessions need me now."
		stale := mk("t-stale", "stale-waiting", board.AgentWaiting)
		m.sessionFilter = SessionFilterWaiting
		if m.ticketMatchesFilter(stale) {
			t.Error("Waiting: ticket with AgentWaiting but no daemon entry must NOT match")
		}
	})
}

func TestCycleSessionFilter_KeybindingW(t *testing.T) {
	// Two tickets in backlog: one with a live daemon session, one without.
	// Pressing 'w' should cycle Filter and hide the no-session ticket on the
	// first press (→ open), then keep hiding it on the second press (→ waiting).
	live := &board.Ticket{
		ID:          board.NewTicketID(),
		Title:       "live-session",
		Status:      board.StatusBacklog,
		AgentStatus: board.AgentWaiting,
		CreatedAt:   time.Now(),
	}
	dormant := &board.Ticket{
		ID:        board.NewTicketID(),
		Title:     "dormant",
		Status:    board.StatusBacklog,
		CreatedAt: time.Now(),
	}
	m := makePrioritySortModel(t, []*board.Ticket{live, dormant})
	m.daemonOwned = map[board.TicketID]struct{}{live.ID: {}}
	m.refreshColumnTickets()

	if got := len(m.columnTickets[0]); got != 2 {
		t.Fatalf("baseline: backlog has %d tickets, want 2", got)
	}

	// First 'w' → SessionFilterOpen. Only the daemon-owned ticket remains.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	if m.sessionFilter != SessionFilterOpen {
		t.Fatalf("after first 'w', sessionFilter = %q, want %q", m.sessionFilter, SessionFilterOpen)
	}
	if got := len(m.columnTickets[0]); got != 1 {
		t.Errorf("after 'w' (open): backlog has %d tickets, want 1", got)
	}
	if m.columnTickets[0][0].ID != live.ID {
		t.Errorf("after 'w' (open): visible ticket = %s, want %s", m.columnTickets[0][0].Title, live.Title)
	}

	// Second 'w' → SessionFilterWaiting. live (status=waiting) still matches.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	if m.sessionFilter != SessionFilterWaiting {
		t.Fatalf("after second 'w', sessionFilter = %q, want %q", m.sessionFilter, SessionFilterWaiting)
	}
	if got := len(m.columnTickets[0]); got != 1 {
		t.Errorf("after 'w' (waiting): backlog has %d tickets, want 1", got)
	}

	// Third 'w' → SessionFilterAll. Both tickets visible again.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	if m.sessionFilter != SessionFilterAll {
		t.Fatalf("after third 'w', sessionFilter = %q, want %q", m.sessionFilter, SessionFilterAll)
	}
	if got := len(m.columnTickets[0]); got != 2 {
		t.Errorf("after 'w' (all): backlog has %d tickets, want 2", got)
	}
}

func TestTicketMatchesFilter_AlwaysShowWorking(t *testing.T) {
	// Two projects, three tickets:
	//   foo-open    — project foo, has daemon session, status=working
	//   bar-open    — project bar, has daemon session, status=working
	//   bar-dormant — project bar, no daemon entry
	// Project filter is narrowed to foo. With alwaysShowWorking=true, any
	// daemon-owned ticket should bypass the project filter; non-daemon
	// tickets in other projects should stay hidden.
	mk := func(id, title, projectID, descr string, status board.AgentStatus) *board.Ticket {
		return &board.Ticket{
			ID:          board.TicketID(id),
			Title:       title,
			Description: descr,
			ProjectID:   projectID,
			Status:      board.StatusBacklog,
			AgentStatus: status,
			CreatedAt:   time.Now(),
		}
	}

	fooOpen := mk("t-foo-open", "foo work", "foo", "alpha", board.AgentWorking)
	barOpen := mk("t-bar-open", "bar work", "bar", "beta", board.AgentWorking)
	barDormant := mk("t-bar-dormant", "bar dormant", "bar", "gamma", board.AgentIdle)

	newModel := func() *Model {
		return &Model{
			daemonOwned: map[board.TicketID]struct{}{
				fooOpen.ID: {},
				barOpen.ID: {},
			},
			filterProjectIDs: map[string]bool{"foo": true},
		}
	}

	t.Run("Bypasses project filter when daemonOwned", func(t *testing.T) {
		m := newModel()
		m.alwaysShowWorking = true
		if !m.ticketMatchesFilter(barOpen) {
			t.Error("expected daemon-owned ticket in non-filtered project to match when alwaysShowWorking=true")
		}
	})

	t.Run("Does not bypass for non-daemon-owned tickets", func(t *testing.T) {
		m := newModel()
		m.alwaysShowWorking = true
		if m.ticketMatchesFilter(barDormant) {
			t.Error("expected non-daemon ticket in non-filtered project to remain hidden even with alwaysShowWorking=true")
		}
	})

	t.Run("Bypasses free-text query", func(t *testing.T) {
		m := newModel()
		m.alwaysShowWorking = true
		m.filterQuery = "zzz-no-match"
		if !m.ticketMatchesFilter(barOpen) {
			t.Error("expected daemon-owned ticket to match despite non-matching free-text query")
		}
	})

	t.Run("Bypasses @projectname query prefix", func(t *testing.T) {
		// The @-prefix is a typed project scope; per plan, it counts as
		// part of the text-search filter and is bypassed.
		m := newModel()
		m.alwaysShowWorking = true
		m.filterQuery = "@foo"
		if !m.ticketMatchesFilter(barOpen) {
			t.Error("expected daemon-owned ticket in 'bar' to match despite typed @foo scope")
		}
	})

	t.Run("Session filter still applies", func(t *testing.T) {
		// alwaysShowWorking bypasses project + query but NOT the session
		// filter. With sessionFilter=Waiting, a working-status open
		// session must remain hidden.
		m := newModel()
		m.alwaysShowWorking = true
		m.sessionFilter = SessionFilterWaiting
		if m.ticketMatchesFilter(barOpen) {
			t.Error("expected open-but-working ticket to remain hidden when sessionFilter=Waiting")
		}
	})

	t.Run("No-op when no project filter active", func(t *testing.T) {
		m := newModel()
		m.filterProjectIDs = map[string]bool{}
		// Compare on/off: should match identically for every ticket.
		for _, tk := range []*board.Ticket{fooOpen, barOpen, barDormant} {
			m.alwaysShowWorking = false
			off := m.ticketMatchesFilter(tk)
			m.alwaysShowWorking = true
			on := m.ticketMatchesFilter(tk)
			if off != on {
				t.Errorf("ticket %s: with no project filter, alwaysShowWorking should be a no-op (off=%v on=%v)", tk.Title, off, on)
			}
		}
	})

	t.Run("No-op when no daemon-owned tickets", func(t *testing.T) {
		m := newModel()
		m.daemonOwned = map[board.TicketID]struct{}{}
		for _, tk := range []*board.Ticket{fooOpen, barOpen, barDormant} {
			m.alwaysShowWorking = false
			off := m.ticketMatchesFilter(tk)
			m.alwaysShowWorking = true
			on := m.ticketMatchesFilter(tk)
			if off != on {
				t.Errorf("ticket %s: with no daemon-owned tickets, alwaysShowWorking should be a no-op (off=%v on=%v)", tk.Title, off, on)
			}
		}
	})
}

// makeAlwaysShowWorkingModel builds a Model wired to a real project
// store with TWO projects ("foo" and "bar") and the given tickets
// pre-loaded, with project filter narrowed to "foo". Used for tests
// that exercise refreshColumnTickets-driven flows like the W keybind.
func makeAlwaysShowWorkingModel(t *testing.T, tickets []*board.Ticket) *Model {
	t.Helper()
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	fooProj := &project.Project{ID: "foo", RepoPath: t.TempDir()}
	barProj := &project.Project{ID: "bar", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(fooProj)
	globalStore.AddProject(barProj)

	for _, tk := range tickets {
		if err := globalStore.Add(tk); err != nil {
			t.Fatalf("Add ticket %q: %v", tk.Title, err)
		}
	}

	cols := board.DefaultColumns()
	m := &Model{
		globalStore:      globalStore,
		panes:            map[board.TicketID]*daemonclient.PaneView{},
		columns:          cols,
		columnTickets:    make([][]*board.Ticket, len(cols)),
		columnOffsets:    make([]int, len(cols)),
		filterProjectIDs: map[string]bool{"foo": true},
		mode:             ModeNormal,
		activeColumn:     0,
		activeTicket:     0,
		width:            120,
		height:           40,
		config:           &config.Config{Agents: map[string]config.AgentConfig{}},
	}
	m.refreshColumnTickets()
	return m
}

func TestToggleAlwaysShowWorking_KeybindingW(t *testing.T) {
	// foo-only is in the filtered project; bar-open is in another project
	// with a live daemon session. Baseline (W=off): only foo-only shows.
	// After pressing W: bar-open shows too. After second W: bar-open hides.
	fooOnly := &board.Ticket{
		ID:          board.NewTicketID(),
		Title:       "foo-only",
		ProjectID:   "foo",
		Status:      board.StatusBacklog,
		AgentStatus: board.AgentIdle,
		CreatedAt:   time.Now(),
	}
	barOpen := &board.Ticket{
		ID:          board.NewTicketID(),
		Title:       "bar-open",
		ProjectID:   "bar",
		Status:      board.StatusBacklog,
		AgentStatus: board.AgentWorking,
		CreatedAt:   time.Now(),
	}

	m := makeAlwaysShowWorkingModel(t, []*board.Ticket{fooOnly, barOpen})
	m.daemonOwned = map[board.TicketID]struct{}{barOpen.ID: {}}
	m.refreshColumnTickets()

	// Baseline: project filter narrows to foo, only fooOnly visible.
	if got := len(m.columnTickets[0]); got != 1 {
		t.Fatalf("baseline: backlog has %d tickets, want 1 (foo-only)", got)
	}
	if m.columnTickets[0][0].ID != fooOnly.ID {
		t.Fatalf("baseline: visible ticket = %q, want %q", m.columnTickets[0][0].Title, fooOnly.Title)
	}

	// First 'W' → alwaysShowWorking=true; bar-open should now also show.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'W'}})
	if !m.alwaysShowWorking {
		t.Fatalf("after first 'W': alwaysShowWorking = false, want true")
	}
	if got := len(m.columnTickets[0]); got != 2 {
		t.Errorf("after 'W' (on): backlog has %d tickets, want 2", got)
	}

	// Second 'W' → alwaysShowWorking=false; back to project-only view.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'W'}})
	if m.alwaysShowWorking {
		t.Fatalf("after second 'W': alwaysShowWorking = true, want false")
	}
	if got := len(m.columnTickets[0]); got != 1 {
		t.Errorf("after 'W' (off): backlog has %d tickets, want 1", got)
	}
}

func TestToggleAlwaysShowWorking_CursorSurvivesToggleOff(t *testing.T) {
	// With W=on, cursor lands on bar-open (cross-project daemon-owned
	// ticket). Toggling W off filters that ticket out — the cursor
	// must land somewhere valid, not point past the end of the now-
	// shorter column.
	fooOnly := &board.Ticket{
		ID:          board.NewTicketID(),
		Title:       "foo-only",
		ProjectID:   "foo",
		Status:      board.StatusBacklog,
		AgentStatus: board.AgentIdle,
		CreatedAt:   time.Now(),
	}
	barOpen := &board.Ticket{
		ID:          board.NewTicketID(),
		Title:       "bar-open",
		ProjectID:   "bar",
		Status:      board.StatusBacklog,
		AgentStatus: board.AgentWorking,
		CreatedAt:   time.Now(),
	}

	m := makeAlwaysShowWorkingModel(t, []*board.Ticket{fooOnly, barOpen})
	m.daemonOwned = map[board.TicketID]struct{}{barOpen.ID: {}}
	m.alwaysShowWorking = true
	m.refreshColumnTickets()

	// Position cursor on whichever index points at bar-open.
	for i, tk := range m.columnTickets[0] {
		if tk.ID == barOpen.ID {
			m.activeTicket = i
			break
		}
	}

	// Toggle W off. bar-open should vanish; cursor must not go OOB.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'W'}})
	if m.alwaysShowWorking {
		t.Fatalf("after 'W' toggle: alwaysShowWorking still true")
	}
	if got := len(m.columnTickets[0]); got != 1 {
		t.Fatalf("after toggle off: backlog has %d tickets, want 1", got)
	}
	if m.activeTicket >= len(m.columnTickets[0]) {
		t.Errorf("cursor out of bounds after toggle: activeTicket=%d, column len=%d", m.activeTicket, len(m.columnTickets[0]))
	}
}
