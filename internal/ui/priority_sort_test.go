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

// makePrioritySortModel builds a Model wired to a real project store
// (under a temp dir) with the given tickets pre-loaded into the first
// column. Using a real store — rather than stubbing Save — keeps the
// test honest about the saveTicket → SaveTicket persistence chain that
// adjustPriority depends on.
func makePrioritySortModel(t *testing.T, tickets []*board.Ticket) *Model {
	t.Helper()
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	for _, tk := range tickets {
		tk.ProjectID = proj.ID
		if err := globalStore.Add(tk); err != nil {
			t.Fatalf("Add ticket %q: %v", tk.Title, err)
		}
	}

	cols := board.DefaultColumns()
	m := &Model{
		globalStore:   globalStore,
		panes:         map[board.TicketID]*daemonclient.PaneView{},
		columns:       cols,
		columnTickets: make([][]*board.Ticket, len(cols)),
		columnOffsets: make([]int, len(cols)),
		mode:          ModeNormal,
		activeColumn:  0,
		activeTicket:  0,
		width:         120,
		height:        40,
		config:        &config.Config{Agents: map[string]config.AgentConfig{}},
	}
	m.refreshColumnTickets()
	return m
}

func titles(tickets []*board.Ticket) []string {
	out := make([]string, len(tickets))
	for i, t := range tickets {
		out[i] = t.Title
	}
	return out
}

func TestSortTickets(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mk := func(title string, prio int, age time.Duration) *board.Ticket {
		return &board.Ticket{
			ID:        board.NewTicketID(),
			Title:     title,
			Status:    board.StatusBacklog,
			Priority:  prio,
			CreatedAt: base.Add(age),
		}
	}

	// Cover each sort branch on the same input set so the assertions
	// pin one behavior at a time. Priority 0 ("unset") in the priority
	// branch must be treated as the default 3 — without that fallback,
	// legacy tickets without the field would clump at the wrong end.
	t.Run("name sorts case-insensitive ascending", func(t *testing.T) {
		input := []*board.Ticket{
			mk("charlie", 3, 0),
			mk("alpha", 3, 0),
			mk("Bravo", 3, 0),
		}
		sortTickets(input, SortName)
		got := titles(input)
		want := []string{"alpha", "Bravo", "charlie"}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("name sort: pos %d = %q, want %q (full=%v)", i, got[i], want[i], got)
			}
		}
	})

	t.Run("age sorts newest first", func(t *testing.T) {
		input := []*board.Ticket{
			mk("old", 3, 0),
			mk("newest", 3, 2*time.Hour),
			mk("middle", 3, time.Hour),
		}
		sortTickets(input, SortAge)
		got := titles(input)
		want := []string{"newest", "middle", "old"}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("age sort: pos %d = %q, want %q (full=%v)", i, got[i], want[i], got)
			}
		}
	})

	t.Run("priority sorts highest first with 0 treated as 3", func(t *testing.T) {
		input := []*board.Ticket{
			mk("low", 5, 0),
			mk("highest", 1, 0),
			mk("legacy-zero", 0, 0),
			mk("high", 2, 0),
		}
		sortTickets(input, SortPriority)
		got := titles(input)
		// legacy-zero (effective 3) and "high" (2) sort before "low" (5);
		// "highest" (1) leads.
		want := []string{"highest", "high", "legacy-zero", "low"}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("priority sort: pos %d = %q, want %q (full=%v)", i, got[i], want[i], got)
			}
		}
	})

	t.Run("default leaves order untouched", func(t *testing.T) {
		input := []*board.Ticket{
			mk("c", 3, 0),
			mk("a", 1, 0),
			mk("b", 5, 0),
		}
		before := titles(input)
		sortTickets(input, SortDefault)
		after := titles(input)
		for i := range before {
			if before[i] != after[i] {
				t.Errorf("default sort changed order: before=%v after=%v", before, after)
			}
		}
	})
}

func TestNextSortMode(t *testing.T) {
	// Cycle must reach every mode and return to default — guards
	// against silently dropping a mode if the slice is reordered.
	cycle := []SortMode{SortDefault, SortName, SortAge, SortPriority, SortDefault}
	cur := SortDefault
	for i := 1; i < len(cycle); i++ {
		cur = nextSortMode(cur)
		if cur != cycle[i] {
			t.Fatalf("step %d: got %q, want %q", i, cur, cycle[i])
		}
	}
}

