package cmd

import (
	"fmt"
	"os"

	"github.com/techdufus/openkanban/internal/board"
)

// inAgentSession reports whether the current process is running inside
// an openkanban-spawned agent session. The daemon injects
// OPENKANBAN_SESSION for every spawn and OPENKANBAN_TICKET_ID for
// ticket spawns (internal/terminal/pane.go:buildCleanEnv). A human's
// TUI or shell has neither. Either present ⇒ agent context.
func inAgentSession() bool {
	return os.Getenv("OPENKANBAN_SESSION") != "" || os.Getenv("OPENKANBAN_TICKET_ID") != ""
}

// guardAgentStatusChange refuses an agent-initiated ticket status
// change unless the user explicitly authorized it with --force. A
// human context (no agent env vars) is always allowed. Returns nil
// when the change may proceed.
func guardAgentStatusChange(target board.TicketStatus, force bool) error {
	if force || !inAgentSession() {
		return nil
	}
	return fmt.Errorf(
		"refusing to set ticket status to %q from inside an agent session: "+
			"openkanban tickets are advanced by the user after review, not by the agent. "+
			"If the user explicitly asked you to do this, re-run with --force",
		target,
	)
}
