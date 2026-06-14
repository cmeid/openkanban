package ui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/board"
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
