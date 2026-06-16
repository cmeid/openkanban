package ui

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/project"
)

// boardResyncInterval is the cadence at which the TUI walks the
// on-disk ticket directories and reconciles against in-memory state.
// 1 second is cheap: a tick stat()s every known ticket file (~tens
// per project on a typical install), compares mtime+size against
// boardResyncSnap, and only re-reads the files whose sentinel moved.
// Stats on the kernel's cached inode tree are sub-microsecond on
// macOS APFS / Linux ext4 — total tick cost is well under a
// millisecond at expected ticket counts.
//
// The resync is the safety net behind the fsnotify watcher
// (internal/watch). The watcher remains the sub-second fast path for
// the happy case; this tick exists to cover the two known gaps:
//
//  1. Projects added after startup are not watched (see
//     internal/app/app.go startFileWatcher comment).
//  2. fsnotify on kqueue/macOS can drop events under burst load or on
//     certain atomic-rename patterns. Same reliability concern
//     daemon_resync.go calls out for its own push-event channel.
const boardResyncInterval = 1 * time.Second

// boardResyncTickMsg fires when the periodic timer expires. The
// Update handler turns it into a goroutine-bound disk scan via a
// tea.Cmd so the I/O doesn't block the Update goroutine.
type boardResyncTickMsg struct{}

// boardResyncMsg carries one tick's snapshot of the on-disk state
// back to the Update loop. The handler diffs snap against
// m.boardResyncSnap to decide which paths to reload (new or
// mtime/size changed) and which in-memory tickets to drop (path was
// previously seen and is now gone). projectIDs is the full set of
// project IDs the goroutine found in projects.json; the handler
// reloads m.projectRegistry and diffs against m.globalStore to
// identify which of those are newly added.
type boardResyncMsg struct {
	snap       map[string]boardFileMeta
	projectIDs []string
	err        error
}

// boardFileMeta is the per-path sentinel the resync uses to detect
// changes. Mirrors selfWriteRecord's mtime+size pairing
// (reload.go:80-82) — false negatives only occur on identical mtime
// AND identical size in a sub-second window, which is theoretical at
// APFS/ext4 timestamp granularity.
type boardFileMeta struct {
	mtime     time.Time
	size      int64
	projectID string
}

// scheduleBoardResync returns a tea.Cmd that fires a
// boardResyncTickMsg after boardResyncInterval. Used both by Init (to
// arm the first tick) and by the resync handler (to re-arm after each
// completed scan). Mirrors scheduleDaemonResync's nil-safety
// convention so callers can batch unconditionally.
func (m *Model) scheduleBoardResync() tea.Cmd {
	return tea.Tick(boardResyncInterval, func(time.Time) tea.Msg {
		return boardResyncTickMsg{}
	})
}

// handleBoardResyncTick fires when the periodic timer expires. It
// returns a tea.Cmd that walks the config dir + per-project ticket
// dirs in a goroutine and emits a boardResyncMsg with the result.
//
// IMPORTANT: the goroutine only does read-only filesystem work and
// touches NO model state (no m.projectRegistry, no m.globalStore
// access — both are mutated by the Update goroutine and reading them
// here would race). It calls project.LoadRegistry to get a fresh,
// local snapshot of projects.json that no other goroutine sees. The
// returned msg carries the project IDs and per-path metadata back to
// the Update handler, which is the only thing allowed to mutate
// m.projectRegistry / m.globalStore.
//
// Re-arming happens in handleBoardResyncMsg, NOT here — same rule as
// daemon_resync.go: re-arming before the scan completes would race a
// slow scan into a runaway loop if a tick fires while the prior tick
// is still in flight.
func (m *Model) handleBoardResyncTick() (tea.Model, tea.Cmd) {
	return m, func() tea.Msg {
		freshRegistry, err := project.LoadRegistry()
		if err != nil {
			return boardResyncMsg{err: err}
		}

		cfgDir, err := config.ConfigDir()
		if err != nil {
			return boardResyncMsg{err: err}
		}

		snap := make(map[string]boardFileMeta)
		projectIDs := make([]string, 0, len(freshRegistry.Projects))

		for _, p := range freshRegistry.Projects {
			projectIDs = append(projectIDs, p.ID)
			dir := filepath.Join(cfgDir, "tickets", p.ID)
			entries, err := os.ReadDir(dir)
			if err != nil {
				// Project directory missing or unreadable is fine: a
				// newly-added project hasn't written any tickets yet, or
				// the user removed the dir manually. The next tick picks
				// up whatever's there. Don't fail the whole scan.
				continue
			}
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				name := e.Name()
				if !strings.HasSuffix(name, ".md") {
					continue
				}
				if strings.HasPrefix(name, ".") {
					continue
				}
				path := filepath.Join(dir, name)
				info, err := e.Info()
				if err != nil {
					continue
				}
				snap[path] = boardFileMeta{
					mtime:     info.ModTime(),
					size:      info.Size(),
					projectID: p.ID,
				}
			}
		}

		return boardResyncMsg{snap: snap, projectIDs: projectIDs}
	}
}

