package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSettings drops a fixture settings.json into $HOME/.claude/
// for the test. Empty content writes nothing (i.e. "no existing file").
func writeSettings(t *testing.T, home, content string) string {
	t.Helper()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dest := filepath.Join(dir, "settings.json")
	if content != "" {
		if err := os.WriteFile(dest, []byte(content), 0644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	return dest
}

// readJSON decodes a settings.json on disk into a generic map.
func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal %s: %v\n%s", path, err, string(data))
	}
	return m
}

// hookCommands returns the inner command strings registered under
// settings["hooks"][event], in order. Returns nil if the event is
// absent.
func hookCommands(t *testing.T, settings map[string]any, event string) []string {
	t.Helper()
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		return nil
	}
	slice, ok := hooks[event].([]any)
	if !ok {
		return nil
	}
	var cmds []string
	for _, item := range slice {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		inner, ok := entry["hooks"].([]any)
		if !ok {
			continue
		}
		for _, h := range inner {
			hobj, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if cmd, ok := hobj["command"].(string); ok {
				cmds = append(cmds, cmd)
			}
		}
	}
	return cmds
}

// findBackup returns the first .bak-* file alongside dest, or "" if none.
func findBackup(t *testing.T, dest string) string {
	t.Helper()
	dir := filepath.Dir(dest)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "settings.json.bak-") {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

func TestInstallHooks_NoExistingFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dest := filepath.Join(home, ".claude", "settings.json")
	var buf bytes.Buffer
	if err := installHooks(dest, false, &buf); err != nil {
		t.Fatalf("install: %v", err)
	}

	settings := readJSON(t, dest)
	for _, h := range managedHooks {
		cmds := hookCommands(t, settings, h.Event)
		if len(cmds) != 1 || cmds[0] != h.Command {
			t.Errorf("event %s commands = %v; want exactly [%q]", h.Event, cmds, h.Command)
		}
	}

	// No file existed, so no backup should be written.
	if got := findBackup(t, dest); got != "" {
		t.Errorf("unexpected backup file: %s", got)
	}
}

func TestInstallHooks_PreservesUnrelatedKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	fixture := `{
  "theme": "dark",
  "permissions": {"defaultMode": "auto"}
}`
	dest := writeSettings(t, home, fixture)
	originalBytes, _ := os.ReadFile(dest)

	var buf bytes.Buffer
	if err := installHooks(dest, false, &buf); err != nil {
		t.Fatalf("install: %v", err)
	}

	settings := readJSON(t, dest)
	if got, _ := settings["theme"].(string); got != "dark" {
		t.Errorf("theme = %q; want %q", got, "dark")
	}
	perms, ok := settings["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions missing or wrong type: %T", settings["permissions"])
	}
	if got, _ := perms["defaultMode"].(string); got != "auto" {
		t.Errorf("permissions.defaultMode = %q; want %q", got, "auto")
	}
	// hooks should now exist with all four events.
	for _, h := range managedHooks {
		if cmds := hookCommands(t, settings, h.Event); len(cmds) != 1 || cmds[0] != h.Command {
			t.Errorf("event %s commands = %v; want exactly [%q]", h.Event, cmds, h.Command)
		}
	}

	backup := findBackup(t, dest)
	if backup == "" {
		t.Fatal("expected a backup file alongside settings.json")
	}
	backupBytes, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if !bytes.Equal(backupBytes, originalBytes) {
		t.Errorf("backup does not match original bytes")
	}
}

func TestInstallHooks_PreservesUserHookOnSameEvent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	fixture := `{
  "hooks": {
    "Stop": [
      {"hooks": [{"type": "command", "command": "echo done"}]}
    ]
  }
}`
	dest := writeSettings(t, home, fixture)

	var buf bytes.Buffer
	if err := installHooks(dest, false, &buf); err != nil {
		t.Fatalf("install: %v", err)
	}

	settings := readJSON(t, dest)
	stopCmds := hookCommands(t, settings, "Stop")
	if len(stopCmds) != 2 {
		t.Fatalf("Stop commands = %v; want 2 entries (user + ours)", stopCmds)
	}
	var sawUser, sawOurs bool
	for _, c := range stopCmds {
		if c == "echo done" {
			sawUser = true
		}
		if c == "openkanban status set idle" {
			sawOurs = true
		}
	}
	if !sawUser {
		t.Errorf("user's `echo done` hook lost from Stop event: %v", stopCmds)
	}
	if !sawOurs {
		t.Errorf("openkanban hook not appended to Stop event: %v", stopCmds)
	}
}

