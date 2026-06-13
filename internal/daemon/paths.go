package daemon

import (
	"fmt"
	"os"
	"path/filepath"
)

// Environment-variable overrides for the daemon's on-disk runtime
// state. The defaults all live under ~/.cache/openkanban/. Tests use
// these to redirect to a temporary directory via t.Setenv.
const (
	EnvSocket = "OPENKANBAN_DAEMON_SOCK"
	EnvPid    = "OPENKANBAN_DAEMON_PID"
	EnvLog    = "OPENKANBAN_DAEMON_LOG"
)

// runtimeDir returns the directory that holds the daemon's socket,
// pidfile, and log. By default that's ~/.cache/openkanban; we colocate
// these so EnsureRuntimeDir only has to mkdir one path even when only
// one of the three has been overridden.
func runtimeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("daemon: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".cache", "openkanban"), nil
}

// SocketPath returns the absolute path to the daemon's Unix socket.
// Honors OPENKANBAN_DAEMON_SOCK if set; otherwise falls back to
// <runtimeDir>/daemon.sock. Does NOT create the parent directory —
// call EnsureRuntimeDir first.
func SocketPath() (string, error) {
	if v := os.Getenv(EnvSocket); v != "" {
		return v, nil
	}
	dir, err := runtimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.sock"), nil
}

// PidPath returns the absolute path to the daemon's advisory pidfile.
// Honors OPENKANBAN_DAEMON_PID if set; otherwise falls back to
// <runtimeDir>/daemon.pid.
func PidPath() (string, error) {
	if v := os.Getenv(EnvPid); v != "" {
		return v, nil
	}
	dir, err := runtimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.pid"), nil
}

// LogPath returns the absolute path to the daemon's log file. Honors
// OPENKANBAN_DAEMON_LOG if set; otherwise falls back to
// <runtimeDir>/daemon.log.
func LogPath() (string, error) {
	if v := os.Getenv(EnvLog); v != "" {
		return v, nil
	}
	dir, err := runtimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.log"), nil
}

// EnsureRuntimeDir creates the directory that holds the socket, pidfile,
// and log if it does not already exist. It chmod's the directory to
// 0700 so the socket and pidfile are not exposed to other local users
// via mode inheritance.
func EnsureRuntimeDir() error {
	dir, err := runtimeDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("daemon: mkdir %s: %w", dir, err)
	}
	return nil
}
