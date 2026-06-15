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

func TestUninstallHooks_NoExistingFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dest := filepath.Join(home, ".claude", "settings.json")
	var buf bytes.Buffer
	if err := uninstallHooks(dest, false, &buf); err != nil {
		t.Fatalf("uninstall on missing file: %v", err)
	}

	if _, err := os.Stat(dest); err == nil {
		t.Errorf("uninstall created settings.json at %s", dest)
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected stat error: %v", err)
	}
	if backup := findBackup(t, dest); backup != "" {
		t.Errorf("backup created despite no input file: %s", backup)
	}
	if !strings.Contains(buf.String(), "nothing to uninstall") {
		t.Errorf("expected reassurance message, got:\n%s", buf.String())
	}
}

func TestUninstallHooks_RemovesAllManagedAfterInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dest := filepath.Join(home, ".claude", "settings.json")
	var buf bytes.Buffer
	if err := installHooks(dest, false, &buf); err != nil {
		t.Fatalf("install: %v", err)
	}
	buf.Reset()

	if err := uninstallHooks(dest, false, &buf); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	settings := readJSON(t, dest)
	for _, h := range managedHooks {
		if cmds := hookCommands(t, settings, h.Event); len(cmds) != 0 {
			t.Errorf("event %s after uninstall has commands %v; want none", h.Event, cmds)
		}
	}
	if _, present := settings["hooks"]; present {
		// hooks map should be dropped entirely when nothing else lived there.
		t.Errorf(`"hooks" key still present after uninstall: %v`, settings["hooks"])
	}
}

func TestUninstallHooks_PreservesUserHookOnSameEvent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	fixture := `{
  "hooks": {
    "Stop": [
      {"hooks": [{"type": "command", "command": "echo done"}]},
      {"hooks": [{"type": "command", "command": "openkanban status set idle"}]}
    ]
  }
}`
	dest := writeSettings(t, home, fixture)

	var buf bytes.Buffer
	if err := uninstallHooks(dest, false, &buf); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	settings := readJSON(t, dest)
	cmds := hookCommands(t, settings, "Stop")
	if len(cmds) != 1 || cmds[0] != "echo done" {
		t.Errorf("Stop commands after uninstall = %v; want [\"echo done\"]", cmds)
	}
}

func TestUninstallHooks_PreservesUnrelatedKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dest := filepath.Join(home, ".claude", "settings.json")
	var buf bytes.Buffer
	if err := installHooks(dest, false, &buf); err != nil {
		t.Fatalf("install: %v", err)
	}
	// Sprinkle some user-owned keys so we can verify they survive uninstall.
	settings := readJSON(t, dest)
	settings["theme"] = "dark"
	settings["permissions"] = map[string]any{"defaultMode": "auto"}
	merged, _ := json.MarshalIndent(settings, "", "  ")
	if err := os.WriteFile(dest, merged, 0644); err != nil {
		t.Fatalf("rewrite fixture: %v", err)
	}

	buf.Reset()
	if err := uninstallHooks(dest, false, &buf); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	after := readJSON(t, dest)
	if got, _ := after["theme"].(string); got != "dark" {
		t.Errorf("theme = %q; want %q", got, "dark")
	}
	perms, ok := after["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions missing or wrong type: %T", after["permissions"])
	}
	if got, _ := perms["defaultMode"].(string); got != "auto" {
		t.Errorf("permissions.defaultMode = %q; want %q", got, "auto")
	}
}

func TestUninstallHooks_NoOurEntries_NoOp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	fixture := `{
  "theme": "dark",
  "hooks": {
    "Stop": [
      {"hooks": [{"type": "command", "command": "echo done"}]}
    ]
  }
}`
	dest := writeSettings(t, home, fixture)
	originalBytes, _ := os.ReadFile(dest)

	var buf bytes.Buffer
	if err := uninstallHooks(dest, false, &buf); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	after, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read after uninstall: %v", err)
	}
	if !bytes.Equal(after, originalBytes) {
		t.Errorf("settings.json was rewritten despite no openkanban entries\nbefore:\n%s\nafter:\n%s",
			originalBytes, after)
	}
	if backup := findBackup(t, dest); backup != "" {
		t.Errorf("backup created on no-op uninstall: %s", backup)
	}
	if !strings.Contains(buf.String(), "nothing to remove") {
		t.Errorf("expected no-op message, got:\n%s", buf.String())
	}
}

