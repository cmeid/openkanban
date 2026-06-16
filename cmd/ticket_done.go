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
		return wrapUpSessionTicketAt(board.StatusDone)
	},
}

// wrapUpSessionTicketAt promotes the env-bound ticket to target and
// terminates the live PTY, mirroring the agent-side "/quit equivalent"
// motion. Used by both `ticket done` (target=StatusDone) and `ticket
// in-review` (target=StatusInReview) — both are "the agent is handing
// off, kill the session" transitions and the side effects (AgentStatus
// flip, status-file write, daemon notification) match exactly.
//
// Idempotent on a ticket already at target: SetStatus is skipped (so
// SetStatus's CompletedAt re-stamp for done, and the UpdatedAt drift
// for in-review, don't fire on repeats), but the status-file and
// daemon notification still run so a re-armed TUI can react.
//
// Caller-visible behavior contract:
//   - $OPENKANBAN_TICKET_ID must be set; loadSessionTicket returns an
//     error otherwise.
//   - .md write is authoritative. Status-file write is best-effort
//     (skipped when $OPENKANBAN_SESSION is empty).
//   - notifyDaemonTicketDone is best-effort and never fails the
//     command — daemon outages downgrade to a stderr warning.
func wrapUpSessionTicketAt(target board.TicketStatus) error {
	ticket, store, err := loadSessionTicket()
	if err != nil {
		return err
	}

	// Idempotency: don't re-stamp timestamps on a repeat invocation.
	// SetStatus unconditionally overwrites CompletedAt (for done) /
	// UpdatedAt (for both), so skip the mutation when already at
	// target. The status-file write below still happens so the TUI's
	// auto-stop transition is re-armed for any pane that's somehow
	// still alive.
	if ticket.Status != target {
		// Route through Move so any claude-code approvals collected in
		// this worktree get promoted to the repo's settings.local.json
		// before the worktree (and the agent's permission scope with
		// it) is dismantled. Move is a thin wrapper over SetStatus —
		// the AgentStatus update and the SaveTicket below land
		// authoritatively after.
		promoted, err := store.Move(ticket.ID, target)
		if err != nil {
			return fmt.Errorf("move ticket %s: %w", ticket.ID, err)
		}
		ticket.AgentStatus = board.AgentCompleted
		if err := store.SaveTicket(ticket); err != nil {
			return fmt.Errorf("save ticket %s: %w", ticket.ID, err)
		}
		if n := len(promoted); n > 0 {
			fmt.Fprintf(os.Stderr, "openkanban: promoted %d claude approval(s) to repo defaults\n", n)
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

	// Tell openkanbankd that this ticket is wrapping up so it can
	// terminate the live PTY and broadcast the expected-exit signal
	// to other TUIs. Strictly best-effort: a scripted CLI invocation
	// must NEVER autostart a daemon, and any failure here is
	// downgraded to a stderr warning — the .md + status-file writes
	// above are the authoritative wrap-up signal.
	notifyDaemonTicketDone(string(ticket.ID))

	return nil
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
