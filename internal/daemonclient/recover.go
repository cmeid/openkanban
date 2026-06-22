package daemonclient

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/techdufus/openkanban/internal/daemon"
)

// DaemonPID reads the daemon's recorded pid from its pidfile. Returns the
// pid and nil when the file exists and parses; otherwise a non-nil error,
// which callers typically treat as "pid unknown" (best-effort, e.g. the
// "(pid N)" suffix on `daemon start`).
func DaemonPID() (int, error) {
	pidPath, err := daemon.PidPath()
	if err != nil {
		return 0, err
	}
	return readPidFile(pidPath)
}

// WaitForExit blocks until process pid is gone (kill(pid,0) → ESRCH) or
// timeout elapses. `daemon restart` calls it between shutting the old
// daemon down and starting a fresh one: the daemon unlinks its socket
// early in shutdown (listener close) but holds its pidlock until the
// process actually exits after cleanup(), so a fork keyed only on
// socket-gone races the dying daemon's lock and dies with "already
// running" — leaving nothing up. Waiting on process death closes that
// window. pid<=0 is a no-op (nothing to wait for).
func WaitForExit(ctx context.Context, pid int, timeout time.Duration) error {
	if pid <= 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		// kill(pid, 0) probes existence without delivering a signal:
		// nil = alive, ESRCH = gone. On the same user EPERM won't occur.
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("daemonclient: daemon pid %d still alive after %s", pid, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func readPidFile(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("daemonclient: bad pidfile %q", path)
	}
	return pid, nil
}

// ForceRestartDaemon kills a wedged daemon (SIGKILL via its pidfile), waits
// for its socket to disappear, then autostarts + dials a fresh one. Use only
// after a liveness probe (PreflightListSessions) returns
// daemon.ErrDaemonUnresponsive: the daemon is up (socket dials) but not
// answering RPCs, so a normal reconnect won't help. Returns the error if
// there's no live daemon to kill (caller should fall back to New).
func ForceRestartDaemon(ctx context.Context) (*Client, error) {
	pidPath, err := daemon.PidPath()
	if err != nil {
		return nil, err
	}
	pid, err := readPidFile(pidPath)
	if err != nil {
		return nil, err
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		// ESRCH = already gone; fine, proceed to restart.
		if err != syscall.ESRCH {
			return nil, fmt.Errorf("daemonclient: kill wedged daemon %d: %w", pid, err)
		}
	}
	sock, err := daemon.SocketPath()
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(sock); os.IsNotExist(statErr) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	// Stale socket file may linger after SIGKILL (no clean unlink). Best-effort
	// remove so DialOrStart doesn't dial a dead socket.
	_ = os.Remove(sock)
	return New(ctx)
}
