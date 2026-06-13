//go:build !windows

package cmd

import "syscall"

// relaunch replaces the current process image with bin, preserving
// argv and environment. On success this call does not return — control
// transfers to the freshly-installed binary.
func relaunch(bin string, argv []string, env []string) error {
	return syscall.Exec(bin, argv, env)
}
