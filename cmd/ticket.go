package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/project"
)

var (
	ticketNewProject         string
	ticketNewTitle           string
	ticketNewDescription     string
	ticketNewDescriptionFile string
	ticketNewStatus          string
	ticketNewLabels          string
	ticketNewPriority        int
	ticketNewNoWorktree      bool
	ticketNewAllowMigration  bool
)

var ticketCmd = &cobra.Command{
	Use:   "ticket",
	Short: "Manage tickets from the command line",
	Long: `Create and (eventually) edit tickets without launching the TUI.

The primary use case is scripted ticket creation from another agent
or session — e.g. a parent Claude session that wants to spin off a
subtask as its own openkanban ticket and pass the resulting file
path to a child session for context.`,
}

var ticketNewCmd = &cobra.Command{
	Use:   "new",
	Short: "Create a new ticket",
	Long: `Create a new ticket in a project and print the path to the resulting .md file.

The --project flag accepts an exact project name, an exact UUID, or a
unique UUID prefix of at least 4 characters. On ambiguous prefix the
command exits non-zero and lists the candidates.

If the project still uses legacy single-file storage (tickets/<id>.json),
ticket new refuses to migrate it on its own so it can't race a running
TUI mid-migration. Launch the TUI once first to migrate, or pass
--allow-migration to migrate here.

Description sources (mutually exclusive, in priority order):
  1. --description "<inline text>"
  2. --description-file <path>
  3. stdin, if piped (i.e. not a TTY)
  4. empty`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		title := strings.TrimSpace(ticketNewTitle)
		if title == "" {
			return fmt.Errorf("--title must not be empty")
		}
		if ticketNewProject == "" {
			return fmt.Errorf("--project is required")
		}
		if ticketNewPriority < 0 || ticketNewPriority > 5 {
			return fmt.Errorf("--priority must be 0-5 (0 = use default)")
		}
		if ticketNewStatus != "" {
			switch board.TicketStatus(ticketNewStatus) {
			case board.StatusBacklog, board.StatusInProgress, board.StatusDone, board.StatusArchived:
			default:
				return fmt.Errorf("--status %q is not one of: backlog, in_progress, done, archived", ticketNewStatus)
			}
		}

		registry, err := project.LoadRegistry()
		if err != nil {
			return fmt.Errorf("load project registry: %w", err)
		}

		proj, err := resolveProject(registry, ticketNewProject)
		if err != nil {
			return err
		}

		state := project.MigrationStateFor(proj.ID)
		if state == project.MigrationPending && !ticketNewAllowMigration {
			return fmt.Errorf("project %q (%s) has legacy single-file ticket storage; "+
				"launch openkanban once to migrate it, or re-run with --allow-migration",
				proj.Name, shortID(proj.ID))
		}

		store, err := project.LoadTicketStore(proj)
		if err != nil {
			return fmt.Errorf("load ticket store: %w", err)
		}

		desc, err := resolveTicketDescription()
		if err != nil {
			return err
		}

		ticket := board.NewTicket(title, proj.ID)
		ticket.Description = desc
		if ticketNewStatus != "" {
			ticket.Status = board.TicketStatus(ticketNewStatus)
		}
		if ticketNewPriority > 0 {
			ticket.Priority = ticketNewPriority
		}
		if ticketNewLabels != "" {
			for _, l := range strings.Split(ticketNewLabels, ",") {
				l = strings.TrimSpace(l)
				if l != "" {
					ticket.Labels = append(ticket.Labels, l)
				}
			}
		}
		if ticketNewNoWorktree {
			ticket.UseWorktree = false
		}

		if err := store.SaveTicket(ticket); err != nil {
			return fmt.Errorf("save ticket: %w", err)
		}

		// Reproduce SaveTicket's path computation so the CLI doesn't
		// have to depend on private fields of TicketStore.
		ticketsRoot, err := configTicketsDir()
		if err != nil {
			return err
		}
		path := filepath.Join(ticketsRoot, proj.ID, project.TicketFilename(ticket))
		fmt.Println(path)
		return nil
	},
}

