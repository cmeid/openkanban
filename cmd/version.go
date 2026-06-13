package cmd

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// Set via ldflags at build time. SourcePath is the absolute path to the
// repository this binary was built from; left empty for release builds
// so `openkanban update` knows to print package-manager instructions
// instead of attempting an in-place git pull.
var (
	Version    = "dev"
	Commit     = "none"
	Date       = "unknown"
	SourcePath = ""
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("openkanban %s\n", Version)
		fmt.Printf("  commit: %s\n", Commit)
		fmt.Printf("  built:  %s\n", Date)
		fmt.Printf("  go:     %s\n", runtime.Version())
		if SourcePath != "" {
			fmt.Printf("  source: %s\n", SourcePath)
		} else {
			fmt.Printf("  source: (release build)\n")
		}
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
