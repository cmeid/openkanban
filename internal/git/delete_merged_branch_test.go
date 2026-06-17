package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitEnv isolates git invocations from the contributor's global config so
// signing / hooks / templates can't break the test.
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
}

func runGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, string(out))
	}
}

func TestDeleteMergedBranch(t *testing.T) {
	repoPath := initTestRepo(t)
	baseDir := filepath.Join(t.TempDir(), "worktrees")
	mgr := NewWorktreeManagerFromPaths(repoPath, baseDir)

	t.Run("merged branch is deleted", func(t *testing.T) {
		// A branch at main's tip is fully merged → safe to delete.
		runGitIn(t, repoPath, "branch", "merged-x", "main")

		deleted, err := mgr.DeleteMergedBranch("merged-x")
		if err != nil {
			t.Fatalf("DeleteMergedBranch: %v", err)
		}
		if !deleted {
			t.Error("merged branch should report deleted=true")
		}
		if mgr.BranchExists("merged-x") {
			t.Error("merged branch still exists after delete")
		}
	})

	t.Run("unmerged branch is preserved", func(t *testing.T) {
		// Build a branch with a commit not on main, via a worktree, then
		// drop the worktree so the branch isn't checked out anywhere.
		wt, err := mgr.CreateWorktree("unmerged-x", "main")
		if err != nil {
			t.Fatalf("CreateWorktree: %v", err)
		}
		if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		runGitIn(t, wt, "add", "f.txt")
		runGitIn(t, wt, "commit", "-m", "unmerged work")
		if err := mgr.RemoveWorktree(wt); err != nil {
			t.Fatalf("RemoveWorktree: %v", err)
		}

		deleted, err := mgr.DeleteMergedBranch("unmerged-x")
		if err != nil {
			t.Fatalf("DeleteMergedBranch returned error for unmerged branch (want graceful skip): %v", err)
		}
		if deleted {
			t.Error("unmerged branch should NOT be deleted (deleted=true)")
		}
		if !mgr.BranchExists("unmerged-x") {
			t.Error("unmerged branch was lost — safe-delete must preserve it")
		}
	})
}
