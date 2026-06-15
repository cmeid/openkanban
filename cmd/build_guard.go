package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// guardStubBuild refuses to proceed when this binary was built without
// the canonical install flow (bare `go install .`, no ldflags). The
// `version` subcommand is allowed through so users can run it to see
// what state their binary is in.
//
// We os.Exit directly rather than returning an error so cobra doesn't
// print "Error: stub build" + a usage dump on top of our message.
//
// Recovery: re-install via `./scripts/install.sh` from the source
// clone, which sets BuildMarker=official and the SourcePath / Commit
// ldflags that `openkanban update` and `openkanban version` rely on.
func guardStubBuild(cmd *cobra.Command) error {
	if isOfficialBuild() {
		return nil
	}
	if cmd == versionCmd {
		// version intentionally still works on stub binaries so users
		// can see "build: STUB" in its output.
		return nil
	}
	stubBuildExit(os.Stderr)
	return nil // unreachable
}

// stubBuildExit prints the stub-binary hint and exits 1. Split out so
// tests can drive the message stream without invoking os.Exit.
func stubBuildExit(w io.Writer) {
	fmt.Fprintln(w, "openkanban: this binary is a STUB.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "It was built via bare `go install .` (or `go build`) without the")
	fmt.Fprintln(w, "ldflags that inject install-time metadata (SourcePath, Commit,")
	fmt.Fprintln(w, "BuildMarker). Without those, `openkanban update`, version")
	fmt.Fprintln(w, "reporting, and the autostart fork all degrade silently — so")
	fmt.Fprintln(w, "the binary refuses to run anything except `openkanban version`.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Re-install via the canonical script:")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "    cd <source-clone> && ./scripts/install.sh")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "(`openkanban version` will still run on this stub and print the")
	fmt.Fprintln(w, " same hint, so feel free to use it to confirm the rebuild fixed it.)")
	os.Exit(1)
}
