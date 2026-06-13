package cmd

import (
	"testing"
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
