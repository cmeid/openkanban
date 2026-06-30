package agent

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// EnsureTicketsGitExcluded appends "tickets/" to the worktree's
// .git/info/exclude (local, never committed) so the generated brief at
// tickets/<slug>.md is never accidentally committed via `git add -A`.
// Idempotent; callers treat errors as non-fatal. No-op if the ignore
// stack already covers tickets/.
func EnsureTicketsGitExcluded(worktreePath string) error {
	// Probe first — if tickets/ is already ignored by any rule, nothing to do.
	if ticketsDirIgnored(worktreePath) {
		return nil
	}
	// Locate the exclude file via git so linked worktrees resolve to the
	// common git dir (repo-wide scope, not per-worktree).
	excludePath, err := resolveExcludeFile(worktreePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return err
	}
	// Read existing content and append only if not already present.
	existing, _ := os.ReadFile(excludePath)
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == "tickets/" {
			return nil // already present
		}
	}
	var buf bytes.Buffer
	buf.Write(existing)
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		buf.WriteByte('\n')
	}
	buf.WriteString("tickets/\n")
	return os.WriteFile(excludePath, buf.Bytes(), 0o644)
}

// ticketsDirIgnored reports whether the tickets/ directory is already
// covered by any ignore rule in the worktree's git stack.
func ticketsDirIgnored(worktreePath string) bool {
	// Probe a representative file path (not the bare dir) to match how
	// git check-ignore resolves patterns.
	cmd := exec.Command("git", "-C", worktreePath, "check-ignore", "-q", "--no-index", "tickets/probe.md")
	return cmd.Run() == nil
}

// resolveExcludeFile returns the absolute path of the info/exclude file
// for the git repository that owns worktreePath. For linked worktrees,
// git rev-parse --git-path returns a path inside the common .git dir.
func resolveExcludeFile(worktreePath string) (string, error) {
	out, err := exec.Command("git", "-C", worktreePath, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		return "", err
	}
	p := strings.TrimSpace(string(out))
	if !filepath.IsAbs(p) {
		p = filepath.Join(worktreePath, p)
	}
	return p, nil
}
