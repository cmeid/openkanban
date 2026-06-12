package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/board"
)

func TestNewTicketStore(t *testing.T) {
	store := NewTicketStore("project-1", "/path/to/repo")

	if store.ProjectID != "project-1" {
		t.Errorf("ProjectID = %q; want %q", store.ProjectID, "project-1")
	}

	if store.Tickets == nil {
		t.Error("Tickets map should not be nil")
	}

	if len(store.Tickets) != 0 {
		t.Errorf("new store should have 0 tickets; got %d", len(store.Tickets))
	}
}

func TestTicketStore_AddAndGet(t *testing.T) {
	store := NewTicketStore("project-1", "/path")

	ticket := board.NewTicket("Test Ticket", "project-1")
	store.Add(ticket)

	retrieved, err := store.Get(ticket.ID)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}

	if retrieved.Title != ticket.Title {
		t.Errorf("retrieved.Title = %q; want %q", retrieved.Title, ticket.Title)
	}

	if retrieved.ProjectID != "project-1" {
		t.Errorf("Add should set ProjectID; got %q", retrieved.ProjectID)
	}
}

func TestTicketStore_GetNotFound(t *testing.T) {
	store := NewTicketStore("project-1", "/path")

	_, err := store.Get("nonexistent-id")
	if err != board.ErrTicketNotFound {
		t.Errorf("Get() error = %v; want ErrTicketNotFound", err)
	}
}

func TestTicketStore_Delete(t *testing.T) {
	store := NewTicketStore("project-1", "/path")

	ticket := board.NewTicket("Test", "project-1")
	store.Add(ticket)

	if err := store.Delete(ticket.ID); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}

	_, err := store.Get(ticket.ID)
	if err != board.ErrTicketNotFound {
		t.Error("ticket should not exist after delete")
	}
}

func TestTicketStore_DeleteNotFound(t *testing.T) {
	store := NewTicketStore("project-1", "/path")

	err := store.Delete("nonexistent")
	if err != board.ErrTicketNotFound {
		t.Errorf("Delete() error = %v; want ErrTicketNotFound", err)
	}
}

func TestTicketStore_Move(t *testing.T) {
	store := NewTicketStore("project-1", "/path")

	ticket := board.NewTicket("Test", "project-1")
	store.Add(ticket)

	if err := store.Move(ticket.ID, board.StatusInProgress); err != nil {
		t.Fatalf("Move() error: %v", err)
	}

	retrieved, _ := store.Get(ticket.ID)
	if retrieved.Status != board.StatusInProgress {
		t.Errorf("Status = %q; want %q", retrieved.Status, board.StatusInProgress)
	}

	if retrieved.StartedAt == nil {
		t.Error("StartedAt should be set after moving to in_progress")
	}
}

func TestTicketStore_GetByStatus(t *testing.T) {
	store := NewTicketStore("project-1", "/path")

	t1 := board.NewTicket("Backlog 1", "project-1")
	t2 := board.NewTicket("Backlog 2", "project-1")
	t3 := board.NewTicket("In Progress", "project-1")
	t3.Status = board.StatusInProgress

	store.Add(t1)
	store.Add(t2)
	store.Add(t3)

	backlog := store.GetByStatus(board.StatusBacklog)
	if len(backlog) != 2 {
		t.Errorf("GetByStatus(backlog) returned %d tickets; want 2", len(backlog))
	}

	inProgress := store.GetByStatus(board.StatusInProgress)
	if len(inProgress) != 1 {
		t.Errorf("GetByStatus(in_progress) returned %d tickets; want 1", len(inProgress))
	}

	done := store.GetByStatus(board.StatusDone)
	if len(done) != 0 {
		t.Errorf("GetByStatus(done) returned %d tickets; want 0", len(done))
	}
}

func TestTicketStore_All(t *testing.T) {
	store := NewTicketStore("project-1", "/path")

	store.Add(board.NewTicket("T1", "project-1"))
	store.Add(board.NewTicket("T2", "project-1"))
	store.Add(board.NewTicket("T3", "project-1"))

	all := store.All()
	if len(all) != 3 {
		t.Errorf("All() returned %d tickets; want 3", len(all))
	}
}

