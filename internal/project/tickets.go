package project

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/techdufus/openkanban/internal/agent"
	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
)

// ticketsDir returns the directory for ticket storage.
// Falls back to current working directory on ConfigDir error.
func ticketsDir() string {
	dir, err := config.ConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "tickets")
}

// TicketStore is the in-memory view of a single project's tickets.
//
// On disk: tickets/{ProjectID}/{slug}-{uuid8}.md, one file per ticket.
// The store is not itself serialised; only individual tickets are.
type TicketStore struct {
	ProjectID string
	Tickets   map[board.TicketID]*board.Ticket
	UpdatedAt time.Time

	// paths caches the on-disk path each ticket was loaded from (or
	// last written to). Used so SaveTicket knows where to atomically
	// rename and DeleteTicketFile knows what to os.Remove without
	// scanning the directory.
	paths map[board.TicketID]string

	repoPath string
}

func NewTicketStore(projectID, repoPath string) *TicketStore {
	return &TicketStore{
		ProjectID: projectID,
		Tickets:   make(map[board.TicketID]*board.Ticket),
		UpdatedAt: time.Now(),
		paths:     make(map[board.TicketID]string),
		repoPath:  repoPath,
	}
}

// LoadTicketStore migrates from legacy single-file storage if needed,
// then reads every .md in the project's per-ticket directory.
func LoadTicketStore(project *Project) (*TicketStore, error) {
	store := NewTicketStore(project.ID, project.RepoPath)

	if _, err := MigrateProjectToPerTicket(project.ID); err != nil {
		// Migration failure is logged but non-fatal — the user can
		// inspect the legacy .json file and the .migrating workspace
		// to recover. Return an empty store so the TUI still boots.
		log.Printf("openkanban: migration for project %s failed: %v", project.ID, err)
	}

	dir := store.ticketDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create project ticket dir: %w", err)
	}

	// Track mtime per ticket id so that a duplicate id (e.g. an
	// interrupted title-edit rename left both old and new files on
	// disk) is resolved by preferring the newer file -- and the
	// stale file is removed so the conflict doesn't recur.
	mtimes := make(map[board.TicketID]time.Time)

	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if path == dir {
				return nil
			}
			return fs.SkipDir
		}
		// Sweep stale per-writer tmp files left by crashed saves
		// (os.CreateTemp(dir, "<final>.tmp-*") names). Best-effort —
		// a remove failure is just logged.
		if strings.Contains(d.Name(), ".tmp-") || strings.HasSuffix(d.Name(), ".tmp") {
			if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
				log.Printf("openkanban: failed to sweep stale tmp %s: %v", path, rmErr)
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			log.Printf("openkanban: skip %s: stat: %v", path, ierr)
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			log.Printf("openkanban: skip %s: read: %v", path, rerr)
			return nil
		}
		ticket, perr := UnmarshalTicket(data)
		if perr != nil {
			log.Printf("openkanban: skip %s: parse: %v", path, perr)
			return nil
		}
		// Frontmatter ID is canonical identity. Filename is cosmetic.
		// If the file's project_id is empty, backfill from the project
		// being loaded (defensive against hand-edited files).
		if ticket.ProjectID == "" {
			ticket.ProjectID = project.ID
		}

		if prevPath, dup := store.paths[ticket.ID]; dup {
			if info.ModTime().After(mtimes[ticket.ID]) {
				// New file is newer. Remove the older file from disk
				// and replace the in-memory entry.
				if rmErr := os.Remove(prevPath); rmErr != nil && !os.IsNotExist(rmErr) {
					log.Printf("openkanban: failed to remove stale duplicate %s: %v", prevPath, rmErr)
				}
			} else {
				// New file is older — drop it.
				if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
					log.Printf("openkanban: failed to remove stale duplicate %s: %v", path, rmErr)
				}
				return nil
			}
		}
		store.Tickets[ticket.ID] = ticket
		store.paths[ticket.ID] = path
		mtimes[ticket.ID] = info.ModTime()
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk project ticket dir: %w", walkErr)
	}

	store.UpdatedAt = time.Now()
	return store, nil
}

