package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/techdufus/openkanban/internal/config"
)

// launchCheckTimeout caps how long we'll block startup waiting for the
// origin/main probe. The MUST-NOT in the task spec is "more than 1.5s".
const launchCheckTimeout = 1500 * time.Millisecond

// MaybePromptForUpdate runs a best-effort update check before the TUI
// starts. When an update is available and the user confirms, it
// fast-forwards + reinstalls the binary and re-execs in place;
// `handled=true` is returned in that case (and also when the user
// chooses to quit) so the caller knows not to fall through to the TUI.
//
// All failure modes are silent: timeout, network errors, no source
// clone, non-TTY, config disabled, --no-update-check, the user
// pressing Esc — all return (false, nil) so startup proceeds normally.
//
// The only non-nil error path is a genuine update failure (git pull or
// go install). In that case we return (false, err) and let the caller
// decide whether to surface it and continue to the TUI.
func MaybePromptForUpdate(cfg *config.Config, isTTY bool, disableFlag bool) (handled bool, err error) {
	warnSourcePathMissingIfNeeded(cfg, SourcePath, isTTY, disableFlag, os.Stderr)
	if !shouldPromptForUpdate(cfg, SourcePath, isTTY, disableFlag) {
		return false, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), launchCheckTimeout)
	defer cancel()

	status, checkErr := CheckForUpdates(ctx)
	if checkErr != nil {
		// Genuine errors (context cancel, malformed git output) stay
		// silent — startup should not spam the user with internal noise.
		return false, nil
	}
	if !status.Available {
		// Surface every non-empty Reason. This deliberately includes the
		// boring "up to date" case so the user can see why auto-update
		// is or isn't doing anything on each launch.
		if status.Reason != "" {
			fmt.Fprintln(os.Stderr, "openkanban: "+reasonForDisplay(status.Reason, SourcePath))
		}
		if !status.OfferBranchSwitch {
			return false, nil
		}
		// Re-derive the branch name for the prompt.
		branch, _, _ := currentBranch(ctx, SourcePath)
		choice, runErr := runBranchSwitchPrompt(branch)
		if runErr != nil {
			return false, nil
		}
		switch choice {
		case promptQuit:
			return true, nil
		case promptApply:
			// fall through to the switch + re-check
		default:
			return false, nil
		}
		newStatus, switchErr := branchSwitchAndRecheck(context.Background(), os.Stderr)
		if switchErr != nil {
			fmt.Fprintln(os.Stderr, "openkanban: switch failed —", switchErr,
				"\nfix the source clone manually or reinstall via ./scripts/install.sh")
			return false, nil
		}
		if !newStatus.Available {
			// Often the re-check returns "up to date" — surface that
			// too so the user knows the switch worked.
			if newStatus.Reason != "" {
				fmt.Fprintln(os.Stderr, "openkanban: "+reasonForDisplay(newStatus.Reason, SourcePath))
			}
			return false, nil
		}
		status = newStatus
		// Fall through into the existing runUpdatePrompt path below.
	}

	choice, runErr := runUpdatePrompt(status)
	if runErr != nil {
		// Bubbletea couldn't start (e.g. stdin closed under us). Best
		// effort: just proceed to the TUI rather than blow up startup.
		return false, nil
	}

	switch choice {
	case promptApply:
		applyCtx, applyCancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer applyCancel()
		if err := ApplyUpdate(applyCtx, status, os.Stderr); err != nil {
			return false, fmt.Errorf("apply update: %w", err)
		}
		bin, err := resolveOpenkanbanBinary()
		if err != nil {
			return false, fmt.Errorf("resolve openkanban binary: %w", err)
		}
		if err := relaunch(bin, os.Args, os.Environ()); err != nil {
			return false, fmt.Errorf("relaunch %s: %w", bin, err)
		}
		// relaunch on unix never returns on success; on windows it
		// calls os.Exit. If we get here, treat as handled so the
		// caller doesn't double up by running the TUI.
		return true, nil

	case promptQuit:
		// Caller should exit 0. Returning handled=true so the caller
		// short-circuits before app.Run.
		return true, nil

	default:
		// Esc / unknown — fall through to the TUI.
		return false, nil
	}
}

