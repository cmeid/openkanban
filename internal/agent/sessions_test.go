package agent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
