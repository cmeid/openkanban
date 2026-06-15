package ui

import (
	"testing"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/project"
)

// TestHandleDaemonSessionEventViewingCounter pins the contract for the
// daemonViewing counter the board card reads to render its "TUI is
// viewing this ticket" indicator:
//
//   - A clean viewing/unviewing pair lands the counter at 0 (no
//     indicator) and deletes the key.
//   - Two viewing events (e.g. two sibling TUIs both entering agent
//     view on the same session) accumulate to 2; one unviewing brings
//     it back to 1 (still indicating "someone is viewing").
//   - "exited" clears the counter regardless of prior state.
//   - "unviewing" when the counter is 0 must not underflow.
//   - "attached" / "detached" are informational and must NOT affect
//     daemonViewing — the indicator semantics are decoupled from PTY-
//     stream ownership.
func TestHandleDaemonSessionEventViewingCounter(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	const tid board.TicketID = "view-1"

	newModel := func() *Model {
		proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
		globalStore := project.NewGlobalTicketStore(nil)
		globalStore.AddProject(proj)

		ticket := &board.Ticket{
			ID:        tid,
			Title:     "t",
			ProjectID: "test",
			Status:    board.StatusInProgress,
		}
		if err := globalStore.Add(ticket); err != nil {
			t.Fatalf("Add ticket: %v", err)
		}

		return &Model{
			globalStore:   globalStore,
			daemonOwned:   map[board.TicketID]struct{}{tid: {}},
			daemonViewing: map[board.TicketID]int{},
		}
	}

	send := func(m *Model, event string) {
		_, _ = m.handleDaemonSessionEvent(daemonSessionEventMsg{
			Event: daemon.SessionEvent{Event: event, TicketID: string(tid)},
		})
	}

	t.Run("viewing then unviewing lands at zero", func(t *testing.T) {
		m := newModel()
		send(m, "viewing")
		if got := m.daemonViewing[tid]; got != 1 {
			t.Errorf("after viewing: counter = %d, want 1", got)
		}
		send(m, "unviewing")
		if _, present := m.daemonViewing[tid]; present {
			t.Errorf("after unviewing: key still present (counter dropped to 0 should delete)")
		}
	})

	t.Run("two concurrent viewers accumulate", func(t *testing.T) {
		m := newModel()
		send(m, "viewing") // TUI A enters agent view
		send(m, "viewing") // TUI B enters agent view on the same session
		if got := m.daemonViewing[tid]; got != 2 {
			t.Errorf("after two viewing events: counter = %d, want 2", got)
		}
		send(m, "unviewing") // TUI A leaves
		if got := m.daemonViewing[tid]; got != 1 {
			t.Errorf("after one unviewing: counter = %d, want 1 (B still viewing)", got)
		}
		send(m, "unviewing") // TUI B leaves
		if _, present := m.daemonViewing[tid]; present {
			t.Errorf("after second unviewing: key still present (should delete at 0)")
		}
	})

	t.Run("exited clears the counter even when actively viewed", func(t *testing.T) {
		m := newModel()
		send(m, "viewing")
		send(m, "exited")
		if _, present := m.daemonViewing[tid]; present {
			t.Errorf("after exited: key still present")
		}
		if _, present := m.daemonOwned[tid]; present {
			t.Errorf("after exited: daemonOwned still has key (exited should clear it too)")
		}
	})

	t.Run("unviewing at zero does not underflow", func(t *testing.T) {
		m := newModel()
		send(m, "unviewing") // no prior viewing
		if got := m.daemonViewing[tid]; got != 0 {
			t.Errorf("after spurious unviewing: counter = %d, want 0", got)
		}
	})

	t.Run("attached and detached do not move daemonViewing", func(t *testing.T) {
		// The board indicator is driven by viewing/unviewing only. PTY-
		// stream attach/detach is informational here — a session is
		// "attached" for most of its lifetime regardless of focus, so
		// using it for the indicator (the previous design) marked
		// everything attached. This test pins that the wires are
		// definitively separated.
		m := newModel()
		send(m, "attached")
		if got := m.daemonViewing[tid]; got != 0 {
			t.Errorf("after attached: daemonViewing = %d, want 0 (attached is not viewing)", got)
		}
		send(m, "detached")
		if got := m.daemonViewing[tid]; got != 0 {
			t.Errorf("after detached: daemonViewing = %d, want 0", got)
		}
	})
}
