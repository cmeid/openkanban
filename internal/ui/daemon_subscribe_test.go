package ui

import (
	"testing"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/project"
)

// TestHandleDaemonSessionEventAttachedCounter pins the contract for the
// daemonAttached counter the board card reads to render its "attached
// to a TUI" indicator:
//
//   - A clean attach/detach pair lands the counter at 0 (no indicator).
//   - A takeover's two events — attached(new) and detached(old) — leave
//     the counter at 1 in either arrival order (the daemon emits them
//     from separate goroutines, so order is not guaranteed). Using a
//     counter instead of a bool is what makes this race-correct.
//   - "exited" clears the counter regardless of prior state.
//   - "detached" when the counter is already 0 must not underflow.
func TestHandleDaemonSessionEventAttachedCounter(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	const tid board.TicketID = "att-1"

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
			globalStore:    globalStore,
			daemonOwned:    map[board.TicketID]struct{}{tid: {}},
			daemonAttached: map[board.TicketID]int{},
		}
	}

	send := func(m *Model, event string) {
		_, _ = m.handleDaemonSessionEvent(daemonSessionEventMsg{
			Event: daemon.SessionEvent{Event: event, TicketID: string(tid)},
		})
	}

	t.Run("clean attach then detach lands at zero", func(t *testing.T) {
		m := newModel()
		send(m, "attached")
		if got := m.daemonAttached[tid]; got != 1 {
			t.Errorf("after attached: counter = %d, want 1", got)
		}
		send(m, "detached")
		if _, present := m.daemonAttached[tid]; present {
			t.Errorf("after detached: key still present (counter dropped to 0 should delete)")
		}
	})

	t.Run("takeover: attached(new) then detached(old) stays attached", func(t *testing.T) {
		m := newModel()
		send(m, "attached") // initial attacher A: counter=1
		send(m, "attached") // takeover by B (new attach event): counter=2
		send(m, "detached") // A's binaryLoop exits and emits detach: counter=1
		if got := m.daemonAttached[tid]; got != 1 {
			t.Errorf("after takeover (B-attach then A-detach): counter = %d, want 1", got)
		}
	})

	t.Run("takeover: detached(old) then attached(new) stays attached", func(t *testing.T) {
		m := newModel()
		send(m, "attached") // A: counter=1
		send(m, "detached") // A's detach lands first (reversed race): counter=0
		send(m, "attached") // B's attach: counter=1
		if got := m.daemonAttached[tid]; got != 1 {
			t.Errorf("after reversed takeover order: counter = %d, want 1", got)
		}
	})

	t.Run("exited clears the counter even when attached", func(t *testing.T) {
		m := newModel()
		send(m, "attached")
		send(m, "exited")
		if _, present := m.daemonAttached[tid]; present {
			t.Errorf("after exited: key still present")
		}
		if _, present := m.daemonOwned[tid]; present {
			t.Errorf("after exited: daemonOwned still has key (regression — exited should clear it too)")
		}
	})

	t.Run("detached at zero does not underflow", func(t *testing.T) {
		m := newModel()
		send(m, "detached") // no prior attach
		if got := m.daemonAttached[tid]; got != 0 {
			t.Errorf("after spurious detached: counter = %d, want 0", got)
		}
	})
}
