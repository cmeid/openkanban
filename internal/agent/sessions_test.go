package agent

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionUUIDPattern(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"canonical lowercase", "7f3a9b2c-1d8e-4a5b-9c3d-2f1e0a8b9c4d", true},
		{"all zeros", "00000000-0000-0000-0000-000000000000", true},
		{"uppercase rejected", "7F3A9B2C-1D8E-4A5B-9C3D-2F1E0A8B9C4D", false},
		{"missing hyphens", "7f3a9b2c1d8e4a5b9c3d2f1e0a8b9c4d", false},
		{"too short", "7f3a9b2c-1d8e-4a5b-9c3d-2f1e0a8b9c4", false},
		{"too long", "7f3a9b2c-1d8e-4a5b-9c3d-2f1e0a8b9c4dd", false},
		{"non-hex chars", "7f3a9b2c-1d8e-4a5b-9c3d-2f1e0a8b9czz", false},
		{"empty", "", false},
		{"opencode-style ref", "ses_abc123", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SessionUUIDPattern.MatchString(tc.in)
			if got != tc.ok {
				t.Errorf("MatchString(%q) = %v, want %v", tc.in, got, tc.ok)
			}
		})
	}
}

// withFakeHome rewires $HOME to a temp dir so SessionPath's globbing
// hits files we control.
func withFakeHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	return tmp
}

func TestSessionPath_NotFound(t *testing.T) {
	withFakeHome(t)
	uuid := "deadbeef-1234-4321-abcd-0123456789ab"
	_, err := SessionPath(uuid)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SessionPath(unknown) error = %v, want os.ErrNotExist", err)
	}
}

func TestSessionPath_RejectsNonUUID(t *testing.T) {
	withFakeHome(t)
	if _, err := SessionPath("not-a-uuid"); err == nil {
		t.Fatal("SessionPath(non-uuid) succeeded; want error")
	}
}

