package app

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTUILogPath_EnvOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "x.log")
	t.Setenv(EnvTUILog, want)

	got, err := TUILogPath()
	if err != nil {
		t.Fatalf("TUILogPath() unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("TUILogPath() = %q, want %q", got, want)
	}
}

func TestTUILogPath_DefaultsToCacheDir(t *testing.T) {
	t.Setenv(EnvTUILog, "")
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := TUILogPath()
	if err != nil {
		t.Fatalf("TUILogPath() unexpected error: %v", err)
	}
	want := filepath.Join(home, ".cache", "openkanban", "tui.log")
	if got != want {
		t.Errorf("TUILogPath() = %q, want %q", got, want)
	}
}

// TestTUILogRedirect exercises the actual contract: a log.Printf
// emitted after log.SetOutput points at the redirected file lands in
// that file. This is what protects Bubble Tea's alt-screen from log
// leaks — testing only the path-string algebra would not.
func TestTUILogRedirect(t *testing.T) {
	tempLog := filepath.Join(t.TempDir(), "tui.log")
	t.Setenv(EnvTUILog, tempLog)

	path, err := TUILogPath()
	if err != nil {
		t.Fatalf("TUILogPath() unexpected error: %v", err)
	}
	if path != tempLog {
		t.Fatalf("TUILogPath() = %q, want %q", path, tempLog)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()

	old := log.Writer()
	log.SetOutput(f)
	t.Cleanup(func() { log.SetOutput(old) })

	log.Printf("redirect-canary %d", 42)

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(contents), "redirect-canary 42") {
		t.Errorf("log file missing canary; contents=%q", string(contents))
	}
}
