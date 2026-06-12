package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"

	"github.com/techdufus/openkanban/internal/board"
)

// MigrationState captures whether a project's tickets are stored in the
// new per-ticket layout, the legacy single-JSON layout, or somewhere
// in between (an orphaned .migrating/ workspace from an interrupted run).
type MigrationState int

const (
	// MigrationNotNeeded: neither the legacy JSON nor the per-ticket
	// directory exists. Fresh project, nothing to do.
	MigrationNotNeeded MigrationState = iota
	// MigrationPending: legacy JSON exists; per-ticket directory does
	// not. Caller should run MigrateProjectToPerTicket.
	MigrationPending
	// MigrationInProgressOrphan: a previous migration was interrupted,
	// leaving a {project_id}.migrating/ workspace on disk. The next
	// migration call removes it before retrying.
	MigrationInProgressOrphan
	// MigrationComplete: per-ticket directory exists and legacy JSON
	// is gone. No-op.
	MigrationComplete
)

func (s MigrationState) String() string {
	switch s {
	case MigrationNotNeeded:
		return "not-needed"
	case MigrationPending:
		return "pending"
	case MigrationInProgressOrphan:
		return "in-progress-orphan"
	case MigrationComplete:
		return "complete"
	}
	return "unknown"
}

// MigrationStateFor inspects disk and reports the current state of
// migration for the given project. Read-only.
func MigrationStateFor(projectID string) MigrationState {
	legacyPath := legacyJSONPath(projectID)
	dirPath := perTicketDirPath(projectID)
	migratingPath := migratingWorkspacePath(projectID)

	_, dirErr := os.Stat(dirPath)
	dirExists := dirErr == nil

	_, legacyErr := os.Stat(legacyPath)
	legacyExists := legacyErr == nil

	_, migratingErr := os.Stat(migratingPath)
	migratingExists := migratingErr == nil

	if migratingExists {
		return MigrationInProgressOrphan
	}
	if dirExists && !legacyExists {
		return MigrationComplete
	}
	if legacyExists {
		return MigrationPending
	}
	return MigrationNotNeeded
}

// MigrateProjectToPerTicket converts a legacy tickets/{projectID}.json
// into per-ticket Markdown files under tickets/{projectID}/. It is
// idempotent: calling on a project already migrated is a no-op.
//
// On success:
//   - tickets/{projectID}/ contains one .md per ticket
//   - tickets/{projectID}.json is renamed to tickets/{projectID}.json.migrated
//     (kept as a one-shot rollback artifact)
//
// On failure: the source JSON is preserved, the partial workspace at
// tickets/{projectID}.migrating/ may be left behind for diagnosis;
// the next call cleans the orphan and retries.
//
// Returns the count of tickets migrated, or 0 if no migration was
// needed.
func MigrateProjectToPerTicket(projectID string) (int, error) {
	state := MigrationStateFor(projectID)

	switch state {
	case MigrationComplete, MigrationNotNeeded:
		return 0, nil
	case MigrationInProgressOrphan:
		if err := os.RemoveAll(migratingWorkspacePath(projectID)); err != nil {
			return 0, fmt.Errorf("clean orphan migrating workspace: %w", err)
		}
		// Re-evaluate after cleanup.
		if MigrationStateFor(projectID) != MigrationPending {
			return 0, nil
		}
		fallthrough
	case MigrationPending:
		return runMigration(projectID)
	}

	return 0, fmt.Errorf("unknown migration state %v", state)
}