func TestCycleSortMode_AppliesSortAndKeepsSelection(t *testing.T) {
	// Two backlog tickets with names that contradict the priority
	// order; cycling to SortPriority must reorder them AND keep the
	// originally-selected ticket selected even though its index moved.
	low := &board.Ticket{
		ID:        board.NewTicketID(),
		Title:     "zzz-low-prio",
		Status:    board.StatusBacklog,
		Priority:  5,
		CreatedAt: time.Now(),
	}
	high := &board.Ticket{
		ID:        board.NewTicketID(),
		Title:     "aaa-high-prio",
		Status:    board.StatusBacklog,
		Priority:  1,
		CreatedAt: time.Now(),
	}
	m := makePrioritySortModel(t, []*board.Ticket{low, high})

	// Select the LOW-priority ticket regardless of where it landed in
	// the post-refresh slice (map iteration is non-deterministic).
	for i, tk := range m.columnTickets[0] {
		if tk.ID == low.ID {
			m.activeTicket = i
			break
		}
	}

	// Press 'o' once: SortDefault → SortName. Names sort A→Z so
	// "aaa-high-prio" should be at index 0.
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}}); m.sortMode != SortName {
		t.Fatalf("after first 'o', sortMode = %q, want %q", m.sortMode, SortName)
	}
	if m.columnTickets[0][0].ID != high.ID {
		t.Errorf("name sort: pos 0 = %q, want %q", m.columnTickets[0][0].Title, high.Title)
	}
	if sel := m.selectedTicket(); sel == nil || sel.ID != low.ID {
		t.Errorf("selection drifted after sort change; selected=%+v want low (%s)", sel, low.ID)
	}

	// 'o' a second time → SortAge, third → SortPriority. Priority
	// sort: high (1) ahead of low (5), and selection still tracks low.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	if m.sortMode != SortPriority {
		t.Fatalf("after three 'o' presses, sortMode = %q, want %q", m.sortMode, SortPriority)
	}
	if m.columnTickets[0][0].ID != high.ID {
		t.Errorf("priority sort: pos 0 = %q, want %q", m.columnTickets[0][0].Title, high.Title)
	}
	if sel := m.selectedTicket(); sel == nil || sel.ID != low.ID {
		t.Errorf("selection drifted under priority sort; selected=%+v want low", sel)
	}
}

func TestAdjustPriority(t *testing.T) {
	mk := func(prio int) *board.Ticket {
		return &board.Ticket{
			ID:        board.NewTicketID(),
			Title:     "t",
			Status:    board.StatusBacklog,
			Priority:  prio,
			CreatedAt: time.Now(),
		}
	}

	// K raises (decrements toward 1) and stops at 1 with a notify;
	// J lowers and stops at 5. Unset priority (0) snaps to the default
	// before being adjusted, so the first raise from 0 lands at 2 (not
	// -1 and not 4).
	t.Run("K raises priority and clamps at 1", func(t *testing.T) {
		tk := mk(2)
		m := makePrioritySortModel(t, []*board.Ticket{tk})
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'K'}})
		if tk.Priority != 1 {
			t.Errorf("after K from 2: priority=%d, want 1", tk.Priority)
		}
		if m.notification != "Priority raised to 1" {
			t.Errorf("notify=%q, want %q", m.notification, "Priority raised to 1")
		}

		m.notification = ""
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'K'}})
		if tk.Priority != 1 {
			t.Errorf("K at 1 should clamp; priority=%d, want 1", tk.Priority)
		}
		if m.notification != "Already at highest priority" {
			t.Errorf("clamp notify=%q, want %q", m.notification, "Already at highest priority")
		}
	})

	t.Run("J lowers priority and clamps at 5", func(t *testing.T) {
		tk := mk(4)
		m := makePrioritySortModel(t, []*board.Ticket{tk})
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'J'}})
		if tk.Priority != 5 {
			t.Errorf("after J from 4: priority=%d, want 5", tk.Priority)
		}
		m.notification = ""
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'J'}})
		if m.notification != "Already at lowest priority" {
			t.Errorf("clamp notify=%q, want %q", m.notification, "Already at lowest priority")
		}
	})

	t.Run("unset priority is treated as 3 before adjusting", func(t *testing.T) {
		tk := mk(0)
		m := makePrioritySortModel(t, []*board.Ticket{tk})
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'K'}})
		if tk.Priority != 2 {
			t.Errorf("after K from 0: priority=%d, want 2 (treated as default 3 → 2)", tk.Priority)
		}
	})

	t.Run("no ticket selected notifies and does not panic", func(t *testing.T) {
		m := makePrioritySortModel(t, nil)
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'K'}})
		if m.notification != "No ticket selected" {
			t.Errorf("notify=%q, want %q", m.notification, "No ticket selected")
		}
	})
}
