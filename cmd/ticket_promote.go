package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/project"
)

// loadSessionTicket resolves the ticket bound to the current openkanban
// session via $OPENKANBAN_TICKET_ID. Shared by ticket-done /
// ticket-in-progress / ticket-in-review, which are all "promote my
// session's ticket to a new status" motions.
//
// Errors when the env var is unset (this command isn't running inside
// a spawned session) or when the referenced ticket can't be found in
// any project store (the .md was deleted out from under us).
func loadSessionTicket() (*board.Ticket, *project.TicketStore, error) {
	ticketID := os.Getenv("OPENKANBAN_TICKET_ID")
	if ticketID == "" {
		return nil, nil, fmt.Errorf("$OPENKANBAN_TICKET_ID is not set; this command must be run from inside an openkanban-spawned session")
	}

	registry, err := project.LoadRegistry()
	if err != nil {
		return nil, nil, fmt.Errorf("load project registry: %w", err)
	}

	ticket, store, found := findTicketAcrossProjects(registry, board.TicketID(ticketID))
	if !found {
		return nil, nil, fmt.Errorf("ticket %s not found in any project; was its .md file deleted?", ticketID)
	}
	return ticket, store, nil
}

// promoteSessionTicketTo applies a status transition to the env-bound
// ticket. Idempotent: skips SetStatus (and the resulting Touch /
// StartedAt restamp) when the ticket is already at the target. Returns
// nil on either a fresh transition or a no-op — callers can layer
// status-specific side effects (e.g. agent_status writes, daemon
// notifications) on top.
//
// Used by `ticket in-progress` and `ticket in-review`. `ticket done`
// has additional invariants (AgentStatus=completed, status-file write,
// daemon-side PTY shutdown) and so reuses loadSessionTicket directly
// rather than going through this helper.
func promoteSessionTicketTo(target board.TicketStatus) error {
	ticket, store, err := loadSessionTicket()
	if err != nil {
		return err
	}
	if ticket.Status == target {
		return nil
	}
	ticket.SetStatus(target)
	if err := store.SaveTicket(ticket); err != nil {
		return fmt.Errorf("save ticket %s: %w", ticket.ID, err)
	}
	return nil
}

var ticketInProgressCmd = &cobra.Command{
	Use:   "in-progress",
	Short: "Move this session's ticket to in-progress",
	Long: `Flip the ticket bound to the current openkanban session to
Status=in_progress. Reads $OPENKANBAN_TICKET_ID (set by openkanban
when spawning the session); exits non-zero if unset.

Use this when a session needs to flag itself as actively working on
the ticket — e.g. an agent that just resumed from a paused state and
wants the board to reflect that. AgentStatus is left untouched; only
the column position changes.

Idempotent on a ticket already in_progress (no second StartedAt
restamp). Unlike 'ticket done', this command does NOT signal the
daemon — the live PTY keeps running.`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return promoteSessionTicketTo(board.StatusInProgress)
	},
}

var ticketInReviewCmd = &cobra.Command{
	Use:   "in-review",
	Short: "Move this session's ticket to in-review",
	Long: `Flip the ticket bound to the current openkanban session to
Status=in_review. Reads $OPENKANBAN_TICKET_ID (set by openkanban
when spawning the session); exits non-zero if unset.

Use this when a session has finished its work but is handing the
ticket off for human review rather than marking it done. AgentStatus
is left untouched; only the column position changes.

Idempotent on a ticket already in_review. Unlike 'ticket done', this
command does NOT signal the daemon — the live PTY keeps running so
the reviewer can ask follow-up questions in the same session.`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return promoteSessionTicketTo(board.StatusInReview)
	},
}

func init() {
	ticketCmd.AddCommand(ticketInProgressCmd)
	ticketCmd.AddCommand(ticketInReviewCmd)
}
