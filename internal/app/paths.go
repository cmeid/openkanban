package app

import (
	"fmt"
	"os"
	"path/filepath"
)

const EnvTUILog = "OPENKANBAN_TUI_LOG"

// TUILogPath returns the absolute path to the TUI process's log file.
// Honors OPENKANBAN_TUI_LOG if set; otherwise falls back to
// <home>/.cache/openkanban/tui.log — the same runtime dir the daemon
// uses (see internal/daemon/paths.go). Callers are expected to ensure
// the parent directory exists first (daemon.EnsureRuntimeDir does that).
func TUILogPath() (string, error) {
	if v := os.Getenv(EnvTUILog); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("app: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".cache", "openkanban", "tui.log"), nil
}
