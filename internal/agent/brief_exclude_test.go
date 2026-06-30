package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureTicketsGitExcluded_PreventsStaging is the core proof: a real
// `git add -A` does NOT stage tickets/x.md after the helper runs, even
// though it would have before.
func TestEnsureTicketsGitExcluded_PreventsStaging(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("HOME", dir) // neutralize ~/.gitignore_global

	mustRun(t, dir, "git", "init")
	mustRun(t, dir, "git", "config", "user.email", "test@test.com")
	mustRun(t, dir, "git", "config", "user.name", "Test")

	// Create the brief file
	ticketsDir := filepath.Join(dir, "tickets")
	if err := os.MkdirAll(ticketsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ticketsDir, "x.md"), []byte("brief"), 0o644); err != nil {
		t.Fatal(err)
	}

	// BEFORE: verify the file IS stageable (negative control)
	mustRun(t, dir, "git", "add", "-A")
	beforeOut := runOutput(t, dir, "git", "status", "--porcelain")
	if !strings.Contains(beforeOut, "tickets/x.md") {
		t.Fatalf("expected tickets/x.md to be staged before helper; git status: %s", beforeOut)
	}
	mustRun(t, dir, "git", "reset") // unstage

	// Run the helper
	if err := EnsureTicketsGitExcluded(dir); err != nil {
		t.Fatalf("EnsureTicketsGitExcluded: %v", err)
	}

	// AFTER: verify the file is NOT staged
	mustRun(t, dir, "git", "add", "-A")
	afterOut := runOutput(t, dir, "git", "status", "--porcelain")
	if strings.Contains(afterOut, "tickets/x.md") {
		t.Fatalf("expected tickets/x.md NOT to be staged after helper; git status: %s", afterOut)
	}
}

// TestEnsureTicketsGitExcluded_Idempotent verifies two calls leave exactly
// one "tickets/" line in the exclude file.
func TestEnsureTicketsGitExcluded_Idempotent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("HOME", dir)

	mustRun(t, dir, "git", "init")

	if err := EnsureTicketsGitExcluded(dir); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := EnsureTicketsGitExcluded(dir); err != nil {
		t.Fatalf("second call: %v", err)
	}

	excludePath := filepath.Join(dir, ".git", "info", "exclude")
	data, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "tickets/" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one tickets/ line, got %d; content:\n%s", count, data)
	}
}

// TestEnsureTicketsGitExcluded_PreSeededGitignore verifies the probe
// short-circuits when a tracked .gitignore already covers tickets/, leaving
// the exclude file untouched.
func TestEnsureTicketsGitExcluded_PreSeededGitignore(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("HOME", dir)

	mustRun(t, dir, "git", "init")
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("tickets/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := EnsureTicketsGitExcluded(dir); err != nil {
		t.Fatalf("EnsureTicketsGitExcluded: %v", err)
	}

	excludePath := filepath.Join(dir, ".git", "info", "exclude")
	data, _ := os.ReadFile(excludePath)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "tickets/" {
			t.Fatalf("exclude file should not have gained a tickets/ line; content:\n%s", data)
		}
	}
}

// TestEnsureTicketsGitExcluded_LinkedWorktree verifies that calling from a
// linked worktree writes into the common git dir's info/exclude (repo-wide
// scope), proven by the primary repo dir also seeing tickets/ ignored.
func TestEnsureTicketsGitExcluded_LinkedWorktree(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("HOME", root)

	primary := filepath.Join(root, "primary")
	if err := os.MkdirAll(primary, 0o755); err != nil {
		t.Fatal(err)
	}
	mustRun(t, primary, "git", "init")
	mustRun(t, primary, "git", "config", "user.email", "test@test.com")
	mustRun(t, primary, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(primary, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, primary, "git", "add", "README.md")
	mustRun(t, primary, "git", "commit", "-m", "init")

	linked := filepath.Join(root, "linked")
	mustRun(t, primary, "git", "worktree", "add", linked, "-b", "feature")

	if err := EnsureTicketsGitExcluded(linked); err != nil {
		t.Fatalf("EnsureTicketsGitExcluded: %v", err)
	}

	// (a) the common-dir's info/exclude contains tickets/.
	commonDir := strings.TrimSpace(runOutput(t, linked, "git", "rev-parse", "--git-common-dir"))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(linked, commonDir)
	}
	excludePath := filepath.Join(commonDir, "info", "exclude")
	data, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read common-dir exclude %s: %v", excludePath, err)
	}
	if !strings.Contains(string(data), "tickets/") {
		t.Fatalf("common-dir exclude missing tickets/; content:\n%s", data)
	}

	// (b) the primary repo dir also sees tickets/ as ignored (common scope).
	cmd := exec.Command("git", "-C", primary, "check-ignore", "-q", "--no-index", "tickets/probe.md")
	if err := cmd.Run(); err != nil {
		t.Fatalf("expected tickets/ ignored from primary repo dir (common scope), check-ignore err: %v", err)
	}
}

// TestEnsureTicketsGitExcluded_NonGitDir verifies a non-git directory yields
// a non-nil error (callers treat it as non-fatal).
func TestEnsureTicketsGitExcluded_NonGitDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("HOME", dir)

	if err := EnsureTicketsGitExcluded(dir); err == nil {
		t.Fatal("expected non-nil error for non-git directory")
	}
}

func mustRun(t *testing.T, dir, cmd string, args ...string) {
	t.Helper()
	c := exec.Command(cmd, args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("command %s %v failed: %v\n%s", cmd, args, err, out)
	}
}

func runOutput(t *testing.T, dir, cmd string, args ...string) string {
	t.Helper()
	c := exec.Command(cmd, args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("command %s %v failed: %v\n%s", cmd, args, err, out)
	}
	return string(out)
}
