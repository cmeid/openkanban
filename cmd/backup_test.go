package cmd

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withFakeBackupHome rewires $HOME plus the XDG/openkanban env knobs
// to a temp dir so backup tests don't accidentally pick up the dev
// machine's real config. Codebase style is per-file helpers (per the
// plan), so this is a deliberate duplicate of withFakeHome in
// cmd/uninstall_test.go rather than a lift.
func withFakeBackupHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// config.ConfigDir checks these before falling back to ~/.config.
	// Blank both so tests deterministically resolve under home.
	t.Setenv("OPENKANBAN_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	return home
}

// resetBackupFlags zeros the package-level backup flag vars so a test
// that mutates one doesn't leak state into the next.
func resetBackupFlags(t *testing.T) {
	t.Helper()
	prevOut, prevDry, prevYes := backupOutput, backupDryRun, backupYes
	t.Cleanup(func() {
		backupOutput, backupDryRun, backupYes = prevOut, prevDry, prevYes
	})
	backupOutput = ""
	backupDryRun = false
	backupYes = false
}

func TestPlanBackup_NoConfigDir(t *testing.T) {
	_ = withFakeBackupHome(t)
	resetBackupFlags(t)

	plan, err := planBackup()
	if err != nil {
		t.Fatalf("planBackup: %v", err)
	}
	if plan.ConfigDirExists {
		t.Errorf("ConfigDirExists = true with empty home; want false")
	}
	if plan.ConfigDir == "" {
		t.Errorf("ConfigDir should be populated even when dir is missing, got %q", plan.ConfigDir)
	}
	if len(plan.Projects) != 0 {
		t.Errorf("Projects = %d; want 0 (no registry)", len(plan.Projects))
	}
}

func TestPlanBackup_EmptyRegistry(t *testing.T) {
	home := withFakeBackupHome(t)
	resetBackupFlags(t)
	// Create config dir but no projects.json.
	if err := os.MkdirAll(filepath.Join(home, ".config", "openkanban"), 0755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}

	plan, err := planBackup()
	if err != nil {
		t.Fatalf("planBackup: %v", err)
	}
	if !plan.ConfigDirExists {
		t.Errorf("ConfigDirExists = false; want true")
	}
	if len(plan.Projects) != 0 {
		t.Errorf("Projects = %d; want 0", len(plan.Projects))
	}
}