// runMigration executes a Pending migration. Assumes no orphan
// workspace exists.
func runMigration(projectID string) (int, error) {
	legacyPath := legacyJSONPath(projectID)
	workspace := migratingWorkspacePath(projectID)
	finalDir := perTicketDirPath(projectID)

	data, err := os.ReadFile(legacyPath)
	if err != nil {
		return 0, fmt.Errorf("read legacy store: %w", err)
	}

	// Unmarshal into a TicketStore-shaped struct directly (avoid
	// pulling in TicketStore here; this is just a wire format).
	var legacy struct {
		ProjectID string                           `json:"project_id"`
		Tickets   map[board.TicketID]*board.Ticket `json:"tickets"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return 0, fmt.Errorf("parse legacy store: %w", err)
	}

	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return 0, fmt.Errorf("create migrating workspace: %w", err)
	}
	// Cleanup-on-failure: any error below removes the workspace.
	cleanupOnFail := true
	defer func() {
		if cleanupOnFail {
			_ = os.RemoveAll(workspace)
		}
	}()

	// Write each ticket as a .md file. Sort by ID for deterministic
	// output (helps tests and diffs).
	ids := make([]board.TicketID, 0, len(legacy.Tickets))
	for id := range legacy.Tickets {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	written := 0
	for _, id := range ids {
		ticket := legacy.Tickets[id]
		if ticket == nil {
			continue
		}
		// Backfill ProjectID from the store wrapper if missing.
		if ticket.ProjectID == "" {
			ticket.ProjectID = legacy.ProjectID
		}
		blob, marr := MarshalTicket(ticket)
		if marr != nil {
			return 0, fmt.Errorf("marshal ticket %s: %w", ticket.ID, marr)
		}
		fname := TicketFilename(ticket)
		dest := filepath.Join(workspace, fname)
		if werr := os.WriteFile(dest, blob, 0o644); werr != nil {
			return 0, fmt.Errorf("write ticket %s: %w", ticket.ID, werr)
		}
		if verr := validateRoundTrip(ticket, dest); verr != nil {
			return 0, fmt.Errorf("validate ticket %s: %w", ticket.ID, verr)
		}
		written++
	}

	// Promote workspace → final dir. If finalDir already exists empty
	// (a prior `openkanban list` / `LoadTicketStore` may have created
	// it before the legacy JSON appeared), remove it so the rename can
	// proceed. If it exists non-empty, refuse — silently merging would
	// risk losing either the existing per-ticket files or the legacy
	// data.
	if entries, statErr := os.ReadDir(finalDir); statErr == nil {
		if len(entries) > 0 {
			return 0, fmt.Errorf("final dir %s already exists and is non-empty; refusing to merge automatically — inspect and resolve manually", finalDir)
		}
		if err := os.Remove(finalDir); err != nil {
			return 0, fmt.Errorf("remove empty final dir before rename: %w", err)
		}
	}
	if err := os.Rename(workspace, finalDir); err != nil {
		return 0, fmt.Errorf("promote workspace to final dir: %w", err)
	}
	// From here on, even if .migrated rename fails, the migration
	// succeeded structurally — don't clean up the workspace anymore
	// (it's been renamed away).
	cleanupOnFail = false

	migratedPath := legacyPath + ".migrated"
	if err := os.Rename(legacyPath, migratedPath); err != nil {
		// Roll forward: legacy JSON is now stale (final dir is the truth).
		// We'd rather leave the legacy file in place than error here;
		// caller can manually remove it.
		log.Printf("openkanban: migrated %d tickets for project %s but failed to rename legacy json to .migrated (%v); the per-ticket dir is the source of truth.", written, projectID, err)
		return written, nil
	}

	log.Printf("openkanban: migrated %d tickets for project %s. Rollback artifact at %s", written, projectID, migratedPath)
	return written, nil
}

// validateRoundTrip re-reads a freshly-written .md and checks the
// load-bearing field invariants against the source ticket. Avoids
// reflect.DeepEqual because (a) YAML round-trip drops monotonic time
// data and (b) []string{} vs nil normalisation is asymmetric.
func validateRoundTrip(src *board.Ticket, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read back: %w", err)
	}
	got, err := UnmarshalTicket(data)
	if err != nil {
		return fmt.Errorf("parse back: %w", err)
	}
	if got.ID != src.ID {
		return fmt.Errorf("id mismatch: got %q, want %q", got.ID, src.ID)
	}
	if got.Title != src.Title {
		return fmt.Errorf("title mismatch: got %q, want %q", got.Title, src.Title)
	}
	if got.Status != src.Status {
		return fmt.Errorf("status mismatch: got %q, want %q", got.Status, src.Status)
	}
	if got.ProjectID != src.ProjectID {
		return fmt.Errorf("project_id mismatch: got %q, want %q", got.ProjectID, src.ProjectID)
	}
	if len(got.Labels) != len(src.Labels) {
		return fmt.Errorf("labels length mismatch: got %d, want %d", len(got.Labels), len(src.Labels))
	}
	if len(got.BlockedBy) != len(src.BlockedBy) {
		return fmt.Errorf("blocked_by length mismatch: got %d, want %d", len(got.BlockedBy), len(src.BlockedBy))
	}
	return nil
}

// Path helpers — single source of truth for migration-related paths.

func legacyJSONPath(projectID string) string {
	return filepath.Join(ticketsDir(), projectID+".json")
}

func perTicketDirPath(projectID string) string {
	return filepath.Join(ticketsDir(), projectID)
}

func migratingWorkspacePath(projectID string) string {
	return filepath.Join(ticketsDir(), projectID+".migrating")
}

// Sentinel for callers that want to distinguish "I tried but the
// source was malformed" from other errors.
var ErrMigrationSourceCorrupt = errors.New("migration: source JSON is malformed")
