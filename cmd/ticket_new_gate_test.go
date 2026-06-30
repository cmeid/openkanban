package cmd

import (
	"strings"
	"testing"
)

// saveAndRestoreTicketNewFlags snapshots the package-level ticketNew* flag
// vars touched by the gate tests and restores them on t.Cleanup. Keeps
// tests hermetic without depending on a shared resetTicketNewFlags helper.
func saveAndRestoreTicketNewFlags(t *testing.T) {
	t.Helper()
	prev := struct {
		project, title, status string
		noWT, force            bool
	}{ticketNewProject, ticketNewTitle, ticketNewStatus, ticketNewNoWorktree, ticketNewForce}
	t.Cleanup(func() {
		ticketNewProject = prev.project
		ticketNewTitle = prev.title
		ticketNewStatus = prev.status
		ticketNewNoWorktree = prev.noWT
		ticketNewForce = prev.force
	})
}

// TestTicketNew_RefusedInProgressFromAgentSession verifies the hard gate:
// ticket new --status in_progress is refused from inside an agent session
// without --force.
// Red-before-green: commenting out the guardAgentStatusChange call in
// ticket.go must make this test fail.
func TestTicketNew_RefusedInProgressFromAgentSession(t *testing.T) {
	_, _, _ = scaffoldTicketDoneEnv(t) // registers "test-proj" in isolated config dir
	saveAndRestoreTicketNewFlags(t)

	t.Setenv("OPENKANBAN_SESSION", "agent-session")
	t.Setenv("OPENKANBAN_TICKET_ID", "00000000-0000-0000-0000-000000000099")

	ticketNewProject = "test-proj"
	ticketNewTitle = "gate-test-ticket"
	ticketNewStatus = "in_progress"
	ticketNewNoWorktree = true
	// ticketNewForce deliberately left false.

	err := ticketNewCmd.RunE(ticketNewCmd, nil)
	if err == nil {
		t.Fatal("expected gate error for --status in_progress from agent context; got nil")
	}
	if !strings.Contains(err.Error(), "refusing to set ticket status") {
		t.Errorf("expected gate error message; got: %v", err)
	}
}

// TestTicketNew_BacklogAllowedFromAgentSession is a regression guard: agents
// may file backlog sub-tasks; the gate must NOT block --status backlog.
func TestTicketNew_BacklogAllowedFromAgentSession(t *testing.T) {
	_, _, _ = scaffoldTicketDoneEnv(t)
	saveAndRestoreTicketNewFlags(t)

	t.Setenv("OPENKANBAN_SESSION", "agent-session")
	t.Setenv("OPENKANBAN_TICKET_ID", "00000000-0000-0000-0000-000000000099")

	ticketNewProject = "test-proj"
	ticketNewTitle = "backlog-sub-task"
	ticketNewStatus = "backlog"
	ticketNewNoWorktree = true

	if err := ticketNewCmd.RunE(ticketNewCmd, nil); err != nil {
		t.Fatalf("ticket new --status backlog from agent context should succeed; got: %v", err)
	}
}
