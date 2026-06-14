package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// UpdateStatus is the result of a CheckForUpdates call.
type UpdateStatus struct {
	// Available is true only when the local HEAD is strictly behind
	// origin/main (i.e., a fast-forward is possible). False when up
	// to date, ahead, diverged, or unable to check.
	Available bool

	// LocalSHA and RemoteSHA are the short-form (10-char) SHAs.
	// Empty when Available is false.
	LocalSHA  string
	RemoteSHA string

	// Reason describes why a check was not actionable when Available
	// is false: "up to date", "ahead", "diverged", "no source clone",
	// "remote unreachable", etc.
	Reason string
}

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
			return nil
		}

		fmt.Fprintf(cmd.OutOrStdout(), "update available: %s -> %s\n", status.LocalSHA, status.RemoteSHA)

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
	fmt.Fprintf(out, "updating %s: %s -> %s\n", SourcePath, status.LocalSHA, status.RemoteSHA)

	pullCtx, cancelPull := context.WithTimeout(ctx, 60*time.Second)
	defer cancelPull()
	pull := exec.CommandContext(pullCtx, "git", "-C", SourcePath, "pull", "--ff-only", "origin", "main")
	pull.Stdout = os.Stderr
	pull.Stderr = os.Stderr
	if err := pull.Run(); err != nil {
		return fmt.Errorf("git pull: %w", err)
	}

	installCtx, cancelInstall := context.WithTimeout(ctx, 5*time.Minute)
	defer cancelInstall()
	ldflags := fmt.Sprintf("-X github.com/techdufus/openkanban/cmd.SourcePath=%s", SourcePath)
	// Bake the post-pull commit so `openkanban version` reflects the
	// rebuilt binary. Mirrors scripts/install.sh: missing commit is
	// non-fatal — we degrade to the default "none" rather than blocking
	// the rebuild.
	if c, err := shortHeadSHA(installCtx, SourcePath); err == nil && c != "" {
		ldflags += fmt.Sprintf(" -X github.com/techdufus/openkanban/cmd.Commit=%s", c)
	}
	install := exec.CommandContext(installCtx, "go", "install", "-ldflags", ldflags, SourcePath)
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
	return nil
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

	if localSHA == remoteSHA {
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
			Available: true,
			LocalSHA:  short(localSHA),
			RemoteSHA: short(remoteSHA),
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
