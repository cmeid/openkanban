package ui

import (
	"os"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/project"
	"github.com/techdufus/openkanban/internal/watch"
)

// rewriteTicketOnDisk rewrites an already-persisted ticket's .md with a
// future mtime so a board-resync diff (and an fsnotify reload) treat it
// as an external change. Returns the file's post-write os.FileInfo.
func rewriteTicketOnDisk(t *testing.T, path string, ticket *board.Ticket) os.FileInfo {
	t.Helper()
	data, err := project.MarshalTicket(ticket)
	if err != nil {
		t.Fatalf("MarshalTicket: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	return info
}

// seedColumn writes each ticket to disk and loads it into the model's
// store, then rebuilds the columns. Returns id→path for later rewrites.
func seedColumn(t *testing.T, m *Model, projID string, tickets []*board.Ticket) map[board.TicketID]string {
	t.Helper()
	paths := map[board.TicketID]string{}
	for _, tk := range tickets {
		p := writeTicketOnDisk(t, tk)
		paths[tk.ID] = p
		if err := m.globalStore.ReloadTicket(projID, p); err != nil {
			t.Fatalf("ReloadTicket %s: %v", tk.Title, err)
		}
	}
	m.refreshColumnTickets()
	return paths
}

// TestBoardResync_PreservesSelectionAcrossReorder pins the fix: a
// background resync that re-sorts a column must keep the SAME ticket
// selected (by ID), not leave the cursor pinned to a now-stale index so
// the highlight jumps to whatever ticket lands there.
//
// Setup: A(prio2) B(prio3) C(prio4) in in_progress under SortPriority →
// order A,B,C. Select B at index 1 (non-zero, so a stale-index pass
// can't coincidentally land on it). An external edit bumps C to the top
// priority; after the resync re-sort the order is C,A,B and B is at
// index 2. Focus must still be on B, not on A (which now occupies the
// old index 1).
func TestBoardResync_PreservesSelectionAcrossReorder(t *testing.T) {
	proj := &project.Project{ID: "p1", RepoPath: t.TempDir()}
	m, _ := makeBoardResyncTestModel(t, []*project.Project{proj})
	m.sortMode = SortPriority

	a := &board.Ticket{ID: board.NewTicketID(), Title: "A", ProjectID: proj.ID, Status: board.StatusInProgress, Priority: 2}
	b := &board.Ticket{ID: board.NewTicketID(), Title: "B", ProjectID: proj.ID, Status: board.StatusInProgress, Priority: 3}
	c := &board.Ticket{ID: board.NewTicketID(), Title: "C", ProjectID: proj.ID, Status: board.StatusInProgress, Priority: 4}
	paths := seedColumn(t, m, proj.ID, []*board.Ticket{a, b, c})

	m.selectTicketByID(b.ID)
	srcCol := m.activeColumn
	if got := m.selectedTicket(); got == nil || got.ID != b.ID {
		t.Fatalf("setup: selected %v, want B", got)
	}
	if m.activeTicket != 1 {
		t.Fatalf("setup: activeTicket=%d, want 1", m.activeTicket)
	}

	cMut := *c
	cMut.Priority = 1
	cMut.UpdatedAt = time.Now()
	info := rewriteTicketOnDisk(t, paths[c.ID], &cMut)

	m.boardResyncSnap = map[string]boardFileMeta{
		paths[c.ID]: {mtime: info.ModTime().Add(-time.Hour), size: info.Size() - 1, projectID: proj.ID},
	}
	msg := boardResyncMsg{snap: map[string]boardFileMeta{
		paths[c.ID]: {mtime: info.ModTime(), size: info.Size(), projectID: proj.ID},
	}}

	if _, cmd := m.handleBoardResyncMsg(msg); cmd == nil {
		t.Fatal("handler returned nil cmd")
	}

	if got := m.selectedTicket(); got == nil || got.ID != b.ID {
		t.Errorf("selection drifted after background re-sort: got %v, want B (%s)", got, b.ID)
	}
	if m.activeColumn != srcCol {
		t.Errorf("activeColumn changed: got %d want %d", m.activeColumn, srcCol)
	}
}

// TestBoardResync_FocusFollowsTicketToNewColumn pins the cross-column
// half of the contract: when an external status change moves the
// selected ticket to a DIFFERENT column, focus follows it there (the
// user-confirmed "the selected ticket must not change" behavior).
//
// A resident owns index 0 of DONE; the selected `mover` starts in
// in_progress at a lower priority and is externally moved to DONE, where
// it lands at index 1 — so a pass that landed on done's index 0 by
// coincidence would NOT satisfy the assertion.
func TestBoardResync_FocusFollowsTicketToNewColumn(t *testing.T) {
	proj := &project.Project{ID: "p1", RepoPath: t.TempDir()}
	m, _ := makeBoardResyncTestModel(t, []*project.Project{proj})
	m.sortMode = SortPriority

	resident := &board.Ticket{ID: board.NewTicketID(), Title: "resident", ProjectID: proj.ID, Status: board.StatusDone, Priority: 1}
	mover := &board.Ticket{ID: board.NewTicketID(), Title: "mover", ProjectID: proj.ID, Status: board.StatusInProgress, Priority: 3}
	paths := seedColumn(t, m, proj.ID, []*board.Ticket{resident, mover})

	m.selectTicketByID(mover.ID)
	srcCol := m.activeColumn

	mMut := *mover
	mMut.Status = board.StatusDone
	mMut.UpdatedAt = time.Now()
	info := rewriteTicketOnDisk(t, paths[mover.ID], &mMut)

	m.boardResyncSnap = map[string]boardFileMeta{
		paths[mover.ID]: {mtime: info.ModTime().Add(-time.Hour), size: info.Size() - 1, projectID: proj.ID},
	}
	msg := boardResyncMsg{snap: map[string]boardFileMeta{
		paths[mover.ID]: {mtime: info.ModTime(), size: info.Size(), projectID: proj.ID},
	}}

	if _, cmd := m.handleBoardResyncMsg(msg); cmd == nil {
		t.Fatal("handler returned nil cmd")
	}

	got := m.selectedTicket()
	if got == nil || got.ID != mover.ID {
		t.Errorf("focus did not follow ticket across columns: got %v, want mover (%s)", got, mover.ID)
	}
	if m.columns[m.activeColumn].Status != board.StatusDone {
		t.Errorf("activeColumn status=%q, want done", m.columns[m.activeColumn].Status)
	}
	if m.activeColumn == srcCol {
		t.Errorf("activeColumn stayed at source %d; expected to follow mover to done", srcCol)
	}
	if m.activeTicket == 0 {
		t.Errorf("activeTicket=0 — landed on top of done by coincidence, not by following mover")
	}
}

// TestFsReload_PreservesSelectionAcrossReorder is the same contract for
// the fsnotify reload path (reload.go's handleFsChanged), the second of
// the two background-refresh sites the fix touches.
func TestFsReload_PreservesSelectionAcrossReorder(t *testing.T) {
	proj := &project.Project{ID: "p1", RepoPath: t.TempDir()}
	m, _ := makeBoardResyncTestModel(t, []*project.Project{proj})
	m.sortMode = SortPriority

	a := &board.Ticket{ID: board.NewTicketID(), Title: "A", ProjectID: proj.ID, Status: board.StatusInProgress, Priority: 2}
	b := &board.Ticket{ID: board.NewTicketID(), Title: "B", ProjectID: proj.ID, Status: board.StatusInProgress, Priority: 3}
	c := &board.Ticket{ID: board.NewTicketID(), Title: "C", ProjectID: proj.ID, Status: board.StatusInProgress, Priority: 4}
	paths := seedColumn(t, m, proj.ID, []*board.Ticket{a, b, c})

	m.selectTicketByID(b.ID)
	if got := m.selectedTicket(); got == nil || got.ID != b.ID {
		t.Fatalf("setup: selected %v, want B", got)
	}

	cMut := *c
	cMut.Priority = 1
	cMut.UpdatedAt = time.Now()
	rewriteTicketOnDisk(t, paths[c.ID], &cMut)

	m.handleFsChanged(FsChangedMsg{Domain: watch.DomainTicket, Path: paths[c.ID], ProjectID: proj.ID})

	if got := m.selectedTicket(); got == nil || got.ID != b.ID {
		t.Errorf("selection drifted after fs-reload re-sort: got %v, want B (%s)", got, b.ID)
	}
}

// TestBoardResync_PreservesSelectionUnderStatusChangeSort pins the doc's
// motivating scenario directly: under SortStatusChange (newest status
// change first), an external bump to a ticket's StatusChangedAt — the
// shape of an agent working↔waiting flip — reorders the column. The
// selected ticket must not change.
//
// Initial StatusChangedAt: A oldest, B middle, C newest → order C,B,A.
// Select B at index 1. Bump A to the newest time; after re-sort the
// order is A,C,B and B is at index 2 (A now occupies the old index 1).
func TestBoardResync_PreservesSelectionUnderStatusChangeSort(t *testing.T) {
	proj := &project.Project{ID: "p1", RepoPath: t.TempDir()}
	m, _ := makeBoardResyncTestModel(t, []*project.Project{proj})
	m.sortMode = SortStatusChange

	base := time.Now()
	tp := func(d time.Duration) *time.Time { tt := base.Add(d); return &tt }
	a := &board.Ticket{ID: board.NewTicketID(), Title: "A", ProjectID: proj.ID, Status: board.StatusInProgress, StatusChangedAt: tp(-30 * time.Second)}
	b := &board.Ticket{ID: board.NewTicketID(), Title: "B", ProjectID: proj.ID, Status: board.StatusInProgress, StatusChangedAt: tp(-20 * time.Second)}
	c := &board.Ticket{ID: board.NewTicketID(), Title: "C", ProjectID: proj.ID, Status: board.StatusInProgress, StatusChangedAt: tp(-10 * time.Second)}
	paths := seedColumn(t, m, proj.ID, []*board.Ticket{a, b, c})

	m.selectTicketByID(b.ID)
	if got := m.selectedTicket(); got == nil || got.ID != b.ID {
		t.Fatalf("setup: selected %v, want B", got)
	}
	if m.activeTicket != 1 {
		t.Fatalf("setup: activeTicket=%d, want 1 (order should be C,B,A)", m.activeTicket)
	}

	aMut := *a
	aMut.StatusChangedAt = tp(10 * time.Second) // now the newest
	aMut.UpdatedAt = time.Now()
	info := rewriteTicketOnDisk(t, paths[a.ID], &aMut)

	m.boardResyncSnap = map[string]boardFileMeta{
		paths[a.ID]: {mtime: info.ModTime().Add(-time.Hour), size: info.Size() - 1, projectID: proj.ID},
	}
	msg := boardResyncMsg{snap: map[string]boardFileMeta{
		paths[a.ID]: {mtime: info.ModTime(), size: info.Size(), projectID: proj.ID},
	}}

	if _, cmd := m.handleBoardResyncMsg(msg); cmd == nil {
		t.Fatal("handler returned nil cmd")
	}

	if got := m.selectedTicket(); got == nil || got.ID != b.ID {
		t.Errorf("selection drifted after status-change re-sort: got %v, want B (%s)", got, b.ID)
	}
}