func TestInstallHooks_IdempotentOnRerun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dest := filepath.Join(home, ".claude", "settings.json")
	var buf bytes.Buffer
	if err := installHooks(dest, false, &buf); err != nil {
		t.Fatalf("first install: %v", err)
	}
	first := readJSON(t, dest)

	if err := installHooks(dest, false, &buf); err != nil {
		t.Fatalf("second install: %v", err)
	}
	second := readJSON(t, dest)

	for _, h := range managedHooks {
		firstCmds := hookCommands(t, first, h.Event)
		secondCmds := hookCommands(t, second, h.Event)
		if len(firstCmds) != 1 || len(secondCmds) != 1 {
			t.Errorf("event %s: first=%v second=%v; want exactly one entry each", h.Event, firstCmds, secondCmds)
		}
		if len(secondCmds) == 1 && secondCmds[0] != h.Command {
			t.Errorf("event %s command after rerun = %q; want %q", h.Event, secondCmds[0], h.Command)
		}
	}
}

func TestInstallHooks_MalformedJSONRefuses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	fixture := `{ this is not json `
	dest := writeSettings(t, home, fixture)
	originalBytes, _ := os.ReadFile(dest)

	var buf bytes.Buffer
	err := installHooks(dest, false, &buf)
	if err == nil {
		t.Fatal("expected error on malformed JSON, got nil")
	}

	after, readErr := os.ReadFile(dest)
	if readErr != nil {
		t.Fatalf("read after refusal: %v", readErr)
	}
	if !bytes.Equal(after, originalBytes) {
		t.Errorf("settings.json was modified despite parse failure")
	}
	if backup := findBackup(t, dest); backup != "" {
		t.Errorf("backup created despite parse failure: %s", backup)
	}
}

func TestInstallHooks_DryRunDoesNotWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	fixture := `{"theme": "dark"}`
	dest := writeSettings(t, home, fixture)
	originalBytes, _ := os.ReadFile(dest)

	var buf bytes.Buffer
	if err := installHooks(dest, true, &buf); err != nil {
		t.Fatalf("dry-run: %v", err)
	}

	after, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read after dry-run: %v", err)
	}
	if !bytes.Equal(after, originalBytes) {
		t.Errorf("dry-run modified settings.json on disk")
	}
	if backup := findBackup(t, dest); backup != "" {
		t.Errorf("dry-run created backup: %s", backup)
	}

	out := buf.String()
	// The stdout dump should contain the proposed JSON, including our hooks.
	if !strings.Contains(out, "openkanban status set working") {
		t.Errorf("dry-run stdout missing managed hook command:\n%s", out)
	}
	if !strings.Contains(out, "SessionStart") {
		t.Errorf("dry-run stdout missing SessionStart event:\n%s", out)
	}

	// And the proposed JSON portion should parse back to a settings map
	// with both our hooks and the preserved theme key. Strip the leading
	// "# would write ..." comment line, then unmarshal.
	if idx := strings.IndexByte(out, '\n'); idx >= 0 {
		jsonPortion := out[idx+1:]
		var m map[string]any
		if err := json.Unmarshal([]byte(jsonPortion), &m); err != nil {
			t.Fatalf("dry-run stdout JSON did not parse: %v\n%s", err, jsonPortion)
		}
		if got, _ := m["theme"].(string); got != "dark" {
			t.Errorf("dry-run dropped theme key: got %v", m["theme"])
		}
	}
}

func TestInstallHooks_DryRunNoExistingFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dest := filepath.Join(home, ".claude", "settings.json")
	var buf bytes.Buffer
	if err := installHooks(dest, true, &buf); err != nil {
		t.Fatalf("dry-run: %v", err)
	}

	if _, err := os.Stat(dest); err == nil {
		t.Errorf("dry-run created settings.json at %s", dest)
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected stat error: %v", err)
	}
}