// ticketDir returns the absolute path to this project's ticket directory.
func (s *TicketStore) ticketDir() string {
	return filepath.Join(ticketsDir(), s.ProjectID)
}

// SaveTicket writes a single ticket as Markdown via tmp+rename. It
// does not touch any other ticket's file — the load-bearing property
// behind hot-reload safety.
//
// If the ticket's filename has changed since the cached path (e.g.
// title was edited), the old file is removed after the new one is
// committed. Identity is always the frontmatter id.
func (s *TicketStore) SaveTicket(t *board.Ticket) error {
	if t == nil {
		return errors.New("SaveTicket: nil ticket")
	}
	if t.ProjectID == "" {
		t.ProjectID = s.ProjectID
	}

	dir := s.ticketDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("ensure project dir: %w", err)
	}

	data, err := MarshalTicket(t)
	if err != nil {
		return fmt.Errorf("marshal ticket: %w", err)
	}

	newPath := filepath.Join(dir, TicketFilename(t))

	// os.CreateTemp gives each writer a unique tmp name
	// (<final>.tmp-<rand>) so concurrent writers — including
	// other openkanban processes subscribed to the same daemon —
	// don't race on a shared "<final>.tmp" path and have one
	// rename consume the tmp file from under another.
	tmp, err := os.CreateTemp(dir, TicketFilename(t)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmpPath, newPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename tmp to dest: %w", err)
	}

	// If the previous cached path was different (title-driven rename),
	// remove the old file. The frontmatter id in both files would be
	// identical, but only the new path should remain.
	if oldPath, hadOld := s.paths[t.ID]; hadOld && oldPath != newPath {
		if rmErr := os.Remove(oldPath); rmErr != nil && !os.IsNotExist(rmErr) {
			log.Printf("openkanban: failed to remove stale path %s after rename: %v", oldPath, rmErr)
		}
	}
	s.paths[t.ID] = newPath
	s.Tickets[t.ID] = t
	s.UpdatedAt = time.Now()
	return nil
}

// DeleteTicketFile removes the on-disk file for a ticket without
// touching peers. Returns nil if the ticket was never persisted.
func (s *TicketStore) DeleteTicketFile(id board.TicketID) error {
	path, ok := s.paths[id]
	if !ok {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove ticket file: %w", err)
	}
	delete(s.paths, id)
	return nil
}

// SaveAll fans out across the in-memory ticket map, writing each via
// SaveTicket. Returns the first error encountered (does not roll back
// successful writes).
func (s *TicketStore) SaveAll() error {
	for _, ticket := range s.Tickets {
		if err := s.SaveTicket(ticket); err != nil {
			return err
		}
	}
	return nil
}

func (s *TicketStore) Add(ticket *board.Ticket) {
	ticket.ProjectID = s.ProjectID
	s.Tickets[ticket.ID] = ticket
}

func (s *TicketStore) Get(id board.TicketID) (*board.Ticket, error) {
	t, ok := s.Tickets[id]
	if !ok {
		return nil, board.ErrTicketNotFound
	}
	return t, nil
}

// Delete removes a ticket from the in-memory map AND from disk.
// Today's callers (GlobalTicketStore.Delete) rely on the file being
// removed without an explicit Save call, since per-ticket storage
// has no "rewrite the whole file without this ticket" pathway.
func (s *TicketStore) Delete(id board.TicketID) error {
	if _, ok := s.Tickets[id]; !ok {
		return board.ErrTicketNotFound
	}
	if err := s.DeleteTicketFile(id); err != nil {
		return err
	}
	delete(s.Tickets, id)
	return nil
}

