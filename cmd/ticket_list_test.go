package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/project"
)

func listTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
}

// registerListProject adds a project to the on-disk registry. RepoPath
// need not be a git repo — ticket list doesn't touch git.
func registerListProject(t *testing.T, name string) *project.Project {
	t.Helper()
	registry, err := project.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	repoDir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	proj := project.NewProject(name, repoDir)
	if err := registry.Add(proj); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}
	return proj
}

func addListTicket(t *testing.T, proj *project.Project, title string, status board.TicketStatus, labels ...string) *board.Ticket {
	t.Helper()
	store := project.NewTicketStore(proj.ID, proj.RepoPath)
	tk := board.NewTicket(title, proj.ID)
	tk.Status = status
	if len(labels) > 0 {
		tk.Labels = labels
	}
	store.Add(tk)
	if err := store.SaveTicket(tk); err != nil {
		t.Fatalf("SaveTicket: %v", err)
	}
	return tk
}

func resetTicketListFlags() {
	ticketListProject = ""
	ticketListStatus = nil
	ticketListTitleContains = ""
	ticketListJSON = false
}

func runTicketListCapture(t *testing.T, projectArg string, statuses []string, titleContains string, asJSON bool) (string, string, error) {
	t.Helper()
	resetTicketListFlags()
	t.Cleanup(resetTicketListFlags)
	ticketListProject = projectArg
	ticketListStatus = statuses
	ticketListTitleContains = titleContains
	ticketListJSON = asJSON

	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	runErr := ticketListCmd.RunE(ticketListCmd, nil)
	os.Stdout, os.Stderr = origOut, origErr
	outW.Close()
	errW.Close()
	return drainPipe(outR), drainPipe(errR), runErr
}

func drainPipe(r *os.File) string {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return string(buf)
}

func TestTicketList_StatusAndTitleFilters(t *testing.T) {
	listTestEnv(t)
	proj := registerListProject(t, "alpha")
	addListTicket(t, proj, "alpha task", board.StatusBacklog)
	addListTicket(t, proj, "beta task", board.StatusInProgress)
	addListTicket(t, proj, "gamma alpha", board.StatusDone)

	// No filter: all three (JSON for stable counting).
	out, _, err := runTicketListCapture(t, "", nil, "", true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var all []ticketListItem
	if jerr := json.Unmarshal([]byte(out), &all); jerr != nil {
		t.Fatalf("json: %v\n%s", jerr, out)
	}
	if len(all) != 3 {
		t.Fatalf("no-filter count = %d, want 3", len(all))
	}

	// Status filter: only in_progress.
	out, _, err = runTicketListCapture(t, "", []string{"in_progress"}, "", true)
	if err != nil {
		t.Fatalf("status filter: %v", err)
	}
	var byStatus []ticketListItem
	json.Unmarshal([]byte(out), &byStatus)
	if len(byStatus) != 1 || byStatus[0].Title != "beta task" {
		t.Fatalf("status filter = %+v, want [beta task]", byStatus)
	}

	// Title-contains: "alpha" matches "alpha task" + "gamma alpha".
	out, _, err = runTicketListCapture(t, "", nil, "alpha", true)
	if err != nil {
		t.Fatalf("title filter: %v", err)
	}
	var byTitle []ticketListItem
	json.Unmarshal([]byte(out), &byTitle)
	if len(byTitle) != 2 {
		t.Fatalf("title-contains alpha count = %d, want 2", len(byTitle))
	}
}

func TestTicketList_InvalidStatusErrors(t *testing.T) {
	listTestEnv(t)
	registerListProject(t, "alpha")
	_, _, err := runTicketListCapture(t, "", []string{"bogus"}, "", true)
	if err == nil {
		t.Fatal("expected error for invalid --status, got nil")
	}
	if !strings.Contains(err.Error(), "not one of") {
		t.Errorf("error %q does not list valid statuses", err.Error())
	}
}

func TestTicketList_JSONStableSchema(t *testing.T) {
	listTestEnv(t)
	proj := registerListProject(t, "alpha")
	addListTicket(t, proj, "no labels", board.StatusBacklog) // nil labels

	out, _, err := runTicketListCapture(t, "", nil, "", true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// Decode into generic maps to assert every documented key is present
	// and labels is an array (not null/absent).
	var raw []map[string]json.RawMessage
	if jerr := json.Unmarshal([]byte(out), &raw); jerr != nil {
		t.Fatalf("json: %v\n%s", jerr, out)
	}
	if len(raw) != 1 {
		t.Fatalf("want 1 item, got %d", len(raw))
	}
	wantKeys := []string{"id", "title", "status", "project_id", "branch_name",
		"agent_session_id", "worktree_path", "priority", "labels", "created_at", "updated_at"}
	for _, k := range wantKeys {
		if _, ok := raw[0][k]; !ok {
			t.Errorf("missing key %q in json object", k)
		}
	}
	if string(raw[0]["labels"]) != "[]" {
		t.Errorf("labels = %s, want [] (never null)", raw[0]["labels"])
	}
}

func TestTicketList_SkipsMigrationOrphanReadOnly(t *testing.T) {
	listTestEnv(t)
	proj := registerListProject(t, "orphaned")

	// Force MigrationInProgressOrphan: a {projectID}.migrating workspace
	// on disk. Drop a sentinel file inside to detect a destructive
	// RemoveAll.
	cfgDir := os.Getenv("OPENKANBAN_CONFIG_DIR")
	migrating := filepath.Join(cfgDir, "tickets", proj.ID+".migrating")
	if err := os.MkdirAll(migrating, 0o755); err != nil {
		t.Fatalf("mkdir migrating: %v", err)
	}
	sentinel := filepath.Join(migrating, "sentinel")
	if err := os.WriteFile(sentinel, []byte("x"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	stdout, stderr, err := runTicketListCapture(t, "", nil, "", false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// The migrating workspace + sentinel must be UNTOUCHED (read-only).
	if _, statErr := os.Stat(sentinel); statErr != nil {
		t.Errorf("migrating workspace was mutated (sentinel gone): %v", statErr)
	}
	if !strings.Contains(stderr, "skipped") || !strings.Contains(stderr, "migration-pending") {
		t.Errorf("stderr missing skip note: %q", stderr)
	}
	if !strings.Contains(stdout, "(no tickets)") {
		t.Errorf("stdout = %q, want (no tickets)", stdout)
	}
}
