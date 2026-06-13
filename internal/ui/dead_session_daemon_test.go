package ui

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/daemon"
)

// ownsStubAPI is a minimal daemonGuardAPI stand-in for the spawn-path
// gate. Only Owns is exercised; the exit-guard surface (PrepareExit /
// Kill / ClientID) returns zero values because shouldCleanupDeadSession
// never touches it.
type ownsStubAPI struct {
	resp  daemon.OwnsResp
	err   error
	delay time.Duration

	// calls counts the number of Owns invocations so tests can assert
	// the probe fired (or didn't).
	calls atomic.Int32

	// lastUUID records the most recently queried session UUID so tests
	// can assert the gate passed through the ticket's AgentSessionID
	// unmodified.
	lastUUID atomic.Value // string
}

func (s *ownsStubAPI) PrepareExit(_ context.Context) (daemon.PrepareExitResp, error) {
	return daemon.PrepareExitResp{}, nil
}

func (s *ownsStubAPI) Kill(_ context.Context, _ string, _ time.Duration) error {
	return nil
}

func (s *ownsStubAPI) ClientID() uint16 { return 1 }

func (s *ownsStubAPI) Owns(ctx context.Context, sessionUUID string) (daemon.OwnsResp, error) {
	s.calls.Add(1)
	s.lastUUID.Store(sessionUUID)
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return daemon.OwnsResp{}, ctx.Err()
		}
	}
	return s.resp, s.err
}

// encodeWorktreeForTest mirrors the encoding claude-code does for its
// per-project session directory name. Duplicated from the agent
// package's test helper because Go's test files don't expose helpers
// across package boundaries.
func encodeWorktreeForTest(p string) string {
	return strings.ReplaceAll(p, "/", "-")
}