func TestSessionPath_Found(t *testing.T) {
	home := withFakeHome(t)
	uuid := "7f3a9b2c-1d8e-4a5b-9c3d-2f1e0a8b9c4d"
	projDir := filepath.Join(home, ".claude", "projects", "encoded-test-cwd")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	want := filepath.Join(projDir, uuid+".jsonl")
	if err := os.WriteFile(want, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := SessionPath(uuid)
	if err != nil {
		t.Fatalf("SessionPath: %v", err)
	}
	if got != want {
		t.Errorf("SessionPath = %q, want %q", got, want)
	}
}

func TestSessionActive_NotHeld(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not on PATH; skipping")
	}
	home := withFakeHome(t)
	uuid := "11111111-2222-3333-4444-555555555555"
	projDir := filepath.Join(home, ".claude", "projects", "encoded-test-cwd")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(projDir, uuid+".jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	holder, err := SessionActive(uuid)
	if err != nil {
		t.Fatalf("SessionActive: %v", err)
	}
	if holder.PID != 0 {
		t.Errorf("PID = %d, want 0 (nothing should hold the file)", holder.PID)
	}
	if holder.Path != path {
		t.Errorf("Path = %q, want %q", holder.Path, path)
	}
}

func TestSessionActive_Held(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not on PATH; skipping")
	}
	if _, err := exec.LookPath("tail"); err != nil {
		t.Skip("tail not on PATH; skipping")
	}

	home := withFakeHome(t)
	uuid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	projDir := filepath.Join(home, ".claude", "projects", "encoded-test-cwd")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(projDir, uuid+".jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// `tail -f path` keeps an open file descriptor on the JSONL.
	cmd := exec.Command("tail", "-f", path)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tail: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// Give tail a moment to actually open the file.
	deadline := time.Now().Add(2 * time.Second)
	var holder SessionHolder
	for time.Now().Before(deadline) {
		h, err := SessionActive(uuid)
		if err != nil {
			t.Fatalf("SessionActive: %v", err)
		}
		if h.PID != 0 {
			holder = h
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if holder.PID == 0 {
		t.Fatal("expected SessionActive to report a holder PID; got 0")
	}
	if holder.PID != cmd.Process.Pid {
		// lsof may report a child or parent depending on platform; just
		// log if it differs, don't fail — the important thing is "non-zero".
		t.Logf("holder PID %d differs from tail PID %d (acceptable)",
			holder.PID, cmd.Process.Pid)
	}
}

// writeJSONL writes the given events as one JSON object per line into
// `<dir>/<name>` and returns the file path. Creates `dir` if needed.
func writeJSONL(t *testing.T, dir, name string, events []map[string]any) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
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

// encodeWorktree mirrors the encoding claude-code does for its
// per-project session directory name.
func encodeWorktree(p string) string {
	return strings.ReplaceAll(p, "/", "-")
}

func TestIsClaudeSessionDead(t *testing.T) {
	t.Run("no_project_dir", func(t *testing.T) {
		withFakeHome(t)
		// Worktree path that has no corresponding claude project dir.
		dead, path, err := IsClaudeSessionDead("/tmp/never-touched-by-claude")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !dead {
			t.Errorf("dead = false, want true (no project dir)")
		}
		if path != "" {
			t.Errorf("sessionPath = %q, want \"\"", path)
		}
	})

	t.Run("empty_dir", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/empty"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		dead, path, err := IsClaudeSessionDead(worktree)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !dead {
			t.Errorf("dead = false, want true (empty dir)")
		}
		if path != "" {
			t.Errorf("sessionPath = %q, want \"\"", path)
		}
	})

	t.Run("non_jsonl_only", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/non-jsonl"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o644); err != nil {
			t.Fatal(err)
		}
		dead, path, err := IsClaudeSessionDead(worktree)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !dead {
			t.Errorf("dead = false, want true (no .jsonl present)")
		}
		if path != "" {
			t.Errorf("sessionPath = %q, want \"\"", path)
		}
	})

	t.Run("dead_local_commands_only", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/commands-only"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))
		events := []map[string]any{
			{"type": "custom-title", "title": "scratch"},
			{"type": "permission-mode", "mode": "acceptEdits"},
			{
				"type": "user",
				"message": map[string]any{
					"role":    "user",
					"content": "<command-name>/exit</command-name>",
				},
			},
			{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": "No response requested.",
				},
			},
			{
				"type": "assistant",
				"message": map[string]any{
					"role": "assistant",
					"content": []map[string]any{
						{"type": "text", "text": "  No response requested.  "},
					},
				},
			},
			{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": "",
				},
			},
		}
		want := writeJSONL(t, dir, "11111111-2222-3333-4444-555555555555.jsonl", events)

		dead, got, err := IsClaudeSessionDead(worktree)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !dead {
			t.Errorf("dead = false, want true (only auto-replies)")
		}
		if got != want {
			t.Errorf("sessionPath = %q, want %q", got, want)
		}
	})

	t.Run("alive_real_assistant_string", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/alive-string"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))
		events := []map[string]any{
			{
				"type": "user",
				"message": map[string]any{
					"role":    "user",
					"content": "do a thing",
				},
			},
			{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": "Sure, I can help with that.",
				},
			},
		}
		want := writeJSONL(t, dir, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.jsonl", events)

		dead, got, err := IsClaudeSessionDead(worktree)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if dead {
			t.Errorf("dead = true, want false (real assistant string)")
		}
		if got != want {
			t.Errorf("sessionPath = %q, want %q", got, want)
		}
	})

	t.Run("alive_real_assistant_array", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/alive-array"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))
		events := []map[string]any{
			{
				"type": "assistant",
				"message": map[string]any{
					"role": "assistant",
					"content": []map[string]any{
						{"type": "text", "text": "Working on it."},
						{"type": "tool_use", "name": "bash", "input": map[string]any{"cmd": "ls"}},
					},
				},
			},
		}
		want := writeJSONL(t, dir, "bbbbbbbb-cccc-dddd-eeee-ffffffffffff.jsonl", events)

		dead, got, err := IsClaudeSessionDead(worktree)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if dead {
			t.Errorf("dead = true, want false (text part present)")
		}
		if got != want {
			t.Errorf("sessionPath = %q, want %q", got, want)
		}
	})

	t.Run("malformed_lines_mixed", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/mixed"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// Hand-craft the file so we can inject malformed lines around the
		// valid assistant event.
		good := map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role":    "assistant",
				"content": "Real reply.",
			},
		}
		goodBytes, err := json.Marshal(good)
		if err != nil {
			t.Fatal(err)
		}
		body := strings.Join([]string{
			"this is not json",
			"{\"type\":\"assistant\",\"message\":{\"role\":\"assistant\",\"content\":",
			string(goodBytes),
			"{not closed",
		}, "\n") + "\n"
		path := filepath.Join(dir, "cccccccc-dddd-eeee-ffff-000000000000.jsonl")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}

		dead, got, err := IsClaudeSessionDead(worktree)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if dead {
			t.Errorf("dead = true, want false (good line should be found)")
		}
		if got != path {
			t.Errorf("sessionPath = %q, want %q", got, path)
		}
	})

	t.Run("picks_most_recent_jsonl", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/multi"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))

		// Older file: has a real assistant reply (would be "alive").
		olderEvents := []map[string]any{
			{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": "Real reply from older session.",
				},
			},
		}
		olderPath := writeJSONL(t, dir, "11111111-1111-1111-1111-111111111111.jsonl", olderEvents)

		// Newer file: only auto-replies (would be "dead").
		newerEvents := []map[string]any{
			{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": "No response requested.",
				},
			},
		}
		newerPath := writeJSONL(t, dir, "22222222-2222-2222-2222-222222222222.jsonl", newerEvents)

		// Force the mtimes so newer is unambiguously newer.
		oldTime := time.Now().Add(-2 * time.Hour)
		newTime := time.Now()
		if err := os.Chtimes(olderPath, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(newerPath, newTime, newTime); err != nil {
			t.Fatal(err)
		}

		dead, got, err := IsClaudeSessionDead(worktree)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !dead {
			t.Errorf("dead = false, want true (newer file has only auto-replies)")
		}
		if got != newerPath {
			t.Errorf("sessionPath = %q, want newer %q", got, newerPath)
		}
	})
}

