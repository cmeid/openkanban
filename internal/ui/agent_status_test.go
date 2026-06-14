package ui

import (
	"testing"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/project"
)

// TestAgentStatusResultMsgAppliesToDaemonOwnedTickets pins the
// post-fix contract for the agentStatusResultMsg handler:
//
//   - Intra-session transitions written by Claude Code hooks
//     (working → idle → waiting → working) MUST reach the UI for
//     daemon-owned tickets. The pre-fix behavior unconditionally
//     skipped daemon-owned tickets in the poll result handler so
//     AgentStatus was frozen at whatever the daemon's "started"
//     event set on spawn, regardless of what the on-disk status
//     file later said. That is the regression this test guards.
//
//   - Two narrow guards stay in place:
//     · An AgentNone from the poll must NOT clobber a previously
//       set state (the poll's "I don't know" is not a transition).
//     · AgentCompleted is terminal — a non-terminal poll value
//       must NOT downgrade it. Mirrors the same guard in
//       cmd/status.go that prevents Stop hook racing TicketDone.
func TestAgentStatusResultMsgAppliesToDaemonOwnedTickets(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	const tid board.TicketID = "owned-1"

	newModel := func(initial board.AgentStatus, daemonLive bool) (*Model, *board.Ticket) {
		proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
		globalStore := project.NewGlobalTicketStore(nil)
		globalStore.AddProject(proj)

		ticket := &board.Ticket{
			ID:          tid,
			Title:       "Owned by daemon",
			ProjectID:   "test",
			Status:      board.StatusInProgress,
			AgentStatus: initial,
		}
		if err := globalStore.Add(ticket); err != nil {
			t.Fatalf("Add ticket: %v", err)
		}

		m := &Model{
			globalStore: globalStore,
			daemonOwned: map[board.TicketID]struct{}{tid: {}},
		}
		m.daemonConnected.Store(daemonLive)
		return m, ticket
	}

	t.Run("daemon-owned ticket reflects file-poll value", func(t *testing.T) {
		// The pre-fix bug: TUI shows AgentWorking forever (because the
		// daemon's "started" event set it and the poll result was
		// skipped). After the fix the AgentIdle from the file poll must
		// land on the ticket.
		m, ticket := newModel(board.AgentWorking, true)
		if _, _ = m.Update(agentStatusResultMsg{tid: board.AgentIdle}); ticket.AgentStatus != board.AgentIdle {
			t.Errorf("AgentStatus = %v, want %v", ticket.AgentStatus, board.AgentIdle)
		}

		// And waiting too — symmetric.
		m, ticket = newModel(board.AgentWorking, true)
		if _, _ = m.Update(agentStatusResultMsg{tid: board.AgentWaiting}); ticket.AgentStatus != board.AgentWaiting {
			t.Errorf("AgentStatus = %v, want %v", ticket.AgentStatus, board.AgentWaiting)
		}
	})

	t.Run("AgentNone from poll does not clobber set state", func(t *testing.T) {
		// The poll returns AgentNone when it can't determine status
		// (no file, no terminal hits). That isn't a transition — don't
		// overwrite a perfectly good AgentWorking with "unknown".
		m, ticket := newModel(board.AgentWorking, true)
		_, _ = m.Update(agentStatusResultMsg{tid: board.AgentNone})
		if ticket.AgentStatus != board.AgentWorking {
			t.Errorf("AgentStatus = %v, want %v (AgentNone must not clobber)", ticket.AgentStatus, board.AgentWorking)
		}
	})

	t.Run("AgentCompleted is preserved against non-terminal poll values", func(t *testing.T) {
		// Once a ticket is marked completed (via the user-initiated
		// `openkanban ticket done` flow, signalled through the daemon's
		// "exited" Expected=true SessionEvent), a stale "working" or
		// "idle" file value must not downgrade it.
		for _, incoming := range []board.AgentStatus{board.AgentWorking, board.AgentIdle, board.AgentWaiting} {
			m, ticket := newModel(board.AgentCompleted, true)
			_, _ = m.Update(agentStatusResultMsg{tid: incoming})
			if ticket.AgentStatus != board.AgentCompleted {
				t.Errorf("AgentStatus = %v after poll(%v), want %v (Completed must be preserved)",
					ticket.AgentStatus, incoming, board.AgentCompleted)
			}
		}
	})

	t.Run("AgentError is allowed to overwrite AgentCompleted", func(t *testing.T) {
		// Symmetric to cmd/status.go's guard: only the two terminal
		// states (Completed, Error) may transition out of Completed.
		// Tests document the asymmetry so future edits don't blanket-
		// freeze Completed.
		m, ticket := newModel(board.AgentCompleted, true)
		_, _ = m.Update(agentStatusResultMsg{tid: board.AgentError})
		if ticket.AgentStatus != board.AgentError {
			t.Errorf("AgentStatus = %v, want %v (Error is allowed to overwrite Completed)",
				ticket.AgentStatus, board.AgentError)
		}
	})

	t.Run("standalone (non-daemon-owned) ticket still updates", func(t *testing.T) {
		// Regression guard for the original code path: when the daemon
		// either isn't running or doesn't own this ticket, the poll
		// remains the only signal and must propagate.
		m, ticket := newModel(board.AgentWorking, false)
		delete(m.daemonOwned, tid)
		_, _ = m.Update(agentStatusResultMsg{tid: board.AgentIdle})
		if ticket.AgentStatus != board.AgentIdle {
			t.Errorf("AgentStatus = %v, want %v (non-owned must follow poll)",
				ticket.AgentStatus, board.AgentIdle)
		}
	})
}
