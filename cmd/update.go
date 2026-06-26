package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/finishskill"
	"github.com/techdufus/openkanban/internal/service"
)

// UpdateStatus is the result of a CheckForUpdates call.
type UpdateStatus struct {
	// Available is true when an update action is possible: either the
	// local HEAD is strictly behind origin/main (a fast-forward exists)
	// OR the installed binary's commit is older than the source HEAD
	// (a rebuild fixes things even with no pull). False when up to
	// date, ahead, diverged, or unable to check.
	Available bool

	// LocalSHA and RemoteSHA are the short-form (10-char) SHAs.
	// Empty when Available is false.
	LocalSHA  string
	RemoteSHA string

	// InstalledSHA is the commit the running binary was built from
	// (resolvedCommit), populated only in the binary-stale case where it
	// differs from the source HEAD. Used purely for display: when the
	// source already matches origin/main (LocalSHA == RemoteSHA) but the
	// binary lags, showing LocalSHA on both sides of the "X -> Y" summary
	// reads as a no-op. Empty otherwise.
	InstalledSHA string

	// Reason describes why a check was not actionable when Available
	// is false: "up to date", "ahead", "diverged", "no source clone",
	// "remote unreachable", etc.
	Reason string

	// OfferBranchSwitch indicates the refusal is fixable by `git checkout
	// main && git pull --ff-only origin main` on the source clone. True
	// only when the source is on a non-main named branch AND is not a
	// linked git worktree (which would refuse the checkout). Callers MAY
	// prompt the user; CheckForUpdates does not switch on its own.
	OfferBranchSwitch bool

	// BinaryStale signals "source is at the right commit but the
	// installed binary is older" — i.e., the user pulled (or the source
	// clone was already up-to-date) but didn't reinstall. The update
	// path still runs `go install` in this case; no `git pull` happens
	// because the source is already correct.
	BinaryStale bool
}

// displayFromSHA returns the SHA to show on the left of the "X -> Y"
// update summary. In the binary-stale-only case the source is already at
// origin/main (LocalSHA == RemoteSHA), so printing LocalSHA on both sides
// reads as a no-op ("c3bab6 -> c3bab6"); we show the installed binary's
// commit instead to make the staleness legible. Falls back to LocalSHA
// whenever the installed commit is unknown or a real pull is pending.
func (s UpdateStatus) displayFromSHA() string {
	if s.BinaryStale && s.LocalSHA == s.RemoteSHA && s.InstalledSHA != "" {
		return s.InstalledSHA
	}
	return s.LocalSHA
}

// daemonUpdatedMsg is printed after the macOS bundle refresh. It's honest
// for both daemon modes: a running daemon picks the new binary up on its
// own (watchBinaryStaleness restarts a persistent daemon once its sessions
// drain, and a default-mode daemon exits when its TUI quits — the next
// launch is new). A manual `daemon restart` is only needed to force the
// update onto in-progress sessions immediately, which ends them.
const daemonUpdatedMsg = "daemon binary updated — a running daemon picks this up automatically once its sessions finish; to apply now (ending sessions) run 'openkanban daemon restart'"

// updateCheckOnly is bound to --check on the update subcommand.
var updateCheckOnly bool

// packageManagerFallback is the text printed when SourcePath is empty
// (release build / installed via brew / `go install`). The instructions
// don't depend on how the binary was actually installed because we have
// no reliable way to detect that — listing both options covers it.
const packageManagerFallback = `update check unavailable (release build / no source clone)
to upgrade:
  brew upgrade openkanban    # if installed via Homebrew
  go install github.com/cmeid/openkanban@latest    # if installed via go install`