func TestUninstallHooks_Idempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dest := filepath.Join(home, ".claude", "settings.json")
	var buf bytes.Buffer
	if err := installHooks(dest, false, &buf); err != nil {
		t.Fatalf("install: %v", err)
	}

	buf.Reset()
	if err := uninstallHooks(dest, false, &buf); err != nil {
		t.Fatalf("first uninstall: %v", err)
	}
	first, _ := os.ReadFile(dest)

	buf.Reset()
	if err := uninstallHooks(dest, false, &buf); err != nil {
		t.Fatalf("second uninstall: %v", err)
	}
	second, _ := os.ReadFile(dest)

	if !bytes.Equal(first, second) {
		t.Errorf("second uninstall modified disk\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestUninstallHooks_DryRunDoesNotWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dest := filepath.Join(home, ".claude", "settings.json")
	var buf bytes.Buffer
	if err := installHooks(dest, false, &buf); err != nil {
		t.Fatalf("install: %v", err)
	}
	originalBytes, _ := os.ReadFile(dest)

	buf.Reset()
	if err := uninstallHooks(dest, true, &buf); err != nil {
		t.Fatalf("dry-run: %v", err)
	}

	after, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read after dry-run: %v", err)
	}
	if !bytes.Equal(after, originalBytes) {
		t.Errorf("dry-run modified settings.json on disk")
	}
	// Only the install backup should exist — no uninstall backup.
	dir := filepath.Dir(dest)
	entries, _ := os.ReadDir(dir)
	bakCount := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "settings.json.bak-") {
			bakCount++
		}
	}
	if bakCount != 0 {
		t.Errorf("dry-run created %d backup file(s)", bakCount)
	}

	out := buf.String()
	if !strings.Contains(out, "would write") {
		t.Errorf("dry-run stdout missing preview header:\n%s", out)
	}
	if strings.Contains(out, "openkanban status set") {
		t.Errorf("dry-run preview should not contain openkanban entries (they're being removed):\n%s", out)
	}
}

func TestUninstallHooks_MalformedJSONRefuses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	fixture := `{ this is not json `
	dest := writeSettings(t, home, fixture)
	originalBytes, _ := os.ReadFile(dest)

	var buf bytes.Buffer
	err := uninstallHooks(dest, false, &buf)
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
}

// TestManagedHooksCoversPostToolUse pins the expected event coverage so a
// future edit that drops PostToolUse fails loudly. PostToolUse → working
// is what brings the file back to "working" after a Notification +
// permission grant — without it the status stays stuck at "waiting"
// until the next Stop hook, which can be a long time away.
func TestManagedHooksCoversPostToolUse(t *testing.T) {
	wantEvents := map[string]string{
		"SessionStart":     "openkanban status set working",
		"UserPromptSubmit": "openkanban status set working",
		"PostToolUse":      "openkanban status set working",
		"Stop":             "openkanban status set idle",
		"Notification":     "openkanban status set waiting",
	}
	got := map[string]string{}
	for _, h := range managedHooks {
		if existing, dup := got[h.Event]; dup {
			t.Errorf("event %q listed twice in managedHooks (%q vs %q)", h.Event, existing, h.Command)
		}
		got[h.Event] = h.Command
	}
	for event, wantCmd := range wantEvents {
		gotCmd, ok := got[event]
		if !ok {
			t.Errorf("managedHooks missing event %q", event)
			continue
		}
		if gotCmd != wantCmd {
			t.Errorf("managedHooks[%q] = %q; want %q", event, gotCmd, wantCmd)
		}
	}
}
