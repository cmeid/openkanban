package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/board"
)

// setupTmpConfigDir points OPENKANBAN_CONFIG_DIR at a per-test tmpdir
// and seeds the tickets/ subdirectory. Returns the absolute config dir.
func setupTmpConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("OPENKANBAN_CONFIG_DIR", dir)
	if err := os.MkdirAll(filepath.Join(dir, "tickets"), 0o755); err != nil {
		t.Fatalf("mkdir tickets: %v", err)
	}
	return dir
}

// seedLegacyJSONStore writes a TicketStore JSON file at the old
// {project_id}.json path, populated with the given tickets.
func seedLegacyJSONStore(t *testing.T, projectID string, tickets ...*board.Ticket) {
	t.Helper()
	store := struct {
		ProjectID string                                 `json:"project_id"`
		Tickets   map[board.TicketID]*board.Ticket       `json:"tickets"`
		UpdatedAt time.Time                              `json:"updated_at"`
	}{
		ProjectID: projectID,
		Tickets:   make(map[board.TicketID]*board.Ticket, len(tickets)),
		UpdatedAt: time.Now(),
	}
	for _, ticket := range tickets {
		store.Tickets[ticket.ID] = ticket
	}
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	path := filepath.Join(ticketsDir(), projectID+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
}

func makeTicket(t *testing.T, id, title string) *board.Ticket {
	t.Helper()
	tk := board.NewTicket(title, "proj-test")
	tk.ID = board.TicketID(id)
	tk.CreatedAt = mustParseTime(t, "2026-06-12T00:00:00Z")
	tk.UpdatedAt = tk.CreatedAt
	return tk
}

func TestMigrationState_NotNeeded(t *testing.T) {
	setupTmpConfigDir(t)
	state := MigrationStateFor("proj-x")
	if state != MigrationNotNeeded {
		t.Errorf("got %v, want MigrationNotNeeded", state)
	}
}

func TestMigrationState_Pending(t *testing.T) {
	setupTmpConfigDir(t)
	seedLegacyJSONStore(t, "proj-x")
	state := MigrationStateFor("proj-x")
	if state != MigrationPending {
		t.Errorf("got %v, want MigrationPending", state)
	}
}

func TestMigrationState_Complete(t *testing.T) {
	dir := setupTmpConfigDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "tickets", "proj-x"), 0o755); err != nil {
		t.Fatal(err)
	}
	state := MigrationStateFor("proj-x")
	if state != MigrationComplete {
		t.Errorf("got %v, want MigrationComplete", state)
	}
}

func TestMigrationState_OrphanFromInterruptedRun(t *testing.T) {
	dir := setupTmpConfigDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "tickets", "proj-x.migrating"), 0o755); err != nil {
		t.Fatal(err)
	}
	seedLegacyJSONStore(t, "proj-x") // pending source still present
	state := MigrationStateFor("proj-x")
	if state != MigrationInProgressOrphan {
		t.Errorf("got %v, want MigrationInProgressOrphan", state)
	}
}