var updateCmd = &cobra.Command{
	Use:           "update",
	Short:         "Check for and apply openkanban updates",
	Long:          `Compare the local source clone of this openkanban binary against origin/main and, when --check is not set and an update is available, fast-forward + go install in place.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if SourcePath == "" {
			fmt.Fprintln(cmd.OutOrStdout(), packageManagerFallback)
			return nil
		}

		timeout := 1500 * time.Millisecond
		if !updateCheckOnly {
			timeout = 5 * time.Second
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
		defer cancel()

		status, err := CheckForUpdates(ctx)
		if err != nil {
			return fmt.Errorf("check for updates: %w", err)
		}

		if !status.Available {
			fmt.Fprintln(cmd.OutOrStdout(), status.Reason)
			if !status.OfferBranchSwitch || updateCheckOnly {
				return nil
			}
			// Re-derive the branch name for the prompt. Cheap (~10ms);
			// avoids adding a field to UpdateStatus just for display.
			branch, _, _ := currentBranch(cmd.Context(), SourcePath)
			choice, _ := runBranchSwitchPrompt(branch)
			if choice != promptApply {
				return nil
			}
			newStatus, err := branchSwitchAndRecheck(cmd.Context(), os.Stderr)
			if err != nil {
				return fmt.Errorf("switch to main failed: %w\n"+
					"fix the source clone manually or reinstall via ./scripts/install.sh", err)
			}
			status = newStatus
			if !status.Available {
				fmt.Fprintln(cmd.OutOrStdout(), status.Reason)
				return nil
			}
			// Fall through into the "update available" path below.
		}

		fmt.Fprintf(cmd.OutOrStdout(), "update available: %s -> %s\n", status.displayFromSHA(), status.RemoteSHA)

		if updateCheckOnly {
			return nil
		}

		return runUpdate(cmd.Context(), cmd.OutOrStdout(), status)
	},
}

// runUpdate performs the fast-forward pull + go install. Output streams
// to stderr so the user sees git/go progress. Delegates the actual
// work to ApplyUpdate so the launch-time prompt and the `update`
// subcommand share one code path.
func runUpdate(ctx context.Context, out io.Writer, status UpdateStatus) error {
	return ApplyUpdate(ctx, status, out)
}

// ApplyUpdate runs the git pull + go install steps for an available
// update. Streams progress to out (for the "updating ..." / "installed"
// summary lines); the underlying git/go subprocess stdout+stderr go to
// os.Stderr so the user sees them live. Returns nil on success.
//
// Caller is responsible for verifying status.Available before calling.
// We give the whole flow a generous timeout — pull + go install can be
// slow on a cold cache.
func ApplyUpdate(ctx context.Context, status UpdateStatus, out io.Writer) error {
	if status.BinaryStale && status.LocalSHA == status.RemoteSHA {
		installed := status.InstalledSHA
		if installed == "" {
			installed = "older commit"
		}
		fmt.Fprintf(out, "rebuilding %s (source at %s; installed binary at %s is older)\n", SourcePath, status.LocalSHA, installed)
	} else {
		fmt.Fprintf(out, "updating %s: %s -> %s\n", SourcePath, status.LocalSHA, status.RemoteSHA)

		// Best-effort: fast-forward the local main ref toward origin/main
		// before pulling on the current branch. Without this, running update
		// from a feature-branch worktree advances the feature branch but
		// leaves local main behind — the next branch cut from a stale base.
		// Errors are intentionally swallowed; the real update is the pull.
		syncCtx, cancelSync := context.WithTimeout(ctx, 60*time.Second)
		syncLocalMain(syncCtx, SourcePath)
		cancelSync()

		pullCtx, cancelPull := context.WithTimeout(ctx, 60*time.Second)
		defer cancelPull()
		pull := exec.CommandContext(pullCtx, "git", "-C", SourcePath, "pull", "--ff-only", "origin", "main")
		pull.Stdout = os.Stderr
		pull.Stderr = os.Stderr
		if err := pull.Run(); err != nil {
			return fmt.Errorf("git pull: %w", err)
		}
	}

	installCtx, cancelInstall := context.WithTimeout(ctx, 5*time.Minute)
	defer cancelInstall()
	install := buildInstallCmd(installCtx, SourcePath)
	install.Stdout = os.Stderr
	install.Stderr = os.Stderr
	if err := install.Run(); err != nil {
		return fmt.Errorf("go install: %w", err)
	}

	if gobin := resolveGoBin(); gobin != "" {
		fmt.Fprintf(out, "installed: %s/openkanban\n", gobin)
	} else {
		fmt.Fprintln(out, "installed")
	}

	// Refresh the vendored close-out skill from the freshly-installed
	// binary's embed. The launch path also self-heals (and the re-exec
	// after update runs it), but doing it here too makes the update
	// immediately complete. Best-effort: never fail the update over it.
	if home, herr := os.UserHomeDir(); herr == nil && home != "" {
		if _, serr := finishskill.EnsureInstalled(home); serr != nil {
			fmt.Fprintf(out, "note: could not refresh close-out skill: %v\n", serr)
		}
	}

	// Refresh the macOS app-bundle daemon binary. The daemon forks from
	// ~/Applications/OpenKanban.app/Contents/MacOS/openkanbankd, so
	// without this step daemon-side changes never deploy via `update`.
	installedBin := filepath.Join(resolveGoBin(), "openkanban")
	if resolveGoBin() == "" {
		installedBin = ""
	}
	restarted := assembleBundle(ctx, out, runtime.GOOS, SourcePath, installedBin,
		func(ctx context.Context, name string, args ...string) error {
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Stdout = out
			cmd.Stderr = out
			return cmd.Run()
		})
	if restarted {
		fmt.Fprintln(out, "launchd service updated and restarted automatically")
	} else {
		fmt.Fprintln(out, daemonUpdatedMsg)
	}

	return nil
}

// assembleBundlePlistInstalledFn and assembleBundleInstallFn are seams for
// testing — they let assembleBundle tests verify the re-bootstrap path without
// shelling out to launchctl. In production they delegate to the service package.
var (
	assembleBundlePlistInstalledFn = service.PlistInstalled
	assembleBundleInstallFn        = func(binPath, logPath string) (string, error) {
		return service.Install(binPath, logPath)
	}
)

// assembleBundle refreshes the macOS .app bundle daemon binary after a
// go install, mirroring scripts/install.sh. No-op off darwin. Non-fatal:
// a missing/failing bundle script warns but does not fail the update
// (the CLI is already updated; the bundle is the daemon's binary source
// that the user can otherwise repair with ./scripts/install.sh).
//
// Returns true when the launchd service was successfully re-bootstrapped
// (daemon restarted automatically via launchd); false otherwise. The caller
// uses this to decide whether to print a "run daemon restart" hint.
func assembleBundle(ctx context.Context, out io.Writer, goos, sourcePath, installedBin string, run func(ctx context.Context, name string, args ...string) error) bool {
	if goos != "darwin" {
		return false
	}
	if installedBin == "" {
		fmt.Fprintln(out, "note: could not resolve installed binary path; daemon bundle not refreshed — run ./scripts/install.sh")
		return false
	}
	script := filepath.Join(sourcePath, "dist", "macos", "build-bundle.sh")
	if st, err := os.Stat(script); err != nil || st.IsDir() {
		fmt.Fprintf(out, "note: bundle script %s not found; daemon binary not refreshed — run ./scripts/install.sh to update the daemon\n", script)
		return false
	}
	dest := filepath.Join(os.Getenv("HOME"), "Applications")
	fmt.Fprintf(out, "refreshing OpenKanban.app daemon bundle (%s)\n", dest)
	if err := run(ctx, script, installedBin, dest); err != nil {
		fmt.Fprintf(out, "warning: bundle refresh failed (%v); daemon binary not updated — run ./scripts/install.sh\n", err)
		return false
	}

	// Re-bootstrap the launchd service when it was previously installed.
	//
	// build-bundle.sh runs `codesign --force --deep`, which invalidates
	// launchd's loaded job registration. Without this step, launchd fails to
	// spawn the updated binary with EX_CONFIG (78) on the next launch and
	// enters a ThrottleInterval loop — making `daemon start` / `daemon restart`
	// hang with "context deadline exceeded" until the user manually runs
	// `openkanban daemon install-service`. Re-bootstrapping here fixes that.
	//
	// Guarded by PlistInstalled so we never silently opt a user into launchd
	// supervision: the re-bootstrap only fires when they already have a plist.
	if ok, _ := assembleBundlePlistInstalledFn(); ok {
		bundleBin := filepath.Join(dest, "OpenKanban.app", "Contents", "MacOS", "openkanbankd")
		lp, lpErr := daemon.LogPath()
		if lpErr != nil {
			fmt.Fprintf(out, "warning: could not resolve daemon log path (%v); run: openkanban daemon install-service\n", lpErr)
			return false
		}
		if _, serr := assembleBundleInstallFn(bundleBin, lp); serr != nil {
			fmt.Fprintf(out, "warning: could not re-bootstrap launchd service (%v); run: openkanban daemon install-service\n", serr)
			return false
		}
		return true // daemon restarted automatically via launchd
	}
	return false
}

// buildInstallCmd constructs the `go install` command that rebuilds and
// reinstalls openkanban from the source clone at sourcePath.
//
// It sets cmd.Dir = sourcePath, which is load-bearing: `go install <pkg>`
// resolves the main module from the subprocess's working directory, NOT
// from the package argument. openkanban is typically launched from the
// user's home dir (or anywhere outside the module), so without Dir the
// rebuild fails with "go.mod file not found in current directory or any
// parent directory". This mirrors scripts/install.sh's `cd "$REPO_ROOT"`
// before `go install`; the package target is "." for the same reason.
//
// ldflags bake: SourcePath (so the rebuilt binary keeps source-clone
// awareness), BuildMarker=official (so the PersistentPreRunE guard lets
// it run — bare `go install .` would skip this and produce a stub), and
// best-effort Commit (so `openkanban version` reflects the rebuild; a
// missing commit is non-fatal and degrades to the default).
func buildInstallCmd(ctx context.Context, sourcePath string) *exec.Cmd {
	ldflags := fmt.Sprintf("-X github.com/techdufus/openkanban/cmd.SourcePath=%s", sourcePath)
	ldflags += " -X github.com/techdufus/openkanban/cmd.BuildMarker=official"
	if c, err := shortHeadSHA(ctx, sourcePath); err == nil && c != "" {
		ldflags += fmt.Sprintf(" -X github.com/techdufus/openkanban/cmd.Commit=%s", c)
	}
	cmd := exec.CommandContext(ctx, "go", "install", "-ldflags", ldflags, ".")
	cmd.Dir = sourcePath
	return cmd
}

// syncLocalMain best-effort fast-forwards the local `main` ref toward
// `origin/main` in the source clone, regardless of which branch is
// currently checked out. Uses `git fetch origin main:main`, which
// refuses non-fast-forwards by construction — a diverged local main
// (has commits not on origin/main) is naturally left untouched.
//
// Skipped when the current branch IS main: the existing
// `git pull --ff-only origin main` in ApplyUpdate already moves it.
// Detached HEAD is NOT main (symbolic-ref fails) so the helper still
// runs, which is what we want — refs/heads/main otherwise stays stale.
//
// All errors are swallowed by design: the helper is additive hygiene
// before the real pull. A failure here must not prevent the update.
func syncLocalMain(ctx context.Context, sourcePath string) {
	if sourcePath == "" {
		return
	}
	out, err := exec.CommandContext(ctx, "git", "-C", sourcePath, "symbolic-ref", "--short", "HEAD").Output()
	if err == nil && strings.TrimSpace(string(out)) == "main" {
		return
	}
	_ = exec.CommandContext(ctx, "git", "-C", sourcePath, "fetch", "origin", "main:main").Run()
}

// resolveGoBin returns the directory where `go install` placed the
// updated binary. Preference order matches the Go toolchain itself:
// $GOBIN, then $GOPATH/bin, then $HOME/go/bin. Returns "" only when
// none of those can be resolved.
func resolveGoBin() string {
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		return gobin
	}
	if gopath := os.Getenv("GOPATH"); gopath != "" {
		return gopath + "/bin"
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home + "/go/bin"
	}
	return ""
}

// CheckForUpdates queries origin/main without fetching and compares
// against the local HEAD of the source clone (cmd.SourcePath). Returns
// a non-error status in normal "not behind" cases; only returns an
// error for genuinely broken inputs (e.g. context cancelled, malformed
// git output). Network failures, missing remote, and empty SourcePath
// all return (status with Available=false, nil).
func CheckForUpdates(ctx context.Context) (UpdateStatus, error) {
	if SourcePath == "" {
		return UpdateStatus{Available: false, Reason: "no source clone"}, nil
	}

	if !isGitRepo(ctx, SourcePath) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return UpdateStatus{}, ctxErr
		}
		return UpdateStatus{
			Available: false,
			Reason: fmt.Sprintf(
				"source clone at %s is not a git repo (or unreadable) — "+
					"reinstall via ./scripts/install.sh from a fresh clone",
				SourcePath),
		}, nil
	}

	branch, detached, err := currentBranch(ctx, SourcePath)
	if err != nil {
		return UpdateStatus{}, err // context-cancel only
	}

	switch {
	case detached:
		return UpdateStatus{
			Available: false,
			Reason: fmt.Sprintf(
				"source clone at %s has detached HEAD — switch back first:\n"+
					"  git -C %s checkout main && git -C %s pull --ff-only origin main\n"+
					"then re-run `openkanban update`",
				SourcePath, SourcePath, SourcePath),
		}, nil

	case branch != "main":
		if isLinkedWorktree(ctx, SourcePath) {
			return UpdateStatus{
				Available: false,
				Reason: fmt.Sprintf(
					"source clone at %s is a linked git worktree on branch %q — "+
						"switch the main clone to main and reinstall to update",
					SourcePath, branch),
			}, nil
		}
		return UpdateStatus{
			Available:         false,
			OfferBranchSwitch: true,
			Reason: fmt.Sprintf(
				"source clone at %s on branch %q, not main — run `openkanban update` to switch, "+
					"or `git -C %s checkout main && git -C %s pull --ff-only origin main` manually",
				SourcePath, branch, SourcePath, SourcePath),
		}, nil
	}

	remoteSHA, err := remoteMainSHA(ctx, SourcePath)
	if err != nil {
		// Context cancellation is a genuine error — propagate. Anything
		// else (no remote, network failure, unknown ref) collapses to
		// "remote unreachable" so callers don't have to switch on the
		// flavor of network problem.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return UpdateStatus{}, ctxErr
		}
		return UpdateStatus{Available: false, Reason: "remote unreachable"}, nil
	}

	localSHA, err := localHeadSHA(ctx, SourcePath)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return UpdateStatus{}, ctxErr
		}
		return UpdateStatus{}, fmt.Errorf("rev-parse HEAD: %w", err)
	}

	// Compute the installed binary's commit so we can detect "source is
	// at the right HEAD but the binary on disk is older" — the classic
	// "I `git pull`-ed but forgot to reinstall" case. installedShort is
	// the short SHA the version-time ldflags / BuildInfo provide (or ""
	// when neither is available, in which case we can't say either way
	// and conservatively don't claim binary-stale).
	//
	// Comparison: prefix-match against the FULL local SHA, because the
	// ldflags-injected Commit (`git rev-parse --short HEAD`, ~7 chars)
	// is shorter than short(localSHA) (10 chars), so a string-equality
	// check would mis-classify identical commits as stale. The prefix
	// match is correct for any short-SHA length (including a fully-
	// expanded 40-char ldflags value, hypothetically).
	installedShort := resolvedCommit()
	binaryStale := installedShort != "" && !strings.HasPrefix(localSHA, installedShort)

	if localSHA == remoteSHA {
		if binaryStale {
			// Source is at origin/main but the binary lags. ApplyUpdate
			// will skip the (no-op) pull and just rebuild.
			return UpdateStatus{
				Available:    true,
				LocalSHA:     short(localSHA),
				RemoteSHA:    short(remoteSHA),
				InstalledSHA: installedShort,
				BinaryStale:  true,
				Reason:       "binary behind source — needs reinstall",
			}, nil
		}
		return UpdateStatus{Available: false, Reason: "up to date"}, nil
	}

	// Determine ahead / behind / diverged via two ancestor probes. Both
	// commits must exist locally for these to be meaningful; if the
	// remote commit isn't in the local object DB (no recent fetch), the
	// ancestor probe will fail and we fall through to "diverged" which
	// is the safest "we can't ff-only" classification.
	behind := isAncestor(ctx, SourcePath, localSHA, remoteSHA)
	ahead := isAncestor(ctx, SourcePath, remoteSHA, localSHA)

	switch {
	case behind && !ahead:
		return UpdateStatus{
			Available:   true,
			LocalSHA:    short(localSHA),
			RemoteSHA:   short(remoteSHA),
			BinaryStale: binaryStale,
		}, nil
	case ahead && !behind:
		return UpdateStatus{Available: false, Reason: "ahead"}, nil
	default:
		// Either truly diverged, or the remote commit isn't reachable
		// locally yet (no fetch since it was pushed). Both are
		// non-actionable for a fast-forward.
		return UpdateStatus{Available: false, Reason: "diverged"}, nil
	}
}

// remoteMainSHA returns the SHA at refs/heads/main on the "origin"
// remote, using `git ls-remote` so we don't pollute local refs with a
// fetch. Cwd is the source clone so the remote URL resolves from its
// git config.
func remoteMainSHA(ctx context.Context, sourcePath string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", sourcePath, "ls-remote", "origin", "refs/heads/main").Output()
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "", errors.New("ls-remote returned no output")
	}
	// Output is `<sha>\trefs/heads/main`; first whitespace-separated
	// token is the SHA. Multi-line shouldn't happen for a single ref
	// query, but tolerate it by taking the first line.
	if nl := strings.IndexByte(text, '\n'); nl != -1 {
		text = text[:nl]
	}
	fields := strings.Fields(text)
	if len(fields) < 1 || !looksLikeSHA(fields[0]) {
		return "", fmt.Errorf("malformed ls-remote output: %q", text)
	}
	return fields[0], nil
}

// localHeadSHA returns the SHA at HEAD of the source clone.
func localHeadSHA(ctx context.Context, sourcePath string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", sourcePath, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	if !looksLikeSHA(sha) {
		return "", fmt.Errorf("malformed rev-parse output: %q", sha)
	}
	return sha, nil
}

// shortHeadSHA returns the abbreviated HEAD SHA of the source clone,
// matching scripts/install.sh's `git rev-parse --short HEAD`. Returns
// "" + a nil error when the path isn't a git repo; callers can treat
// either error or empty as "no commit to bake."
func shortHeadSHA(ctx context.Context, sourcePath string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", sourcePath, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// isGitRepo reports whether sourcePath is the working tree of a git
// repo (handles linked worktrees too — `rev-parse --is-inside-work-tree`
// returns "true" from inside any worktree). Distinguishes "not a repo"
// from "detached HEAD" before we touch symbolic-ref.
func isGitRepo(ctx context.Context, sourcePath string) bool {
	out, err := exec.CommandContext(ctx, "git", "-C", sourcePath, "rev-parse", "--is-inside-work-tree").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// currentBranch returns the short name of the branch HEAD points to.
// Returns ("", true, nil) when HEAD is detached. Returns ("", false,
// err) only on context cancellation. Callers MUST verify the path is a
// git repo (via isGitRepo) before calling; behavior on a non-repo path
// would otherwise collapse to "detached" which would mislead the user.
func currentBranch(ctx context.Context, sourcePath string) (branch string, detached bool, err error) {
	out, runErr := exec.CommandContext(ctx, "git", "-C", sourcePath, "symbolic-ref", "--short", "HEAD").Output()
	if runErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", false, ctxErr
		}
		return "", true, nil
	}
	return strings.TrimSpace(string(out)), false, nil
}

// isLinkedWorktree reports whether sourcePath is a linked git worktree
// (as opposed to the original clone). True when --git-dir differs from
// --git-common-dir; false on any error (we already verified isGitRepo,
// so an error here is unexpected — fall through to offering the switch
// rather than suppressing it on a transient git glitch).
func isLinkedWorktree(ctx context.Context, sourcePath string) bool {
	gitDir, err1 := exec.CommandContext(ctx, "git", "-C", sourcePath, "rev-parse", "--git-dir").Output()
	commonDir, err2 := exec.CommandContext(ctx, "git", "-C", sourcePath, "rev-parse", "--git-common-dir").Output()
	if err1 != nil || err2 != nil {
		return false
	}
	return strings.TrimSpace(string(gitDir)) != strings.TrimSpace(string(commonDir))
}

// applyBranchSwitch checks out main and fast-forwards from origin in
// the source clone. Streams git progress to out. Caller is responsible
// for verifying the clone is not a linked worktree (where `git
// checkout main` would refuse because main is already checked out in
// the original clone).
func applyBranchSwitch(ctx context.Context, sourcePath string, out io.Writer) error {
	fmt.Fprintf(out, "switching %s to main\n", sourcePath)
	co := exec.CommandContext(ctx, "git", "-C", sourcePath, "checkout", "main")
	co.Stdout, co.Stderr = os.Stderr, os.Stderr
	if err := co.Run(); err != nil {
		return fmt.Errorf("git checkout main: %w", err)
	}
	pull := exec.CommandContext(ctx, "git", "-C", sourcePath, "pull", "--ff-only", "origin", "main")
	pull.Stdout, pull.Stderr = os.Stderr, os.Stderr
	if err := pull.Run(); err != nil {
		return fmt.Errorf("git pull: %w", err)
	}
	return nil
}

// branchSwitchAndRecheck runs applyBranchSwitch with a 60s budget and
// then re-runs CheckForUpdates with a fresh 5s budget. Extracted from
// the update subcommand's RunE and the launch-time path so the wiring
// is unit-testable without driving the bubbletea prompt. Callers are
// responsible for verifying status.OfferBranchSwitch before calling.
func branchSwitchAndRecheck(ctx context.Context, out io.Writer) (UpdateStatus, error) {
	switchCtx, cancelSw := context.WithTimeout(ctx, 60*time.Second)
	defer cancelSw()
	if err := applyBranchSwitch(switchCtx, SourcePath, out); err != nil {
		return UpdateStatus{}, err
	}
	recheckCtx, cancelRe := context.WithTimeout(ctx, 5*time.Second)
	defer cancelRe()
	return CheckForUpdates(recheckCtx)
}

// isAncestor reports whether `ancestor` is an ancestor of `descendant`
// in the source clone's history. Exit 0 = yes; any other exit code
// (including the commit being missing from the local object DB) = no.
// Context cancellation is treated as "no" — the caller will detect the
// cancellation via ctx.Err() on the next op.
func isAncestor(ctx context.Context, sourcePath, ancestor, descendant string) bool {
	cmd := exec.CommandContext(ctx, "git", "-C", sourcePath, "merge-base", "--is-ancestor", ancestor, descendant)
	return cmd.Run() == nil
}

// looksLikeSHA does a cheap sanity check on git's hex output.
func looksLikeSHA(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// short returns the 10-char prefix of a SHA, or the SHA itself when
// shorter than 10 chars (shouldn't happen post-validation, but
// defensive).
func short(sha string) string {
	if len(sha) <= 10 {
		return sha
	}
	return sha[:10]
}

func init() {
	updateCmd.Flags().BoolVar(&updateCheckOnly, "check", false, "Only print status; do not perform the update")
	rootCmd.AddCommand(updateCmd)
}