// handleBoardResyncMsg applies one tick's snapshot. Diff semantics:
//
//   - Reload m.projectRegistry from disk (safe here — Update is
//     serialized by the tea framework). For each project ID the
//     goroutine reported that m.globalStore doesn't yet track, install
//     it via globalStore.AddProjectAndLoad.
//   - For each path in snap whose (mtime, size) differs from
//     m.boardResyncSnap (or is absent there): ReloadTicket. The
//     reload is idempotent for identical content, so an extra call
//     for an in-flight self-write costs at most one re-parse.
//   - For each path in m.boardResyncSnap that is NOT in snap: call
//     ReloadTicket with the prior projectID. ReloadTicket's
//     os.IsNotExist branch drops the in-memory ticket. (We pass the
//     deletion path through the same routine so the in-memory cleanup
//     happens by id-from-paths-map lookup, not by hand.)
//
// Re-arms the next tick unconditionally. Errors are logged but never
// surfaced as a notification — periodic-failure noise would drown out
// real signal, and the fsnotify watcher's separate watch-errors.log
// covers the persistent-failure case.
func (m *Model) handleBoardResyncMsg(msg boardResyncMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		log.Printf("openkanban: periodic board resync failed: %v", msg.err)
		return m, m.scheduleBoardResync()
	}

	changed := false

	// Reload the registry in the Update goroutine where it's safe to
	// mutate, then install any newly-discovered projects.
	if err := m.projectRegistry.ReloadFromDisk(); err != nil {
		log.Printf("openkanban: resync registry reload: %v", err)
		// Don't return early — the per-path diff below still works
		// against the pre-existing registry view.
	}
	for _, pid := range msg.projectIDs {
		if m.globalStore.HasProject(pid) {
			continue
		}
		p, err := m.projectRegistry.Get(pid)
		if err != nil || p == nil {
			continue
		}
		if err := m.globalStore.AddProjectAndLoad(p); err != nil {
			log.Printf("openkanban: load new project %s: %v", pid, err)
			// Project still installed (AddProjectAndLoad keeps it
			// registered on load error); continue applying the rest.
		}
		changed = true
	}

	for path, meta := range msg.snap {
		if prev, had := m.boardResyncSnap[path]; had &&
			prev.mtime.Equal(meta.mtime) && prev.size == meta.size {
			continue
		}
		if err := m.globalStore.ReloadTicket(meta.projectID, path); err != nil {
			log.Printf("openkanban: resync reload %s: %v", path, err)
			continue
		}
		changed = true
	}

	for path, prev := range m.boardResyncSnap {
		if _, stillThere := msg.snap[path]; stillThere {
			continue
		}
		if err := m.globalStore.ReloadTicket(prev.projectID, path); err != nil {
			log.Printf("openkanban: resync drop %s: %v", path, err)
			continue
		}
		changed = true
	}

	m.boardResyncSnap = msg.snap

	if changed {
		m.refreshColumnTickets()
	}

	return m, m.scheduleBoardResync()
}