func TestTicketStore_Count(t *testing.T) {
	store := NewTicketStore("project-1", "/path")

	if store.Count() != 0 {
		t.Errorf("Count() = %d; want 0", store.Count())
	}

	store.Add(board.NewTicket("T1", "project-1"))
	store.Add(board.NewTicket("T2", "project-1"))

	if store.Count() != 2 {
		t.Errorf("Count() = %d; want 2", store.Count())
	}
}

func TestTicketStore_CountByStatus(t *testing.T) {
	store := NewTicketStore("project-1", "/path")

	t1 := board.NewTicket("T1", "project-1")
	t2 := board.NewTicket("T2", "project-1")
	t3 := board.NewTicket("T3", "project-1")
	t3.Status = board.StatusDone

	store.Add(t1)
	store.Add(t2)
	store.Add(t3)

	if store.CountByStatus(board.StatusBacklog) != 2 {
		t.Errorf("CountByStatus(backlog) = %d; want 2", store.CountByStatus(board.StatusBacklog))
	}

	if store.CountByStatus(board.StatusDone) != 1 {
		t.Errorf("CountByStatus(done) = %d; want 1", store.CountByStatus(board.StatusDone))
	}
}

func TestTicketStore_SaveAndLoad(t *testing.T) {
	configDir := setupTmpConfigDir(t)
	repoDir := filepath.Join(t.TempDir(), "repo")
	os.MkdirAll(repoDir, 0o755)

	store := NewTicketStore("project-1", repoDir)
	ticket := board.NewTicket("Persistent Ticket", "project-1")
	ticket.Description = "This should persist"
	ticket.Status = board.StatusInProgress
	store.Add(ticket)

	if err := store.SaveTicket(ticket); err != nil {
		t.Fatalf("SaveTicket: %v", err)
	}

	// Per-ticket file lives under tickets/{project}/{filename}.md
	projectDir := filepath.Join(configDir, "tickets", "project-1")
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		t.Fatalf("read project dir: %v", err)
	}
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".md") {
		t.Fatalf("expected one .md file under %s, got %v", projectDir, entries)
	}

	project := &Project{ID: "project-1", RepoPath: repoDir}
	loaded, err := LoadTicketStore(project)
	if err != nil {
		t.Fatalf("LoadTicketStore: %v", err)
	}

	if loaded.Count() != 1 {
		t.Fatalf("loaded store should have 1 ticket; got %d", loaded.Count())
	}

	loadedTicket, err := loaded.Get(ticket.ID)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}

	if loadedTicket.Title != "Persistent Ticket" {
		t.Errorf("Title = %q; want %q", loadedTicket.Title, "Persistent Ticket")
	}

	if loadedTicket.Description != "This should persist" {
		t.Errorf("Description = %q; want %q", loadedTicket.Description, "This should persist")
	}

	if loadedTicket.Status != board.StatusInProgress {
		t.Errorf("Status = %q; want %q", loadedTicket.Status, board.StatusInProgress)
	}
}

func TestLoadTicketStore_EmptyProjectDir(t *testing.T) {
	setupTmpConfigDir(t)
	project := &Project{ID: "project-empty", RepoPath: t.TempDir()}

	store, err := LoadTicketStore(project)
	if err != nil {
		t.Fatalf("LoadTicketStore should not error for empty project: %v", err)
	}
	if store.Count() != 0 {
		t.Errorf("loaded store should be empty; got %d tickets", store.Count())
	}
}

