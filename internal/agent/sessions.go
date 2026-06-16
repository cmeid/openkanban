package agent

import (
	"bufio"
	"encoding/json"
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

// latestClaudeJSONL returns the path to the most-recent (.jsonl, by
// mtime) claude session transcript for the given worktree, scanning
// `~/.claude/projects/<encoded-worktree-path>/`. Returns ("", nil) for
// an empty worktreePath, a missing project dir, or a dir with no
// .jsonl files — i.e. "no session found" is not an error. A read
// failure on the project dir is a real error.
func latestClaudeJSONL(worktreePath string) (string, error) {
	if worktreePath == "" {
		return "", nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home dir: %w", err)
	}
	encoded := strings.ReplaceAll(worktreePath, "/", "-")
	dir := filepath.Join(homeDir, ".claude", "projects", encoded)

	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read claude projects dir: %w", err)
	}

	var latestPath string
	var latestMtime time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if latestPath == "" || info.ModTime().After(latestMtime) {
			latestPath = filepath.Join(dir, e.Name())
			latestMtime = info.ModTime()
		}
	}
	return latestPath, nil
}

// jsonlHasRealAssistantContent scans a claude session transcript and
// reports whether any assistant message has user-visible text other
// than the auto-reply "No response requested.". Returns (false, err)
// only if the file can't be opened; malformed lines mid-stream are
// skipped, not treated as failure.
func jsonlHasRealAssistantContent(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open session %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20) // 1MB initial, 16MB max

	for scanner.Scan() {
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Type != "assistant" || ev.Message.Role != "assistant" {
			continue
		}
		text := extractAssistantText(ev.Message.Content)
		text = strings.TrimSpace(text)
		if text == "" || text == "No response requested." {
			continue
		}
		return true, nil
	}
	return false, nil
}

// IsClaudeSessionDead reports whether the most-recent claude session for
// the given worktree has no real assistant work in its transcript. Used
// by openkanban to silently clean up never-engaged sessions when the
// user respawns.
//
// "Dead" means: no assistant message has text content other than the
// auto-response "No response requested.". A missing project directory
// or missing .jsonl also counts as dead.
//
// Returns the path to the most-recent session JSONL (so callers can
// delete it) and any non-fatal error encountered while walking the
// directory or reading the file.
func IsClaudeSessionDead(worktreePath string) (dead bool, sessionPath string, err error) {
	latestPath, err := latestClaudeJSONL(worktreePath)
	if err != nil {
		return false, "", err
	}
	if latestPath == "" {
		return true, "", nil
	}

	alive, err := jsonlHasRealAssistantContent(latestPath)
	if err != nil {
		return false, latestPath, err
	}
	return !alive, latestPath, nil
}

// FindClaudeSession returns the UUID of the most-recent live claude
// session for the given worktree — the inverse of IsClaudeSessionDead.
// "Live" means the same alive-check IsClaudeSessionDead uses: at least
// one assistant message with real text. Returns "" when there's no
// project dir, no .jsonl, the most-recent .jsonl has only auto-replies,
// or the .jsonl basename isn't a UUID (defensive — claude always names
// them by session UUID, but we don't want to write garbage back to the
// ticket's AgentSessionID field).
//
// Used by the TUI status-poll loop to back-fill Ticket.AgentSessionID
// after a fresh claude spawn so subsequent re-spawns can pass
// --resume <uuid> deterministically. Mirrors FindOpencodeSession /
// FindGeminiSession / FindCodexSession in signature (no error path —
// returns "" on any failure since the caller retries on the next tick).
func FindClaudeSession(worktreePath string) string {
	latestPath, err := latestClaudeJSONL(worktreePath)
	if err != nil || latestPath == "" {
		return ""
	}
	alive, err := jsonlHasRealAssistantContent(latestPath)
	if err != nil || !alive {
		return ""
	}
	base := filepath.Base(latestPath)
	uuid := strings.TrimSuffix(base, ".jsonl")
	if !SessionUUIDPattern.MatchString(uuid) {
		return ""
	}
	return uuid
}

// extractAssistantText pulls user-visible text from a claude message
// content field, which may be either a plain string or an array of
// part objects. Non-text parts (tool_use, etc) are ignored.
func extractAssistantText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	// Try plain string first
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	// Try array of parts
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "text" && p.Text != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// DeleteClaudeSession removes the JSONL transcript at sessionPath.
// Returns nil if sessionPath is empty. Wraps os.Remove errors.
func DeleteClaudeSession(sessionPath string) error {
	if sessionPath == "" {
		return nil
	}
	if err := os.Remove(sessionPath); err != nil {
		return fmt.Errorf("delete claude session %s: %w", sessionPath, err)
	}
	return nil
}
