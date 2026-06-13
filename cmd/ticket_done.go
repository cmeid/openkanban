package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/techdufus/openkanban/internal/agent"
	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

// ticketDoneCmd is `openkanban ticket done`. It's designed to be
// invoked from inside a spawned Claude (or other agent) session to
// mark its ticket complete and signal the openkanban TUI to gracefully
// stop the pane — the agent-side "/quit equivalent."
//
// The ticket is identified by $OPENKANBAN_TICKET_ID (injected at spawn
// time, see internal/terminal/pane.go's buildCleanEnv). Unlike
// `openkanban status set`, which silently no-ops when its env var is
// missing, `ticket done` exits non-zero if $OPENKANBAN_TICKET_ID is
// unset — the env var being present IS the signal that this is an
// openkanban session.
var ticketDoneCmd = &cobra.Command{
	Use:   "done",
	Short: "Mark this session's ticket as done and signal openkanban to wrap up",
	Long: `Mark the ticket bound to the current openkanban session as done.

Reads $OPENKANBAN_TICKET_ID (set by openkanban when spawning the
session) and flips that ticket to Status=done + AgentStatus=completed.
If $OPENKANBAN_SESSION is also set, writes the session's status file so
the openkanban TUI sees the completion immediately and gracefully stops
the pane.

Exits non-zero if not run inside an openkanban session, or if the
ticket file referenced by $OPENKANBAN_TICKET_ID has been deleted.

Idempotent on a ticket that's already done — no second CompletedAt
timestamp, but the status file is re-written so a re-armed TUI can
still react.`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		ticketID := os.Getenv("OPENKANBAN_TICKET_ID")
		if ticketID == "" {
			return fmt.Errorf("$OPENKANBAN_TICKET_ID is not set; this command must be run from inside an openkanban-spawned session")
		}

		registry, err := project.LoadRegistry()
		if err != nil {
			return fmt.Errorf("load project registry: %w", err)
		}

		// Find which project owns this ticket. Ticket IDs are global
		// UUIDs, so first-match wins.
		ticket, store, found := findTicketAcrossProjects(registry, board.TicketID(ticketID))
		if !found {
			return fmt.Errorf("ticket %s not found in any project; was its .md file deleted?", ticketID)
		}

		// Idempotency: don't re-stamp CompletedAt on a second invocation.
		// SetStatus unconditionally overwrites CompletedAt = now, so we
		// skip the mutation when already Done. The status-file write
		// below still happens so the TUI's auto-stop transition is
		// re-armed for any pane that's somehow still alive.
		if ticket.Status != board.StatusDone {
			ticket.SetStatus(board.StatusDone)
			ticket.AgentStatus = board.AgentCompleted
			if err := store.SaveTicket(ticket); err != nil {
				return fmt.Errorf("save ticket %s: %w", ticketID, err)
			}
		}

		// $OPENKANBAN_SESSION may be empty (e.g. legacy spawns that
		// didn't set it). The status file is a fast hint to the TUI;
		// the .md write above is the authoritative source.
		if session := os.Getenv("OPENKANBAN_SESSION"); session != "" {
			if err := agent.WriteStatusFile(session, board.AgentCompleted); err != nil {
				return fmt.Errorf("write status file for session %q: %w", session, err)
			}
		}

		// Tell openkanbankd that this ticket is done so it can terminate
		// the live PTY and broadcast the expected-exit signal to other
		// TUIs. Strictly best-effort: a scripted CLI invocation must
		// NEVER autostart a daemon, and any failure here is downgraded
		// to a stderr warning — the .md + status-file writes above are
		// the authoritative ticket-done signal.
		notifyDaemonTicketDone(ticketID)

		return nil
	},
}

// notifyDaemonTicketDone makes a best-effort daemon RPC announcing the
// ticket-done event. All failure modes (daemon unreachable, dial timeout,
// hello failure, RPC failure) result in a single stderr line prefixed
// with "openkanbankd:" and exit code 0 — the on-disk writes performed
// before this call are authoritative.
//
// Timeouts are pinned tight (500ms dial / 2s overall RPC) so the agent's
// exit doesn't hang on a flaky daemon.
func notifyDaemonTicketDone(ticketID string) {
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer dialCancel()
	conn, err := daemonclient.Dial(dialCtx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "openkanbankd: %v\n", err)
		return
	}

	rpcCtx, rpcCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer rpcCancel()
	client, err := daemonclient.NewWithConn(rpcCtx, conn)
	if err != nil {
		_ = conn.Close()
		fmt.Fprintf(os.Stderr, "openkanbankd: hello failed: %v\n", err)
		return
	}
	defer client.Close()

	resp, err := client.TicketDone(rpcCtx, ticketID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "openkanbankd: %v\n", err)
		return
	}
	if !resp.Killed {
		fmt.Fprintf(os.Stderr, "openkanbankd: no live session for ticket %s (was it spawned by a different instance?)\n", ticketID)
	}
}

// findTicketAcrossProjects searches every project's ticket store for the
// ticket with the given id. Returns the ticket, its owning store, and
// true on hit; nil/nil/false on miss. Stores are loaded lazily — first
// match short-circuits the rest.
func findTicketAcrossProjects(registry *project.ProjectRegistry, id board.TicketID) (*board.Ticket, *project.TicketStore, bool) {
	for _, proj := range registry.List() {
		store, err := project.LoadTicketStore(proj)
		if err != nil {
			continue
		}
		if t, err := store.Get(id); err == nil {
			return t, store, true
		}
	}
	return nil, nil, false
}

func init() {
	ticketCmd.AddCommand(ticketDoneCmd)
}
