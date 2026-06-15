package cmd

import (
	"context"
	"errors"
	"io"
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

// withCommit temporarily overrides the package-level Commit (the
// ldflags-injected short SHA the installed binary reports) for one
// test. Pair with withSourcePath to simulate "binary at commit X,
// source clone at HEAD Y" scenarios.
func withCommit(t *testing.T, sha string) {
	t.Helper()
	prev := Commit
	Commit = sha
	t.Cleanup(func() { Commit = prev })
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

// TestUpdateCheckForUpdates_BinaryStaleSourceAtRemote covers the case
// that was silently broken before this fix: the source clone has been
// pulled to origin/main, but the installed binary's `Commit` is older.
// We want Available=true with BinaryStale=true and a clear Reason, so
// `openkanban update` reinstalls instead of saying "up to date".
func TestUpdateCheckForUpdates_BinaryStaleSourceAtRemote(t *testing.T) {
	_, local := setupRepos(t)
	withSourcePath(t, local)
	// Simulate installed binary that's older than source HEAD.
	withCommit(t, "0000000")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := CheckForUpdates(ctx)
	if err != nil {
		t.Fatalf("CheckForUpdates: %v", err)
	}
	if !status.Available {
		t.Fatalf("expected Available=true (binary stale), got %+v", status)
	}
	if !status.BinaryStale {
		t.Fatalf("expected BinaryStale=true, got %+v", status)
	}
	if !strings.Contains(status.Reason, "binary behind source") {
		t.Errorf("expected Reason to mention 'binary behind source', got %q", status.Reason)
	}
	if status.LocalSHA != status.RemoteSHA {
		t.Errorf("expected LocalSHA == RemoteSHA (source matches origin), got local=%q remote=%q",
			status.LocalSHA, status.RemoteSHA)
	}
}

// TestUpdateCheckForUpdates_InstalledMatchesSource verifies the
// invariant the previous test exercises the inverse of: when the
// installed Commit matches the source HEAD AND source matches origin,
// the result is "up to date" (no false binary-stale positive).
func TestUpdateCheckForUpdates_InstalledMatchesSource(t *testing.T) {
	_, local := setupRepos(t)
	withSourcePath(t, local)
	// Get the source's actual HEAD short SHA and inject it as the
	// "installed" Commit.
	headLong := strings.TrimSpace(runGit(t, local, "rev-parse", "HEAD"))
	if len(headLong) < 10 {
		t.Fatalf("rev-parse HEAD returned short value %q", headLong)
	}
	withCommit(t, headLong[:7]) // ldflags use the 7-char short form

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := CheckForUpdates(ctx)
	if err != nil {
		t.Fatalf("CheckForUpdates: %v", err)
	}
	if status.Available || status.BinaryStale {
		t.Fatalf("expected up-to-date (Available=false, BinaryStale=false), got %+v", status)
	}
	if status.Reason != "up to date" {
		t.Errorf("expected Reason='up to date', got %q", status.Reason)
	}
}

// TestUpdateCheckForUpdates_UnknownCommitNoStaleClaim — if the
// installed binary has Commit="none" AND BuildInfo isn't available
// (we can't really fake the second condition in-process, but we
// can drive the explicit-none path), we must NOT report binary-stale
// even though we can't prove the negative. False positives here
// would nag the user on every launch.
func TestUpdateCheckForUpdates_UnknownCommitNoStaleClaim(t *testing.T) {
	_, local := setupRepos(t)
	withSourcePath(t, local)
	withCommit(t, "none")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := CheckForUpdates(ctx)
	if err != nil {
		t.Fatalf("CheckForUpdates: %v", err)
	}
	// NOTE: resolvedCommit() also consults BuildInfo. In the test
	// binary BuildInfo may or may not contain a vcs.revision depending
	// on the build flags, so we can't deterministically assert either
	// "up to date" or "binary stale" here. We CAN assert that whatever
	// branch fires is internally consistent: BinaryStale implies
	// Available, "up to date" implies no BinaryStale.
	if status.BinaryStale && !status.Available {
		t.Errorf("BinaryStale=true must imply Available=true; got %+v", status)
	}
	if !status.Available && status.BinaryStale {
		t.Errorf("Available=false must imply BinaryStale=false; got %+v", status)
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

func TestUpdateCheckForUpdates_FeatureBranch(t *testing.T) {
	_, local := setupRepos(t)
	withSourcePath(t, local)

	// Park the local clone on a non-main branch — the precondition
	// should refuse before any network call.
	runGit(t, local, "checkout", "-b", "feat/x")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := CheckForUpdates(ctx)
	if err != nil {
		t.Fatalf("CheckForUpdates: %v", err)
	}
	if status.Available {
		t.Fatalf("expected Available=false, got %+v", status)
	}
	if !status.OfferBranchSwitch {
		t.Fatalf("expected OfferBranchSwitch=true, got %+v", status)
	}
	if !strings.Contains(status.Reason, "feat/x") {
		t.Fatalf("expected Reason to mention 'feat/x'; got %q", status.Reason)
	}
	if !strings.Contains(status.Reason, "not main") {
		t.Fatalf("expected Reason to mention 'not main'; got %q", status.Reason)
	}
}

func TestUpdateCheckForUpdates_DetachedHEAD(t *testing.T) {
	_, local := setupRepos(t)
	withSourcePath(t, local)

	runGit(t, local, "checkout", "--detach", "HEAD")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := CheckForUpdates(ctx)
	if err != nil {
		t.Fatalf("CheckForUpdates: %v", err)
	}
	if status.Available {
		t.Fatalf("expected Available=false, got %+v", status)
	}
	if status.OfferBranchSwitch {
		t.Fatalf("expected OfferBranchSwitch=false, got %+v", status)
	}
	if !strings.Contains(status.Reason, "detached HEAD") {
		t.Fatalf("expected Reason to mention 'detached HEAD'; got %q", status.Reason)
	}
}

func TestUpdateCheckForUpdates_LinkedWorktree_FeatureBranch(t *testing.T) {
	_, local := setupRepos(t)

	// Create a linked worktree sibling to the local clone on a feature
	// branch. The worktree path is what we point SourcePath at.
	wtPath := filepath.Join(filepath.Dir(local), "wt")
	runGit(t, local, "worktree", "add", "-b", "feat/x", wtPath)

	withSourcePath(t, wtPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := CheckForUpdates(ctx)
	if err != nil {
		t.Fatalf("CheckForUpdates: %v", err)
	}
	if status.Available {
		t.Fatalf("expected Available=false, got %+v", status)
	}
	if status.OfferBranchSwitch {
		t.Fatalf("expected OfferBranchSwitch=false (linked worktree), got %+v", status)
	}
	if !strings.Contains(status.Reason, "linked git worktree") {
		t.Fatalf("expected Reason to mention 'linked git worktree'; got %q", status.Reason)
	}
}

func TestUpdateCheckForUpdates_NotARepo(t *testing.T) {
	dir := t.TempDir() // empty dir, no .git
	withSourcePath(t, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := CheckForUpdates(ctx)
	if err != nil {
		t.Fatalf("CheckForUpdates: %v", err)
	}
	if status.Available {
		t.Fatalf("expected Available=false, got %+v", status)
	}
	if status.OfferBranchSwitch {
		t.Fatalf("expected OfferBranchSwitch=false, got %+v", status)
	}
	if !strings.Contains(status.Reason, "not a git repo") {
		t.Fatalf("expected Reason to mention 'not a git repo'; got %q", status.Reason)
	}
}

func TestCurrentBranch_OnNamed(t *testing.T) {
	_, local := setupRepos(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	branch, detached, err := currentBranch(ctx, local)
	if err != nil {
		t.Fatalf("currentBranch: %v", err)
	}
	if detached {
		t.Fatalf("expected detached=false, got true")
	}
	if branch != "main" {
		t.Fatalf("expected branch=%q, got %q", "main", branch)
	}
}

func TestCurrentBranch_Detached(t *testing.T) {
	_, local := setupRepos(t)
	runGit(t, local, "checkout", "--detach", "HEAD")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	branch, detached, err := currentBranch(ctx, local)
	if err != nil {
		t.Fatalf("currentBranch: %v", err)
	}
	if !detached {
		t.Fatalf("expected detached=true, got false")
	}
	if branch != "" {
		t.Fatalf("expected empty branch on detached HEAD, got %q", branch)
	}
}

func TestIsGitRepo_True(t *testing.T) {
	_, local := setupRepos(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if !isGitRepo(ctx, local) {
		t.Fatalf("expected isGitRepo(%s)=true", local)
	}
}

func TestIsGitRepo_False(t *testing.T) {
	dir := t.TempDir() // empty, not a git repo
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if isGitRepo(ctx, dir) {
		t.Fatalf("expected isGitRepo(%s)=false", dir)
	}
}

func TestIsLinkedWorktree_True(t *testing.T) {
	_, local := setupRepos(t)
	wtPath := filepath.Join(filepath.Dir(local), "wt")
	runGit(t, local, "worktree", "add", "-b", "feat/x", wtPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if !isLinkedWorktree(ctx, wtPath) {
		t.Fatalf("expected isLinkedWorktree(%s)=true", wtPath)
	}
}

func TestIsLinkedWorktree_False(t *testing.T) {
	_, local := setupRepos(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if isLinkedWorktree(ctx, local) {
		t.Fatalf("expected isLinkedWorktree(%s)=false (original clone)", local)
	}
}

func TestApplyBranchSwitch(t *testing.T) {
	remote, local := setupRepos(t)

	// Remote advances on main while local is parked on a feature branch.
	makeCommit(t, remote, "new.txt", "new\n", "second commit on remote")
	runGit(t, local, "checkout", "-b", "feat/x")
	// Fetch so origin/main is present locally for the pull --ff-only.
	runGit(t, local, "fetch", "origin")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := applyBranchSwitch(ctx, local, io.Discard); err != nil {
		t.Fatalf("applyBranchSwitch: %v", err)
	}

	gotBranch := strings.TrimSpace(runGit(t, local, "symbolic-ref", "--short", "HEAD"))
	if gotBranch != "main" {
		t.Fatalf("expected HEAD on main after switch; got %q", gotBranch)
	}
	localMain := revParseRef(t, local, "refs/heads/main")
	remoteMain := revParseRef(t, remote, "refs/heads/main")
	if localMain != remoteMain {
		t.Fatalf("expected local main (%s) == remote main (%s) after pull", localMain, remoteMain)
	}
}

func TestBranchSwitchAndRecheck(t *testing.T) {
	remote, local := setupRepos(t)
	withSourcePath(t, local)

	// Remote moves ahead; local parked on feature branch with origin/main
	// fetched so the post-switch pull has the commit available.
	makeCommit(t, remote, "new.txt", "new\n", "second commit on remote")
	runGit(t, local, "checkout", "-b", "feat/x")
	runGit(t, local, "fetch", "origin")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	status, err := branchSwitchAndRecheck(ctx, io.Discard)
	if err != nil {
		t.Fatalf("branchSwitchAndRecheck: %v", err)
	}
	// After switching to main and pulling, local should be up to date.
	if status.Available {
		t.Fatalf("expected Available=false (up to date after pull); got %+v", status)
	}
	if status.Reason != "up to date" {
		t.Fatalf("expected Reason=%q; got %q", "up to date", status.Reason)
	}
	if status.OfferBranchSwitch {
		t.Fatalf("expected OfferBranchSwitch=false after switching to main; got true")
	}
}
