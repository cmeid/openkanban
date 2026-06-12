package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testDebounce = 30 * time.Millisecond

// atomicWrite mirrors openkanban's Save pattern: write to a .tmp
// sibling, then rename onto the destination. This is the editor
// pattern and the openkanban pattern; both must be detected.
func atomicWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename: %v", err)
	}
}

func newTestWatcher(t *testing.T) (*Watcher, string) {
	t.Helper()
	configDir := t.TempDir()
	// Pre-create the tickets dir so AddProject doesn't fail.
	if err := os.MkdirAll(filepath.Join(configDir, "tickets"), 0o755); err != nil {
		t.Fatal(err)
	}
	w, err := NewWithDebounce(configDir, testDebounce)
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, configDir
}

func collect(t *testing.T, w *Watcher, want int, timeout time.Duration) []Event {
	t.Helper()
	var got []Event
	deadline := time.After(timeout)
	for len(got) < want {
		select {
		case ev, ok := <-w.Events():
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			return got
		}
	}
	return got
}

func TestClassifyConfigJSON(t *testing.T) {
	w, dir := newTestWatcher(t)

	atomicWrite(t, filepath.Join(dir, "config.json"), []byte(`{"k":"v"}`))

	got := collect(t, w, 1, 500*time.Millisecond)
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d: %+v", len(got), got)
	}
	if got[0].Domain != DomainConfig {
		t.Errorf("want DomainConfig, got %v", got[0].Domain)
	}
	if filepath.Base(got[0].Path) != "config.json" {
		t.Errorf("unexpected path %q", got[0].Path)
	}
}

func TestClassifyProjectsJSON(t *testing.T) {
	w, dir := newTestWatcher(t)

	atomicWrite(t, filepath.Join(dir, "projects.json"), []byte(`{}`))

	got := collect(t, w, 1, 500*time.Millisecond)
	if len(got) != 1 || got[0].Domain != DomainProjects {
		t.Fatalf("want 1 DomainProjects event, got %+v", got)
	}
}

func TestClassifyTicketMD(t *testing.T) {
	w, dir := newTestWatcher(t)

	projectID := "proj-abc"
	projectDir := filepath.Join(dir, "tickets", projectID)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := w.AddProject(projectID); err != nil {
		t.Fatalf("AddProject: %v", err)
	}

	atomicWrite(t, filepath.Join(projectDir, "hello-deadbeef.md"), []byte("---\nid: x\n---\nbody"))

	got := collect(t, w, 1, 500*time.Millisecond)
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d: %+v", len(got), got)
	}
	if got[0].Domain != DomainTicket {
		t.Errorf("want DomainTicket, got %v", got[0].Domain)
	}
	if got[0].ProjectID != projectID {
		t.Errorf("ProjectID: want %q, got %q", projectID, got[0].ProjectID)
	}
}

func TestIgnoredFilesDoNotEmit(t *testing.T) {
	w, dir := newTestWatcher(t)
	projectDir := filepath.Join(dir, "tickets", "proj-x")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := w.AddProject("proj-x"); err != nil {
		t.Fatal(err)
	}

	// Write a bunch of editor swap files and tmp files that should
	// all be ignored.
	ignored := []string{
		"foo.tmp",
		"foo.swp",
		"foo~",
		".hidden.md",
		"4913",
	}
	for _, name := range ignored {
		if err := os.WriteFile(filepath.Join(projectDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := collect(t, w, 1, 2*testDebounce+50*time.Millisecond)
	if len(got) != 0 {
		t.Errorf("want 0 events for ignored files, got %+v", got)
	}
}

func TestDebouncesRapidWrites(t *testing.T) {
	w, dir := newTestWatcher(t)
	path := filepath.Join(dir, "config.json")

	for i := 0; i < 5; i++ {
		atomicWrite(t, path, []byte(`{"i":`+string(rune('0'+i))+`}`))
	}

	got := collect(t, w, 2, 4*testDebounce+50*time.Millisecond)
	if len(got) != 1 {
		t.Errorf("want 1 coalesced event, got %d: %+v", len(got), got)
	}
}

func TestEventsClosedAfterClose(t *testing.T) {
	w, _ := newTestWatcher(t)
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Double-close is safe.
	_ = w.Close()

	select {
	case _, ok := <-w.Events():
		if ok {
			t.Error("events channel should be closed after Close")
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("events channel did not close within 200ms of Close")
	}
}

func TestAddProjectErrsOnMissingDir(t *testing.T) {
	w, _ := newTestWatcher(t)
	if err := w.AddProject("nonexistent-proj"); err == nil {
		t.Error("AddProject should error when the project dir does not exist")
	}
}

func TestRemoveProjectStopsEmittingFromIt(t *testing.T) {
	w, dir := newTestWatcher(t)
	projectID := "proj-removable"
	projectDir := filepath.Join(dir, "tickets", projectID)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := w.AddProject(projectID); err != nil {
		t.Fatal(err)
	}

	// Confirm we get an event with the project active.
	atomicWrite(t, filepath.Join(projectDir, "one-1.md"), []byte("---\nid: x\n---\n"))
	if got := collect(t, w, 1, 500*time.Millisecond); len(got) != 1 {
		t.Fatalf("baseline: want 1 event, got %d", len(got))
	}

	if err := w.RemoveProject(projectID); err != nil {
		t.Fatalf("RemoveProject: %v", err)
	}

	// Writes after removal should not emit.
	atomicWrite(t, filepath.Join(projectDir, "two-2.md"), []byte("---\nid: y\n---\n"))
	if got := collect(t, w, 1, 2*testDebounce+50*time.Millisecond); len(got) != 0 {
		t.Errorf("want 0 events after RemoveProject, got %+v", got)
	}
}
