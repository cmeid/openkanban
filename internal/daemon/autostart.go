package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ErrDaemonNotRunning is returned by Dial when the socket file is
// absent or refuses connections (ECONNREFUSED). Distinct sentinel so
// the autostart path knows to fork a fresh daemon rather than surface
// the error to the user.
var ErrDaemonNotRunning = errors.New("daemon: not running")

// dialTimeout is how long Dial waits for the socket to accept a
// connection. A successful dial against a healthy daemon completes
// well under 100ms; the bound here is generous for slow CI.
const dialTimeout = 1 * time.Second

// startWait is the upper bound on how long DialOrStart waits for a
// freshly-forked daemon to bind its socket.
const startWait = 2 * time.Second

// Dial attempts a single connection to the daemon socket. If the
// socket file does not exist or the daemon refuses the connection it
// returns ErrDaemonNotRunning so the caller can decide to autostart.
//
// All other errors (permissions, timeouts) are returned unchanged.
func Dial(ctx context.Context, sock string) (net.Conn, error) {
	if _, err := os.Stat(sock); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrDaemonNotRunning
		}
		return nil, fmt.Errorf("daemon: stat socket: %w", err)
	}

	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		// ECONNREFUSED means the socket file exists but no process
		// is listening — usually a crashed daemon. Treat the same
		// as "not running" so the caller can autostart.
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil, ErrDaemonNotRunning
		}
		return nil, err
	}
	return conn, nil
}

// DialOrStart tries to connect to the daemon; if it's not running, it
// forks a fresh one and retries until startWait elapses. Returns the
// established net.Conn or the underlying error.
func DialOrStart(ctx context.Context) (net.Conn, error) {
	sock, err := SocketPath()
	if err != nil {
		return nil, err
	}

	conn, err := Dial(ctx, sock)
	if err == nil {
		return conn, nil
	}
	if !errors.Is(err, ErrDaemonNotRunning) {
		return nil, err
	}

	if err := forkDaemon(); err != nil {
		return nil, fmt.Errorf("daemon: fork: %w", err)
	}

	// Poll for the socket to come up. We sleep in short increments
	// so a healthy spawn is observed in ~50ms.
	deadline := time.Now().Add(startWait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		conn, err := Dial(ctx, sock)
		if err == nil {
			return conn, nil
		}
		if !errors.Is(err, ErrDaemonNotRunning) {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("daemon: forked daemon did not bind %s within %s", sock, startWait)
}

// forkDaemon execs `<self> daemon` in a new session-leader process so
// the child outlives the current shell, with stdout/stderr redirected
// to the daemon log file (append mode).
//
// The child is intentionally not Waited on: cmd.Process.Release frees
// the parent's bookkeeping and the kernel reaps the child when it
// eventually exits.
func forkDaemon() error {
	if err := EnsureRuntimeDir(); err != nil {
		return err
	}

	logPath, err := LogPath()
	if err != nil {
		return err
	}

	exe, _, err := ResolveBinary()
	if err != nil {
		return err
	}

	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("daemon: open log %s: %w", logPath, err)
	}

	cmd := exec.Command(exe, "daemon")
	cmd.Stdout = logF
	cmd.Stderr = logF
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		logF.Close()
		return err
	}
	// The child has dup'd the log fd via cmd.Stdout/Stderr; closing
	// our handle drops our refcount.
	logF.Close()
	// Release detaches the child from this process's lifecycle.
	return cmd.Process.Release()
}

// Binary lookup (resolveDaemonBinary, bundleDaemonCandidates,
// isExecutableFile) lives in binary.go as the exported ResolveBinary so the
// TUI autostart (internal/daemonclient/dial.go) and the launchd installer
// (cmd/daemon_service.go) share a single implementation.
