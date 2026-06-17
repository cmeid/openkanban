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
