package cmd

import (
	"context"
	"fmt"
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
	if !shouldPromptForUpdate(cfg, SourcePath, isTTY, disableFlag) {
		return false, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), launchCheckTimeout)
	defer cancel()

	status, checkErr := CheckForUpdates(ctx)
	if checkErr != nil || !status.Available {
		// Timeout, network failure, no source clone, up to date,
		// ahead, diverged — all fall through silently.
		return false, nil
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
		fmt.Sprintf("Update available: %s -> %s.", m.status.LocalSHA, m.status.RemoteSHA),
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

