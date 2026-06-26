package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/project"
)

var (
	ticketListProject       string
	ticketListStatus        []string
	ticketListTitleContains string
	ticketListJSON          bool
)

// ticketListItem is the stable --json schema for `ticket list`. Every field
// is always present (no omitempty) so machine consumers can rely on the shape
// regardless of a ticket's state; Labels is always an array (never null), and
// timestamps are RFC3339. It is deliberately decoupled from board.Ticket so
// future board fields don't silently leak into the documented CLI contract.
type ticketListItem struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Status         string   `json:"status"`
	ProjectID      string   `json:"project_id"`
	BranchName     string   `json:"branch_name"`
	AgentSessionID string   `json:"agent_session_id"`
	WorktreePath   string   `json:"worktree_path"`
	Priority       int      `json:"priority"`
	Labels         []string `json:"labels"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
}

func newTicketListItem(tk *board.Ticket) ticketListItem {
	labels := tk.Labels
	if labels == nil {
		labels = []string{}
	}
	return ticketListItem{
		ID:             string(tk.ID),
		Title:          tk.Title,
		Status:         string(tk.Status),
		ProjectID:      tk.ProjectID,
		BranchName:     tk.BranchName,
		AgentSessionID: tk.AgentSessionID,
		WorktreePath:   tk.WorktreePath,
		Priority:       tk.Priority,
		Labels:         labels,
		CreatedAt:      tk.CreatedAt.Format(time.RFC3339),
		UpdatedAt:      tk.UpdatedAt.Format(time.RFC3339),
	}
}

var ticketListCmd = &cobra.Command{
	Use:   "list",
	Short: "List tickets across projects",
	Long: `Enumerate tickets, optionally filtered by project, status, or title.

With no filters, lists every ticket across all registered projects in
all statuses (sorted by most-recently-updated). This is the canonical
id-discovery path: the short id shown here (and the full "id" in --json)
is the value the --id verbs (e.g. ticket delete) expect.

Filters:
  --project          name, UUID, or unique 4+ char UUID prefix
  --status           one or more of backlog,next,in_progress,in_review,
                     done,archived (comma-separated or repeated)
  --title-contains   case-insensitive substring match on the title

This command is READ-ONLY: it never migrates a project. Projects whose
storage is still migration-pending are skipped (with a note on stderr)
rather than migrated out from under a possibly-running TUI.

Use --json for a stable, machine-readable array; every object has the
same keys (Labels is always an array, timestamps are RFC3339).`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Validate and collect the status filter, reusing the same enum
		// set ticket new enforces.
		statusFilter := map[board.TicketStatus]bool{}
		for _, raw := range ticketListStatus {
			s := strings.TrimSpace(raw)
			if s == "" {
				continue
			}
			ts, err := board.ParseStatus(s)
			if err != nil {
				return fmt.Errorf("--status %s", err)
			}
			statusFilter[ts] = true
		}

		registry, err := project.LoadRegistry()
		if err != nil {
			return fmt.Errorf("load project registry: %w", err)
		}

		var projects []*project.Project
		if ticketListProject != "" {
			p, err := resolveProject(registry, ticketListProject)
			if err != nil {
				return err
			}
			projects = []*project.Project{p}
		} else {
			projects = registry.List()
		}

		// Read-only enumeration. LoadTicketStore migrates a project as a
		// side effect (MigrateProjectToPerTicket), so only load projects
		// whose state is a guaranteed no-op: Complete or NotNeeded. Skip
		// Pending AND InProgressOrphan — the latter's migration deletes a
		// .migrating workspace, which a read command must never trigger.
		projName := map[string]string{}
		var tickets []*board.Ticket
		skipped := 0
		for _, p := range projects {
			switch project.MigrationStateFor(p.ID) {
			case project.MigrationComplete, project.MigrationNotNeeded:
				// safe — loading these never mutates disk
			default:
				skipped++
				continue
			}
			store, err := project.LoadTicketStore(p)
			if err != nil {
				// Surface the omission rather than vanishing the project
				// silently; one bad project shouldn't abort the listing.
				fmt.Fprintf(os.Stderr, "note: skipped project %q (%s): %v\n", p.Name, shortID(p.ID), err)
				continue
			}
			projName[p.ID] = p.Name
			tickets = append(tickets, store.All()...)
		}

		q := strings.ToLower(strings.TrimSpace(ticketListTitleContains))
		var rows []*board.Ticket
		for _, tk := range tickets {
			if len(statusFilter) > 0 && !statusFilter[tk.Status] {
				continue
			}
			if q != "" && !strings.Contains(strings.ToLower(tk.Title), q) {
				continue
			}
			rows = append(rows, tk)
		}

		sort.SliceStable(rows, func(i, j int) bool {
			if !rows[i].UpdatedAt.Equal(rows[j].UpdatedAt) {
				return rows[i].UpdatedAt.After(rows[j].UpdatedAt)
			}
			return rows[i].Title < rows[j].Title
		})

		if skipped > 0 {
			fmt.Fprintf(os.Stderr, "note: skipped %d migration-pending project(s) (launch openkanban to migrate)\n", skipped)
		}

		if ticketListJSON {
			items := make([]ticketListItem, 0, len(rows))
			for _, tk := range rows {
				items = append(items, newTicketListItem(tk))
			}
			enc, err := json.MarshalIndent(items, "", "  ")
			if err != nil {
				return fmt.Errorf("marshal json: %w", err)
			}
			fmt.Println(string(enc))
			return nil
		}

		if len(rows) == 0 {
			fmt.Println("(no tickets)")
			return nil
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tTITLE\tSTATUS\tPROJECT\tUPDATED")
		for _, tk := range rows {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				shortID(string(tk.ID)),
				truncateTitle(tk.Title, 50),
				tk.Status,
				projName[tk.ProjectID],
				tk.UpdatedAt.Format("2006-01-02"),
			)
		}
		return tw.Flush()
	},
}

// truncateTitle shortens a title for the human table, appending an ellipsis
// when it overflows. Width counts runes so multibyte titles don't over-clip.
func truncateTitle(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}

func init() {
	ticketCmd.AddCommand(ticketListCmd)

	ticketListCmd.Flags().StringVar(&ticketListProject, "project", "",
		"Filter by project: name, UUID, or unique 4+ char UUID prefix")
	ticketListCmd.Flags().StringSliceVar(&ticketListStatus, "status", nil,
		"Filter by status (comma-separated or repeated): backlog,next,in_progress,in_review,done,archived")
	ticketListCmd.Flags().StringVar(&ticketListTitleContains, "title-contains", "",
		"Filter to tickets whose title contains this substring (case-insensitive)")
	ticketListCmd.Flags().BoolVar(&ticketListJSON, "json", false,
		"Emit a stable JSON array instead of a human-readable table")
}