func TestPlanBackup_OutputPathResolution(t *testing.T) {
	home := withFakeBackupHome(t)

	cases := []struct {
		name      string
		output    string
		checkPath func(t *testing.T, got string)
	}{
		{
			name:   "empty defaults to home/backup/openkanban",
			output: "",
			checkPath: func(t *testing.T, got string) {
				want := filepath.Join(home, "backup", "openkanban")
				if !strings.HasPrefix(got, want+string(os.PathSeparator)) {
					t.Errorf("default output %q should live under %q", got, want)
				}
				if !strings.HasSuffix(got, ".zip") {
					t.Errorf("default output %q should end in .zip", got)
				}
				base := filepath.Base(got)
				if !strings.HasPrefix(base, "openkanban-") {
					t.Errorf("default basename %q should start with openkanban-", base)
				}
			},
		},
		{
			name:   "directory gets auto-named .zip inside",
			output: filepath.Join(home, "mydir"),
			checkPath: func(t *testing.T, got string) {
				wantDir := filepath.Join(home, "mydir")
				if filepath.Dir(got) != wantDir {
					t.Errorf("dir output: parent = %q; want %q", filepath.Dir(got), wantDir)
				}
				if !strings.HasSuffix(got, ".zip") {
					t.Errorf("dir output should auto-name .zip, got %q", got)
				}
			},
		},
		{
			name:   "zip path used verbatim",
			output: filepath.Join(home, "explicit.zip"),
			checkPath: func(t *testing.T, got string) {
				want := filepath.Join(home, "explicit.zip")
				if got != want {
					t.Errorf("verbatim zip: got %q; want %q", got, want)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetBackupFlags(t)
			backupOutput = tc.output

			plan, err := planBackup()
			if err != nil {
				t.Fatalf("planBackup: %v", err)
			}
			tc.checkPath(t, plan.OutputPath)
		})
	}
}

func TestPlanBackup_RefusesExistingOutput(t *testing.T) {
	home := withFakeBackupHome(t)
	resetBackupFlags(t)

	existing := filepath.Join(home, "already-there.zip")
	if err := os.WriteFile(existing, []byte("anything"), 0644); err != nil {
		t.Fatalf("seed existing zip: %v", err)
	}
	backupOutput = existing

	_, err := planBackup()
	if err == nil {
		t.Fatal("planBackup returned nil error when output file already exists")
	}
	if !strings.Contains(err.Error(), existing) {
		t.Errorf("error should mention the conflicting path %q, got: %v", existing, err)
	}
}

func TestExecuteBackup_AutoCreatesOutputDir(t *testing.T) {
	home := withFakeBackupHome(t)
	resetBackupFlags(t)

	target := filepath.Join(home, "nested", "deeper", "still", "out.zip")
	backupOutput = target

	plan, err := planBackup()
	if err != nil {
		t.Fatalf("planBackup: %v", err)
	}

	var buf bytes.Buffer
	if err := executeBackupPlan(&buf, plan); err != nil {
		t.Fatalf("executeBackupPlan: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("archive not at expected path: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(target)); err != nil {
		t.Fatalf("parent dir not created: %v", err)
	}
}

func TestPlanBackup_EnvVarWarning(t *testing.T) {
	home := withFakeBackupHome(t)
	resetBackupFlags(t)

	configDir := filepath.Join(home, ".config", "openkanban")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	// Write a config.json with a non-empty env on the claude agent.
	cfg := `{
  "agents": {
    "claude": {
      "command": "claude",
      "args": ["--dangerously-skip-permissions"],
      "env": {"FOO": "bar"},
      "status_file": ".claude/status.json",
      "init_prompt": ""
    }
  }
}`
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(cfg), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	plan, err := planBackup()
	if err != nil {
		t.Fatalf("planBackup: %v", err)
	}
	if len(plan.EnvWarnings) == 0 {
		t.Fatal("expected at least one EnvWarning for non-empty agent env, got none")
	}
	joined := strings.Join(plan.EnvWarnings, "\n")
	if !strings.Contains(joined, "claude") {
		t.Errorf("warning should mention agent name 'claude', got:\n%s", joined)
	}
	if !strings.Contains(joined, "FOO") {
		t.Errorf("warning should mention env key 'FOO', got:\n%s", joined)
	}
}

func TestExecuteBackup_RoundtripContents(t *testing.T) {
	home := withFakeBackupHome(t)
	resetBackupFlags(t)

	// Seed config dir: config.json + projects.json + a ticket.
	configDir := filepath.Join(home, ".config", "openkanban")
	if err := os.MkdirAll(filepath.Join(configDir, "tickets", "proj-id-1"), 0755); err != nil {
		t.Fatalf("mkdir tickets: %v", err)
	}
	configJSON := []byte(`{"defaults":{}}`)
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), configJSON, 0644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	// Seed a fake repo with tickets/<slug>.md.
	repoPath := filepath.Join(home, "fake-repo")
	if err := os.MkdirAll(filepath.Join(repoPath, "tickets"), 0755); err != nil {
		t.Fatalf("mkdir repo tickets: %v", err)
	}
	repoTicket := []byte("# My ticket\n\nbody bytes here\n")
	if err := os.WriteFile(filepath.Join(repoPath, "tickets", "my-ticket.md"), repoTicket, 0644); err != nil {
		t.Fatalf("write repo ticket: %v", err)
	}

	// projects.json with one project pointing at the fake repo.
	registry := `{
  "projects": {
    "proj-id-1": {
      "id": "proj-id-1",
      "name": "fakerepo",
      "repo_path": "` + repoPath + `",
      "worktree_dir": "",
      "created_at": "2025-01-01T00:00:00Z",
      "updated_at": "2025-01-01T00:00:00Z",
      "settings": {"auto_spawn_agent": false, "auto_create_branch": false}
    }
  }
}`
	if err := os.WriteFile(filepath.Join(configDir, "projects.json"), []byte(registry), 0644); err != nil {
		t.Fatalf("write projects.json: %v", err)
	}

	// Stub ticket brief inside config tickets dir.
	ticketBrief := []byte("---\ntitle: x\n---\n\nbody\n")
	if err := os.WriteFile(filepath.Join(configDir, "tickets", "proj-id-1", "my-ticket-abcd1234.md"), ticketBrief, 0644); err != nil {
		t.Fatalf("write ticket brief: %v", err)
	}

	target := filepath.Join(home, "out.zip")
	backupOutput = target

	plan, err := planBackup()
	if err != nil {
		t.Fatalf("planBackup: %v", err)
	}
	if len(plan.Projects) != 1 {
		t.Fatalf("plan.Projects = %d; want 1", len(plan.Projects))
	}

	var buf bytes.Buffer
	if err := executeBackupPlan(&buf, plan); err != nil {
		t.Fatalf("executeBackupPlan: %v", err)
	}

	// Open zip and assert contents.
	zr, err := zip.OpenReader(target)
	if err != nil {
		t.Fatalf("zip.OpenReader: %v", err)
	}
	defer zr.Close()

	contents := map[string][]byte{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open zip entry %q: %v", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read zip entry %q: %v", f.Name, err)
		}
		contents[f.Name] = data
	}

	wantPaths := []string{
		"manifest.json",
		"config/config.json",
		"config/projects.json",
		"config/tickets/proj-id-1/my-ticket-abcd1234.md",
		"repos/fakerepo/tickets/my-ticket.md",
	}
	for _, p := range wantPaths {
		if _, ok := contents[p]; !ok {
			var keys []string
			for k := range contents {
				keys = append(keys, k)
			}
			t.Errorf("missing zip entry %q; have: %v", p, keys)
		}
	}

	// Byte-for-byte check on the key files.
	if got := contents["config/config.json"]; !bytes.Equal(got, configJSON) {
		t.Errorf("config.json bytes differ:\n got: %q\nwant: %q", got, configJSON)
	}
	if got := contents["config/tickets/proj-id-1/my-ticket-abcd1234.md"]; !bytes.Equal(got, ticketBrief) {
		t.Errorf("ticket brief bytes differ:\n got: %q\nwant: %q", got, ticketBrief)
	}
	if got := contents["repos/fakerepo/tickets/my-ticket.md"]; !bytes.Equal(got, repoTicket) {
		t.Errorf("repo ticket bytes differ:\n got: %q\nwant: %q", got, repoTicket)
	}
}

