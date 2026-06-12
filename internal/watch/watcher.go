// Package watch observes the openkanban config directory and emits
// debounced, classified events when config.json, projects.json, or a
// per-project tickets/<project_id>/<file>.md is modified.
//
// Design notes:
//   - We watch DIRECTORIES, not individual files. On macOS, fsnotify
//     uses kqueue, which would require one fd per watched file — the
//     fsnotify maintainers explicitly recommend watching the parent
//     dir and filtering by Event.Name. We follow that recommendation.
//   - Atomic-rename writes (write-tmp + rename-onto-target, used by
//     vim/editors and openkanban itself) manifest as Create/Rename
//     events. We accept Create/Write/Rename/Remove as "the file may
//     have changed."
//   - Events for the same path within a debounce window are coalesced
//     into a single emitted Event. This is how editors with multi-write
//     save patterns (vim's 4913 + write + rename) collapse into one
//     reload signal.
package watch

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

type Domain int

const (
	DomainUnknown Domain = iota
	DomainConfig
	DomainProjects
	DomainTicket
)

func (d Domain) String() string {
	switch d {
	case DomainConfig:
		return "config"
	case DomainProjects:
		return "projects"
	case DomainTicket:
		return "ticket"
	}
	return "unknown"
}

// Event identifies a file that may have changed. ProjectID is set
// only when Domain == DomainTicket. Path is always absolute.
type Event struct {
	Domain    Domain
	Path      string
	ProjectID string
}

const defaultDebounce = 100 * time.Millisecond

type Watcher struct {
	fsw       *fsnotify.Watcher
	configDir string
	out       chan Event
	done      chan struct{}
	debounce  time.Duration
}

// New starts a Watcher rooted at configDir. It subscribes to the
// configDir itself (catching config.json + projects.json changes) but
// not to any per-project ticket subdirs — callers must AddProject(id)
// for each project they want ticket events from.
func New(configDir string) (*Watcher, error) {
	return NewWithDebounce(configDir, defaultDebounce)
}

// NewWithDebounce is like New but allows the debounce window to be
// overridden, primarily for tests.
func NewWithDebounce(configDir string, debounce time.Duration) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := fsw.Add(configDir); err != nil {
		fsw.Close()
		return nil, err
	}
	w := &Watcher{
		fsw:       fsw,
		configDir: configDir,
		out:       make(chan Event, 16),
		done:      make(chan struct{}),
		debounce:  debounce,
	}
	go w.loop()
	return w, nil
}

// AddProject subscribes the watcher to a project's tickets directory.
// Returns the underlying fsnotify error if the dir does not exist or
// cannot be watched.
func (w *Watcher) AddProject(projectID string) error {
	dir := filepath.Join(w.configDir, "tickets", projectID)
	if _, err := os.Stat(dir); err != nil {
		return err
	}
	return w.fsw.Add(dir)
}

// RemoveProject stops watching a project's tickets directory.
func (w *Watcher) RemoveProject(projectID string) error {
	dir := filepath.Join(w.configDir, "tickets", projectID)
	return w.fsw.Remove(dir)
}

// Events is the channel of debounced, classified events. The channel
// is closed when the Watcher is closed.
func (w *Watcher) Events() <-chan Event { return w.out }

// Close stops the watcher's goroutine and closes Events. Safe to call
// multiple times.
func (w *Watcher) Close() error {
	select {
	case <-w.done:
	default:
		close(w.done)
	}
	return w.fsw.Close()
}

func (w *Watcher) loop() {
	defer close(w.out)

	pending := make(map[Event]struct{})
	var (
		timer  *time.Timer
		timerC <-chan time.Time
	)

	for {
		select {
		case <-w.done:
			return
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			e, want := w.classify(ev)
			if !want {
				continue
			}
			pending[e] = struct{}{}
			if timer == nil {
				timer = time.NewTimer(w.debounce)
				timerC = timer.C
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(w.debounce)
			}
		case <-timerC:
			for e := range pending {
				select {
				case w.out <- e:
				case <-w.done:
					return
				}
				delete(pending, e)
			}
			timer = nil
			timerC = nil
		case _, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			// Errors are dropped: the watcher is best-effort. Don't
			// crash the TUI because of a transient fs event.
		}
	}
}

// classify maps a raw fsnotify event onto a logical Event. Returns
// want=false for events that should be ignored (editor swap/tmp
// files, hidden files, unrelated paths).
func (w *Watcher) classify(ev fsnotify.Event) (Event, bool) {
	base := filepath.Base(ev.Name)
	if isIgnored(base) {
		return Event{}, false
	}

	// Treat Create/Write/Rename/Remove as "file may have changed."
	// Skip Chmod-only events.
	if !ev.Op.Has(fsnotify.Write) &&
		!ev.Op.Has(fsnotify.Create) &&
		!ev.Op.Has(fsnotify.Rename) &&
		!ev.Op.Has(fsnotify.Remove) {
		return Event{}, false
	}

	parent := filepath.Dir(ev.Name)

	// configDir-level files.
	if parent == w.configDir {
		switch base {
		case "config.json":
			return Event{Domain: DomainConfig, Path: ev.Name}, true
		case "projects.json":
			return Event{Domain: DomainProjects, Path: ev.Name}, true
		}
		return Event{}, false
	}

	// tickets/<projectID>/<file>.md
	grandparent := filepath.Dir(parent)
	if grandparent == filepath.Join(w.configDir, "tickets") && strings.HasSuffix(base, ".md") {
		projectID := filepath.Base(parent)
		return Event{Domain: DomainTicket, Path: ev.Name, ProjectID: projectID}, true
	}

	return Event{}, false
}

// isIgnored filters out filenames that are noise from editors or our
// own atomic-write tmp files.
func isIgnored(name string) bool {
	if name == "" || name == "." || name == ".." {
		return true
	}
	if strings.HasSuffix(name, ".tmp") {
		return true
	}
	if strings.HasSuffix(name, ".swp") {
		return true
	}
	if strings.HasSuffix(name, "~") {
		return true
	}
	if strings.HasPrefix(name, ".") {
		return true
	}
	if name == "4913" { // vim's "can we write here?" probe
		return true
	}
	return false
}
