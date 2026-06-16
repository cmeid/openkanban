package ui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

// makeBoardResyncTestModel wires a Model with a real on-disk config
// dir under t.TempDir(), a persisted ProjectRegistry containing the
// supplied projects, and a GlobalTicketStore loaded from disk. The
// returned model is sufficient to call handleBoardResyncTick /
// handleBoardResyncMsg AND refreshColumnTickets, because the latter
// needs columns + columnTickets allocated.
//
// The width/height match the convention pinned by other UI tests so
// any future renderColumn-style assertion would Just Work.
func makeBoardResyncTestModel(t *testing.T, projects []*project.Project) (*Model, *project.ProjectRegistry) {
	t.Helper()
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	cfgDir, err := config.ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir cfg: %v", err)
	}

	registry, err := project.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	for _, p := range projects {
		// Use the registry's in-memory map directly (skipping Add's
		// duplicate-path scan) because each test project gets a fresh
		// t.TempDir() RepoPath so the path-uniqueness check is moot,
		// and Add eagerly Save()s — we want one persist at the end.
		registry.Projects[p.ID] = p
		// Pre-create the per-project tickets directory so the resync's
		// ReadDir doesn't have to tolerate it being absent for the
		// happy-path tests.
		if err := os.MkdirAll(filepath.Join(cfgDir, "tickets", p.ID), 0o755); err != nil {
			t.Fatalf("mkdir tickets/%s: %v", p.ID, err)
		}
	}
	if err := registry.Save(); err != nil {
		t.Fatalf("registry.Save: %v", err)
	}

	store, err := project.LoadGlobalTicketStore(registry)
	if err != nil {
		t.Fatalf("LoadGlobalTicketStore: %v", err)
	}

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	cols := board.DefaultColumns()
	m := &Model{
		globalStore:     store,
		projectRegistry: registry,
		columns:         cols,
		columnTickets:   make([][]*board.Ticket, len(cols)),
		columnOffsets:   make([]int, len(cols)),
		panes:           map[board.TicketID]*daemonclient.PaneView{},
		daemonOwned:     map[board.TicketID]struct{}{},
		daemonViewing:   map[board.TicketID]int{},
		lastPTYActivity: map[board.TicketID]time.Time{},
		spinner:         sp,
		width:           120,
		height:          40,
		config:          &config.Config{Agents: map[string]config.AgentConfig{}},
		colors:          newUIColors(config.DefaultConfig().GetTheme()),
		filterProjectIDs: map[string]bool{},
	}
	return m, registry
}

