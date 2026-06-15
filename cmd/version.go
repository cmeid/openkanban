package cmd

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Set via ldflags at build time. SourcePath is the absolute path to the
// repository this binary was built from; left empty for release builds
// so `openkanban update` knows to print package-manager instructions
// instead of attempting an in-place git pull.
//
// BuildMarker is set to "official" ONLY by the canonical install paths
// (scripts/install.sh and openkanban update's reinstall). Empty means
// the binary was built via bare `go install .` or similar, missing the
// install-time metadata that update / version reporting / source-clone
// awareness all depend on. In that case the root command's
// PersistentPreRunE refuses to run anything except `version` and
// directs the user to the install script.
var (
	Version     = "dev"
	Commit      = "none"
	Date        = "unknown"
	SourcePath  = ""
	BuildMarker = ""
)

// resolvedCommit returns the best-effort short commit SHA for this
// binary. Prefers the ldflags-injected Commit; if missing or "none",
// falls back to the VCS revision Go embeds in BuildInfo on >=1.18 for
// modules built from a git working tree. Returns "" when neither is
// available (e.g., binary built from a tarball / pre-1.18 toolchain).
func resolvedCommit() string {
	if Commit != "" && Commit != "none" {
		return Commit
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			short := s.Value
			if len(short) > 7 {
				short = short[:7]
			}
			return short
		}
	}
	return ""
}

// isOfficialBuild reports whether this binary was produced via the
// canonical install flow (scripts/install.sh or openkanban update).
// Bare `go install .` leaves BuildMarker empty.
func isOfficialBuild() bool {
	return BuildMarker == "official"
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("openkanban %s\n", Version)
		commit := resolvedCommit()
		if commit == "" {
			commit = "unknown"
		}
		fmt.Printf("  commit: %s\n", commit)
		fmt.Printf("  built:  %s\n", Date)
		fmt.Printf("  go:     %s\n", runtime.Version())
		if SourcePath != "" {
			fmt.Printf("  source: %s\n", SourcePath)
		} else {
			fmt.Printf("  source: (release build)\n")
		}
		if !isOfficialBuild() {
			fmt.Printf("  build:  STUB — bare `go install .` produces a non-runnable binary.\n")
			fmt.Printf("          Re-install via: cd <source-clone> && ./scripts/install.sh\n")
		}
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
