package daemon

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// UnresponsiveHint returns a user-facing remediation message for a daemon
// that is not answering (dial failed, or an RPC deadline fired). When the
// advisory pidfile names a process that is still alive, the message names
// its PID and the exact kill command, so a user staring at a wedged
// daemon can recover it without hunting for the PID themselves. When the
// pidfile is missing, unparseable, or names a process that is gone, it
// falls back to the plain restart hint (there is nothing to kill).
//
// Shared by the CLI (cmd.mapDaemonErr) and the TUI startup preflight
// (internal/app) so both paths report a wedged daemon identically.
func UnresponsiveHint() string {
	const restart = "openkanbankd is not responding — run: openkanban daemon restart"

	pid, ok := livePidFromFile()
	if !ok {
		return restart
	}
	return fmt.Sprintf("openkanbankd (pid %d) is not responding.\n"+
		"Kill it:  kill -9 %d\n"+
		"Then run: openkanban daemon restart", pid, pid)
}

// livePidFromFile reads the advisory pidfile and returns the PID only when
// it parses AND names a process that currently exists. Returns ok=false on
// any failure (no pidfile, garbage contents, or a dead/stale PID) so the
// caller falls back to a kill-free message.
func livePidFromFile() (pid int, ok bool) {
	path, err := PidPath()
	if err != nil {
		return 0, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if !pidAlive(pid) {
		return 0, false
	}
	return pid, true
}

// pidAlive reports whether a process with the given PID currently exists.
// Signal 0 performs the kernel's existence/permission check without
// delivering a signal: nil means the process exists and is ours; EPERM
// means it exists but is owned by another user (still alive); ESRCH means
// it is gone.
func pidAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
