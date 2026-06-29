package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/techdufus/openkanban/internal/project"
)

type WorktreeManager struct {
	repoPath string
	baseDir  string
}

func NewWorktreeManager(p *project.Project) *WorktreeManager {
	return &WorktreeManager{
		repoPath: p.RepoPath,
		baseDir:  p.GetWorktreeDir(),
	}
}

func NewWorktreeManagerFromPaths(repoPath, baseDir string) *WorktreeManager {
	return &WorktreeManager{
		repoPath: repoPath,
		baseDir:  baseDir,
	}
}

func (m *WorktreeManager) CreateWorktree(branchName, baseBranch string) (string, error) {
	if err := os.MkdirAll(m.baseDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create worktree base directory: %w", err)
	}

	worktreePath := filepath.Join(m.baseDir, sanitizeBranchName(branchName))

	if _, err := os.Stat(worktreePath); err == nil {
		if m.isValidWorktree(worktreePath) {
			branch, err := m.BranchForWorktree(worktreePath)
			if err != nil {
				return "", err
			}
			expectedBranch := strings.TrimPrefix(branchName, "refs/heads/")
			if branch != expectedBranch {
				return "", fmt.Errorf("worktree path %s is already used by branch %q, not %q", worktreePath, branch, expectedBranch)
			}
			return worktreePath, nil
		}
		os.RemoveAll(worktreePath)
	}

	cmd := exec.Command("git", "worktree", "add", "-b", branchName, worktreePath, baseBranch)
	cmd.Dir = m.repoPath

	if output, err := cmd.CombinedOutput(); err != nil {
		out := string(output)
		if strings.Contains(out, "already exists") {
			// The branch already exists; retry without -b to reuse it.
			cmd = exec.Command("git", "worktree", "add", worktreePath, branchName)
			cmd.Dir = m.repoPath
			if output2, err2 := cmd.CombinedOutput(); err2 != nil {
				out2 := string(output2)
				// STALE/LIVE case surfaces here: the no-`-b` retry reports the
				// branch is already used by another worktree. (LIVE surfaces on
				// the primary add below.)
				if strings.Contains(out2, "is already used by worktree at") {
					return m.resolveBranchCheckedOutElsewhere(branchName, worktreePath, err2)
				}
				return "", fmt.Errorf("failed to create worktree: %s: %w", out2, err2)
			}
			return worktreePath, nil
		}
		// LIVE case: the conflicting worktree dir is on disk with the branch
		// checked out there, so the primary `add -b` fails directly.
		if strings.Contains(out, "is already used by worktree at") {
			return m.resolveBranchCheckedOutElsewhere(branchName, worktreePath, err)
		}
		return "", fmt.Errorf("failed to create worktree: %s: %w", out, err)
	}

	return worktreePath, nil
}

// resolveBranchCheckedOutElsewhere handles the "is already used by worktree
// at" failure from `git worktree add`. branchName is the branch we tried to
// create; worktreePath is the (free) path we wanted to add it at; cause is the
// originating git error.
//
// It distinguishes two cases by whether the conflicting worktree is still on
// disk:
//   - STALE (dir gone, registration lingers): prune the dead registration and
//     re-add the worktree at worktreePath, reusing the branch.
//   - LIVE (dir present, branch checked out there): return an actionable error
//     telling the operator how to unblock.
func (m *WorktreeManager) resolveBranchCheckedOutElsewhere(branchName, worktreePath string, cause error) (string, error) {
	conflictPath := m.conflictingWorktreePath(branchName)

	// No structured match (and no prose to scrape) — fall back to the generic
	// failure so we never silently swallow the error.
	if conflictPath == "" {
		return "", fmt.Errorf("failed to create worktree: branch %q is already used by another worktree: %w", branchName, cause)
	}

	if _, statErr := os.Stat(canonicalPath(conflictPath)); statErr == nil {
		// LIVE: the conflicting worktree exists on disk.
		return "", fmt.Errorf("branch %q is already checked out at %s; to unblock, run: git worktree remove %s\n  (or rename the ticket to derive a different branch): %w", branchName, conflictPath, conflictPath, cause)
	}

	// STALE: the directory is gone but the registration lingers. Prune it and
	// reuse the branch at the path we wanted.
	pruneCmd := exec.Command("git", "worktree", "prune")
	pruneCmd.Dir = m.repoPath
	pruneCmd.CombinedOutput()

	addCmd := exec.Command("git", "worktree", "add", worktreePath, branchName)
	addCmd.Dir = m.repoPath
	if output, err := addCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("failed to create worktree after pruning stale registration: %s: %w", string(output), err)
	}
	return worktreePath, nil
}