// Move flips a ticket to newStatus. When the transition crosses into
// in_review or done, any new claude-code approvals collected in the
// ticket's worktree are promoted into the source repo's
// .claude/settings.local.json so they persist across future tickets.
// Returns the slice of promoted approval entries (empty when nothing
// new went global, nil on error or non-promoting transition).
func (s *TicketStore) Move(id board.TicketID, newStatus board.TicketStatus) ([]string, error) {
	t, ok := s.Tickets[id]
	if !ok {
		return nil, board.ErrTicketNotFound
	}
	oldStatus := t.Status
	t.SetStatus(newStatus)
	added, err := agent.PromoteClaudeSettingsOnTransition(t.WorktreePath, s.repoPath, oldStatus, newStatus)
	if err != nil {
		log.Printf("openkanban: promote claude settings (%s → %s): %v", oldStatus, newStatus, err)
		return nil, nil
	}
	return added, nil
}

func (s *TicketStore) GetByStatus(status board.TicketStatus) []*board.Ticket {
	var result []*board.Ticket
	for _, t := range s.Tickets {
		if t.Status == status {
			result = append(result, t)
		}
	}
	return result
}

func (s *TicketStore) All() []*board.Ticket {
	result := make([]*board.Ticket, 0, len(s.Tickets))
	for _, t := range s.Tickets {
		result = append(result, t)
	}
	return result
}

func (s *TicketStore) Count() int {
	return len(s.Tickets)
}

func (s *TicketStore) CountByStatus(status board.TicketStatus) int {
	count := 0
	for _, t := range s.Tickets {
		if t.Status == status {
			count++
		}
	}
	return count
}

// GlobalTicketStore aggregates tickets from all projects
type GlobalTicketStore struct {
	registry     *ProjectRegistry
	projects     map[string]*Project
	ticketStores map[string]*TicketStore
	allTickets   map[board.TicketID]*board.Ticket
}

func NewGlobalTicketStore(registry *ProjectRegistry) *GlobalTicketStore {
	return &GlobalTicketStore{
		registry:     registry,
		projects:     make(map[string]*Project),
		ticketStores: make(map[string]*TicketStore),
		allTickets:   make(map[board.TicketID]*board.Ticket),
	}
}

func LoadGlobalTicketStore(registry *ProjectRegistry) (*GlobalTicketStore, error) {
	g := NewGlobalTicketStore(registry)

	for _, p := range registry.Projects {
		store, err := LoadTicketStore(p)
		if err != nil {
			continue
		}

		g.projects[p.ID] = p
		g.ticketStores[p.ID] = store

		for id, ticket := range store.Tickets {
			g.allTickets[id] = ticket
		}
	}

	return g, nil
}

func (g *GlobalTicketStore) GetProject(id string) *Project {
	return g.projects[id]
}

func (g *GlobalTicketStore) GetProjectForTicket(ticket *board.Ticket) *Project {
	return g.projects[ticket.ProjectID]
}

func (g *GlobalTicketStore) GetStoreForTicket(ticket *board.Ticket) *TicketStore {
	return g.ticketStores[ticket.ProjectID]
}

func (g *GlobalTicketStore) Get(id board.TicketID) (*board.Ticket, error) {
	t, ok := g.allTickets[id]
	if !ok {
		return nil, board.ErrTicketNotFound
	}
	return t, nil
}

func (g *GlobalTicketStore) Add(ticket *board.Ticket) error {
	store := g.ticketStores[ticket.ProjectID]
	if store == nil {
		return board.ErrTicketNotFound
	}
	store.Add(ticket)
	g.allTickets[ticket.ID] = ticket
	return nil
}

func (g *GlobalTicketStore) Delete(id board.TicketID) error {
	ticket, ok := g.allTickets[id]
	if !ok {
		return board.ErrTicketNotFound
	}

	store := g.ticketStores[ticket.ProjectID]
	if store != nil {
		if err := store.Delete(id); err != nil {
			return err
		}
	}
	delete(g.allTickets, id)
	return nil
}

