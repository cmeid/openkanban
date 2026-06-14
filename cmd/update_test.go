package cmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withSourcePath temporarily overrides the package-level SourcePath for
// a single test. Uses t.Cleanup so the original value is restored even
// if the test panics.
func withSourcePath(t *testing.T, path string) {
	t.Helper()
	prev := SourcePath
	SourcePath = path
	t.Cleanup(func() { SourcePath = prev })
}

// gitEnv is the environment we inject into every test `git` call so
// commits land cleanly in CI / sandbox environments that have no user
// config and no signing keys.
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.com",
		// Disable any user-level commit.gpgsign config that would
		// otherwise prompt for a signing key.
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
}

// runGit executes `git args...` in dir with our scrubbed env. Fails the
// test on non-zero exit so each step shows up close to where it broke.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, string(out))
	}
	return string(out)
}

// makeCommit writes filename with content under dir, stages it, and
// records a commit with the given message. Signing is disabled so the
// test doesn't depend on the user's GPG / SSH agent setup.
func makeCommit(t *testing.T, dir, filename, content, message string) {
	t.Helper()
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	runGit(t, dir, "add", filename)
	runGit(t, dir, "commit", "-m", message, "--no-gpg-sign")
}

// setupRepos creates a remote + local repo pair sharing one initial
// commit. The local repo is the "source clone"; the remote is added
// as `origin`. Returns (remotePath, localPath).
func setupRepos(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote")
	local := filepath.Join(root, "local")

	if err := os.Mkdir(remote, 0755); err != nil {
		t.Fatalf("mkdir remote: %v", err)
	}
	runGit(t, remote, "init", "-b", "main")
	// Configure locally so commits land in the right branch even on
	// older git versions where init -b is a no-op.
	runGit(t, remote, "config", "commit.gpgsign", "false")
	makeCommit(t, remote, "README.md", "initial\n", "initial commit")

	// Clone into local — origin is wired automatically.
	cmd := exec.Command("git", "clone", remote, local)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, string(out))
	}
	runGit(t, local, "config", "commit.gpgsign", "false")

	return remote, local
}

func TestUpdateCheckForUpdates_NoSourcePath(t *testing.T) {
	withSourcePath(t, "")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	status, err := CheckForUpdates(ctx)
	if err != nil {
		t.Fatalf("CheckForUpdates: %v", err)
	}
	if status.Available {
		t.Fatalf("expected Available=false, got %+v", status)
	}
	if status.Reason != "no source clone" {
		t.Fatalf("expected Reason=%q, got %q", "no source clone", status.Reason)
	}
}

func TestUpdateCheckForUpdates_UpToDate(t *testing.T) {
	_, local := setupRepos(t)
	withSourcePath(t, local)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := CheckForUpdates(ctx)
	if err != nil {
		t.Fatalf("CheckForUpdates: %v", err)
	}
	if status.Available {
		t.Fatalf("expected Available=false, got %+v", status)
	}
	if status.Reason != "up to date" {
		t.Fatalf("expected Reason=%q, got %q", "up to date", status.Reason)
	}
}

func TestUpdateCheckForUpdates_Behind(t *testing.T) {
	remote, local := setupRepos(t)
	withSourcePath(t, local)

	// Add a commit on remote that local doesn't have. We then fetch it
	// into local's object DB (without merging) so isAncestor can see
	// it — this mirrors what would happen on a real machine after any
	// prior `git fetch`. Without this, the remote commit isn't in
	// local's object DB and the ancestor probe returns false on both
	// sides, classifying as "diverged" instead of "behind".
	makeCommit(t, remote, "new.txt", "new\n", "second commit on remote")
	runGit(t, local, "fetch", "origin")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := CheckForUpdates(ctx)
	if err != nil {
		t.Fatalf("CheckForUpdates: %v", err)
	}
	if !status.Available {
		t.Fatalf("expected Available=true, got %+v", status)
	}
	if status.LocalSHA == "" || status.RemoteSHA == "" {
		t.Fatalf("expected non-empty SHAs, got %+v", status)
	}
	if status.LocalSHA == status.RemoteSHA {
		t.Fatalf("expected distinct SHAs, got both = %s", status.LocalSHA)
	}
	if len(status.LocalSHA) != 10 || len(status.RemoteSHA) != 10 {
		t.Fatalf("expected 10-char short SHAs, got local=%q remote=%q",
			status.LocalSHA, status.RemoteSHA)
	}
}

func TestUpdateCheckForUpdates_Ahead(t *testing.T) {
	_, local := setupRepos(t)
	withSourcePath(t, local)

	// Add a commit only on local — remote is unchanged. Local is
	// strictly ahead.
	makeCommit(t, local, "ahead.txt", "ahead\n", "local-only commit")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := CheckForUpdates(ctx)
	if err != nil {
		t.Fatalf("CheckForUpdates: %v", err)
	}
	if status.Available {
		t.Fatalf("expected Available=false, got %+v", status)
	}
	if status.Reason != "ahead" {
		t.Fatalf("expected Reason=%q, got %q", "ahead", status.Reason)
	}
}

func TestUpdateCheckForUpdates_Diverged(t *testing.T) {
	remote, local := setupRepos(t)
	withSourcePath(t, local)

	// Both sides get a unique commit on top of the shared base. After
	// a fetch into local, neither side's HEAD is an ancestor of the
	// other.
	makeCommit(t, remote, "remote-only.txt", "remote\n", "remote-only commit")
	makeCommit(t, local, "local-only.txt", "local\n", "local-only commit")
	runGit(t, local, "fetch", "origin")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := CheckForUpdates(ctx)
	if err != nil {
		t.Fatalf("CheckForUpdates: %v", err)
	}
	if status.Available {
		t.Fatalf("expected Available=false, got %+v", status)
	}
	if status.Reason != "diverged" {
		t.Fatalf("expected Reason=%q, got %q", "diverged", status.Reason)
	}
}

