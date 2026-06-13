// Package daemonclient is the client-side library that talks to
// openkanbankd over its per-user Unix socket. It exposes two top-level
// types:
//
//   - *Client: one per TUI process. Holds the long-lived control
//     connection and serializes JSON-mode RPCs (Hello / Spawn / List /
//     Kill / Subscribe / PrepareExit / Shutdown).
//   - *PaneView: one per ticket-pane. Drop-in replacement for
//     terminal.Pane from the UI's perspective, backed by a local
//     xvt.SafeEmulator and a per-attach binary connection.
//
// daemonclient intentionally re-implements the dial / autostart logic
// rather than importing internal/daemon so the TUI does not depend on
// the daemon's server-side code (pidlock, listener bind, accept loop).
// Wire types and framing helpers ARE shared via internal/daemon's
// pure-codec surface (WriteFrame / ReadFrame / Msg* constants).
package daemonclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// Environment-variable overrides — must match the ones in
// internal/daemon/paths.go so a single export honors both sides.
const (
	envSocket = "OPENKANBAN_DAEMON_SOCK"
	envLog    = "OPENKANBAN_DAEMON_LOG"
	// envBinary lets tests point the autostart fork at a pre-built
	// binary rather than os.Args[0]. Not honored in production builds.
	envBinary = "OPENKANBAN_DAEMON_BINARY"
)

// ErrDaemonUnavailable is the public sentinel returned when the client
// cannot reach openkanbankd — either the socket file is absent, the
// daemon refused the connection, or an autostart attempt did not bring
// it up within the start-wait window.
var ErrDaemonUnavailable = errors.New("daemonclient: cannot reach openkanbankd")

// ErrProtocolVersionSkew is the public sentinel returned by New /
// NewWithConn when the daemon's ProtocolVersion in HelloResp does not
// match the client's compiled-in daemon.ProtocolVersion. The caller is
// expected to surface a "run `openkanban daemon restart`" hint and
// continue in degraded (daemonless) mode rather than crash.
var ErrProtocolVersionSkew = errors.New("daemonclient: protocol version skew between client and daemon")

const (
	dialTimeout = 1 * time.Second
	startWait   = 3 * time.Second
)

// SocketPath returns the absolute path to the daemon's Unix socket.
// Honors OPENKANBAN_DAEMON_SOCK if set; otherwise falls back to
// ~/.cache/openkanban/daemon.sock. Mirrors daemon.SocketPath so a
// single env override drives both ends.
func SocketPath() (string, error) {
	if v := os.Getenv(envSocket); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("daemonclient: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".cache", "openkanban", "daemon.sock"), nil
}

// logPath returns the daemon's log file. Used when autostart needs to
// redirect the child's stdout/stderr.
func logPath() (string, error) {
	if v := os.Getenv(envLog); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("daemonclient: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".cache", "openkanban", "daemon.log"), nil
}

// ensureRuntimeDir creates the directory holding the socket / pidfile /
// log so a freshly forked daemon has somewhere to bind. Idempotent.
func ensureRuntimeDir() error {
	sock, err := SocketPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(sock)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("daemonclient: mkdir %s: %w", dir, err)
	}
	return nil
}

// Dial attempts a single connection to the daemon socket. Returns
// ErrDaemonUnavailable if the socket file is missing or the daemon
// refuses the connection.
func Dial(ctx context.Context) (net.Conn, error) {
	sock, err := SocketPath()
	if err != nil {
		return nil, err
	}
	return dial(ctx, sock)
}

func dial(ctx context.Context, sock string) (net.Conn, error) {
	if _, err := os.Stat(sock); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrDaemonUnavailable
		}
		return nil, fmt.Errorf("daemonclient: stat socket: %w", err)
	}

	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil, ErrDaemonUnavailable
		}
		return nil, fmt.Errorf("daemonclient: dial %s: %w", sock, err)
	}
	return conn, nil
}

// DialOrStart tries to connect; if the daemon isn't running, forks
// `<self> daemon` (or $OPENKANBAN_DAEMON_BINARY if set) in a new
// session and polls until the socket comes up. Returns
// ErrDaemonUnavailable wrapping the underlying cause if the daemon
// can't be reached within startWait.
func DialOrStart(ctx context.Context) (net.Conn, error) {
	sock, err := SocketPath()
	if err != nil {
		return nil, err
	}

	conn, err := dial(ctx, sock)
	if err == nil {
		return conn, nil
	}
	if !errors.Is(err, ErrDaemonUnavailable) {
		return nil, err
	}

	if ferr := forkDaemon(); ferr != nil {
		return nil, fmt.Errorf("%w: fork: %v", ErrDaemonUnavailable, ferr)
	}

	deadline := time.Now().Add(startWait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		conn, err := dial(ctx, sock)
		if err == nil {
			return conn, nil
		}
		if !errors.Is(err, ErrDaemonUnavailable) {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("%w: forked daemon did not bind %s within %s", ErrDaemonUnavailable, sock, startWait)
}

// forkDaemon execs `<self> daemon` (or OPENKANBAN_DAEMON_BINARY if set)
// detached in a new session, with stdio redirected to the daemon log.
func forkDaemon() error {
	if err := ensureRuntimeDir(); err != nil {
		return err
	}

	log, err := logPath()
	if err != nil {
		return err
	}

	exe := os.Getenv(envBinary)
	if exe == "" {
		exe, err = os.Executable()
		if err != nil {
			return fmt.Errorf("daemonclient: locate self: %w", err)
		}
	}

	logF, err := os.OpenFile(log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("daemonclient: open log %s: %w", log, err)
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
	logF.Close()
	return cmd.Process.Release()
}