// writeTicketOnDisk persists a ticket to its expected on-disk path
// WITHOUT going through globalStore.Save — that's the point: these
// tests simulate a SIBLING TUI mutating the file directly.
func writeTicketOnDisk(t *testing.T, ticket *board.Ticket) string {
	t.Helper()
	cfgDir, err := config.ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	data, err := project.MarshalTicket(ticket)
	if err != nil {
		t.Fatalf("MarshalTicket: %v", err)
	}
	dir := filepath.Join(cfgDir, "tickets", ticket.ProjectID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, string(ticket.ID)+".md")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestBoardResync_PicksUpExternalStatusChange — TUI A's in-memory
// ticket is StatusBacklog; a sibling rewrites the .md to
// StatusInProgress. After one resync tick, TUI A's in-memory ticket
// reflects the new status.
func TestBoardResync_PicksUpExternalStatusChange(t *testing.T) {
	proj := &project.Project{ID: "p1", RepoPath: t.TempDir()}
	m, _ := makeBoardResyncTestModel(t, []*project.Project{proj})

	// Seed: write ticket as StatusBacklog and load it into the store.
	tid := board.NewTicketID()
	original := &board.Ticket{
		ID:        tid,
		Title:     "demo",
		ProjectID: proj.ID,
		Status:    board.StatusBacklog,
	}
	path := writeTicketOnDisk(t, original)
	if err := m.globalStore.ReloadTicket(proj.ID, path); err != nil {
		t.Fatalf("initial ReloadTicket: %v", err)
	}

	// Externally rewrite the file as StatusInProgress with a different
	// mtime so the sentinel detects the change.
	mutated := *original
	mutated.Status = board.StatusInProgress
	mutated.UpdatedAt = time.Now()
	data, err := project.MarshalTicket(&mutated)
	if err != nil {
		t.Fatalf("MarshalTicket: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("rewrite ticket: %v", err)
	}
	// Bump mtime explicitly — WriteFile already sets it but on fast
	// systems it may be identical to the prior tick's timestamp.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Prime the snap with the OLD mtime/size so the diff sees a change.
	prevInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat post-rewrite: %v", err)
	}
	m.boardResyncSnap = map[string]boardFileMeta{
		path: {
			mtime:     prevInfo.ModTime().Add(-1 * time.Hour), // stale
			size:      prevInfo.Size() - 1,
			projectID: proj.ID,
		},
	}

	msg := boardResyncMsg{
		snap: map[string]boardFileMeta{
			path: {
				mtime:     prevInfo.ModTime(),
				size:      prevInfo.Size(),
				projectID: proj.ID,
			},
		},
	}

	_, cmd := m.handleBoardResyncMsg(msg)
	if cmd == nil {
		t.Fatal("handler returned nil cmd; expected re-arm")
	}

	got, err := m.globalStore.Get(tid)
	if err != nil {
		t.Fatalf("Get reloaded ticket: %v", err)
	}
	if got.Status != board.StatusInProgress {
		t.Errorf("Status = %q, want %q", got.Status, board.StatusInProgress)
	}
}

// TestBoardResync_PicksUpExternalDelete — a ticket disappears from
// disk while the model has it loaded. After the resync handler runs,
// the in-memory store no longer has the ticket.
func TestBoardResync_PicksUpExternalDelete(t *testing.T) {
	proj := &project.Project{ID: "p1", RepoPath: t.TempDir()}
	m, _ := makeBoardResyncTestModel(t, []*project.Project{proj})

	tid := board.NewTicketID()
	ticket := &board.Ticket{
		ID:        tid,
		Title:     "to-be-deleted",
		ProjectID: proj.ID,
		Status:    board.StatusInProgress,
	}
	path := writeTicketOnDisk(t, ticket)
	if err := m.globalStore.ReloadTicket(proj.ID, path); err != nil {
		t.Fatalf("initial ReloadTicket: %v", err)
	}
	if _, err := m.globalStore.Get(tid); err != nil {
		t.Fatalf("seeded ticket missing: %v", err)
	}

	// Prime the prior-snap so the handler sees the path go from
	// present to absent.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	m.boardResyncSnap = map[string]boardFileMeta{
		path: {mtime: info.ModTime(), size: info.Size(), projectID: proj.ID},
	}

	// Sibling deletes the file.
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Resync now sees an empty snap.
	_, cmd := m.handleBoardResyncMsg(boardResyncMsg{snap: map[string]boardFileMeta{}})
	if cmd == nil {
		t.Fatal("handler returned nil cmd")
	}

	if _, err := m.globalStore.Get(tid); err == nil {
		t.Errorf("ticket %s still present after delete-resync", tid)
	}
}

