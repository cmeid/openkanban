package cmd

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withFakeRestoreHome mirrors withFakeBackupHome. Per-file helpers
// are the established style in this package (see uninstall_test.go +
// backup_test.go), so we duplicate rather than lift.
func withFakeRestoreHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OPENKANBAN_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	return home
}

// resetRestoreFlags pins the restore flag vars back to their defaults
// after each test that mutates them.
func resetRestoreFlags(t *testing.T) {
	t.Helper()
	prevDry, prevYes, prevConflict := restoreDryRun, restoreYes, restoreOnConflict
	t.Cleanup(func() {
		restoreDryRun, restoreYes, restoreOnConflict = prevDry, prevYes, prevConflict
	})
	restoreDryRun = false
	restoreYes = false
	restoreOnConflict = "prompt"
}

// buildFixtureArchive constructs a zip in a temp file with the given
// manifest + config files + repo files, returning the temp path.
// Layout matches what cmd/backup.go writes:
//
//	manifest.json (last)
//	config/<path> for each configFiles[<path>]
//	repos/<archive-dir>/tickets/<path> for each repoFiles[<archive-dir>][<path>]
//
// Tests can also add extra arbitrary entries via the rawExtra map
// (e.g. zip-slip payloads). rawExtra entries are written BEFORE
// manifest.json to match the real layout.
func buildFixtureArchive(
	t *testing.T,
	manifest backupManifest,
	configFiles map[string][]byte,
	repoFiles map[string]map[string][]byte,
	rawExtra map[string][]byte,
) string {
	t.Helper()
	tmp, err := os.CreateTemp("", "openkanban-restore-fixture-*.zip")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer tmp.Close()
	t.Cleanup(func() { _ = os.Remove(tmp.Name()) })

	zw := zip.NewWriter(tmp)

	// Deterministic key order for repeatable tests.
	configKeys := sortedKeys(configFiles)
	for _, name := range configKeys {
		if err := writeFixtureEntry(zw, "config/"+name, configFiles[name]); err != nil {
			t.Fatalf("write config entry %q: %v", name, err)
		}
	}

	repoDirs := sortedKeys(repoFiles)
	for _, archiveDir := range repoDirs {
		files := repoFiles[archiveDir]
		for _, name := range sortedKeys(files) {
			full := "repos/" + archiveDir + "/tickets/" + name
			if err := writeFixtureEntry(zw, full, files[name]); err != nil {
				t.Fatalf("write repo entry %q: %v", full, err)
			}
		}
	}

	for _, name := range sortedKeys(rawExtra) {
		if err := writeFixtureEntry(zw, name, rawExtra[name]); err != nil {
			t.Fatalf("write raw entry %q: %v", name, err)
		}
	}

	mb, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := writeFixtureEntry(zw, "manifest.json", mb); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return tmp.Name()
}

func writeFixtureEntry(zw *zip.Writer, name string, data []byte) error {
	hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
	hdr.SetMode(0644)
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Avoid a sort import: in-place insertion sort, fine for small maps.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}

// emptyReader produces a bufio.Reader bound to an empty input. Used
// for tests where no interactive prompts should fire (e.g. -y paths,
// --on-conflict=skip/overwrite, or zero-conflict restores).
func emptyReader() *bufio.Reader {
	return bufio.NewReader(strings.NewReader(""))
}

func TestPlanRestore_MissingArchive(t *testing.T) {
	_ = withFakeRestoreHome(t)
	resetRestoreFlags(t)

	_, err := planRestore("/nonexistent/path/to/backup.zip")
	if err == nil {
		t.Fatal("planRestore should error on missing archive")
	}
	if !strings.Contains(err.Error(), "archive") {
		t.Errorf("error should mention archive, got: %v", err)
	}
}

func TestPlanRestore_RejectsUnknownConflict(t *testing.T) {
	home := withFakeRestoreHome(t)
	resetRestoreFlags(t)
	restoreOnConflict = "garbage"

	// We need a valid archive on disk so the only thing planRestore
	// rejects is the flag value.
	archive := buildFixtureArchive(t,
		backupManifest{CreatedAt: time.Now().UTC().Format(time.RFC3339)},
		nil, nil, nil)
	// Place it under home for clean output.
	final := filepath.Join(home, "fixture.zip")
	if err := os.Rename(archive, final); err != nil {
		t.Fatalf("rename: %v", err)
	}

	_, err := planRestore(final)
	if err == nil {
		t.Fatal("planRestore should error on garbage --on-conflict")
	}
	for _, want := range []string{"skip", "overwrite", "prompt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should list valid value %q, got: %v", want, err)
		}
	}
}

