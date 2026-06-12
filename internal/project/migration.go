package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

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

	// Stale-snapshot escape: if the per-ticket dir already exists with
	// content AND the legacy JSON is a strict-subset older snapshot of
	// the dir's state, the migration is unneeded. This typically
	// happens when an old binary (built before the per-ticket schema
	// existed) was accidentally launched and wrote out a stale
	// snapshot of its in-memory store. Rename the legacy JSON aside
	// to .stale-<ts> and return without doing any work.
	if isStaleSnapshot(legacy.Tickets, finalDir) {
		stalePath := legacyPath + ".stale-" + strconv.FormatInt(time.Now().Unix(), 10)
		if err := os.Rename(legacyPath, stalePath); err != nil {
			return 0, fmt.Errorf("rename stale snapshot aside: %w", err)
		}
		log.Printf("openkanban: legacy %s was a stale subset of the per-ticket dir for project %s; renamed to %s (per-ticket dir is the source of truth)", filepath.Base(legacyPath), projectID, filepath.Base(stalePath))
		return 0, nil
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
			// We already ran the stale-snapshot check above and it
			// returned false — the legacy JSON has at least one ticket
			// the per-ticket dir doesn't (or has newer than). Refusing
			// is the safe call; auto-merging risks losing data either
			// way the merge resolves.
			return 0, fmt.Errorf(
				"cannot migrate project %s: per-ticket dir %s already exists and is non-empty, AND legacy JSON %s has tickets not present (or newer than) the dir. Compare them manually: e.g. `jq '.tickets | keys' %s` vs `ls %s/`; either remove the legacy JSON if it's stale, or merge any missing tickets into the dir as .md files before retrying",
				projectID, finalDir, legacyPath, legacyPath, finalDir,
			)
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

// isStaleSnapshot returns true if every ticket in legacyTickets is
// present in finalDir as a .md file with an UpdatedAt no older than
// the legacy record. When true, the legacy JSON is provably a stale
// subset of the per-ticket dir — the dir is the source of truth and
// the JSON can be safely renamed aside without data loss.
//
// Conservative: returns false on any uncertainty (missing files,
// parse errors, an .md older than its legacy peer). Better to error
// loudly than silently lose data.
func isStaleSnapshot(legacyTickets map[board.TicketID]*board.Ticket, finalDir string) bool {
	if len(legacyTickets) == 0 {
		// An empty legacy JSON next to a populated dir is also "stale"
		// in the sense that the dir has more data; no risk of loss.
		entries, err := os.ReadDir(finalDir)
		if err != nil || len(entries) == 0 {
			return false
		}
		return true
	}

	dirState := make(map[board.TicketID]time.Time)
	entries, err := os.ReadDir(finalDir)
	if err != nil {
		return false
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".md") {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(finalDir, ent.Name()))
		if rerr != nil {
			return false
		}
		ticket, perr := UnmarshalTicket(data)
		if perr != nil {
			return false
		}
		dirState[ticket.ID] = ticket.UpdatedAt
	}

	for id, lt := range legacyTickets {
		if lt == nil {
			continue
		}
		dirUpdated, ok := dirState[id]
		if !ok {
			return false // legacy has a ticket the dir doesn't — data risk
		}
		if dirUpdated.Before(lt.UpdatedAt) {
			return false // dir's copy is older — legacy might have newer data
		}
	}
	return true
}
