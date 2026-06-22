package cmd

import (
	"testing"

	"github.com/spf13/cobra"
)

// TestDaemonRestart_HasForceFlag confirms that `openkanban daemon restart`
// exposes a --force flag. The intent of --force is to skip the
// interactive "confirm killing N sessions" prompt; the actual prompt
// behavior is exercised in the manual smoke (it's bound to stderr being
// a TTY, which is hostile to a unit test). Asserting on the flag's
// existence is what stops a refactor from silently removing the safety
// hatch.
func TestDaemonRestart_HasForceFlag(t *testing.T) {
	f := daemonRestartCmd.Flags().Lookup("force")
	if f == nil {
		t.Fatalf("daemon restart: --force flag not registered")
	}
	if f.Value.Type() != "bool" {
		t.Errorf("daemon restart --force: type = %q want %q", f.Value.Type(), "bool")
	}
	if f.DefValue != "false" {
		t.Errorf("daemon restart --force: default = %q want %q", f.DefValue, "false")
	}
}

// TestDaemonRestart_RegisteredOnDaemonCmd confirms `restart` is wired
// in as a subcommand of `daemon`. Without this `openkanban daemon
// restart` would silently print the daemonCmd's help text.
func TestDaemonRestart_RegisteredOnDaemonCmd(t *testing.T) {
	found := false
	for _, sub := range daemonCmd.Commands() {
		if sub.Name() == "restart" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("daemon: restart subcommand not registered")
	}
}

// TestDaemonStart_RegisteredOnDaemonCmd confirms `start` is wired in as a
// subcommand of `daemon`. Without it, `openkanban daemon start` falls
// through to daemonCmd.RunE and runs the daemon in the FOREGROUND (cobra
// treats the unknown word "start" as a positional arg), which is exactly
// the bug this command fixes — so guard the wiring.
func TestDaemonStart_RegisteredOnDaemonCmd(t *testing.T) {
	found := false
	for _, sub := range daemonCmd.Commands() {
		if sub.Name() == "start" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("daemon: start subcommand not registered")
	}
}

// TestDaemonClose_HasYesFlag asserts `openkanban daemon close` exposes
// a -y/--yes flag for skipping the interactive confirmation. Same intent
// as TestDaemonRestart_HasForceFlag: lock the safety hatch in place.
func TestDaemonClose_HasYesFlag(t *testing.T) {
	f := daemonCloseCmd.Flags().Lookup("yes")
	if f == nil {
		t.Fatalf("daemon close: --yes flag not registered")
	}
	if f.Shorthand != "y" {
		t.Errorf("daemon close --yes: shorthand = %q want %q", f.Shorthand, "y")
	}
	if f.Value.Type() != "bool" {
		t.Errorf("daemon close --yes: type = %q want %q", f.Value.Type(), "bool")
	}
	if f.DefValue != "false" {
		t.Errorf("daemon close --yes: default = %q want %q", f.DefValue, "false")
	}
}

// TestDaemonClose_HasGraceFlag asserts the --grace duration flag is
// registered with the default that matches the daemon's internal
// shutdownGraceSeconds (3s).
func TestDaemonClose_HasGraceFlag(t *testing.T) {
	f := daemonCloseCmd.Flags().Lookup("grace")
	if f == nil {
		t.Fatalf("daemon close: --grace flag not registered")
	}
	if f.Value.Type() != "duration" {
		t.Errorf("daemon close --grace: type = %q want %q", f.Value.Type(), "duration")
	}
	if f.DefValue != "3s" {
		t.Errorf("daemon close --grace: default = %q want %q", f.DefValue, "3s")
	}
}

// TestDaemonClose_RegisteredOnDaemonCmd confirms `close` is wired in as
// a subcommand of `daemon` — without this `openkanban daemon close`
// silently prints `daemon`'s help.
func TestDaemonClose_RegisteredOnDaemonCmd(t *testing.T) {
	found := false
	for _, sub := range daemonCmd.Commands() {
		if sub.Name() == "close" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("daemon: close subcommand not registered")
	}
}

// TestDaemonClose_RequiresExactlyOneArg locks in cobra.ExactArgs(1) so a
// refactor to MaximumNArgs / NoArgs is caught here rather than as a
// runtime "panic: index out of range" on args[0].
func TestDaemonClose_RequiresExactlyOneArg(t *testing.T) {
	if daemonCloseCmd.Args == nil {
		t.Fatalf("daemon close: Args validator not set; want cobra.ExactArgs(1)")
	}
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"zero args", []string{}, true},
		{"one arg", []string{"abc"}, false},
		{"two args", []string{"abc", "def"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// daemonCloseCmd.Args is the cobra.PositionalArgs func.
			err := daemonCloseCmd.Args(&cobra.Command{}, tt.args)
			if tt.wantErr && err == nil {
				t.Errorf("Args(%v): want error, got nil", tt.args)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Args(%v): want nil, got %v", tt.args, err)
			}
		})
	}
}