func TestUpdateCheckForUpdates_NoOriginRemote(t *testing.T) {
	// Plain git repo with one commit but no `origin` remote configured.
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	runGit(t, repo, "init", "-b", "main")
	runGit(t, repo, "config", "commit.gpgsign", "false")
	makeCommit(t, repo, "x.txt", "x\n", "init")

	withSourcePath(t, repo)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := CheckForUpdates(ctx)
	if err != nil {
		t.Fatalf("CheckForUpdates: %v", err)
	}
	if status.Available {
		t.Fatalf("expected Available=false, got %+v", status)
	}
	if status.Reason != "remote unreachable" {
		t.Fatalf("expected Reason=%q, got %q", "remote unreachable", status.Reason)
	}
}

// revParseHead returns the full SHA at the named ref in dir.
func revParseRef(t *testing.T, dir, ref string) string {
	t.Helper()
	return strings.TrimSpace(runGit(t, dir, "rev-parse", ref))
}

func TestSyncLocalMain_FeatureBranchAdvances(t *testing.T) {
	remote, local := setupRepos(t)

	// Remote is ahead of local main by one commit.
	makeCommit(t, remote, "new.txt", "new\n", "second commit on remote")

	// Switch to a feature branch so HEAD's symbolic-ref is not main.
	runGit(t, local, "checkout", "-b", "feat/x")

	preMain := revParseRef(t, local, "refs/heads/main")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	syncLocalMain(ctx, local)

	postMain := revParseRef(t, local, "refs/heads/main")
	remoteMain := revParseRef(t, remote, "refs/heads/main")

	if postMain == preMain {
		t.Fatalf("expected local main to advance; stayed at %s", preMain)
	}
	if postMain != remoteMain {
		t.Fatalf("expected local main to match remote (%s); got %s", remoteMain, postMain)
	}
}

func TestSyncLocalMain_OnMainSkips(t *testing.T) {
	remote, local := setupRepos(t)

	// Remote is ahead, but we stay on main so the helper should skip
	// (the existing pull in ApplyUpdate handles main).
	makeCommit(t, remote, "new.txt", "new\n", "second commit on remote")

	preMain := revParseRef(t, local, "refs/heads/main")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	syncLocalMain(ctx, local)

	postMain := revParseRef(t, local, "refs/heads/main")

	if postMain != preMain {
		t.Fatalf("expected no-op on main; local main moved from %s to %s", preMain, postMain)
	}
}

func TestSyncLocalMain_DivergedNoClobber(t *testing.T) {
	remote, local := setupRepos(t)

	// Diverge: remote gets one commit, local main gets a different commit,
	// then we switch off main so the helper actually runs.
	makeCommit(t, remote, "remote-only.txt", "remote\n", "remote-only commit")
	makeCommit(t, local, "local-only.txt", "local\n", "local-only on main")
	runGit(t, local, "checkout", "-b", "feat/x")

	preMain := revParseRef(t, local, "refs/heads/main")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	syncLocalMain(ctx, local)

	postMain := revParseRef(t, local, "refs/heads/main")

	if postMain != preMain {
		t.Fatalf("expected diverged local main to be untouched; moved from %s to %s", preMain, postMain)
	}
}

func TestSyncLocalMain_DetachedHEAD(t *testing.T) {
	remote, local := setupRepos(t)

	// Remote is ahead. Detach HEAD so symbolic-ref returns an error —
	// the helper must still advance local main.
	makeCommit(t, remote, "new.txt", "new\n", "second commit on remote")
	runGit(t, local, "checkout", "--detach", "HEAD")

	preMain := revParseRef(t, local, "refs/heads/main")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	syncLocalMain(ctx, local)

	postMain := revParseRef(t, local, "refs/heads/main")
	remoteMain := revParseRef(t, remote, "refs/heads/main")

	if postMain == preMain {
		t.Fatalf("expected detached-HEAD path to advance local main; stayed at %s", preMain)
	}
	if postMain != remoteMain {
		t.Fatalf("expected local main to match remote (%s); got %s", remoteMain, postMain)
	}
}

func TestUpdateCheckForUpdates_ContextCancelled(t *testing.T) {
	_, local := setupRepos(t)
	withSourcePath(t, local)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the call so every subprocess sees it

	_, err := CheckForUpdates(ctx)
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestShortHeadSHA_ReturnsAbbrev(t *testing.T) {
	_, local := setupRepos(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	short, err := shortHeadSHA(ctx, local)
	if err != nil {
		t.Fatalf("shortHeadSHA: %v", err)
	}
	if short == "" {
		t.Fatal("expected non-empty short SHA")
	}
	if len(short) >= 40 {
		t.Errorf("expected abbreviated SHA, got full-length %q", short)
	}

	full, err := localHeadSHA(ctx, local)
	if err != nil {
		t.Fatalf("localHeadSHA: %v", err)
	}
	// The abbreviated SHA must be a prefix of the full SHA; this catches
	// any future drift where we accidentally call a different ref.
	if len(full) < len(short) || full[:len(short)] != short {
		t.Errorf("short %q is not a prefix of full %q", short, full)
	}
}

func TestShortHeadSHA_NonRepo(t *testing.T) {
	dir := t.TempDir() // empty, not a git repo
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := shortHeadSHA(ctx, dir); err == nil {
		t.Fatal("expected error for non-repo path, got nil")
	}
}
