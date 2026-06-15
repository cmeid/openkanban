package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withFakeHome rewires $HOME (and XDG/openkanban env overrides that
// would otherwise leak in from the developer's real machine) to a
// temp directory and returns it. Used by all plan/execute tests so
// they're hermetic.
func withFakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// config.ConfigDir checks these before falling back to ~/.config.
	// Force the fallback so tests deterministically resolve under home.
	t.Setenv("OPENKANBAN_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	return home
}

func TestPlanUninstall_BinaryAlwaysSet(t *testing.T) {
	_ = withFakeHome(t)

	plan, err := planUninstall()
	if err != nil {
		t.Fatalf("planUninstall: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if plan.Binary != exe {
		t.Errorf("plan.Binary = %q; want os.Executable() = %q", plan.Binary, exe)
	}
}

func TestPlanUninstall_NoClaudeDir(t *testing.T) {
	_ = withFakeHome(t)

	plan, err := planUninstall()
	if err != nil {
		t.Fatalf("planUninstall: %v", err)
	}
	if plan.HooksHomeExists {
		t.Errorf("HooksHomeExists = true with no ~/.claude; want false")
	}
	if plan.HooksSettings != "" {
		t.Errorf("HooksSettings = %q with no ~/.claude; want empty", plan.HooksSettings)
	}
}

func TestPlanUninstall_ClaudeDirPresent(t *testing.T) {
	home := withFakeHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	plan, err := planUninstall()
	if err != nil {
		t.Fatalf("planUninstall: %v", err)
	}
	if !plan.HooksHomeExists {
		t.Errorf("HooksHomeExists = false with ~/.claude present; want true")
	}
	want := filepath.Join(home, ".claude", "settings.json")
	if plan.HooksSettings != want {
		t.Errorf("HooksSettings = %q; want %q", plan.HooksSettings, want)
	}
}

func TestPlanUninstall_LegacyScriptDetected(t *testing.T) {
	home := withFakeHome(t)

	plan, _ := planUninstall()
	if plan.LegacyUpdateScript != "" {
		t.Errorf("LegacyUpdateScript should be empty before file exists, got %q", plan.LegacyUpdateScript)
	}

	dir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	legacy := filepath.Join(dir, "update-openkanban")
	if err := os.WriteFile(legacy, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	plan, _ = planUninstall()
	if plan.LegacyUpdateScript != legacy {
		t.Errorf("LegacyUpdateScript = %q; want %q", plan.LegacyUpdateScript, legacy)
	}
}

func TestPlanUninstall_DataDirsCovered(t *testing.T) {
	home := withFakeHome(t)
	// Materialize one of the three so we can verify Exists tracking.
	cacheDir := filepath.Join(home, ".cache", "openkanban")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	plan, err := planUninstall()
	if err != nil {
		t.Fatalf("planUninstall: %v", err)
	}
	if len(plan.DataDirs) != 3 {
		t.Fatalf("DataDirs count = %d; want 3", len(plan.DataDirs))
	}
	byLabel := map[string]dataDir{}
	for _, d := range plan.DataDirs {
		byLabel[d.Label] = d
	}
	want := []struct {
		label   string
		path    string
		exists  bool
	}{
		{"config", filepath.Join(home, ".config", "openkanban"), false},
		{"cache", cacheDir, true},
		{"status", filepath.Join(home, ".cache", "openkanban-status"), false},
	}
	for _, w := range want {
		got, ok := byLabel[w.label]
		if !ok {
			t.Errorf("missing data dir label %q", w.label)
			continue
		}
		if got.Path != w.path {
			t.Errorf("%s path = %q; want %q", w.label, got.Path, w.path)
		}
		if got.Exists != w.exists {
			t.Errorf("%s Exists = %v; want %v", w.label, got.Exists, w.exists)
		}
	}
}

func TestPrintPlan_MentionsKeyArtifacts(t *testing.T) {
	plan := uninstallPlan{
		Binary:             "/tmp/openkanban",
		HooksSettings:      "/tmp/.claude/settings.json",
		HooksHomeExists:    true,
		LegacyUpdateScript: "/tmp/.local/bin/update-openkanban",
		DataDirs: []dataDir{
			{Path: "/tmp/.config/openkanban", Label: "config", Description: "x", Exists: true},
		},
	}
	var buf bytes.Buffer
	printPlan(&buf, plan)
	out := buf.String()
	want := []string{
		"Will remove",
		"/tmp/openkanban",
		"/tmp/.claude/settings.json",
		"/tmp/.local/bin/update-openkanban",
		"Will NOT remove",
		"/tmp/.config/openkanban",
		"openkanban daemon stop",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("printPlan output missing %q\n---\n%s", w, out)
		}
	}
}

func TestPrintPlan_NoClaudeDir(t *testing.T) {
	plan := uninstallPlan{
		Binary:          "/tmp/openkanban",
		HooksHomeExists: false,
		DataDirs: []dataDir{
			{Path: "/tmp/.config/openkanban", Label: "config", Description: "x", Exists: false},
		},
	}
	var buf bytes.Buffer
	printPlan(&buf, plan)
	out := buf.String()
	if !strings.Contains(out, "no ~/.claude directory") {
		t.Errorf("printPlan missing 'no ~/.claude directory' note when HooksHomeExists is false:\n%s", out)
	}
}

func TestExecutePlan_RemovesArtifactsAndPreservesData(t *testing.T) {
	home := withFakeHome(t)

	// Fake binary: not actually openkanban, but executePlan only os.Remove's it.
	bin := filepath.Join(home, "fake-openkanban")
	if err := os.WriteFile(bin, []byte("not a real binary"), 0755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}

	// Hooks: install then uninstall path.
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	var prep bytes.Buffer
	if err := installHooks(settings, false, &prep); err != nil {
		t.Fatalf("install hooks: %v", err)
	}

	// Legacy script.
	legacyDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(legacyDir, 0755); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	legacy := filepath.Join(legacyDir, "update-openkanban")
	if err := os.WriteFile(legacy, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	// Data we expect to survive uninstall.
	configDir := filepath.Join(home, ".config", "openkanban")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	configFile := filepath.Join(configDir, "config.json")
	if err := os.WriteFile(configFile, []byte(`{"preserve":true}`), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	plan := uninstallPlan{
		Binary:             bin,
		HooksSettings:      settings,
		HooksHomeExists:    true,
		LegacyUpdateScript: legacy,
		DataDirs: []dataDir{
			{Path: configDir, Label: "config", Description: "x", Exists: true},
		},
	}

	var buf bytes.Buffer
	if err := executePlan(&buf, plan); err != nil {
		t.Fatalf("executePlan: %v", err)
	}

	// Binary gone.
	if _, err := os.Stat(bin); !os.IsNotExist(err) {
		t.Errorf("binary still present after executePlan: err=%v", err)
	}
	// Legacy gone.
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy script still present after executePlan: err=%v", err)
	}
	// Hooks scrubbed.
	after := readJSON(t, settings)
	if _, present := after["hooks"]; present {
		t.Errorf(`"hooks" still present after executePlan: %v`, after["hooks"])
	}
	// Config preserved verbatim.
	got, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("read config after uninstall: %v", err)
	}
	if string(got) != `{"preserve":true}` {
		t.Errorf("config file was modified: %s", got)
	}
}

func TestExecutePlan_SurfacesAllErrors(t *testing.T) {
	home := withFakeHome(t)

	// Binary that doesn't exist → os.Remove returns ENOENT.
	missingBin := filepath.Join(home, "does-not-exist")
	missingLegacy := filepath.Join(home, ".local", "bin", "update-openkanban")

	plan := uninstallPlan{
		Binary:             missingBin,
		HooksHomeExists:    false,
		LegacyUpdateScript: missingLegacy, // present in plan but absent on disk
	}

	var buf bytes.Buffer
	err := executePlan(&buf, plan)
	if err == nil {
		t.Fatal("expected error from executePlan with missing binary+legacy, got nil")
	}
	// Both removal failures should be surfaced via errors.Join.
	msg := err.Error()
	if !strings.Contains(msg, "binary") {
		t.Errorf("error missing binary context: %v", err)
	}
	if !strings.Contains(msg, "legacy script") {
		t.Errorf("error missing legacy context: %v", err)
	}
}

func TestConfirm_AcceptsAndRejects(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"empty defaults to no", "\n", false},
		{"lowercase y", "y\n", true},
		{"uppercase Y", "Y\n", true},
		{"yes spelled out", "yes\n", true},
		{"n is no", "n\n", false},
		{"garbage is no", "maybe\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			got := confirm(strings.NewReader(tc.input), &out, "Proceed? [y/N] ")
			if got != tc.want {
				t.Errorf("confirm(%q) = %v; want %v", tc.input, got, tc.want)
			}
			if !strings.Contains(out.String(), "Proceed?") {
				t.Errorf("prompt not written to out: %q", out.String())
			}
		})
	}
}