func (g *GlobalTicketStore) Move(id board.TicketID, newStatus board.TicketStatus) ([]string, error) {
	ticket, ok := g.allTickets[id]
	if !ok {
		return nil, board.ErrTicketNotFound
	}

	store := g.ticketStores[ticket.ProjectID]
	if store == nil {
		return nil, nil
	}
	return store.Move(id, newStatus)
}

// MoveProject reassigns a ticket from its current project to a new
// one. Atomically removes the source .md file and writes a fresh .md
// in the destination project's directory. Refuses if the ticket has
// an active worktree -- the worktree is bound to the source repo;
// moving would orphan it.
func (g *GlobalTicketStore) MoveProject(id board.TicketID, newProjectID string) error {
	ticket, ok := g.allTickets[id]
	if !ok {
		return board.ErrTicketNotFound
	}
	if ticket.ProjectID == newProjectID {
		return nil
	}

	srcStore := g.ticketStores[ticket.ProjectID]
	dstStore := g.ticketStores[newProjectID]
	if dstStore == nil {
		return ErrProjectNotFound
	}
	if ticket.WorktreePath != "" {
		return ErrTicketHasWorktree
	}

	// Remove from the source store. Delete handles both
	// `delete(srcStore.Tickets, id)` AND removing the on-disk .md
	// file in the source project's directory.
	if srcStore != nil {
		if err := srcStore.Delete(id); err != nil {
			return err
		}
	}

	// Reassign and persist. SaveTicket writes the new .md in the
	// destination project's directory and updates dstStore.Tickets
	// and dstStore.paths.
	ticket.ProjectID = newProjectID
	ticket.Touch()
	return dstStore.SaveTicket(ticket)
}

// Save persists a single ticket. Atomic; touches only that ticket's file.
func (g *GlobalTicketStore) Save(ticket *board.Ticket) error {
	store := g.ticketStores[ticket.ProjectID]
	if store == nil {
		return board.ErrTicketNotFound
	}
	return store.SaveTicket(ticket)
}

func (g *GlobalTicketStore) SaveAll() error {
	for _, store := range g.ticketStores {
		if err := store.SaveAll(); err != nil {
			return err
		}
	}
	return nil
}

func (g *GlobalTicketStore) GetByStatus(status board.TicketStatus) []*board.Ticket {
	var result []*board.Ticket
	for _, t := range g.allTickets {
		if t.Status == status {
			result = append(result, t)
		}
	}
	return result
}

func (g *GlobalTicketStore) All() []*board.Ticket {
	result := make([]*board.Ticket, 0, len(g.allTickets))
	for _, t := range g.allTickets {
		result = append(result, t)
	}
	return result
}

func (g *GlobalTicketStore) Count() int {
	return len(g.allTickets)
}

