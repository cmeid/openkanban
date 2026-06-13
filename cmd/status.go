package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/techdufus/openkanban/internal/agent"
	"github.com/techdufus/openkanban/internal/board"
)

// statusCmd is the parent for status-reporting subcommands invoked from
// Claude Code hooks (or any other external process) that knows its
// openkanban session via the OPENKANBAN_SESSION env var.
var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Report session status to openkanban",
	Long: `Report the current state of this Claude Code (or other agent) session back
to openkanban by writing the session's status file under
~/.cache/openkanban-status/<session>.status.

These commands are designed to be invoked from Claude Code hooks. They
no-op silently if OPENKANBAN_SESSION is unset, so a globally-installed
hook is safe in unrelated Claude Code sessions.`,
}

// statusSetCmd writes the named state to the current session's status file.
//
// The five accepted states match agent.WriteStatusFile's mapping back to
// board.AgentStatus: working, idle, waiting, completed, error.
var statusSetCmd = &cobra.Command{
	Use:           "set <working|idle|waiting|completed|error>",
	Short:         "Set the current session's agent status",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		session := os.Getenv("OPENKANBAN_SESSION")
		if session == "" {
			// Hook installed globally; this isn't an openkanban session.
			// Silent no-op so we don't spam Claude Code's hook log.
			return nil
		}

		state := args[0]
		status, err := parseAgentStatus(state)
		if err != nil {
			return err
		}

		// Don't downgrade a terminal "completed" status. When the TUI auto-
		// stops the pane after `openkanban ticket done`, Claude's Stop hook
		// fires `status set idle` during the SIGTERM grace window — that
		// must not clobber the completion signal. Only `completed` or
		// `error` (terminal states) may overwrite `completed`.
		if status != board.AgentCompleted && status != board.AgentError {
			current, readErr := agent.ReadStatusFile(session)
			if readErr == nil && current == "completed" {
				return nil
			}
		}

		if err := agent.WriteStatusFile(session, status); err != nil {
			return fmt.Errorf("write status file for session %q: %w", session, err)
		}
		return nil
	},
}

// parseAgentStatus maps the CLI state string to a board.AgentStatus.
// Accepts exactly the five strings agent.WriteStatusFile knows how to
// serialize; anything else is a hard error so a typo'd hook fails loudly.
func parseAgentStatus(state string) (board.AgentStatus, error) {
	switch state {
	case "working":
		return board.AgentWorking, nil
	case "idle":
		return board.AgentIdle, nil
	case "waiting":
		return board.AgentWaiting, nil
	case "completed":
		return board.AgentCompleted, nil
	case "error":
		return board.AgentError, nil
	default:
		return "", fmt.Errorf("unknown status %q; want one of: working, idle, waiting, completed, error", state)
	}
}

func init() {
	statusCmd.AddCommand(statusSetCmd)
	rootCmd.AddCommand(statusCmd)
}