func TestMigrate_FromJSONToPerTicket(t *testing.T) {
	dir := setupTmpConfigDir(t)
	tk1 := makeTicket(t, "00000001-aaaa-bbbb-cccc-dddddddddddd", "First ticket")
	tk2 := makeTicket(t, "00000002-aaaa-bbbb-cccc-dddddddddddd", "Second ticket")
	tk2.Description = "Body of second\nover multiple\nlines"
	tk2.Status = board.StatusInProgress
	seedLegacyJSONStore(t, "proj-x", tk1, tk2)

	n, err := MigrateProjectToPerTicket("proj-x")
	if err != nil {
		t.Fatalf("MigrateProjectToPerTicket: %v", err)
	}
	if n != 2 {
		t.Errorf("migrated count: got %d, want 2", n)
	}

	// New per-ticket directory exists with both files.
	projectDir := filepath.Join(dir, "tickets", "proj-x")
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		t.Fatalf("read project dir: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("got %d .md files in project dir, want 2", len(entries))
	}

	// Original JSON renamed to .migrated rollback artifact.
	if _, err := os.Stat(filepath.Join(dir, "tickets", "proj-x.json")); !os.IsNotExist(err) {
		t.Error("legacy .json should be renamed away after migration")
	}
	if _, err := os.Stat(filepath.Join(dir, "tickets", "proj-x.json.migrated")); err != nil {
		t.Errorf(".json.migrated rollback artifact missing: %v", err)
	}

	// .migrating/ workspace cleaned up.
	if _, err := os.Stat(filepath.Join(dir, "tickets", "proj-x.migrating")); !os.IsNotExist(err) {
		t.Error(".migrating/ workspace should be gone after successful migration")
	}

	// Per-ticket files round-trip back to the same field values.
	for _, want := range []*board.Ticket{tk1, tk2} {
		found := false
		for _, ent := range entries {
			data, rerr := os.ReadFile(filepath.Join(projectDir, ent.Name()))
			if rerr != nil {
				t.Fatalf("read %s: %v", ent.Name(), rerr)
			}
			got, perr := UnmarshalTicket(data)
			if perr != nil {
				continue
			}
			if got.ID == want.ID {
				found = true
				if got.Title != want.Title {
					t.Errorf("title mismatch for %s: got %q, want %q", want.ID, got.Title, want.Title)
				}
				if got.Status != want.Status {
					t.Errorf("status mismatch for %s: got %q, want %q", want.ID, got.Status, want.Status)
				}
				// Description: trimmed of trailing newlines on round-trip.
				wantDesc := want.Description
				for len(wantDesc) > 0 && wantDesc[len(wantDesc)-1] == '\n' {
					wantDesc = wantDesc[:len(wantDesc)-1]
				}
				if got.Description != wantDesc {
					t.Errorf("description mismatch for %s:\n got: %q\nwant: %q", want.ID, got.Description, wantDesc)
				}
				break
			}
		}
		if !found {
			t.Errorf("ticket %s not found in migrated dir", want.ID)
		}
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	setupTmpConfigDir(t)
	tk := makeTicket(t, "abcdef01-aaaa-bbbb-cccc-dddddddddddd", "Only one")
	seedLegacyJSONStore(t, "proj-x", tk)

	if _, err := MigrateProjectToPerTicket("proj-x"); err != nil {
		t.Fatalf("first migration: %v", err)
	}
	n, err := MigrateProjectToPerTicket("proj-x")
	if err != nil {
		t.Fatalf("second migration: %v", err)
	}
	if n != 0 {
		t.Errorf("second migration should be no-op (0 migrated); got %d", n)
	}
}

func TestMigrate_OrphanWorkspaceCleanedAndRetried(t *testing.T) {
	dir := setupTmpConfigDir(t)
	// Leave an orphan .migrating/ dir with junk inside, then seed the source.
	orphanDir := filepath.Join(dir, "tickets", "proj-x.migrating")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, "junk.md"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	tk := makeTicket(t, "feedface-aaaa-bbbb-cccc-dddddddddddd", "Survivor")
	seedLegacyJSONStore(t, "proj-x", tk)

	n, err := MigrateProjectToPerTicket("proj-x")
	if err != nil {
		t.Fatalf("MigrateProjectToPerTicket: %v", err)
	}
	if n != 1 {
		t.Errorf("got %d migrated, want 1", n)
	}

	// Orphan workspace gone; real per-ticket dir exists.
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Error("orphan .migrating/ should have been cleaned before retry")
	}
	entries, err := os.ReadDir(filepath.Join(dir, "tickets", "proj-x"))
	if err != nil {
		t.Fatalf("read final dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("got %d files in final dir, want 1", len(entries))
	}
}

// TestMigrate_FinalDirExistsEmpty exercises the path where some prior
// code (`openkanban list`, the TUI, anything that called LoadTicketStore)
// created the per-project dir before the legacy JSON was placed.
// Migration must clean up the empty dir and proceed, not error out.
func TestMigrate_FinalDirExistsEmpty(t *testing.T) {
	dir := setupTmpConfigDir(t)
	// Per-project dir exists but is empty.
	if err := os.MkdirAll(filepath.Join(dir, "tickets", "proj-x"), 0o755); err != nil {
		t.Fatal(err)
	}
	tk := makeTicket(t, "11111111-aaaa-bbbb-cccc-dddddddddddd", "Survivor")
	seedLegacyJSONStore(t, "proj-x", tk)

	n, err := MigrateProjectToPerTicket("proj-x")
	if err != nil {
		t.Fatalf("expected migration to succeed despite empty final dir, got: %v", err)
	}
	if n != 1 {
		t.Errorf("migrated count: got %d, want 1", n)
	}

	entries, err := os.ReadDir(filepath.Join(dir, "tickets", "proj-x"))
	if err != nil {
		t.Fatalf("read final dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 file in final dir after migration, got %d", len(entries))
	}
}

// TestMigrate_FinalDirExistsNonEmpty guards against silent merging
// when the final dir already has unrelated content. Better to error
// loudly than to overwrite or interleave.
func TestMigrate_FinalDirExistsNonEmpty(t *testing.T) {
	dir := setupTmpConfigDir(t)
	finalDir := filepath.Join(dir, "tickets", "proj-x")
	if err := os.MkdirAll(finalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(finalDir, "unrelated.md"), []byte("---\nid: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tk := makeTicket(t, "22222222-aaaa-bbbb-cccc-dddddddddddd", "Should not land")
	seedLegacyJSONStore(t, "proj-x", tk)

	n, err := MigrateProjectToPerTicket("proj-x")
	if err == nil {
		t.Error("expected error on non-empty final dir, got nil")
	}
	if n != 0 {
		t.Errorf("got %d migrated, want 0 on refusal", n)
	}
	// Legacy JSON retained.
	if _, err := os.Stat(filepath.Join(dir, "tickets", "proj-x.json")); err != nil {
		t.Errorf("legacy json should be retained on refusal: %v", err)
	}
}

// TestMigrate_StaleSnapshotRenamedAside covers the recovery case
// where a legacy JSON appears AFTER the migration has already
// completed (e.g. an old binary was accidentally launched and wrote a
// stale snapshot). If every ticket in the legacy JSON is present in
// the per-ticket dir with an UpdatedAt no older than the JSON's
// record, the JSON is provably a stale subset — rename aside and
// return cleanly.
func TestMigrate_StaleSnapshotRenamedAside(t *testing.T) {
	dir := setupTmpConfigDir(t)
	projectID := "proj-stale"
	finalDir := filepath.Join(dir, "tickets", projectID)
	if err := os.MkdirAll(finalDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a current .md ticket (newer state).
	tk := makeTicket(t, "ffff1111-aaaa-bbbb-cccc-dddddddddddd", "Current title")
	tk.Status = board.StatusDone
	tk.UpdatedAt = mustParseTime(t, "2026-06-12T18:40:00Z")
	mdData, err := MarshalTicket(tk)
	if err != nil {
		t.Fatal(err)
	}
	mdPath := filepath.Join(finalDir, TicketFilename(tk))
	if err := os.WriteFile(mdPath, mdData, 0o644); err != nil {
		t.Fatal(err)
	}

	// Seed a legacy JSON with the SAME ticket id but OLDER UpdatedAt.
	staleTicket := *tk
	staleTicket.Status = board.StatusInProgress
	staleTicket.UpdatedAt = mustParseTime(t, "2026-06-12T17:00:00Z") // older
	seedLegacyJSONStore(t, projectID, &staleTicket)

	n, err := MigrateProjectToPerTicket(projectID)
	if err != nil {
		t.Fatalf("expected stale-snapshot escape to succeed, got: %v", err)
	}
	if n != 0 {
		t.Errorf("got %d migrated, want 0 (no migration; just stale rename)", n)
	}

	// Legacy JSON should be renamed to .stale-<ts>; the per-ticket
	// file should be unchanged.
	if _, err := os.Stat(filepath.Join(dir, "tickets", projectID+".json")); !os.IsNotExist(err) {
		t.Error("legacy .json should be renamed away after stale detection")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "tickets", projectID+".json.stale-*"))
	if len(matches) != 1 {
		t.Errorf("expected exactly one .stale-* artifact, got %d: %v", len(matches), matches)
	}
	if _, err := os.Stat(mdPath); err != nil {
		t.Errorf("per-ticket .md should be untouched: %v", err)
	}

	// Next call should be a no-op (state is now MigrationComplete).
	if n, err := MigrateProjectToPerTicket(projectID); err != nil || n != 0 {
		t.Errorf("second call should be no-op; got n=%d err=%v", n, err)
	}
}

// TestMigrate_NonStaleDirRefusesWithActionableError covers the case
// where the per-ticket dir is non-empty BUT the legacy JSON contains
// a ticket that the dir doesn't have (or has it with an older
// timestamp). Cannot safely auto-resolve — must refuse with a
// message that names both paths so the user can compare them.
func TestMigrate_NonStaleDirRefusesWithActionableError(t *testing.T) {
	dir := setupTmpConfigDir(t)
	projectID := "proj-conflict"
	finalDir := filepath.Join(dir, "tickets", projectID)
	if err := os.MkdirAll(finalDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Put an unrelated ticket in the per-ticket dir.
	dirTicket := makeTicket(t, "aaaa1111-aaaa-bbbb-cccc-dddddddddddd", "Unrelated")
	mdData, _ := MarshalTicket(dirTicket)
	if err := os.WriteFile(filepath.Join(finalDir, TicketFilename(dirTicket)), mdData, 0o644); err != nil {
		t.Fatal(err)
	}

	// Legacy JSON contains a DIFFERENT ticket the dir doesn't have —
	// auto-resolve must NOT fire because that'd lose data.
	missingTicket := makeTicket(t, "bbbb2222-aaaa-bbbb-cccc-dddddddddddd", "Only in legacy")
	seedLegacyJSONStore(t, projectID, missingTicket)

	n, err := MigrateProjectToPerTicket(projectID)
	if err == nil {
		t.Fatal("expected error on non-stale non-empty dir, got nil")
	}
	if n != 0 {
		t.Errorf("got %d migrated, want 0 on refusal", n)
	}

	// Error should name both paths so the user can act on it.
	msg := err.Error()
	if !strings.Contains(msg, projectID) {
		t.Errorf("error should reference project id %q; got: %v", projectID, err)
	}
	for _, want := range []string{"legacy", "per-ticket"} {
		if !strings.Contains(strings.ToLower(msg), want) {
			t.Errorf("error should mention %q to be actionable; got: %v", want, err)
		}
	}

	// Legacy JSON must be retained (not renamed) on refusal.
	if _, err := os.Stat(filepath.Join(dir, "tickets", projectID+".json")); err != nil {
		t.Errorf("legacy .json should be retained on refusal: %v", err)
	}
}

func TestMigrate_CorruptSourceLeavesOriginal(t *testing.T) {
	dir := setupTmpConfigDir(t)
	// Write garbage JSON.
	if err := os.WriteFile(filepath.Join(dir, "tickets", "proj-x.json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	n, err := MigrateProjectToPerTicket("proj-x")
	if err == nil {
		t.Error("expected error on corrupt source, got nil")
	}
	if n != 0 {
		t.Errorf("got %d migrated, want 0", n)
	}

	// Original JSON retained.
	if _, err := os.Stat(filepath.Join(dir, "tickets", "proj-x.json")); err != nil {
		t.Errorf("original .json should be retained on failure; stat err: %v", err)
	}
	// No final dir or .migrated artifact created.
	if _, err := os.Stat(filepath.Join(dir, "tickets", "proj-x")); !os.IsNotExist(err) {
		t.Error("final dir should not exist on failed migration")
	}
	if _, err := os.Stat(filepath.Join(dir, "tickets", "proj-x.json.migrated")); !os.IsNotExist(err) {
		t.Error(".json.migrated should not exist on failed migration")
	}
}
