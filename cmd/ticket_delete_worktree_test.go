package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/git"
	"github.com/techdufus/openkanban/internal/project"
)

// initDeleteTestRepo creates a fresh git repo with one commit on main and
// returns its path. Git invocations are isolated from the contributor's global
// config so signing / hooks / templates can't break the test.
func initDeleteTestRepo(t *testing.T, repoPath string) {
	t.Helper()
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoPath
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, string(out))
		}
	}
	runGit("init", "--initial-branch=main", repoPath)
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	runGit("add", "README.md")
	runGit("commit", "-m", "init")
}

// TestTicketDelete_RemovesWorktree verifies the CLI `ticket delete` tears down
// the ticket's worktree (Bug A — orphaned worktrees collide with later spawns).
func TestTicketDelete_RemovesWorktree(t *testing.T) {
	// Daemon-down isolation: no real daemon, autostart neutered. The daemon
	// RPCs in delete are best-effort and must not block the worktree teardown.
	daemonTestEnv(t)
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")

	// Anti-vacuity guard: the test asserts the worktree is removed, which only
	// happens when DeleteWorktree is on. If a future default-flip turns it off,
	// fail here rather than passing vacuously.
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if !cfg.Cleanup.DeleteWorktree {
		t.Fatal("test assumes Cleanup.DeleteWorktree default is true; guard tripped")
	}

	// A real git repo as the project root so worktree ops actually run.
	repoPath := filepath.Join(t.TempDir(), "repo")
	initDeleteTestRepo(t, repoPath)

	registry, err := project.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	proj := project.NewProject("wt-proj", repoPath)
	if err := registry.Add(proj); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	// Provision a real worktree the same way the CLI/TUI does.
	mgr := git.NewWorktreeManager(proj)
	wtPath, err := mgr.CreateWorktree("delete-me-x", "main")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("worktree dir not created: %v", err)
	}

	store, err := project.LoadTicketStore(proj)
	if err != nil {
		t.Fatalf("LoadTicketStore: %v", err)
	}
	ticket := board.NewTicket("delete me", proj.ID)
	ticket.WorktreePath = wtPath
	ticket.BranchName = "delete-me-x"
	if err := store.SaveTicket(ticket); err != nil {
		t.Fatalf("SaveTicket: %v", err)
	}

	ticketDeleteProject = proj.ID
	ticketDeleteID = string(ticket.ID)
	t.Cleanup(func() { ticketDeleteProject = ""; ticketDeleteID = "" })

	if err := ticketDeleteCmd.RunE(ticketDeleteCmd, nil); err != nil {
		t.Fatalf("ticketDeleteCmd.RunE: %v", err)
	}

	// The worktree directory must be gone after delete.
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("worktree dir %s still present after delete (err=%v); want removed", wtPath, err)
	}
}