func TestTicketStore_AtomicSaveNoTmpLeftover(t *testing.T) {
	configDir := setupTmpConfigDir(t)
	store := NewTicketStore("project-1", t.TempDir())
	ticket := board.NewTicket("AtomicTest", "project-1")
	store.Add(ticket)

	if err := store.SaveTicket(ticket); err != nil {
		t.Fatalf("SaveTicket: %v", err)
	}

	projectDir := filepath.Join(configDir, "tickets", "project-1")
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		t.Fatalf("read project dir: %v", err)
	}
	for _, ent := range entries {
		if strings.HasSuffix(ent.Name(), ".tmp") {
			t.Errorf("leftover .tmp file: %s", ent.Name())
		}
	}
}

// TestSave_DoesNotTouchPeerFiles is the load-bearing regression test
// for the original cross-session clobber bug. Per-ticket storage means
// saving ticket A must not modify ticket B's file in any way.
func TestSave_DoesNotTouchPeerFiles(t *testing.T) {
	setupTmpConfigDir(t)
	store := NewTicketStore("project-1", t.TempDir())

	tA := board.NewTicket("Ticket A", "project-1")
	tB := board.NewTicket("Ticket B", "project-1")
	store.Add(tA)
	store.Add(tB)

	if err := store.SaveTicket(tA); err != nil {
		t.Fatalf("save A: %v", err)
	}
	if err := store.SaveTicket(tB); err != nil {
		t.Fatalf("save B: %v", err)
	}

	pathB := store.paths[tB.ID]
	statB1, err := os.Stat(pathB)
	if err != nil {
		t.Fatalf("stat B: %v", err)
	}

	// Give the filesystem enough resolution to detect any modification.
	time.Sleep(20 * time.Millisecond)

	// Mutate and save A. B's file MUST be untouched.
	tA.Description = "Edit that should not affect B"
	tA.UpdatedAt = time.Now()
	if err := store.SaveTicket(tA); err != nil {
		t.Fatalf("re-save A: %v", err)
	}

	statB2, err := os.Stat(pathB)
	if err != nil {
		t.Fatalf("re-stat B: %v", err)
	}

	if !statB1.ModTime().Equal(statB2.ModTime()) {
		t.Errorf("saving A modified B's mtime (before=%v after=%v) — cross-ticket clobber regression!",
			statB1.ModTime(), statB2.ModTime())
	}
	if statB1.Size() != statB2.Size() {
		t.Errorf("saving A changed B's size (before=%d after=%d)", statB1.Size(), statB2.Size())
	}
}

// TestSaveTicket_TitleEditRemovesOldFile guards against orphan files
// when the slug part of the filename changes due to a title edit.
func TestSaveTicket_TitleEditRemovesOldFile(t *testing.T) {
	configDir := setupTmpConfigDir(t)
	store := NewTicketStore("project-1", t.TempDir())

	tk := board.NewTicket("Original Title", "project-1")
	store.Add(tk)
	if err := store.SaveTicket(tk); err != nil {
		t.Fatalf("save: %v", err)
	}
	oldPath := store.paths[tk.ID]

	// Edit the title; the filename slug will change.
	tk.Title = "Brand New Title"
	tk.Touch()
	if err := store.SaveTicket(tk); err != nil {
		t.Fatalf("re-save with new title: %v", err)
	}
	newPath := store.paths[tk.ID]

	if oldPath == newPath {
		t.Fatal("test bug: filename should change when title changes")
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("old file at %s should be removed after rename", oldPath)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("new file at %s should exist: %v", newPath, err)
	}

	// Project dir should have exactly one file.
	projectDir := filepath.Join(configDir, "tickets", "project-1")
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		t.Fatalf("read project dir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected exactly 1 file after rename, got %d: %v", len(entries), names)
	}
}