// conflictingWorktreePath finds the path of the worktree currently holding
// branchName. It prefers ListWorktrees (structured + already canonicalized for
// macOS /var vs /private/var), falling back to nothing if no match is found.
func (m *WorktreeManager) conflictingWorktreePath(branchName string) string {
	expected := strings.TrimPrefix(branchName, "refs/heads/")
	if worktrees, err := m.ListWorktrees(); err == nil {
		for _, wt := range worktrees {
			if strings.TrimPrefix(wt.Branch, "refs/heads/") == expected {
				return wt.Path
			}
		}
	}
	return ""
}

func (m *WorktreeManager) isValidWorktree(path string) bool {
	gitPath := filepath.Join(path, ".git")
	info, err := os.Stat(gitPath)
	if err != nil {
		return false
	}
	// Worktrees have a .git file (not directory) pointing to the main repo
	return !info.IsDir()
}

// BranchForWorktree returns the branch checked out at the given worktree path,
// as reported by `git worktree list --porcelain`. The returned name has the
// refs/heads/ prefix stripped so callers can compare against the user-facing
// branch name directly.
//
// Paths are compared after symlink resolution because git canonicalizes the
// paths it stores (on macOS, `/var/...` becomes `/private/var/...`), and a
// naive string compare would miss the same on-disk location.
func (m *WorktreeManager) BranchForWorktree(path string) (string, error) {
	worktrees, err := m.ListWorktrees()
	if err != nil {
		return "", err
	}
	target := canonicalPath(path)
	for _, wt := range worktrees {
		if canonicalPath(wt.Path) == target {
			return strings.TrimPrefix(wt.Branch, "refs/heads/"), nil
		}
	}
	return "", fmt.Errorf("worktree %s not found in git worktree list", path)
}

// canonicalPath returns a cleaned, symlink-resolved absolute path for
// comparison. If symlink resolution fails (e.g. the path doesn't exist), it
// falls back to filepath.Clean so callers still get a deterministic key.
func canonicalPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

func (m *WorktreeManager) RemoveWorktree(worktreePath string) error {
	cmd := exec.Command("git", "worktree", "remove", worktreePath, "--force")
	cmd.Dir = m.repoPath

	if output, err := cmd.CombinedOutput(); err != nil {
		if !strings.Contains(string(output), "not a working tree") {
			return fmt.Errorf("failed to remove worktree: %s: %w", string(output), err)
		}
	}

	if _, err := os.Stat(worktreePath); err == nil {
		if err := os.RemoveAll(worktreePath); err != nil {
			return fmt.Errorf("failed to remove worktree directory: %w", err)
		}
	}

	return nil
}

func (m *WorktreeManager) ListWorktrees() ([]Worktree, error) {
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	cmd.Dir = m.repoPath

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list worktrees: %w", err)
	}

	return parseWorktreeList(string(output)), nil
}

type Worktree struct {
	Path   string
	HEAD   string
	Branch string
}

func parseWorktreeList(output string) []Worktree {
	var worktrees []Worktree
	var current Worktree

	for _, line := range strings.Split(output, "\n") {
		if after, found := strings.CutPrefix(line, "worktree "); found {
			if current.Path != "" {
				worktrees = append(worktrees, current)
			}
			current = Worktree{Path: after}
		} else if after, found := strings.CutPrefix(line, "HEAD "); found {
			current.HEAD = after
		} else if after, found := strings.CutPrefix(line, "branch refs/heads/"); found {
			current.Branch = after
		}
	}

	if current.Path != "" {
		worktrees = append(worktrees, current)
	}

	return worktrees
}

