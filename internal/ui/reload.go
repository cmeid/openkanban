package ui

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/watch"
)

// FsChangedMsg is a Bubble Tea message emitted (via program.Send) by
// the goroutine in app.go that pumps watch.Watcher events. It carries
// enough info for the model to route the reload to the right
// in-memory state.
type FsChangedMsg struct {
	Domain    watch.Domain
	Path      string
	ProjectID string
}

// selfWriteRecord tracks a file the TUI itself just wrote, so that
// the watcher event for that same write doesn't trigger a redundant
// reload (and worse, a transient stat-the-disk during an in-flight
// rename).
type selfWriteRecord struct {
	mtime time.Time
	size  int64
	until time.Time
}

// markSelfWrite records the on-disk mtime + size of a file we just
// wrote, so we can suppress the inbound watcher event for it.
//
// Granularity: APFS mtimes have sub-second resolution; collisions
// within the same TTL window are extremely rare. If a real external
// write hits with identical mtime+size inside the TTL, we miss it —
// the user can edit again.
func (m *Model) markSelfWrite(path string) {
	if path == "" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if m.recentSelfWrites == nil {
		m.recentSelfWrites = make(map[string]selfWriteRecord)
	}
	m.recentSelfWrites[path] = selfWriteRecord{
		mtime: info.ModTime(),
		size:  info.Size(),
		until: time.Now().Add(5 * time.Second),
	}
}

// isSelfWrite returns true if the watcher event for path should be
// suppressed because the TUI itself just wrote it.
func (m *Model) isSelfWrite(path string) bool {
	if m.recentSelfWrites == nil {
		return false
	}
	rec, ok := m.recentSelfWrites[path]
	if !ok {
		return false
	}
	if time.Now().After(rec.until) {
		delete(m.recentSelfWrites, path)
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		// File gone since we wrote it — that's not a self-write; let
		// the reload path handle the drop.
		return false
	}
	if info.ModTime().Equal(rec.mtime) && info.Size() == rec.size {
		return true
	}
	return false
}

// handleFsChanged routes a watcher event to the right in-place
// reload. The model's columnTickets are rebuilt at the end so any
// status change is reflected immediately.
func (m *Model) handleFsChanged(msg FsChangedMsg) {
	switch msg.Domain {
	case watch.DomainConfig:
		if err := m.config.ReloadFromDisk(); err != nil {
			log.Printf("openkanban: reload config: %v", err)
			return
		}
		m.theme = m.config.GetTheme()
		m.colors = newUIColors(m.theme)

	case watch.DomainProjects:
		if err := m.projectRegistry.ReloadFromDisk(); err != nil {
			log.Printf("openkanban: reload projects: %v", err)
			return
		}

	case watch.DomainTicket:
		if m.isSelfWrite(msg.Path) {
			return
		}
		if err := m.globalStore.ReloadTicket(msg.ProjectID, msg.Path); err != nil {
			// Surface the failure to the user (notification is
			// transient; log file is persistent for post-mortem).
			short := filepath.Base(msg.Path)
			m.notify(fmt.Sprintf("Reload failed: %s — %s", short, err.Error()))
			appendWatchErrorLog(msg.Path, err)
			log.Printf("openkanban: reload ticket %s: %v", msg.Path, err)
			return
		}
		// Keep the cursor on the selected ticket by ID across the
		// re-sort an external reload triggers (mirrors the board-resync
		// path). Follows it across columns if its status changed;
		// clamp-degrades if it was deleted. No filter relaxation.
		sel := m.selectedTicket()
		m.refreshColumnTickets()
		if sel != nil {
			m.selectTicketByID(sel.ID)
			m.ensureColumnVisible()
		}
	}
}

// appendWatchErrorLog writes a single line to
// ~/.config/openkanban/watch-errors.log for post-mortem inspection
// of reload failures (typically malformed YAML in a hand-edited
// .md). Best-effort: silent if the log can't be written.
func appendWatchErrorLog(path string, reloadErr error) {
	cfgDir, err := config.ConfigDir()
	if err != nil {
		return
	}
	logPath := filepath.Join(cfgDir, "watch-errors.log")
	config.GuardHomeWrite(logPath)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\t%s\t%v\n", time.Now().Format(time.RFC3339), path, reloadErr)
}

// recordSavedTicket should be called after globalStore.Save(ticket)
// to mark the on-disk file as a self-write. Looked up via the store's
// path cache.
func (m *Model) recordSavedTicket(ticket *board.Ticket) {
	if ticket == nil {
		return
	}
	if path, ok := m.globalStore.PathOf(ticket.ID); ok {
		m.markSelfWrite(path)
	}
}
