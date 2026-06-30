package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/git"
	"github.com/techdufus/openkanban/internal/project"
)

// newTicketTestProject sets up an isolated config dir and registers a
// project whose RepoPath is a real git repo (one empty commit on the
// default branch, so GetDefaultBranch + worktree provisioning work).
// Returns the registered project.
func newTicketTestProject(t *testing.T, name string) *project.Project {
	t.Helper()
	cfgDir := t.TempDir()
	t.Setenv("OPENKANBAN_CONFIG_DIR", cfgDir)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENKANBAN_SESSION", "")
	t.Setenv("OPENKANBAN_TICKET_ID", "")

	repoDir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	gitInitRepo(t, repoDir)

	registry, err := project.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	proj := project.NewProject(name, repoDir)
	if err := registry.Add(proj); err != nil { // Add persists to disk
		t.Fatalf("registry.Add: %v", err)
	}
	return proj
}

func gitInitRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	run("commit", "--allow-empty", "-m", "init")
}

// runTicketNewCapture sets the package-level ticketNew* flags from opts,
// invokes ticketNewCmd.RunE, and returns captured stdout. os.Stdout is
// swapped because the command prints via fmt.Println/Printf directly.
func runTicketNewCapture(t *testing.T, project, title string, worktree, asJSON bool) (string, error) {
	t.Helper()
	resetTicketNewFlags()
	t.Cleanup(resetTicketNewFlags)
	prevProj, prevTitle, prevNoWT := ticketNewProject, ticketNewTitle, ticketNewNoWorktree
	t.Cleanup(func() {
		ticketNewProject, ticketNewTitle, ticketNewNoWorktree = prevProj, prevTitle, prevNoWT
	})
	ticketNewProject = project
	ticketNewTitle = title
	ticketNewWorktree = worktree
	ticketNewJSON = asJSON

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	runErr := ticketNewCmd.RunE(ticketNewCmd, nil)
	os.Stdout = orig
	w.Close()

	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, rerr := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if rerr != nil {
			break
		}
	}
	return string(buf), runErr
}

func TestTicketNew_EmitsIDAndPathFinalLine(t *testing.T) {
	proj := newTicketTestProject(t, "alpha")

	out, err := runTicketNewCapture(t, proj.ID, "Improve Ticket CLI", false, false)
	if err != nil {
		t.Fatalf("ticket new: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected >=2 output lines, got %d: %q", len(lines), out)
	}
	// id line is first.
	if !strings.HasPrefix(lines[0], "id=") {
		t.Errorf("first line %q does not start with id=", lines[0])
	}
	id := strings.TrimPrefix(lines[0], "id=")
	if id == "" {
		t.Error("id= line has empty value")
	}
	// path is the FINAL line and points at a real .md file (back-compat).
	final := lines[len(lines)-1]
	if !strings.HasSuffix(final, ".md") {
		t.Errorf("final line %q is not a .md path", final)
	}
	if _, statErr := os.Stat(final); statErr != nil {
		t.Errorf("final-line path does not exist: %v", statErr)
	}
	// No worktree provisioned without --worktree.
	if strings.Contains(out, "worktree=") {
		t.Errorf("unexpected worktree= line without --worktree: %q", out)
	}
}

func TestTicketNew_JSON(t *testing.T) {
	proj := newTicketTestProject(t, "beta")

	out, err := runTicketNewCapture(t, proj.ID, "Some Title", false, true)
	if err != nil {
		t.Fatalf("ticket new --json: %v", err)
	}
	var res ticketNewResult
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatalf("output is not valid json: %v\n%s", jerr, out)
	}
	if res.ID == "" {
		t.Error("json id empty")
	}
	if res.ProjectID != proj.ID {
		t.Errorf("json project_id = %q, want %q", res.ProjectID, proj.ID)
	}
	if !strings.HasSuffix(res.Path, ".md") {
		t.Errorf("json path %q not a .md path", res.Path)
	}
	if res.Status == "" {
		t.Error("json status empty")
	}
	if res.Slug == "" {
		t.Error("json slug empty")
	}
	// No --worktree: worktree fields are present-but-empty (stable schema).
	if res.WorktreePath != "" || res.BranchName != "" || res.BaseBranch != "" {
		t.Errorf("worktree fields should be empty without --worktree: %+v", res)
	}
}

func TestTicketNew_Worktree(t *testing.T) {
	proj := newTicketTestProject(t, "gamma")

	out, err := runTicketNewCapture(t, proj.ID, "Worktree Ticket", true, true)
	if err != nil {
		t.Fatalf("ticket new --worktree --json: %v", err)
	}
	var res ticketNewResult
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatalf("invalid json: %v\n%s", jerr, out)
	}
	if res.WorktreePath == "" {
		t.Fatal("worktree_path empty with --worktree")
	}
	if _, statErr := os.Stat(res.WorktreePath); statErr != nil {
		t.Errorf("worktree path does not exist: %v", statErr)
	}
	// Branch name must match the shared derivation, so spawn reuses it.
	wantBranch := project.BranchNameForTitle("Worktree Ticket", proj, mustDefaults(t))
	if res.BranchName != wantBranch {
		t.Errorf("branch_name = %q, want %q (shared derivation)", res.BranchName, wantBranch)
	}
	// Reuse invariant: a second CreateWorktree with the same branch+base
	// returns the SAME path, not a duplicate or an error.
	mgr := git.NewWorktreeManager(proj)
	reuse, werr := mgr.CreateWorktree(res.BranchName, res.BaseBranch)
	if werr != nil {
		t.Fatalf("reuse CreateWorktree: %v", werr)
	}
	if reuse != res.WorktreePath {
		t.Errorf("reuse path = %q, want %q (should reuse, not duplicate)", reuse, res.WorktreePath)
	}
}

func TestTicketNew_WorktreeConflictsNoWorktree(t *testing.T) {
	proj := newTicketTestProject(t, "delta")

	resetTicketNewFlags()
	t.Cleanup(resetTicketNewFlags)
	prevProj, prevTitle, prevNoWT := ticketNewProject, ticketNewTitle, ticketNewNoWorktree
	t.Cleanup(func() {
		ticketNewProject, ticketNewTitle, ticketNewNoWorktree = prevProj, prevTitle, prevNoWT
	})
	ticketNewProject = proj.ID
	ticketNewTitle = "Conflict"
	ticketNewWorktree = true
	ticketNewNoWorktree = true

	err := ticketNewCmd.RunE(ticketNewCmd, nil)
	if err == nil {
		t.Fatal("expected error for --worktree + --no-worktree, got nil")
	}
	if !strings.Contains(err.Error(), "contradictory") {
		t.Errorf("error %q does not mention contradictory", err.Error())
	}
}

// mustDefaults loads config defaults the same way `ticket new --worktree`
// does, so the branch-derivation assertion uses identical inputs.
func mustDefaults(t *testing.T) config.BoardSettings {
	t.Helper()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg.Defaults
}
