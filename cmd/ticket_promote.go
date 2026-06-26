package cmd

import (
	"fmt"
	"os"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/project"
)

// loadSessionTicket resolves the ticket bound to the current openkanban
// session via $OPENKANBAN_TICKET_ID. Used by ticket-done, which is
// the "promote my session's ticket to a new status" motion that also
// signals the daemon to wrap up the live PTY.
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