// writeDeadJSONL creates a JSONL transcript in the fake home that
// agent.IsClaudeSessionDead will classify as "dead": one assistant
// event whose content is the auto-reply "No response requested.".
// Returns the path of the on-disk JSONL so the test can assert
// whether it survives the gate.
func writeDeadJSONL(t *testing.T, homeDir, worktree, uuid string) string {
	t.Helper()
	dir := filepath.Join(homeDir, ".claude", "projects", encodeWorktreeForTest(worktree))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, uuid+".jsonl")
	events := []map[string]any{
		{
			"type": "assistant",
			"message": map[string]any{
				"role":    "assistant",
				"content": "No response requested.",
			},
		},
	}
	var sb strings.Builder
	for _, ev := range events {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		sb.Write(b)
		sb.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// pathExists reports whether a file is present on disk.
func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// TestSpawnGate_OwnsTrueSkipsDeadCheck asserts the contract that
// drives T3: when the daemon reports it owns the live session, the
// dead-session gate must NOT delete the on-disk JSONL — even if that
// JSONL would otherwise qualify as dead. Deleting it would break a
// future claude --continue when the daemon eventually exits.
func TestSpawnGate_OwnsTrueSkipsDeadCheck(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	worktree := filepath.Join(home, "wt")
	uuid := "11111111-2222-3333-4444-555555555555"
	jsonl := writeDeadJSONL(t, home, worktree, uuid)

	stub := &ownsStubAPI{resp: daemon.OwnsResp{Owned: true}}
	m := &Model{guardAPI: stub}
	now := time.Now()
	ticket := &board.Ticket{
		WorktreePath:    worktree,
		AgentSessionID:  uuid,
		AgentSpawnedAt:  &now,
	}

	cleanup, deadPath := m.shouldCleanupDeadSession(ticket)
	if cleanup {
		t.Errorf("shouldCleanupDeadSession = true, want false (daemon owns the session)")
	}
	if deadPath != "" {
		t.Errorf("deadPath = %q, want \"\" when daemon owns the session", deadPath)
	}
	if got := stub.calls.Load(); got != 1 {
		t.Errorf("Owns calls = %d, want 1", got)
	}
	if got, _ := stub.lastUUID.Load().(string); got != uuid {
		t.Errorf("Owns sessionUUID = %q, want %q", got, uuid)
	}
	if !pathExists(jsonl) {
		t.Errorf("JSONL %q was deleted; daemon-owned sessions must be preserved", jsonl)
	}
}

// TestSpawnGate_OwnsFalseFiresDeadCheck is the negative complement: the
// daemon disowns the UUID, so the on-disk dead-check runs and reports
// the JSONL path for cleanup. The test does NOT assert deletion (that
// happens in spawnAgent, not in shouldCleanupDeadSession) — instead it
// verifies the gate's return values match the cleanup contract:
// (true, <path-of-dead-jsonl>).
func TestSpawnGate_OwnsFalseFiresDeadCheck(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	worktree := filepath.Join(home, "wt")
	uuid := "22222222-3333-4444-5555-666666666666"
	jsonl := writeDeadJSONL(t, home, worktree, uuid)

	stub := &ownsStubAPI{resp: daemon.OwnsResp{Owned: false}}
	m := &Model{guardAPI: stub}
	now := time.Now()
	ticket := &board.Ticket{
		WorktreePath:    worktree,
		AgentSessionID:  uuid,
		AgentSpawnedAt:  &now,
	}

	cleanup, deadPath := m.shouldCleanupDeadSession(ticket)
	if !cleanup {
		t.Errorf("shouldCleanupDeadSession = false, want true (daemon disowns + JSONL is dead)")
	}
	if deadPath != jsonl {
		t.Errorf("deadPath = %q, want %q", deadPath, jsonl)
	}
	if got := stub.calls.Load(); got != 1 {
		t.Errorf("Owns calls = %d, want 1", got)
	}
}

// TestSpawnGate_OwnsErrorFallsThrough covers the timeout / RPC-error
// path: per T3's "MUST NOT" list, a slow or unreachable daemon must
// NOT block the spawn flow. The gate should log and fall through to
// IsClaudeSessionDead, treating the probe failure as "not owned".
func TestSpawnGate_OwnsErrorFallsThrough(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	worktree := filepath.Join(home, "wt")
	uuid := "33333333-4444-5555-6666-777777777777"
	jsonl := writeDeadJSONL(t, home, worktree, uuid)

	stub := &ownsStubAPI{err: errors.New("daemon unreachable")}
	m := &Model{guardAPI: stub}
	now := time.Now()
	ticket := &board.Ticket{
		WorktreePath:    worktree,
		AgentSessionID:  uuid,
		AgentSpawnedAt:  &now,
	}

	cleanup, deadPath := m.shouldCleanupDeadSession(ticket)
	if !cleanup {
		t.Errorf("shouldCleanupDeadSession = false, want true (probe failed → fall through to disk check)")
	}
	if deadPath != jsonl {
		t.Errorf("deadPath = %q, want %q", deadPath, jsonl)
	}
}

// TestSpawnGate_NoGuardAPIFallsThrough covers the daemon-less startup
// path: when m.guardAPI is nil (TUI started before the daemon was up),
// the gate must still work — fall through to the on-disk check
// without dereferencing the nil interface.
func TestSpawnGate_NoGuardAPIFallsThrough(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	worktree := filepath.Join(home, "wt")
	uuid := "44444444-5555-6666-7777-888888888888"
	jsonl := writeDeadJSONL(t, home, worktree, uuid)

	m := &Model{guardAPI: nil}
	now := time.Now()
	ticket := &board.Ticket{
		WorktreePath:    worktree,
		AgentSessionID:  uuid,
		AgentSpawnedAt:  &now,
	}

	cleanup, deadPath := m.shouldCleanupDeadSession(ticket)
	if !cleanup {
		t.Errorf("shouldCleanupDeadSession = false, want true (no daemon, JSONL is dead)")
	}
	if deadPath != jsonl {
		t.Errorf("deadPath = %q, want %q", deadPath, jsonl)
	}
}

// TestSpawnGate_EmptySessionIDFallsThrough: if the ticket has no
// AgentSessionID stored yet (e.g., this is a re-spawn after a crash
// that lost the UUID), Owns is meaningless — skip it and let the disk
// check decide.
func TestSpawnGate_EmptySessionIDFallsThrough(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	worktree := filepath.Join(home, "wt")
	jsonl := writeDeadJSONL(t, home, worktree, "55555555-6666-7777-8888-999999999999")

	stub := &ownsStubAPI{resp: daemon.OwnsResp{Owned: true}} // would lie if asked
	m := &Model{guardAPI: stub}
	now := time.Now()
	ticket := &board.Ticket{
		WorktreePath:   worktree,
		AgentSessionID: "", // empty → skip Owns
		AgentSpawnedAt: &now,
	}

	cleanup, deadPath := m.shouldCleanupDeadSession(ticket)
	if !cleanup {
		t.Errorf("shouldCleanupDeadSession = false, want true (no UUID → skip Owns, JSONL is dead)")
	}
	if deadPath != jsonl {
		t.Errorf("deadPath = %q, want %q", deadPath, jsonl)
	}
	if got := stub.calls.Load(); got != 0 {
		t.Errorf("Owns calls = %d, want 0 (no session UUID to query)", got)
	}
}
