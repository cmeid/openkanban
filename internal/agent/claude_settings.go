package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/techdufus/openkanban/internal/board"
)

// claudeSettingsLocalFile is the basename Claude Code uses for the
// project-local settings file (gitignored by convention).
const claudeSettingsLocalFile = "settings.local.json"

// claudeSubdir is the directory Claude Code reads settings from,
// relative to the project root.
const claudeSubdir = ".claude"

// SeedClaudeSettings merges entries from
// <repoPath>/.claude/settings.local.json into
// <worktreePath>/.claude/settings.local.json. Source-repo entries are
// added to the worktree file if absent; existing worktree entries are
// preserved. Creates directories and files as needed. Idempotent.
//
// If the source repo's gitignore stack does not already ignore
// .claude/, SeedClaudeSettings also writes a defensive
// <repoPath>/.claude/.gitignore containing settings.local.json so
// user-specific approvals cannot accidentally be committed.
//
// Either path being empty, or both being equal, is a no-op.
func SeedClaudeSettings(worktreePath, repoPath string) error {
	if worktreePath == "" || repoPath == "" || worktreePath == repoPath {
		return nil
	}
	if err := ensureRepoSettingsScaffolding(repoPath); err != nil {
		return fmt.Errorf("seed claude settings: %w", err)
	}
	src, err := readClaudeSettings(repoPath)
	if err != nil {
		return fmt.Errorf("seed claude settings: read source: %w", err)
	}
	dst, err := readClaudeSettings(worktreePath)
	if err != nil {
		return fmt.Errorf("seed claude settings: read worktree: %w", err)
	}
	merged, _ := mergeSettingsLocal(dst, src)
	if err := writeClaudeSettings(worktreePath, merged); err != nil {
		return fmt.Errorf("seed claude settings: write worktree: %w", err)
	}
	return nil
}

// PromoteClaudeSettingsOnTransition calls PromoteClaudeSettings only
// when a ticket's status transition matches the policy: oldStatus
// differs from newStatus, and newStatus is in_review or done. All
// other transitions (in_progress, backlog, same-status no-ops) are
// silent no-ops returning (nil, nil). This is the single
// policy gate for "the user has consciously moved this ticket far
// enough along the lifecycle that approvals granted during it should
// become per-repo defaults."
func PromoteClaudeSettingsOnTransition(worktreePath, repoPath string, oldStatus, newStatus board.TicketStatus) ([]string, error) {
	if oldStatus == newStatus {
		return nil, nil
	}
	if newStatus != board.StatusInReview && newStatus != board.StatusDone {
		return nil, nil
	}
	if worktreePath == "" || repoPath == "" {
		return nil, nil
	}
	return PromoteClaudeSettings(worktreePath, repoPath)
}

// PromoteClaudeSettings merges entries from
// <worktreePath>/.claude/settings.local.json into
// <repoPath>/.claude/settings.local.json. Worktree entries are added
// to the repo file if absent; existing repo entries are preserved.
// Idempotent. Returns the slice of newly-promoted entry strings for
// logging.
//
// Either path being empty, or both being equal, is a no-op
// (nil, nil). If the repo file would be unchanged, no write occurs and
// added is nil.
func PromoteClaudeSettings(worktreePath, repoPath string) ([]string, error) {
	if worktreePath == "" || repoPath == "" || worktreePath == repoPath {
		return nil, nil
	}
	src, err := readClaudeSettings(worktreePath)
	if err != nil {
		return nil, fmt.Errorf("promote claude settings: read worktree: %w", err)
	}
	if err := ensureRepoSettingsScaffolding(repoPath); err != nil {
		return nil, fmt.Errorf("promote claude settings: %w", err)
	}
	dst, err := readClaudeSettings(repoPath)
	if err != nil {
		return nil, fmt.Errorf("promote claude settings: read repo: %w", err)
	}
	merged, added := mergeSettingsLocal(dst, src)
	if len(added) == 0 {
		return nil, nil
	}
	if err := writeClaudeSettings(repoPath, merged); err != nil {
		return nil, fmt.Errorf("promote claude settings: write repo: %w", err)
	}
	return added, nil
}

// mergeSettingsLocal merges src into dst, preserving every dst entry
// and adding any permissions.{allow,ask,deny} entries from src that
// dst does not already have. Only the permissions arrays are touched;
// every other top-level key in dst is left untouched, and keys in src
// outside of permissions are ignored. The returned added slice lists
// new entries by their string form in stable per-bucket order (allow,
// then ask, then deny). dst is mutated in place and also returned.
func mergeSettingsLocal(dst, src map[string]any) (map[string]any, []string) {
	if dst == nil {
		dst = map[string]any{}
	}
	if src == nil {
		return dst, nil
	}
	srcPerms, _ := src["permissions"].(map[string]any)
	if srcPerms == nil {
		return dst, nil
	}
	dstPerms, _ := dst["permissions"].(map[string]any)
	if dstPerms == nil {
		dstPerms = map[string]any{}
	}
	var added []string
	for _, bucket := range []string{"allow", "ask", "deny"} {
		srcList, _ := srcPerms[bucket].([]any)
		if len(srcList) == 0 {
			continue
		}
		dstList, _ := dstPerms[bucket].([]any)
		seen := map[string]bool{}
		for _, e := range dstList {
			if s, ok := e.(string); ok {
				seen[s] = true
			}
		}
		for _, e := range srcList {
			s, ok := e.(string)
			if !ok || seen[s] {
				continue
			}
			seen[s] = true
			dstList = append(dstList, s)
			added = append(added, s)
		}
		if len(dstList) > 0 {
			dstPerms[bucket] = dstList
		}
	}
	if len(dstPerms) > 0 {
		dst["permissions"] = dstPerms
	}
	return dst, added
}

func readClaudeSettings(root string) (map[string]any, error) {
	path := filepath.Join(root, claudeSubdir, claudeSettingsLocalFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	out := map[string]any{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return out, nil
}

func writeClaudeSettings(root string, settings map[string]any) error {
	dir := filepath.Join(root, claudeSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, claudeSettingsLocalFile)
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// ensureRepoSettingsScaffolding makes sure <repoPath>/.claude/ exists
// and, when the repo's gitignore stack doesn't already cover .claude/,
// writes <repoPath>/.claude/.gitignore with settings.local.json so a
// user-specific approvals file can never be accidentally committed.
func ensureRepoSettingsScaffolding(repoPath string) error {
	claudeDir := filepath.Join(repoPath, claudeSubdir)
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		return err
	}
	if repoGitignoresClaudeDir(repoPath) {
		return nil
	}
	innerGitignore := filepath.Join(claudeDir, ".gitignore")
	if _, err := os.Stat(innerGitignore); err == nil {
		return nil
	}
	return os.WriteFile(innerGitignore, []byte(claudeSettingsLocalFile+"\n"), 0o644)
}

// repoGitignoresClaudeDir asks git whether
// <repoPath>/.claude/settings.local.json would be ignored by the
// existing gitignore stack. Uses `git check-ignore` so root .gitignore,
// nested .gitignores, info/exclude, and core.excludesFile are all
// respected. Returns false on any git error (including the repo not
// being a git repo) so the defensive inner .gitignore still gets
// written.
func repoGitignoresClaudeDir(repoPath string) bool {
	probe := filepath.Join(claudeSubdir, claudeSettingsLocalFile)
	cmd := exec.Command("git", "-C", repoPath, "check-ignore", "-q", "--no-index", probe)
	return cmd.Run() == nil
}