// TestLoadTicketStore_DuplicateIDPrefersNewer covers the interrupted-
// rename recovery case: title edit wrote the new file but crashed
// before deleting the old, leaving both on disk with the same
// frontmatter id. Load must pick the newer (by mtime) and remove
// the older.
func TestLoadTicketStore_DuplicateIDPrefersNewer(t *testing.T) {
	configDir := setupTmpConfigDir(t)
	repoDir := filepath.Join(t.TempDir(), "repo")
	os.MkdirAll(repoDir, 0o755)

	// Manually write two .md files with the same frontmatter id but
	// different titles / filenames / mtimes. The newer one (by mtime)
	// must win; the older one must be cleaned up.
	projDir := filepath.Join(configDir, "tickets", "proj-dup")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	tk := board.NewTicket("Original Title", "proj-dup")
	tk.CreatedAt = mustParseTime(t, "2026-06-01T00:00:00Z")
	tk.UpdatedAt = tk.CreatedAt

	stalePath := filepath.Join(projDir, "original-title-"+string(tk.ID)[:8]+".md")
	staleData, err := MarshalTicket(tk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stalePath, staleData, 0o644); err != nil {
		t.Fatal(err)
	}
	// Backdate the stale file so it's clearly older.
	oldTime := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(stalePath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	tk.Title = "Renamed Title"
	tk.Touch()
	freshPath := filepath.Join(projDir, "renamed-title-"+string(tk.ID)[:8]+".md")
	freshData, err := MarshalTicket(tk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(freshPath, freshData, 0o644); err != nil {
		t.Fatal(err)
	}

	project := &Project{ID: "proj-dup", RepoPath: repoDir}
	loaded, err := LoadTicketStore(project)
	if err != nil {
		t.Fatalf("LoadTicketStore: %v", err)
	}

	if loaded.Count() != 1 {
		t.Errorf("expected 1 ticket post-dedup, got %d", loaded.Count())
	}
	got, _ := loaded.Get(tk.ID)
	if got == nil {
		t.Fatal("ticket missing after dedup")
	}
	if got.Title != "Renamed Title" {
		t.Errorf("dedup kept the wrong (older) file; got title %q, want %q", got.Title, "Renamed Title")
	}

	// Stale file should be gone from disk.
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("stale duplicate %s should have been removed; stat err: %v", stalePath, err)
	}
	if _, err := os.Stat(freshPath); err != nil {
		t.Errorf("fresh file should still exist; stat err: %v", err)
	}
}

func TestGlobalTicketStore_RemoveProjectArchivesWholeDir(t *testing.T) {
	configDir := setupTmpConfigDir(t)
	repoDir := filepath.Join(t.TempDir(), "repo")
	os.MkdirAll(repoDir, 0o755)

	registry := newRegistry()
	p := &Project{ID: "project-1", Name: "Test", RepoPath: repoDir}
	if err := registry.Add(p); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	store := NewTicketStore(p.ID, p.RepoPath)
	tk := board.NewTicket("Archived ticket", p.ID)
	store.Add(tk)
	if err := store.SaveTicket(tk); err != nil {
		t.Fatalf("SaveTicket: %v", err)
	}

	globalStore := NewGlobalTicketStore(registry)
	globalStore.projects[p.ID] = p
	globalStore.ticketStores[p.ID] = store

	projectDir := filepath.Join(configDir, "tickets", "project-1")
	if _, err := os.Stat(projectDir); err != nil {
		t.Fatalf("project dir should exist before removal: %v", err)
	}

	if err := globalStore.RemoveProject(p.ID); err != nil {
		t.Fatalf("RemoveProject: %v", err)
	}

	// Project dir gone from its original location.
	if _, err := os.Stat(projectDir); !os.IsNotExist(err) {
		t.Error("project dir should be removed after archival")
	}

	// Archived under tickets/archived/<id>_<ts>/
	archivedRoot := filepath.Join(configDir, "tickets", "archived")
	entries, err := os.ReadDir(archivedRoot)
	if err != nil {
		t.Fatalf("read archived root: %v", err)
	}
	found := false
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), "project-1_") && ent.IsDir() {
			found = true
			// Confirm the per-ticket file lives inside.
			inner, _ := os.ReadDir(filepath.Join(archivedRoot, ent.Name()))
			if len(inner) != 1 {
				t.Errorf("expected 1 ticket file inside archived dir, got %d", len(inner))
			}
		}
	}
	if !found {
		t.Errorf("no archived directory found matching project-1_*; entries: %v", entries)
	}
}