// TestBoardResync_DiscoversNewProject — sibling adds a new project to
// projects.json. The resync msg lists the project ID in
// newProjectIDs; the handler installs it and loads its tickets.
func TestBoardResync_DiscoversNewProject(t *testing.T) {
	known := &project.Project{ID: "known", RepoPath: t.TempDir()}
	m, registry := makeBoardResyncTestModel(t, []*project.Project{known})

	// Sibling adds a new project, persisted to projects.json.
	newProj := &project.Project{ID: "new", RepoPath: t.TempDir()}
	registry.Projects[newProj.ID] = newProj
	if err := registry.Save(); err != nil {
		t.Fatalf("save registry: %v", err)
	}

	// Seed a ticket in the new project's directory.
	tid := board.NewTicketID()
	ticket := &board.Ticket{
		ID:        tid,
		Title:     "from-new-project",
		ProjectID: newProj.ID,
		Status:    board.StatusBacklog,
	}
	cfgDir, _ := config.ConfigDir()
	if err := os.MkdirAll(filepath.Join(cfgDir, "tickets", newProj.ID), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := writeTicketOnDisk(t, ticket)

	// The handler reloads the registry itself, so the model's
	// projectRegistry only needs to have been wired (not pre-reloaded).

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	msg := boardResyncMsg{
		projectIDs: []string{known.ID, newProj.ID},
		snap: map[string]boardFileMeta{
			path: {mtime: info.ModTime(), size: info.Size(), projectID: newProj.ID},
		},
	}

	_, _ = m.handleBoardResyncMsg(msg)

	if !m.globalStore.HasProject(newProj.ID) {
		t.Errorf("globalStore missing new project after resync")
	}
	if _, err := m.globalStore.Get(tid); err != nil {
		t.Errorf("ticket from new project missing: %v", err)
	}
}

// TestBoardResync_RespectsFilter — the brief's "even if they are
// filtered" requirement: a sibling-added ticket in a filtered-out
// project lands in the store BUT does not appear in m.columnTickets.
func TestBoardResync_RespectsFilter(t *testing.T) {
	projA := &project.Project{ID: "A", RepoPath: t.TempDir()}
	projB := &project.Project{ID: "B", RepoPath: t.TempDir()}
	m, _ := makeBoardResyncTestModel(t, []*project.Project{projA, projB})

	// Filter to project A only.
	m.filterProjectIDs = map[string]bool{projA.ID: true}

	// Sibling adds a ticket in project B (filtered out).
	tid := board.NewTicketID()
	ticket := &board.Ticket{
		ID:        tid,
		Title:     "should-be-hidden",
		ProjectID: projB.ID,
		Status:    board.StatusInProgress,
	}
	path := writeTicketOnDisk(t, ticket)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	msg := boardResyncMsg{
		snap: map[string]boardFileMeta{
			path: {mtime: info.ModTime(), size: info.Size(), projectID: projB.ID},
		},
	}
	_, _ = m.handleBoardResyncMsg(msg)

	// In store: yes.
	if _, err := m.globalStore.Get(tid); err != nil {
		t.Errorf("ticket missing from store: %v", err)
	}

	// In columnTickets: no.
	for ci, col := range m.columnTickets {
		for _, ct := range col {
			if ct.ID == tid {
				t.Errorf("filtered-out ticket %s appeared in column %d (%s)", tid, ci, m.columns[ci].Name)
			}
		}
	}
}

// TestBoardResync_ReArmsOnError — the handler must return a non-nil
// re-arm cmd even when the msg carries an error, so the periodic chain
// doesn't break on a single failed tick.
func TestBoardResync_ReArmsOnError(t *testing.T) {
	proj := &project.Project{ID: "p1", RepoPath: t.TempDir()}
	m, _ := makeBoardResyncTestModel(t, []*project.Project{proj})

	_, cmd := m.handleBoardResyncMsg(boardResyncMsg{err: os.ErrInvalid})
	if cmd == nil {
		t.Errorf("handler returned nil cmd on error msg; expected re-arm")
	}
}

// TestBoardResyncTick_ScansDiskAndReturnsSnap — end-to-end check of
// the goroutine-bound scan: place a real .md file under
// tickets/<projectID>/, fire the tick, and assert the resulting
// boardResyncMsg's snap contains the path.
func TestBoardResyncTick_ScansDiskAndReturnsSnap(t *testing.T) {
	proj := &project.Project{ID: "p1", RepoPath: t.TempDir()}
	m, _ := makeBoardResyncTestModel(t, []*project.Project{proj})

	ticket := &board.Ticket{
		ID:        board.NewTicketID(),
		Title:     "scan-target",
		ProjectID: proj.ID,
		Status:    board.StatusBacklog,
	}
	expectedPath := writeTicketOnDisk(t, ticket)

	_, cmd := m.handleBoardResyncTick()
	if cmd == nil {
		t.Fatal("tick handler returned nil cmd")
	}

	msg, ok := cmd().(boardResyncMsg)
	if !ok {
		t.Fatalf("tick cmd returned %T, want boardResyncMsg", cmd())
	}
	if msg.err != nil {
		t.Fatalf("scan error: %v", msg.err)
	}
	if _, found := msg.snap[expectedPath]; !found {
		t.Errorf("snap missing %s; got keys: %v", expectedPath, snapKeys(msg.snap))
	}
}

func snapKeys(m map[string]boardFileMeta) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