// warnSourcePathMissingIfNeeded emits a one-line notice to w when the
// user is on a release build (SourcePath=="") but otherwise expects
// auto-update to run (TTY, --no-update-check not set, config flag
// enabled). The intent is to break the silent-no-op: a Homebrew or
// plain-`go install` binary that won't auto-update should at least
// say so on launch, instead of leaving the user to wonder why their
// shipped changes never land.
//
// Strictly a warning — never a blocker. Startup proceeds regardless.
func warnSourcePathMissingIfNeeded(cfg *config.Config, sourcePath string, isTTY bool, disableFlag bool, w io.Writer) {
	if sourcePath != "" {
		return
	}
	if !isTTY || disableFlag {
		return
	}
	if cfg == nil || !cfg.Behavior.CheckForUpdatesOnLaunch {
		return
	}
	fmt.Fprintln(w, "openkanban: auto-update disabled (release build / no source clone). Run ./scripts/install.sh from a clone to enable.")
}

// warnMissingAgentsIfNeeded emits a one-line notice to w when review/
// validation subagents the standardized close-out relies on can't be
// resolved. The `resolve` callback is injected so the gating logic is
// unit-testable without touching the filesystem (see launch_check_test).
//
// Strictly a best-effort hint — never a blocker. The finish skill
// degrades to self-review when these agents are absent, so a missing
// agent costs rigor, not correctness. Non-TTY callers stay silent.
func warnMissingAgentsIfNeeded(expected []string, resolve func(string) bool, isTTY bool, w io.Writer) {
	if !isTTY || len(expected) == 0 || resolve == nil {
		return
	}
	var missing []string
	for _, name := range expected {
		if !resolve(name) {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return
	}
	fmt.Fprintf(w, "openkanban: close-out review subagents not found: %s. "+
		"The finish skill will self-review; install the oh-my-claude plugin to enable them.\n",
		strings.Join(missing, ", "))
}

// agentResolver returns a callback that reports whether a Claude subagent
// named `name` is resolvable under the given home dir — either a user
// agent (~/.claude/agents/<name>.md) or one provided by an installed
// plugin (~/.claude/plugins/cache/.../agents/<name>.md). Heuristic and
// best-effort; only feeds the non-blocking launch warning.
func agentResolver(home string) func(string) bool {
	return func(name string) bool {
		patterns := []string{
			filepath.Join(home, ".claude", "agents", name+".md"),
			filepath.Join(home, ".claude", "plugins", "cache", "*", "*", "*", "agents", name+".md"),
			filepath.Join(home, ".claude", "plugins", "cache", "*", "*", "agents", name+".md"),
		}
		for _, p := range patterns {
			if matches, _ := filepath.Glob(p); len(matches) > 0 {
				return true
			}
		}
		return false
	}
}

// shouldPromptForUpdate is the pure gating predicate. It does NOT
// perform any I/O — it just evaluates the four preconditions for
// running the launch-time check. Split out so it can be unit-tested
// without spawning bubbletea or git.
func shouldPromptForUpdate(cfg *config.Config, sourcePath string, isTTY bool, disableFlag bool) bool {
	if !isTTY {
		return false
	}
	if disableFlag {
		return false
	}
	if cfg != nil && !cfg.Behavior.CheckForUpdatesOnLaunch {
		return false
	}
	if sourcePath == "" {
		return false
	}
	return true
}

// promptChoice is the user's decision on the launch-time prompt.
type promptChoice int

const (
	promptDismiss promptChoice = iota // Esc, fall through
	promptApply                       // Enter, run update + relaunch
	promptQuit                        // Q / Ctrl+C, exit
)

// updatePromptModel is the tiny bubbletea model backing the one-line
// prompt. State is just the chosen action; the view re-renders the
// same line on every tick.
type updatePromptModel struct {
	status UpdateStatus
	choice promptChoice
}

func (m updatePromptModel) Init() tea.Cmd { return nil }

func (m updatePromptModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch keyMsg.String() {
	case "enter":
		m.choice = promptApply
		return m, tea.Quit
	case "esc":
		m.choice = promptDismiss
		return m, tea.Quit
	case "q", "Q", "ctrl+c":
		m.choice = promptQuit
		return m, tea.Quit
	}
	return m, nil
}

func (m updatePromptModel) View() string {
	prefix := lipgloss.NewStyle().Bold(true).Render(
		fmt.Sprintf("Update available: %s -> %s.", m.status.displayFromSHA(), m.status.RemoteSHA),
	)
	return prefix + " [Enter] apply & relaunch · [Esc] skip · [Q] quit\n"
}

// runUpdatePrompt drives the tea program on stderr (so it doesn't
// scribble on stdout, which a caller might be piping). Returns the
// final choice, or an error if the program failed to start.
func runUpdatePrompt(status UpdateStatus) (promptChoice, error) {
	model := updatePromptModel{status: status, choice: promptDismiss}
	// Deliberately NOT using tea.WithAltScreen — a one-line prompt
	// should not flip the screen buffer.
	prog := tea.NewProgram(model,
		tea.WithInput(os.Stdin),
		tea.WithOutput(os.Stderr),
	)
	final, err := prog.Run()
	if err != nil {
		return promptDismiss, err
	}
	finalModel, ok := final.(updatePromptModel)
	if !ok {
		return promptDismiss, nil
	}
	return finalModel.choice, nil
}

// branchSwitchPromptModel backs the one-line prompt offering to switch
// the source clone from its current branch to main. Sibling of
// updatePromptModel; same three choices, different View.
type branchSwitchPromptModel struct {
	branch string
	choice promptChoice
}

func (m branchSwitchPromptModel) Init() tea.Cmd { return nil }

func (m branchSwitchPromptModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch keyMsg.String() {
	case "enter":
		m.choice = promptApply
		return m, tea.Quit
	case "esc":
		m.choice = promptDismiss
		return m, tea.Quit
	case "q", "Q", "ctrl+c":
		m.choice = promptQuit
		return m, tea.Quit
	}
	return m, nil
}

func (m branchSwitchPromptModel) View() string {
	prefix := lipgloss.NewStyle().Bold(true).Render(
		fmt.Sprintf("Source on %q.", m.branch),
	)
	return prefix + " [Enter] switch to main & update · [Esc] skip · [Q] quit\n"
}

// runBranchSwitchPrompt drives the branch-switch tea program on stderr.
// Mirrors runUpdatePrompt.
func runBranchSwitchPrompt(branch string) (promptChoice, error) {
	model := branchSwitchPromptModel{branch: branch, choice: promptDismiss}
	prog := tea.NewProgram(model,
		tea.WithInput(os.Stdin),
		tea.WithOutput(os.Stderr),
	)
	final, err := prog.Run()
	if err != nil {
		return promptDismiss, err
	}
	finalModel, ok := final.(branchSwitchPromptModel)
	if !ok {
		return promptDismiss, nil
	}
	return finalModel.choice, nil
}

// resolveOpenkanbanBinary picks the path we should re-exec after a
// successful `go install`. Preference order matches resolveGoBin in
// update.go. As a last-ditch fallback we shell out to `go env GOBIN
// GOPATH` so we behave correctly under exotic toolchain setups.
func resolveOpenkanbanBinary() (string, error) {
	if gobin := resolveGoBin(); gobin != "" {
		return filepath.Join(gobin, "openkanban"), nil
	}
	out, err := exec.Command("go", "env", "GOBIN", "GOPATH").Output()
	if err != nil {
		return "", fmt.Errorf("go env: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) >= 1 && strings.TrimSpace(lines[0]) != "" {
		return filepath.Join(strings.TrimSpace(lines[0]), "openkanban"), nil
	}
	if len(lines) >= 2 && strings.TrimSpace(lines[1]) != "" {
		return filepath.Join(strings.TrimSpace(lines[1]), "bin", "openkanban"), nil
	}
	return "", fmt.Errorf("no GOBIN or GOPATH resolvable")
}