func TestPlanRestore_AcceptsValidConflict(t *testing.T) {
	home := withFakeRestoreHome(t)

	archive := buildFixtureArchive(t,
		backupManifest{CreatedAt: time.Now().UTC().Format(time.RFC3339)},
		nil, nil, nil)
	final := filepath.Join(home, "fixture.zip")
	if err := os.Rename(archive, final); err != nil {
		t.Fatalf("rename: %v", err)
	}

	for _, mode := range []string{"skip", "overwrite", "prompt", ""} {
		t.Run("mode="+mode, func(t *testing.T) {
			resetRestoreFlags(t)
			restoreOnConflict = mode
			plan, err := planRestore(final)
			if err != nil {
				t.Fatalf("planRestore(mode=%q): %v", mode, err)
			}
			want := mode
			if mode == "" {
				want = "prompt"
			}
			if plan.ConflictMode != want {
				t.Errorf("ConflictMode = %q; want %q", plan.ConflictMode, want)
			}
		})
	}
}

func TestExecuteRestore_FreshTarget(t *testing.T) {
	home := withFakeRestoreHome(t)
	resetRestoreFlags(t)
	restoreYes = true

	// Build a fake repo on disk so its RepoPath is "present".
	repoPath := filepath.Join(home, "fake-repo")
	if err := os.MkdirAll(repoPath, 0755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	configBytes := []byte(`{"defaults":{}}`)
	ticketBytes := []byte("# brief\n\nbody\n")
	repoTicketBytes := []byte("# repo ticket\n\nstuff\n")

	manifest := backupManifest{
		CreatedAt:         time.Now().UTC().Format(time.RFC3339),
		OpenkanbanVersion: "test",
		SourceConfigDir:   "/orig/config",
		Projects: []manifestProjectEntry{
			{ID: "p1", Name: "fakerepo", RepoPath: repoPath},
		},
	}
	archive := buildFixtureArchive(t,
		manifest,
		map[string][]byte{
			"config.json":                   configBytes,
			"tickets/p1/my-ticket-1234.md":  ticketBytes,
		},
		map[string]map[string][]byte{
			"fakerepo": {"my-ticket.md": repoTicketBytes},
		},
		nil,
	)

	plan, err := planRestore(archive)
	if err != nil {
		t.Fatalf("planRestore: %v", err)
	}

	var buf bytes.Buffer
	if err := executeRestorePlan(emptyReader(), &buf, plan); err != nil {
		t.Fatalf("executeRestorePlan: %v", err)
	}

	// Assert all three files landed at the right places with the right bytes.
	configDir := filepath.Join(home, ".config", "openkanban")
	assertFileBytes(t, filepath.Join(configDir, "config.json"), configBytes)
	assertFileBytes(t, filepath.Join(configDir, "tickets", "p1", "my-ticket-1234.md"), ticketBytes)
	assertFileBytes(t, filepath.Join(repoPath, "tickets", "my-ticket.md"), repoTicketBytes)
}

func TestExecuteRestore_IdenticalFilesSkipped(t *testing.T) {
	home := withFakeRestoreHome(t)
	resetRestoreFlags(t)
	restoreYes = true

	configBytes := []byte(`{"identical":true}`)

	// Pre-populate the dest with identical bytes.
	configDir := filepath.Join(home, ".config", "openkanban")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	destPath := filepath.Join(configDir, "config.json")
	if err := os.WriteFile(destPath, configBytes, 0644); err != nil {
		t.Fatalf("seed dest: %v", err)
	}
	// Capture mtime; sleep so any rewrite is observable.
	origStat, err := os.Stat(destPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	origMtime := origStat.ModTime()
	time.Sleep(50 * time.Millisecond)

	manifest := backupManifest{
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	archive := buildFixtureArchive(t,
		manifest,
		map[string][]byte{"config.json": configBytes},
		nil, nil,
	)

	plan, err := planRestore(archive)
	if err != nil {
		t.Fatalf("planRestore: %v", err)
	}
	var buf bytes.Buffer
	if err := executeRestorePlan(emptyReader(), &buf, plan); err != nil {
		t.Fatalf("executeRestorePlan: %v", err)
	}

	afterStat, err := os.Stat(destPath)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !afterStat.ModTime().Equal(origMtime) {
		t.Errorf("mtime changed for identical file: before=%v, after=%v",
			origMtime, afterStat.ModTime())
	}
}

func TestExecuteRestore_ConflictSkip(t *testing.T) {
	home := withFakeRestoreHome(t)
	resetRestoreFlags(t)
	restoreYes = true
	restoreOnConflict = "skip"

	existing := []byte("ORIGINAL")
	archived := []byte("NEW")

	configDir := filepath.Join(home, ".config", "openkanban")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	destPath := filepath.Join(configDir, "config.json")
	if err := os.WriteFile(destPath, existing, 0644); err != nil {
		t.Fatalf("seed dest: %v", err)
	}

	archive := buildFixtureArchive(t,
		backupManifest{CreatedAt: time.Now().UTC().Format(time.RFC3339)},
		map[string][]byte{"config.json": archived},
		nil, nil,
	)

	plan, err := planRestore(archive)
	if err != nil {
		t.Fatalf("planRestore: %v", err)
	}
	var buf bytes.Buffer
	if err := executeRestorePlan(emptyReader(), &buf, plan); err != nil {
		t.Fatalf("executeRestorePlan: %v", err)
	}

	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(got, existing) {
		t.Errorf("dest changed under --on-conflict=skip:\n got: %q\nwant: %q", got, existing)
	}
}

func TestExecuteRestore_ConflictOverwrite(t *testing.T) {
	home := withFakeRestoreHome(t)
	resetRestoreFlags(t)
	restoreYes = true
	restoreOnConflict = "overwrite"

	existing := []byte("ORIGINAL")
	archived := []byte("NEW BYTES")

	configDir := filepath.Join(home, ".config", "openkanban")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	destPath := filepath.Join(configDir, "config.json")
	if err := os.WriteFile(destPath, existing, 0644); err != nil {
		t.Fatalf("seed dest: %v", err)
	}

	archive := buildFixtureArchive(t,
		backupManifest{CreatedAt: time.Now().UTC().Format(time.RFC3339)},
		map[string][]byte{"config.json": archived},
		nil, nil,
	)

	plan, err := planRestore(archive)
	if err != nil {
		t.Fatalf("planRestore: %v", err)
	}
	var buf bytes.Buffer
	if err := executeRestorePlan(emptyReader(), &buf, plan); err != nil {
		t.Fatalf("executeRestorePlan: %v", err)
	}

	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(got, archived) {
		t.Errorf("dest not overwritten:\n got: %q\nwant: %q", got, archived)
	}
}

func TestExecuteRestore_ZipSlipDefense(t *testing.T) {
	home := withFakeRestoreHome(t)
	resetRestoreFlags(t)
	restoreYes = true

	// Build a fixture with a path-traversal entry. We use rawExtra so
	// the entry name is exactly what we want (no prefix munging).
	bad := "config/../../../../etc/passwd-stolen"

	archive := buildFixtureArchive(t,
		backupManifest{CreatedAt: time.Now().UTC().Format(time.RFC3339)},
		map[string][]byte{"benign.txt": []byte("ok")},
		nil,
		map[string][]byte{bad: []byte("malicious")},
	)

	plan, err := planRestore(archive)
	if err != nil {
		t.Fatalf("planRestore: %v", err)
	}
	var buf bytes.Buffer
	err = executeRestorePlan(emptyReader(), &buf, plan)
	if err == nil {
		t.Fatal("executeRestorePlan should reject zip-slip entry")
	}
	if !strings.Contains(err.Error(), "rejecting archive") {
		t.Errorf("error should explain the rejection, got: %v", err)
	}

	// Nothing should have been written under the dest config dir...
	configDir := filepath.Join(home, ".config", "openkanban")
	if _, err := os.Stat(filepath.Join(configDir, "benign.txt")); err == nil {
		t.Errorf("benign file was written despite pre-flight rejection")
	}
	// ...and certainly not under any of the parent dirs the bad entry tried to escape into.
	suspicious := []string{
		filepath.Join(home, "passwd-stolen"),
		filepath.Join(filepath.Dir(home), "passwd-stolen"),
		filepath.Join(filepath.Dir(filepath.Dir(home)), "passwd-stolen"),
		"/etc/passwd-stolen",
	}
	for _, p := range suspicious {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("zip-slip target landed on disk at %s", p)
		}
	}
}

func TestExecuteRestore_MissingRepoSkipped(t *testing.T) {
	home := withFakeRestoreHome(t)
	resetRestoreFlags(t)
	restoreYes = true // -y → missing repos default to skip

	missingRepo := filepath.Join(home, "this-repo-does-not-exist")

	manifest := backupManifest{
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Projects: []manifestProjectEntry{
			{ID: "p1", Name: "missingrepo", RepoPath: missingRepo},
		},
	}
	archive := buildFixtureArchive(t,
		manifest,
		map[string][]byte{"config.json": []byte("{}")},
		map[string]map[string][]byte{
			"missingrepo": {"orphan-ticket.md": []byte("would-not-fit-anywhere")},
		},
		nil,
	)

	plan, err := planRestore(archive)
	if err != nil {
		t.Fatalf("planRestore: %v", err)
	}
	if len(plan.MissingRepos) != 1 {
		t.Fatalf("expected 1 missing repo, got %d", len(plan.MissingRepos))
	}

	// Drive the missing-repo prompt loop with -y → auto-skip.
	var buf bytes.Buffer
	reader := emptyReader()
	if err := resolveMissingRepos(reader, &buf, &plan); err != nil {
		t.Fatalf("resolveMissingRepos: %v", err)
	}

	if err := executeRestorePlan(reader, &buf, plan); err != nil {
		t.Fatalf("executeRestorePlan: %v", err)
	}

	// The missing repo's tickets dir should NOT exist (we never created it).
	repoTickets := filepath.Join(missingRepo, "tickets")
	if _, err := os.Stat(repoTickets); err == nil {
		t.Errorf("tickets dir under missing repo was created: %s", repoTickets)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unexpected stat error: %v", err)
	}
}

func TestExecuteRestore_MissingRepoLeavesProjectsJsonIntact(t *testing.T) {
	home := withFakeRestoreHome(t)
	resetRestoreFlags(t)
	restoreYes = true

	missingRepo := filepath.Join(home, "still-not-here")
	projectsJSON := []byte(`{"projects":{"p1":{"id":"p1","name":"missingrepo","repo_path":"` + missingRepo + `"}}}`)

	manifest := backupManifest{
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Projects: []manifestProjectEntry{
			{ID: "p1", Name: "missingrepo", RepoPath: missingRepo},
		},
	}
	archive := buildFixtureArchive(t,
		manifest,
		map[string][]byte{"projects.json": projectsJSON},
		map[string]map[string][]byte{
			"missingrepo": {"never-extracted.md": []byte("dropped")},
		},
		nil,
	)

	plan, err := planRestore(archive)
	if err != nil {
		t.Fatalf("planRestore: %v", err)
	}
	var buf bytes.Buffer
	reader := emptyReader()
	if err := resolveMissingRepos(reader, &buf, &plan); err != nil {
		t.Fatalf("resolveMissingRepos: %v", err)
	}
	if err := executeRestorePlan(reader, &buf, plan); err != nil {
		t.Fatalf("executeRestorePlan: %v", err)
	}

	// projects.json IS extracted even though its referenced repo is missing.
	configDir := filepath.Join(home, ".config", "openkanban")
	got, err := os.ReadFile(filepath.Join(configDir, "projects.json"))
	if err != nil {
		t.Fatalf("projects.json not extracted: %v", err)
	}
	if !bytes.Equal(got, projectsJSON) {
		t.Errorf("projects.json content differs:\n got: %q\nwant: %q", got, projectsJSON)
	}
}

// assertFileBytes is a tiny helper that asserts the file at path
// exists and contains exactly want.
func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("file %q bytes differ:\n got: %q\nwant: %q", path, got, want)
	}
}

// Ensure unused import warnings don't fire across go versions when a
// helper is removed in the future. (io is used by buildFixtureArchive
// via writeFixtureEntry's writer; keep this no-op live.)
var _ = io.Discard
