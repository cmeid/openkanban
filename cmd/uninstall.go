package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/techdufus/openkanban/internal/config"
)

var (
	uninstallDryRun bool
	uninstallYes    bool
)

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove openkanban binaries and hooks (leaves data intact)",
	Long: `Remove the openkanban binary and any Claude Code hooks it installed.

The motion is intentionally one-way for install artifacts and zero-touch
for data:

  REMOVED   the running openkanban binary (as reported by os.Executable),
            the openkanban entries in ~/.claude/settings.json, and the
            legacy ~/.local/bin/update-openkanban script if present.

  KEPT      ~/.config/openkanban (your projects + config), the cache
            directories under ~/.cache, and any ticket files therein.
            These are listed in the closing summary so you can decide
            whether to remove them by hand.

There is purposefully no automated way to remove data: a re-install
should find your projects and config exactly where they were.

Use --dry-run to preview the plan without touching disk. Use --yes to
skip the interactive confirmation.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		plan, err := planUninstall()
		if err != nil {
			return err
		}

		out := cmd.OutOrStdout()
		printPlan(out, plan)

		if uninstallDryRun {
			fmt.Fprintln(out, "\n(dry-run — no changes written)")
			return nil
		}

		if !uninstallYes {
			if !confirm(cmd.InOrStdin(), out, "Proceed? [y/N] ") {
				fmt.Fprintln(out, "aborted")
				return nil
			}
		}

		return executePlan(out, plan)
	},
}

// uninstallPlan is the resolved set of artifacts an uninstall will
// remove, plus the data directories it will report-but-keep. Built by
// planUninstall and consumed by both the printer and the executor so
// they stay in lockstep.
type uninstallPlan struct {
	// Binary is the absolute path of the openkanban executable currently
	// running this command. Always populated — we know it because we're
	// running. Removed last so a mid-run failure still leaves the binary
	// callable for a retry.
	Binary string

	// HooksSettings is the absolute path to the Claude Code settings.json
	// we'll scrub. The actual entry detection happens at execute time so
	// we re-read the file in case the user edited it between plan and
	// commit; the plan only records that we intend to try.
	HooksSettings string

	// HooksHomeExists records whether the ~/.claude directory was present
	// when we planned. Drives whether we even mention hooks in the plan
	// output (no Claude Code → no need to confuse the user with a hooks
	// line).
	HooksHomeExists bool

	// LegacyUpdateScript is ~/.local/bin/update-openkanban when present,
	// "" otherwise. install.sh's closing banner historically pointed
	// users here so we mop it up if they ever installed it.
	LegacyUpdateScript string

	// DataDirs are absolute paths that openkanban creates but uninstall
	// MUST NOT touch — config, projects, ticket files, cache. Printed
	// in the closing summary so the user knows where to find them if
	// they want to clean up by hand.
	DataDirs []dataDir
}

type dataDir struct {
	// Path is the absolute path to the directory.
	Path string

	// Label is a short human tag ("config", "cache", "status") for the
	// summary line. Keeps the output scannable when several dirs exist.
	Label string

	// Description is a one-liner about what lives there, e.g.
	// "config.json, projects.json, ticket briefs".
	Description string

	// Exists is false when the directory isn't present on disk — we
	// still list it (with a note) so the user knows we *checked* and
	// found nothing, rather than silently omitting it.
	Exists bool
}

// planUninstall resolves every path the uninstall will touch (or
// explicitly skip). It does not modify the filesystem. Returns an
// error only when a path can't be resolved at all — e.g. os.Executable
// fails or no $HOME is set.
func planUninstall() (uninstallPlan, error) {
	exe, err := os.Executable()
	if err != nil {
		return uninstallPlan{}, fmt.Errorf("resolve running binary path: %w", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return uninstallPlan{}, fmt.Errorf("resolve home dir: %w", err)
	}

	plan := uninstallPlan{Binary: exe}

	claudeHome := filepath.Join(home, ".claude")
	if st, err := os.Stat(claudeHome); err == nil && st.IsDir() {
		plan.HooksHomeExists = true
		plan.HooksSettings = filepath.Join(claudeHome, "settings.json")
	}

	legacy := filepath.Join(home, ".local", "bin", "update-openkanban")
	if st, err := os.Stat(legacy); err == nil && !st.IsDir() {
		plan.LegacyUpdateScript = legacy
	}

	// Data dirs: config (XDG-aware via config.ConfigDir), runtime cache,
	// and the per-session status cache. None of these are removed; the
	// listing is purely informational.
	configDir, _ := config.ConfigDir()
	if configDir != "" {
		plan.DataDirs = append(plan.DataDirs, dataDir{
			Path:        configDir,
			Label:       "config",
			Description: "config.json, projects.json, ticket briefs",
			Exists:      dirExists(configDir),
		})
	}
	plan.DataDirs = append(plan.DataDirs, dataDir{
		Path:        filepath.Join(home, ".cache", "openkanban"),
		Label:       "cache",
		Description: "daemon runtime (socket, pid, log), tui.log",
		Exists:      dirExists(filepath.Join(home, ".cache", "openkanban")),
	})
	plan.DataDirs = append(plan.DataDirs, dataDir{
		Path:        filepath.Join(home, ".cache", "openkanban-status"),
		Label:       "status",
		Description: "per-session status files written by Claude Code hooks",
		Exists:      dirExists(filepath.Join(home, ".cache", "openkanban-status")),
	})

	return plan, nil
}

// printPlan dumps the plan to out in a stable, scannable shape. The
// printer never returns an error — write failures on stdout are not
// actionable here and would only mask the real failure (the plan
// itself).
func printPlan(out io.Writer, plan uninstallPlan) {
	fmt.Fprintln(out, "openkanban uninstall plan")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Will remove:")
	fmt.Fprintf(out, "  binary:           %s\n", plan.Binary)
	if plan.HooksHomeExists {
		fmt.Fprintf(out, "  Claude Code hooks: openkanban entries in %s\n", plan.HooksSettings)
	} else {
		fmt.Fprintln(out, "  Claude Code hooks: (skipped — no ~/.claude directory)")
	}
	if plan.LegacyUpdateScript != "" {
		fmt.Fprintf(out, "  legacy script:    %s\n", plan.LegacyUpdateScript)
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Will NOT remove (your data — delete by hand if you want a clean slate):")
	for _, d := range plan.DataDirs {
		marker := ""
		if !d.Exists {
			marker = " (not present)"
		}
		fmt.Fprintf(out, "  %-7s %s%s\n", d.Label+":", d.Path, marker)
		fmt.Fprintf(out, "          %s\n", d.Description)
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Tip: stop any running daemon first via `openkanban daemon stop`.")
	fmt.Fprintln(out)
}

// executePlan runs the removals. Each step prints a one-line status
// to out. The binary is removed LAST so a mid-flight failure still
// leaves a working `openkanban` for a retry.
//
// Errors from individual steps are collected and joined into a single
// returned error rather than short-circuiting — a hook-removal failure
// shouldn't block the binary cleanup, and vice versa.
func executePlan(out io.Writer, plan uninstallPlan) error {
	var errs []error

	if plan.HooksHomeExists {
		if err := uninstallHooks(plan.HooksSettings, false, out); err != nil {
			errs = append(errs, fmt.Errorf("hooks: %w", err))
		}
	}

	if plan.LegacyUpdateScript != "" {
		if err := os.Remove(plan.LegacyUpdateScript); err != nil {
			errs = append(errs, fmt.Errorf("remove legacy script %s: %w", plan.LegacyUpdateScript, err))
		} else {
			fmt.Fprintf(out, "removed %s\n", plan.LegacyUpdateScript)
		}
	}

	if err := os.Remove(plan.Binary); err != nil {
		errs = append(errs, fmt.Errorf("remove binary %s: %w", plan.Binary, err))
	} else {
		fmt.Fprintf(out, "removed %s\n", plan.Binary)
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Data left in place:")
	for _, d := range plan.DataDirs {
		if !d.Exists {
			continue
		}
		fmt.Fprintf(out, "  %s\n", d.Path)
	}
	fmt.Fprintln(out, "Remove them manually if you want a full reset.")

	return errors.Join(errs...)
}

// confirm reads a single line from in and returns true iff it starts
// with a 'y' or 'Y'. Default (empty line) is no, matching the [y/N]
// prompt convention used elsewhere in the codebase. The prompt itself
// is written to out so it shows up in tests as part of captured output.
func confirm(in io.Reader, out io.Writer, prompt string) bool {
	fmt.Fprint(out, prompt)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	ans := strings.TrimSpace(line)
	return ans == "y" || ans == "Y" || ans == "yes" || ans == "YES"
}

// dirExists reports whether path exists and is a directory. Any error
// (including permission denied) collapses to false — uninstall has no
// way to remove a path it can't stat, and the user will see a clearer
// message if execution actually attempts the path.
func dirExists(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	return st.IsDir()
}

func init() {
	uninstallCmd.Flags().BoolVar(&uninstallDryRun, "dry-run", false,
		"Print the plan without making any changes")
	uninstallCmd.Flags().BoolVarP(&uninstallYes, "yes", "y", false,
		"Skip the interactive confirmation prompt")

	rootCmd.AddCommand(uninstallCmd)
}