func TestFindClaudeSession(t *testing.T) {
	t.Run("no_project_dir", func(t *testing.T) {
		withFakeHome(t)
		if got := FindClaudeSession("/tmp/never-touched"); got != "" {
			t.Errorf("FindClaudeSession = %q, want \"\" (no project dir)", got)
		}
	})

	t.Run("empty_string_worktree", func(t *testing.T) {
		withFakeHome(t)
		if got := FindClaudeSession(""); got != "" {
			t.Errorf("FindClaudeSession(\"\") = %q, want \"\"", got)
		}
	})

	t.Run("no_jsonl_files", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/empty"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := FindClaudeSession(worktree); got != "" {
			t.Errorf("FindClaudeSession = %q, want \"\" (empty dir)", got)
		}
	})

	t.Run("dead_session_returns_empty", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/dead"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))
		events := []map[string]any{
			{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": "No response requested.",
				},
			},
		}
		writeJSONL(t, dir, "11111111-2222-3333-4444-555555555555.jsonl", events)
		if got := FindClaudeSession(worktree); got != "" {
			t.Errorf("FindClaudeSession = %q, want \"\" (only auto-replies)", got)
		}
	})

	t.Run("live_session_returns_uuid", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/live"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))
		uuid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		events := []map[string]any{
			{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": "Real reply.",
				},
			},
		}
		writeJSONL(t, dir, uuid+".jsonl", events)
		if got := FindClaudeSession(worktree); got != uuid {
			t.Errorf("FindClaudeSession = %q, want %q", got, uuid)
		}
	})

	t.Run("non_uuid_basename_returns_empty", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/garbage"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))
		events := []map[string]any{
			{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": "Real reply.",
				},
			},
		}
		writeJSONL(t, dir, "not-a-uuid.jsonl", events)
		if got := FindClaudeSession(worktree); got != "" {
			t.Errorf("FindClaudeSession = %q, want \"\" (non-uuid basename)", got)
		}
	})

	t.Run("picks_freshest_by_mtime", func(t *testing.T) {
		home := withFakeHome(t)
		worktree := "/worktree/multi-fresh"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))

		liveEvents := []map[string]any{
			{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": "Real reply.",
				},
			},
		}
		olderUUID := "11111111-1111-1111-1111-111111111111"
		newerUUID := "22222222-2222-2222-2222-222222222222"
		olderPath := writeJSONL(t, dir, olderUUID+".jsonl", liveEvents)
		newerPath := writeJSONL(t, dir, newerUUID+".jsonl", liveEvents)

		oldTime := time.Now().Add(-2 * time.Hour)
		newTime := time.Now()
		if err := os.Chtimes(olderPath, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(newerPath, newTime, newTime); err != nil {
			t.Fatal(err)
		}

		if got := FindClaudeSession(worktree); got != newerUUID {
			t.Errorf("FindClaudeSession = %q, want newer %q", got, newerUUID)
		}
	})

	t.Run("newer_dead_takes_precedence_over_older_live", func(t *testing.T) {
		// Matches IsClaudeSessionDead's "picks_most_recent_jsonl"
		// behaviour: we look at the freshest file only, and judge its
		// aliveness. An older live session does NOT rescue a newer
		// dead one — that prevents us from back-filling a stale UUID
		// when the user just did ForceFresh and the new session is
		// still warming up.
		home := withFakeHome(t)
		worktree := "/worktree/newer-dead"
		dir := filepath.Join(home, ".claude", "projects", encodeWorktree(worktree))

		liveEvents := []map[string]any{
			{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": "Old session said something real.",
				},
			},
		}
		deadEvents := []map[string]any{
			{
				"type": "assistant",
				"message": map[string]any{
					"role":    "assistant",
					"content": "No response requested.",
				},
			},
		}
		olderPath := writeJSONL(t, dir, "11111111-1111-1111-1111-111111111111.jsonl", liveEvents)
		newerPath := writeJSONL(t, dir, "22222222-2222-2222-2222-222222222222.jsonl", deadEvents)

		oldTime := time.Now().Add(-2 * time.Hour)
		newTime := time.Now()
		if err := os.Chtimes(olderPath, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(newerPath, newTime, newTime); err != nil {
			t.Fatal(err)
		}

		if got := FindClaudeSession(worktree); got != "" {
			t.Errorf("FindClaudeSession = %q, want \"\" (newer file is dead)", got)
		}
	})
}

func TestDeleteClaudeSession(t *testing.T) {
	t.Run("empty_path", func(t *testing.T) {
		if err := DeleteClaudeSession(""); err != nil {
			t.Errorf("DeleteClaudeSession(\"\") err = %v, want nil", err)
		}
	})

	t.Run("removes_existing_file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "session.jsonl")
		if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := DeleteClaudeSession(path); err != nil {
			t.Fatalf("DeleteClaudeSession: %v", err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("file still present after delete: %v", err)
		}
	})

	t.Run("nonexistent_path_returns_error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "missing.jsonl")
		err := DeleteClaudeSession(path)
		if err == nil {
			t.Fatal("DeleteClaudeSession(missing) err = nil, want error")
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("err = %v, want one wrapping os.ErrNotExist", err)
		}
	})
}