func TestExecuteBackup_SkipsTmpFiles(t *testing.T) {
	home := withFakeBackupHome(t)
	resetBackupFlags(t)

	configDir := filepath.Join(home, ".config", "openkanban")
	ticketsDir := filepath.Join(configDir, "tickets", "pid")
	if err := os.MkdirAll(ticketsDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// One real file, two tmp-style files that should be skipped.
	if err := os.WriteFile(filepath.Join(ticketsDir, "good.md"), []byte("ok"), 0644); err != nil {
		t.Fatalf("write good: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ticketsDir, "good.md.tmp-xyz"), []byte("garbage"), 0644); err != nil {
		t.Fatalf("write .tmp-: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ticketsDir, "bar.tmp"), []byte("garbage"), 0644); err != nil {
		t.Fatalf("write .tmp suffix: %v", err)
	}

	target := filepath.Join(home, "out.zip")
	backupOutput = target

	plan, err := planBackup()
	if err != nil {
		t.Fatalf("planBackup: %v", err)
	}
	var buf bytes.Buffer
	if err := executeBackupPlan(&buf, plan); err != nil {
		t.Fatalf("executeBackupPlan: %v", err)
	}

	zr, err := zip.OpenReader(target)
	if err != nil {
		t.Fatalf("zip.OpenReader: %v", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		base := filepath.Base(f.Name)
		if strings.Contains(base, ".tmp-") || strings.HasSuffix(base, ".tmp") {
			t.Errorf("zip should not contain tmp file %q", f.Name)
		}
	}
	// And the real file is present.
	foundGood := false
	for _, f := range zr.File {
		if f.Name == "config/tickets/pid/good.md" {
			foundGood = true
			break
		}
	}
	if !foundGood {
		t.Errorf("expected config/tickets/pid/good.md in zip")
	}
}

func TestExecuteBackup_ManifestShape(t *testing.T) {
	home := withFakeBackupHome(t)
	resetBackupFlags(t)

	configDir := filepath.Join(home, ".config", "openkanban")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	repoPath := filepath.Join(home, "repo")
	if err := os.MkdirAll(filepath.Join(repoPath, "tickets"), 0755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	registry := `{
  "projects": {
    "uuid-aaa": {
      "id": "uuid-aaa",
      "name": "alpha",
      "repo_path": "` + repoPath + `",
      "worktree_dir": "",
      "created_at": "2025-01-01T00:00:00Z",
      "updated_at": "2025-01-01T00:00:00Z",
      "settings": {}
    }
  }
}`
	if err := os.WriteFile(filepath.Join(configDir, "projects.json"), []byte(registry), 0644); err != nil {
		t.Fatalf("write registry: %v", err)
	}

	target := filepath.Join(home, "out.zip")
	backupOutput = target

	plan, err := planBackup()
	if err != nil {
		t.Fatalf("planBackup: %v", err)
	}
	var buf bytes.Buffer
	if err := executeBackupPlan(&buf, plan); err != nil {
		t.Fatalf("executeBackupPlan: %v", err)
	}

	zr, err := zip.OpenReader(target)
	if err != nil {
		t.Fatalf("zip.OpenReader: %v", err)
	}
	defer zr.Close()

	var manifestBytes []byte
	for _, f := range zr.File {
		if f.Name == "manifest.json" {
			rc, err := f.Open()
			if err != nil {
				t.Fatalf("open manifest: %v", err)
			}
			manifestBytes, err = io.ReadAll(rc)
			rc.Close()
			if err != nil {
				t.Fatalf("read manifest: %v", err)
			}
		}
	}
	if manifestBytes == nil {
		t.Fatal("manifest.json missing from archive")
	}

	var m backupManifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	// created_at must parse as RFC3339.
	if _, err := time.Parse(time.RFC3339, m.CreatedAt); err != nil {
		t.Errorf("created_at %q does not parse as RFC3339: %v", m.CreatedAt, err)
	}
	if m.SourceConfigDir != configDir {
		t.Errorf("source_config_dir = %q; want %q", m.SourceConfigDir, configDir)
	}
	if m.OpenkanbanVersion == "" {
		t.Errorf("openkanban_version should be non-empty")
	}
	if len(m.Projects) != 1 {
		t.Fatalf("projects = %d; want 1", len(m.Projects))
	}
	if m.Projects[0].ID != "uuid-aaa" || m.Projects[0].Name != "alpha" || m.Projects[0].RepoPath != repoPath {
		t.Errorf("project entry shape wrong: %+v", m.Projects[0])
	}

	// service_was_installed: on darwin without a plist on disk → false.
	// Non-darwin → always false. This test never installs a plist.
	if m.ServiceInstalled {
		t.Errorf("service_was_installed = true; want false (no plist installed; GOOS=%s)", runtime.GOOS)
	}

	// Verify manifest is the LAST entry in the archive (per plan: so
	// partial archives are easy to detect).
	last := zr.File[len(zr.File)-1]
	if last.Name != "manifest.json" {
		t.Errorf("manifest.json should be last entry; got %q (all entries: %d)", last.Name, len(zr.File))
	}
}

func TestExecuteBackup_ConcurrentWrite(t *testing.T) {
	home := withFakeBackupHome(t)
	resetBackupFlags(t)

	configDir := filepath.Join(home, ".config", "openkanban")
	ticketsDir := filepath.Join(configDir, "tickets", "p1")
	if err := os.MkdirAll(ticketsDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Seed plenty of real files so the walk has work to do — without
	// this the goroutine can race past the walk and never collide.
	for i := 0; i < 200; i++ {
		name := filepath.Join(ticketsDir, "real-"+timeStamp(i)+".md")
		if err := os.WriteFile(name, []byte("body"), 0644); err != nil {
			t.Fatalf("seed real file: %v", err)
		}
	}

	target := filepath.Join(home, "out.zip")
	backupOutput = target

	plan, err := planBackup()
	if err != nil {
		t.Fatalf("planBackup: %v", err)
	}

	// Concurrent writer: drops .tmp-XXX files into the same directory
	// while the executor walks. Stops when stop is closed.
	stop := make(chan struct{})
	var stopped atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				stopped.Store(true)
				return
			default:
			}
			name := filepath.Join(ticketsDir, "race-"+timeStamp(i)+".md.tmp-abc")
			_ = os.WriteFile(name, []byte("garbage"), 0644)
			// And clean it up immediately to simulate mid-flight
			// atomic-rename activity. If it's gone before walk sees it,
			// fine; if it's there during walk, the .tmp- skip should
			// catch it.
			_ = os.Remove(name)
			i++
		}
	}()

	var buf bytes.Buffer
	execErr := executeBackupPlan(&buf, plan)
	close(stop)
	wg.Wait()

	if execErr != nil {
		t.Fatalf("executeBackupPlan: %v", execErr)
	}

	// Archive must be well-formed and contain zero .tmp-* entries.
	zr, err := zip.OpenReader(target)
	if err != nil {
		t.Fatalf("zip.OpenReader: %v", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		base := filepath.Base(f.Name)
		if strings.Contains(base, ".tmp-") || strings.HasSuffix(base, ".tmp") {
			t.Errorf("concurrent write leaked into archive: %q", f.Name)
		}
	}
	// Manifest must be last regardless of races.
	if zr.File[len(zr.File)-1].Name != "manifest.json" {
		t.Errorf("manifest.json should be last entry after concurrent writes; got %q",
			zr.File[len(zr.File)-1].Name)
	}
	if !stopped.Load() {
		t.Errorf("concurrent writer goroutine never observed stop signal")
	}
}

// timeStamp produces a short deterministic string for seeding test
// filenames. Avoids time.Now to keep tests reproducible.
func timeStamp(i int) string {
	return strings.ReplaceAll(strings.ReplaceAll(
		(time.Time{}).Add(time.Duration(i)*time.Second).Format("20060102150405"),
		"-", ""), ":", "")
}
