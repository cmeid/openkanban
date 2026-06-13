package agent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SessionUUIDPattern matches a canonical Claude Code session UUID
// (lowercase hex, 8-4-4-4-12). The Claude Code CLI writes its session
// JSONLs as `<uuid>.jsonl` under `~/.claude/projects/<encoded-cwd>/`,
// and any --resume / --fork-session arg must be a UUID of this shape.
var SessionUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// SessionHolder describes a process currently holding the session's
// JSONL file open. PID is 0 when nothing has the file open.
type SessionHolder struct {
	PID  int
	Path string
}

// claudeProjectsRoot returns the directory the Claude Code CLI uses
// for per-project session storage: `$HOME/.claude/projects`. Honors
// the HOME env var so tests can redirect with t.Setenv.
func claudeProjectsRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// SessionPath returns the on-disk JSONL path for the given session
// UUID by scanning `~/.claude/projects/*/<uuid>.jsonl`. Returns
// ("", os.ErrNotExist) if no match. The encoded-cwd directory name
// isn't known up front, so we glob the projects root.
func SessionPath(uuid string) (string, error) {
	if !SessionUUIDPattern.MatchString(uuid) {
		return "", fmt.Errorf("session ref %q is not a UUID", uuid)
	}
	root, err := claudeProjectsRoot()
	if err != nil {
		return "", err
	}
	matches, err := filepath.Glob(filepath.Join(root, "*", uuid+".jsonl"))
	if err != nil {
		return "", fmt.Errorf("glob session path: %w", err)
	}
	if len(matches) == 0 {
		return "", os.ErrNotExist
	}
	// If somehow multiple project dirs hold a file with the same UUID,
	// prefer the first match deterministically (glob returns sorted).
	return matches[0], nil
}

// SessionActive checks via `lsof -t <path>` whether any process has
// the session JSONL open. Returns the first holder PID (0 if none).
// lsof exits 1 when no process holds the file — that's not a failure.
func SessionActive(uuid string) (SessionHolder, error) {
	path, err := SessionPath(uuid)
	if err != nil {
		return SessionHolder{}, err
	}
	holder := SessionHolder{Path: path}

	if _, err := exec.LookPath("lsof"); err != nil {
		return holder, fmt.Errorf("lsof not found on PATH: %w", err)
	}

	out, err := exec.Command("lsof", "-t", path).Output()
	if err != nil {
		// lsof exits 1 when no process holds the file. Treat that as
		// "not held" rather than a real error.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return holder, nil
		}
		return holder, fmt.Errorf("lsof %s: %w", path, err)
	}

	// `lsof -t` prints one PID per line. Take the first non-empty.
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, perr := strconv.Atoi(line)
		if perr != nil {
			return holder, fmt.Errorf("parse lsof pid %q: %w", line, perr)
		}
		holder.PID = pid
		break
	}
	return holder, nil
}

// ForceExitSession sends SIGTERM to the process holding the session
// JSONL, polls for clean exit up to `grace`, then SIGKILLs whatever
// remains. Returns nil once no process holds the JSONL. No-op when
// SessionActive reports nothing to kill.
func ForceExitSession(uuid string, grace time.Duration) error {
	holder, err := SessionActive(uuid)
	if err != nil {
		return err
	}
	if holder.PID == 0 {
		return nil
	}

	proc, err := os.FindProcess(holder.PID)
	if err != nil {
		return fmt.Errorf("find pid %d: %w", holder.PID, err)
	}

	if err := proc.Signal(syscall.SIGTERM); err != nil {
		// ESRCH: already gone. Anything else is a real failure.
		if !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("SIGTERM pid %d: %w", holder.PID, err)
		}
	}

	// Poll until the file is no longer held, or the grace expires.
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		h, err := SessionActive(uuid)
		if err != nil {
			return err
		}
		if h.PID == 0 {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Still held — escalate to SIGKILL on the original holder.
	if err := proc.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("SIGKILL pid %d: %w", holder.PID, err)
	}

	// Give the kernel a brief moment to reap the file descriptor.
	for i := 0; i < 30; i++ {
		h, err := SessionActive(uuid)
		if err != nil {
			return err
		}
		if h.PID == 0 {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("session %s still held after SIGKILL", uuid)
}