func (m *WorktreeManager) GetDefaultBranch() (string, error) {
	cmd := exec.Command("git", "symbolic-ref", "refs/remotes/origin/HEAD")
	cmd.Dir = m.repoPath

	output, err := cmd.Output()
	if err == nil {
		branch := strings.TrimSpace(string(output))
		branch = strings.TrimPrefix(branch, "refs/remotes/origin/")
		return branch, nil
	}

	for _, branch := range []string{"main", "master"} {
		cmd := exec.Command("git", "rev-parse", "--verify", branch)
		cmd.Dir = m.repoPath
		if err := cmd.Run(); err == nil {
			return branch, nil
		}
	}

	return "main", nil
}

func (m *WorktreeManager) DeleteBranch(branchName string) error {
	cmd := exec.Command("git", "branch", "-D", branchName)
	cmd.Dir = m.repoPath

	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to delete branch: %s: %w", string(output), err)
	}

	return nil
}

// DeleteMergedBranch deletes branchName only if git considers it fully
// merged (`git branch -d`, which refuses an unmerged branch). It returns
// (true, nil) when the branch was deleted, (false, nil) when git declined
// because the branch has unmerged commits, and (false, err) on any other
// failure. Use this — not DeleteBranch — for branches openkanban did not
// itself create (e.g. a divergent branch a worktree was switched to),
// where force-deleting could silently discard real work.
func (m *WorktreeManager) DeleteMergedBranch(branchName string) (deleted bool, err error) {
	cmd := exec.Command("git", "branch", "-d", branchName)
	cmd.Dir = m.repoPath

	if output, err := cmd.CombinedOutput(); err != nil {
		out := string(output)
		if strings.Contains(out, "not fully merged") {
			return false, nil
		}
		return false, fmt.Errorf("failed to delete branch: %s: %w", out, err)
	}

	return true, nil
}

func (m *WorktreeManager) BranchExists(branchName string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", branchName)
	cmd.Dir = m.repoPath
	return cmd.Run() == nil
}

func (m *WorktreeManager) CreateBranch(branchName, baseBranch string) error {
	cmd := exec.Command("git", "branch", branchName, baseBranch)
	cmd.Dir = m.repoPath

	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create branch: %s: %w", string(output), err)
	}

	return nil
}

func (m *WorktreeManager) CheckoutBranch(branchName string) error {
	cmd := exec.Command("git", "checkout", branchName)
	cmd.Dir = m.repoPath

	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to checkout branch: %s: %w", string(output), err)
	}

	return nil
}

func (m *WorktreeManager) SetupBranch(branchName, baseBranch string) error {
	if !m.BranchExists(branchName) {
		if err := m.CreateBranch(branchName, baseBranch); err != nil {
			return err
		}
	}
	return m.CheckoutBranch(branchName)
}

func (m *WorktreeManager) HasUncommittedChanges(worktreePath string) (bool, error) {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = worktreePath

	output, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("failed to check git status: %w", err)
	}

	return len(strings.TrimSpace(string(output))) > 0, nil
}

func sanitizeBranchName(name string) string {
	name = strings.TrimPrefix(name, "refs/heads/")
	name = strings.TrimPrefix(name, "agent/")
	name = strings.TrimPrefix(name, "feature/")
	name = strings.ReplaceAll(name, "/", "-")
	return name
}

func ResolveMainRepo(path string) string {
	gitPath := filepath.Join(path, ".git")
	info, err := os.Stat(gitPath)
	if err != nil {
		return path
	}

	if info.IsDir() {
		return path
	}

	content, err := os.ReadFile(gitPath)
	if err != nil {
		return path
	}

	line := strings.TrimSpace(string(content))
	if after, found := strings.CutPrefix(line, "gitdir: "); found {
		if idx, _, hasWorktrees := strings.Cut(after, "/.git/worktrees/"); hasWorktrees {
			return idx
		}
	}

	return path
}