func (g *GlobalTicketStore) Projects() []*Project {
	result := make([]*Project, 0, len(g.projects))
	for _, p := range g.projects {
		result = append(result, p)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

func (g *GlobalTicketStore) HasProjects() bool {
	return len(g.projects) > 0
}

func (g *GlobalTicketStore) AddProject(p *Project) {
	g.projects[p.ID] = p
	g.ticketStores[p.ID] = NewTicketStore(p.ID, p.RepoPath)
}

// RemoveProject archives the project's whole ticket directory by
// renaming tickets/{id}/ to tickets/archived/{id}_{ts}/, then drops
// in-memory references.
func (g *GlobalTicketStore) RemoveProject(id string) error {
	if _, ok := g.projects[id]; !ok {
		return ErrProjectNotFound
	}

	srcDir := filepath.Join(ticketsDir(), id)
	if _, err := os.Stat(srcDir); err == nil {
		archivedRoot := filepath.Join(ticketsDir(), "archived")
		if err := os.MkdirAll(archivedRoot, 0o755); err != nil {
			return err
		}
		dstDir := filepath.Join(archivedRoot, fmt.Sprintf("%s_%d", id, time.Now().Unix()))
		if err := os.Rename(srcDir, dstDir); err != nil {
			return err
		}
		log.Printf("openkanban: archived project %s tickets to %s", id, dstDir)
	}

	delete(g.projects, id)
	delete(g.ticketStores, id)

	return g.registry.Delete(id)
}

func (g *GlobalTicketStore) GetBlockedBy(ticketID board.TicketID) []*board.Ticket {
	ticket, ok := g.allTickets[ticketID]
	if !ok || len(ticket.BlockedBy) == 0 {
		return nil
	}

	var blockers []*board.Ticket
	for _, blockerID := range ticket.BlockedBy {
		if blocker, exists := g.allTickets[blockerID]; exists {
			blockers = append(blockers, blocker)
		}
	}
	return blockers
}

func (g *GlobalTicketStore) GetBlocks(ticketID board.TicketID) []*board.Ticket {
	var blocks []*board.Ticket
	for _, ticket := range g.allTickets {
		for _, blockerID := range ticket.BlockedBy {
			if blockerID == ticketID {
				blocks = append(blocks, ticket)
				break
			}
		}
	}
	return blocks
}

// PathOf returns the on-disk path of a ticket if known. Returns
// false if the ticket has never been saved or was loaded into the
// store from an unknown source.
func (g *GlobalTicketStore) PathOf(id board.TicketID) (string, bool) {
	ticket, ok := g.allTickets[id]
	if !ok {
		return "", false
	}
	store := g.ticketStores[ticket.ProjectID]
	if store == nil {
		return "", false
	}
	p, ok := store.paths[id]
	return p, ok
}

// ReloadTicket reads (or, if the file is gone, drops) a single ticket
// file. Identity is taken from the file's frontmatter id, not from
// the path — so a renamed file (e.g. title edit while the TUI is open)
// reloads as the same ticket and updates the in-memory paths cache.
//
// Safe to call from a Bubble Tea Update goroutine. Not safe to call
// concurrently with itself or with Save methods.
func (g *GlobalTicketStore) ReloadTicket(projectID, path string) error {
	store := g.ticketStores[projectID]
	if store == nil {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// File was deleted externally. Find which ticket was at
			// this path and drop it from in-memory state.
			for id, p := range store.paths {
				if p == path {
					delete(store.Tickets, id)
					delete(store.paths, id)
					delete(g.allTickets, id)
					return nil
				}
			}
			return nil
		}
		return fmt.Errorf("read ticket %s: %w", path, err)
	}

	ticket, err := UnmarshalTicket(data)
	if err != nil {
		return fmt.Errorf("parse ticket %s: %w", path, err)
	}
	if ticket.ProjectID == "" {
		ticket.ProjectID = projectID
	}

	// If a different filename previously held this id (title edit),
	// clean up the stale paths-map entry so we don't leak the old key.
	if oldPath, hadOld := store.paths[ticket.ID]; hadOld && oldPath != path {
		// We don't os.Remove here — the old file may still exist on
		// disk; the file-system event for its removal will come
		// separately and trigger the drop branch above.
		_ = oldPath
	}

	g.allTickets[ticket.ID] = ticket
	store.Tickets[ticket.ID] = ticket
	store.paths[ticket.ID] = path
	return nil
}

func (g *GlobalTicketStore) RemoveBlockerReferences(ticketID board.TicketID) {
	for _, ticket := range g.allTickets {
		if len(ticket.BlockedBy) == 0 {
			continue
		}
		var filtered []board.TicketID
		for _, blockerID := range ticket.BlockedBy {
			if blockerID != ticketID {
				filtered = append(filtered, blockerID)
			}
		}
		if len(filtered) != len(ticket.BlockedBy) {
			ticket.BlockedBy = filtered
		}
	}
}
