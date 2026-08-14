package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/techdufus/openkanban/internal/agent"
	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/project"
)

// ticketDoneCmd is `openkanban ticket done`. It's designed to be
// invoked from inside a spawned Claude (or other agent) session to
// mark its ticket complete — the agent-side "hand it back" motion. It
// does NOT stop the session: the pane stays live so the user can
// re-enter the finished ticket and see what happened.
//
// The ticket is identified by $OPENKANBAN_TICKET_ID (injected at spawn
// time, see internal/terminal/pane.go's buildCleanEnv). Unlike
// `openkanban status set`, which silently no-ops when its env var is
// missing, `ticket done` exits non-zero if $OPENKANBAN_TICKET_ID is
// unset — the env var being present IS the signal that this is an
// openkanban session.
var ticketDoneForce bool

var ticketDoneCmd = &cobra.Command{
	Use:   "done",
	Short: "Mark this session's ticket as done (the session stays alive)",
	Long: `Mark the ticket bound to the current openkanban session as done.

Reads $OPENKANBAN_TICKET_ID (set by openkanban when spawning the
session) and flips that ticket to Status=done + AgentStatus=completed.
If $OPENKANBAN_SESSION is also set, writes the session's status file so
the openkanban TUI sees the completion immediately.

The live session is left running. Sessions are durable across every
status change, so pressing Enter on the done card re-attaches to the
same agent with its full transcript.

Exits non-zero if not run inside an openkanban session, or if the
ticket file referenced by $OPENKANBAN_TICKET_ID has been deleted.

Idempotent on a ticket that's already done — no second CompletedAt
timestamp, but the status file is re-written so a re-armed TUI can
still react.

When run from inside a spawned agent session (OPENKANBAN_SESSION or
OPENKANBAN_TICKET_ID is set), this command is refused unless --force is
passed. Tickets are advanced by the user after reviewing agent work,
not by the agent itself.`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return wrapUpSessionTicketAt(board.StatusDone, ticketDoneForce)
	},
}

// wrapUpSessionTicketAt promotes the env-bound ticket to target,
// leaving the live PTY alone. `ticket done` (target=StatusDone) is its
// only caller today; the target parameter survives from the removed
// `ticket in-review` verb, which shared the identical side effects
// (AgentStatus flip, status-file write) and would again if a second
// "the agent is handing off" transition is ever added.
//
// Idempotent on a ticket already at target: SetStatus is skipped (so
// SetStatus's CompletedAt re-stamp for done, and the UpdatedAt drift
// for any other target, don't fire on repeats), but the status-file
// write still runs so the TUI re-reads the completion.
//
// Caller-visible behavior contract:
//   - $OPENKANBAN_TICKET_ID must be set; loadSessionTicket returns an
//     error otherwise.
//   - .md write is authoritative. Status-file write is best-effort
//     (skipped when $OPENKANBAN_SESSION is empty).
//   - The daemon is not contacted at all, so a scripted invocation
//     never autostarts one and the session is never torn down.
func wrapUpSessionTicketAt(target board.TicketStatus, force bool) error {
	ticket, store, err := loadSessionTicket()
	if err != nil {
		return err
	}

	// Idempotency: don't re-stamp timestamps on a repeat invocation.
	// SetStatus unconditionally overwrites CompletedAt (for done) /
	// UpdatedAt (for both), so skip the mutation when already at
	// target. The status-file write below still happens so the TUI's
	// status poll re-reads the completion through the still-live pane.
	if ticket.Status != target {
		if err := guardAgentStatusChange(target, force); err != nil {
			return err
		}
		// Route through Move so any claude-code approvals collected in
		// this worktree get promoted to the repo's settings.local.json
		// before the worktree (and the agent's permission scope with
		// it) is dismantled. Move is a thin wrapper over SetStatus —
		// the AgentStatus update and the SaveTicket below land
		// authoritatively after.
		promoted, pruned, err := store.Move(ticket.ID, target)
		if err != nil {
			return fmt.Errorf("move ticket %s: %w", ticket.ID, err)
		}
		ticket.SetAgentStatus(board.AgentCompleted)
		if err := store.SaveTicket(ticket); err != nil {
			return fmt.Errorf("save ticket %s: %w", ticket.ID, err)
		}
		if n := len(promoted); n > 0 {
			fmt.Fprintf(os.Stderr, "openkanban: promoted %d claude approval(s) to repo defaults\n", n)
		}
		if n := len(pruned); n > 0 {
			fmt.Fprintf(os.Stderr, "openkanban: pruned %d stale allowlist entr(y/ies) (see .claude/.pruned-log)\n", n)
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

	// No daemon notification, and deliberately no PTY teardown. This is
	// a status change like any other, and openkanban's top-level
	// guarantee is exactly one DURABLE session per ticket: pressing
	// Enter on the now-done card must land the user back in the same
	// live agent with its scrollback intact. The .md write and the
	// status-file write above are what propagate completion; because
	// the session survives, the TUI's pollAgentStatusesAsync keeps
	// reading that status file through the still-live pane, so the
	// badge lands without a daemon event.
	//
	// (This used to send TicketDone, which killed the PTY. Sessions now
	// end only on ticket/project delete, explicit 'x' in the TUI, the
	// exit guard, or the agent's own exit — the agent calling this
	// command is not its own exit; the process keeps running.)

	return nil
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
	ticketDoneCmd.Flags().BoolVar(&ticketDoneForce, "force", false,
		"allow status change from inside an agent session (only use when the user explicitly asked)")
}
