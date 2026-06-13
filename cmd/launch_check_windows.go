//go:build windows

package cmd

import (
	"os"
	"os/exec"
)

// relaunch is the Windows fallback for syscall.Exec: spawn the new
// binary as a child, wire stdio through, wait, and exit with the
// child's status. Doesn't return on success — os.Exit terminates the
// caller.
func relaunch(bin string, argv []string, env []string) error {
	cmd := exec.Command(bin, argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		return err
	}
	os.Exit(cmd.ProcessState.ExitCode())
	return nil
}