// resolveProject matches the CLI --project arg against the registry.
//
// Match precedence:
//   1. exact name match
//   2. exact UUID match
//   3. unique UUID prefix (min 4 chars)
//
// Ambiguous prefix and zero-match both return errors with hints.
func resolveProject(reg *project.ProjectRegistry, arg string) (*project.Project, error) {
	if arg == "" {
		return nil, fmt.Errorf("--project value is empty")
	}

	var exact, prefix []*project.Project
	for _, p := range reg.List() {
		if p.Name == arg || p.ID == arg {
			exact = append(exact, p)
			continue
		}
		if len(arg) >= 4 && strings.HasPrefix(p.ID, arg) {
			prefix = append(prefix, p)
		}
	}

	if len(exact) == 1 {
		return exact[0], nil
	}
	if len(exact) > 1 {
		return nil, fmt.Errorf("multiple projects match %q exactly:\n%s",
			arg, formatProjectMatches(exact))
	}
	if len(prefix) == 1 {
		return prefix[0], nil
	}
	if len(prefix) > 1 {
		return nil, fmt.Errorf("project prefix %q is ambiguous (%d matches); specify more characters:\n%s",
			arg, len(prefix), formatProjectMatches(prefix))
	}
	return nil, fmt.Errorf("no project matches %q; run 'openkanban list' to see available projects", arg)
}

func formatProjectMatches(ps []*project.Project) string {
	lines := make([]string, 0, len(ps))
	for _, p := range ps {
		lines = append(lines, fmt.Sprintf("  %s  %s", shortID(p.ID), p.Name))
	}
	return strings.Join(lines, "\n")
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func resolveTicketDescription() (string, error) {
	if ticketNewDescription != "" && ticketNewDescriptionFile != "" {
		return "", fmt.Errorf("--description and --description-file are mutually exclusive")
	}
	if ticketNewDescription != "" {
		return ticketNewDescription, nil
	}
	if ticketNewDescriptionFile != "" {
		data, err := os.ReadFile(ticketNewDescriptionFile)
		if err != nil {
			return "", fmt.Errorf("read description file %q: %w", ticketNewDescriptionFile, err)
		}
		return string(data), nil
	}
	// Stdin fallback: only consume if piped (i.e. not a TTY).
	stat, err := os.Stdin.Stat()
	if err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
		data, rerr := io.ReadAll(os.Stdin)
		if rerr != nil {
			return "", fmt.Errorf("read stdin: %w", rerr)
		}
		return string(data), nil
	}
	return "", nil
}

// configTicketsDir replicates internal/project.ticketsDir() without
// importing the unexported helper. Keeps the CLI's view of the
// ticket root in sync with what the store writes.
func configTicketsDir() (string, error) {
	return ticketsRootDir()
}

func init() {
	ticketCmd.AddCommand(ticketNewCmd)

	ticketNewCmd.Flags().StringVar(&ticketNewProject, "project", "",
		"Project name, UUID, or unique 4+ char UUID prefix (required)")
	ticketNewCmd.Flags().StringVar(&ticketNewTitle, "title", "",
		"Ticket title (required)")
	ticketNewCmd.Flags().StringVar(&ticketNewDescription, "description", "",
		"Ticket description (markdown body of the resulting file)")
	ticketNewCmd.Flags().StringVar(&ticketNewDescriptionFile, "description-file", "",
		"Read description from this file path instead of --description")
	ticketNewCmd.Flags().StringVar(&ticketNewStatus, "status", "",
		"Initial status: backlog (default), in_progress, done, archived")
	ticketNewCmd.Flags().StringVar(&ticketNewLabels, "labels", "",
		"Comma-separated labels")
	ticketNewCmd.Flags().IntVar(&ticketNewPriority, "priority", 0,
		"Priority 1-5 (0 = use default, which is 3)")
	ticketNewCmd.Flags().BoolVar(&ticketNewNoWorktree, "no-worktree", false,
		"Don't use a git worktree for this ticket")
	ticketNewCmd.Flags().BoolVar(&ticketNewAllowMigration, "allow-migration", false,
		"Allow migrating legacy single-file ticket storage instead of refusing")

	_ = ticketNewCmd.MarkFlagRequired("project")
	_ = ticketNewCmd.MarkFlagRequired("title")

	rootCmd.AddCommand(ticketCmd)
}
