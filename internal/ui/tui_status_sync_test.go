package ui

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

// ticketDoneStubAPI is a minimal daemonAPI focused on the
// wrap-up-on-promotion path. It records every TicketDone invocation so
// tests can assert the daemon notification fired (or didn't). The
// wrap-up helper only calls TicketDone, so every other method falls
// through to daemonAPINoop's zero-value returns.
type ticketDoneStubAPI struct {
	daemonAPINoop

	calls         atomic.Int32
	lastTicketID  atomic.Value // string
	ticketDoneErr atomic.Value // error
}

func (s *ticketDoneStubAPI) TicketDone(_ context.Context, ticketID string) (daemon.TicketDoneResp, error) {
	s.calls.Add(1)
	s.lastTicketID.Store(ticketID)
	if v := s.ticketDoneErr.Load(); v != nil {
		if e, ok := v.(error); ok && e != nil {
			return daemon.TicketDoneResp{}, e
		}
	}
	return daemon.TicketDoneResp{Killed: true}, nil
}

// newWrapUpModel builds a minimal Model wired with the column stack,
// pane map, and global store needed to exercise the board-promotion
// wrap-up path. The seeded ticket starts at StatusInProgress with a
// daemon-owned PTY entry so the wrap-up has work to do; a stub
// PaneView is placed in m.panes so the call site can verify the
// teardown. The PaneView is constructed without a live daemon client
// — its Stop() call short-circuits on sessionID="" so the test
// doesn't need a real socket.
func newWrapUpModel(t *testing.T, status board.TicketStatus) (*Model, *board.Ticket, *ticketDoneStubAPI) {
	t.Helper()
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	now := time.Now()
	ticket := &board.Ticket{
		ID:             "T-1",
		Title:          "wrap-up target",
		ProjectID:      "test",
		Status:         status,
		AgentStatus:    board.AgentWorking,
		AgentSessionID: "deadbeef-0001",
		AgentSpawnedAt: &now,
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	stub := &ticketDoneStubAPI{}

	// PaneView with empty sessionID — Stop() short-circuits, so no
	// real daemon is needed. The map entry is what wrapUpSessionForTicket
	// removes; the Stop call is verified by the absence of a panic.
	pv := daemonclient.NewPaneView(nil, string(ticket.ID), "", nil)

	m := &Model{
		globalStore:     globalStore,
		daemon:          stub,
		panes:           map[board.TicketID]*daemonclient.PaneView{ticket.ID: pv},
		daemonOwned:     map[board.TicketID]struct{}{ticket.ID: {}},
		daemonViewing:   map[board.TicketID]int{},
		lastPTYActivity: map[board.TicketID]time.Time{ticket.ID: now},
		columns:         board.DefaultColumns(),
		focusedPane:     ticket.ID,
		mode:            ModeAgentView,
	}
	m.refreshColumnTickets()
	// quickMoveTicket / dropTicket use selectedTicket() / dragSource*
	// to pick the ticket — both ultimately read columnTickets. Locate
	// the in-progress column and point activeColumn/activeTicket at it.
	for colIdx, col := range m.columns {
		if col.Status == status {
			m.activeColumn = colIdx
			m.activeTicket = 0
			m.dragSourceColumn = colIdx
			m.dragSourceTicket = 0
			break
		}
	}
	return m, ticket, stub
}

// TestWrapUpSessionForTicket_InProgressToInReview_StopsPaneAndNotifiesDaemon
// pins the helper's contract on the canonical "in_progress → in_review"
// transition: the pane is removed, the daemon is told, AgentStatus
// flips to Completed (via SetAgentStatus, which stamps
// StatusChangedAt), and the focused-pane mode unwinds.
func TestWrapUpSessionForTicket_InProgressToInReview_StopsPaneAndNotifiesDaemon(t *testing.T) {
	m, ticket, stub := newWrapUpModel(t, board.StatusInProgress)

	// wrapUpSessionForTicket now returns a tea.Cmd that performs the
	// daemon-side teardown (pane.Stop + TicketDone) in a goroutine —
	// keeping the Update loop unblocked on session-end. The tests drive
	// it inline to assert on the daemon-fake call counts.
	cmd := m.wrapUpSessionForTicket(ticket, board.StatusInReview)
	if cmd != nil {
		_ = cmd()
	}

	if _, ok := m.panes[ticket.ID]; ok {
		t.Errorf("panes[%s] still present after wrap-up", ticket.ID)
	}
	if stub.calls.Load() != 1 {
		t.Errorf("TicketDone calls = %d, want 1", stub.calls.Load())
	}
	if got := stub.lastTicketID.Load(); got != string(ticket.ID) {
		t.Errorf("TicketDone ticketID = %v, want %s", got, ticket.ID)
	}
	if ticket.AgentStatus != board.AgentCompleted {
		t.Errorf("AgentStatus = %v, want %v", ticket.AgentStatus, board.AgentCompleted)
	}
	if m.focusedPane != "" {
		t.Errorf("focusedPane = %q, want \"\" (unwound after pane removal)", m.focusedPane)
	}
	if m.mode != ModeNormal {
		t.Errorf("mode = %v, want %v (unwound from ModeAgentView)", m.mode, ModeNormal)
	}
}

// TestWrapUpSessionForTicket_LocalSyncDaemonAsync pins the sync-vs-async
// split that the multi-second-freeze fix introduced. Local state
// mutations (panes map, AgentStatus, focusedPane, mode) MUST happen
// synchronously in wrapUpSessionForTicket so the next render reflects
// the wrap-up. The daemon-side RPCs (pane.Stop + TicketDone), which
// previously blocked the BubbleTea Update loop for up to ~7s, MUST be
// deferred to the returned tea.Cmd so the loop returns immediately.
// This test asserts BOTH halves of that split.
func TestWrapUpSessionForTicket_LocalSyncDaemonAsync(t *testing.T) {
	m, ticket, stub := newWrapUpModel(t, board.StatusInProgress)

	cmd := m.wrapUpSessionForTicket(ticket, board.StatusInReview)

	// BEFORE running the Cmd: local state mutations must already be
	// visible. If any of these asserts fail, the helper has regressed
	// into doing local work inside the goroutine — that breaks the
	// "card renders correctly immediately" contract.
	if _, ok := m.panes[ticket.ID]; ok {
		t.Errorf("panes[%s] still present BEFORE Cmd ran — should be removed synchronously", ticket.ID)
	}
	if ticket.AgentStatus != board.AgentCompleted {
		t.Errorf("AgentStatus BEFORE Cmd ran = %v, want %v (must be set synchronously)", ticket.AgentStatus, board.AgentCompleted)
	}
	if m.focusedPane != "" {
		t.Errorf("focusedPane BEFORE Cmd ran = %q, want \"\" (must unwind synchronously)", m.focusedPane)
	}
	if m.mode != ModeNormal {
		t.Errorf("mode BEFORE Cmd ran = %v, want %v (must unwind synchronously)", m.mode, ModeNormal)
	}

	// BEFORE running the Cmd: the daemon RPC must NOT have fired. If
	// it has, wrapUpSessionForTicket has regressed into blocking the
	// Update loop on the RPC, which is the original bug.
	if calls := stub.calls.Load(); calls != 0 {
		t.Errorf("TicketDone calls BEFORE Cmd ran = %d, want 0 (daemon RPC must be deferred to the Cmd)", calls)
	}

	// Now run the Cmd. The daemon RPC fires here, on whatever
	// goroutine tea picks — for the test, the same goroutine.
	if cmd == nil {
		t.Fatal("wrapUpSessionForTicket returned nil Cmd despite a live pane + daemon")
	}
	_ = cmd()

	if calls := stub.calls.Load(); calls != 1 {
		t.Errorf("TicketDone calls AFTER Cmd ran = %d, want 1", calls)
	}
}

// TestWrapUpSessionForTicket_InProgressToDone covers the second
// "leaving in_progress for a terminal" transition. Behaviour matches
// the in_review case exactly — both go through TicketDone with
// Expected=true on the daemon side.
func TestWrapUpSessionForTicket_InProgressToDone(t *testing.T) {
	m, ticket, stub := newWrapUpModel(t, board.StatusInProgress)

	cmd := m.wrapUpSessionForTicket(ticket, board.StatusDone)
	if cmd != nil {
		_ = cmd()
	}

	if _, ok := m.panes[ticket.ID]; ok {
		t.Errorf("panes[%s] still present after wrap-up", ticket.ID)
	}
	if stub.calls.Load() != 1 {
		t.Errorf("TicketDone calls = %d, want 1", stub.calls.Load())
	}
	if ticket.AgentStatus != board.AgentCompleted {
		t.Errorf("AgentStatus = %v, want %v", ticket.AgentStatus, board.AgentCompleted)
	}
}

// TestWrapUpSessionForTicket_BacklogToInProgress_NoOp asserts that the
// helper does NOT tear anything down when the ticket isn't leaving
// in_progress. This is the "moving the OTHER direction" sanity check —
// promoting a backlog ticket to in_progress must not stop its
// (just-spawned) pane.
func TestWrapUpSessionForTicket_BacklogToInProgress_NoOp(t *testing.T) {
	m, ticket, stub := newWrapUpModel(t, board.StatusBacklog)

	m.wrapUpSessionForTicket(ticket, board.StatusInProgress)

	if _, ok := m.panes[ticket.ID]; !ok {
		t.Errorf("panes[%s] removed despite backlog→in_progress (should be no-op)", ticket.ID)
	}
	if stub.calls.Load() != 0 {
		t.Errorf("TicketDone calls = %d, want 0 (no wrap-up on backlog→in_progress)", stub.calls.Load())
	}
	if ticket.AgentStatus != board.AgentWorking {
		t.Errorf("AgentStatus = %v, want %v (unchanged)", ticket.AgentStatus, board.AgentWorking)
	}
}

// TestWrapUpSessionForTicket_InReviewToDone_NoOp pins the
// "in_progress only" gate. A wrap-up move from in_review → done must
// not double-tear-down: by the time we reach in_review the session
// has already been killed by the prior in_progress → in_review hop
// (which IS the wrap-up moment). Calling TicketDone again would
// at best be redundant and at worst race with the daemon's
// post-kill cleanup.
func TestWrapUpSessionForTicket_InReviewToDone_NoOp(t *testing.T) {
	m, ticket, stub := newWrapUpModel(t, board.StatusInReview)

	m.wrapUpSessionForTicket(ticket, board.StatusDone)

	if stub.calls.Load() != 0 {
		t.Errorf("TicketDone calls = %d, want 0 (no wrap-up on in_review→done)", stub.calls.Load())
	}
}

// TestWrapUpSessionForTicket_NoLivePane_StillNotifiesDaemon covers the
// "ticket has no PaneView but daemon may still own its session"
// edge — e.g., a second TUI moved the ticket but this one never
// attached. The daemon notification must still fire so the daemon's
// orphaned PTY is reaped. Local map mutations are no-ops by definition.
func TestWrapUpSessionForTicket_NoLivePane_StillNotifiesDaemon(t *testing.T) {
	m, ticket, stub := newWrapUpModel(t, board.StatusInProgress)
	// Drop the pane so we exercise the "no local pane" branch.
	delete(m.panes, ticket.ID)
	m.focusedPane = ""
	m.mode = ModeNormal

	cmd := m.wrapUpSessionForTicket(ticket, board.StatusInReview)
	if cmd != nil {
		_ = cmd()
	}

	if stub.calls.Load() != 1 {
		t.Errorf("TicketDone calls = %d, want 1 (daemon must still be told)", stub.calls.Load())
	}
	if ticket.AgentStatus != board.AgentCompleted {
		t.Errorf("AgentStatus = %v, want %v", ticket.AgentStatus, board.AgentCompleted)
	}
}

// TestWrapUpSessionForTicket_NilDaemonAPI_DoesNotPanic exercises the
// daemon-unreachable case: the TUI started before the daemon was up,
// so m.daemon is nil. Local cleanup must still proceed; the daemon
// notification is silently skipped.
func TestWrapUpSessionForTicket_NilDaemonAPI_DoesNotPanic(t *testing.T) {
	m, ticket, _ := newWrapUpModel(t, board.StatusInProgress)
	m.daemon = nil

	cmd := m.wrapUpSessionForTicket(ticket, board.StatusInReview)
	if cmd != nil {
		// Invoke the Cmd to also exercise pane.Stop()/Close() under
		// a nil daemon API — the closure must skip the TicketDone
		// branch but still drain the pane handle without panicking.
		_ = cmd()
	}

	if _, ok := m.panes[ticket.ID]; ok {
		t.Errorf("panes[%s] still present after wrap-up with nil m.daemon", ticket.ID)
	}
	if ticket.AgentStatus != board.AgentCompleted {
		t.Errorf("AgentStatus = %v, want %v", ticket.AgentStatus, board.AgentCompleted)
	}
}

// TestQuickMoveTicket_InProgressToInReview_WrapsUp verifies that the
// quick-move (forward) keyboard path threads the wrap-up call BEFORE
// the store's Move. The ticket's pre-Move status (in_progress) is
// what gates the wrap-up; if the wire-up landed AFTER Move the gate
// would never fire because Move mutates Status in place.
func TestQuickMoveTicket_InProgressToInReview_WrapsUp(t *testing.T) {
	m, ticket, stub := newWrapUpModel(t, board.StatusInProgress)

	_, cmd := m.quickMoveTicket()
	if cmd != nil {
		_ = cmd()
	}

	// Move went through.
	if ticket.Status != board.StatusInReview {
		t.Errorf("Status = %v, want %v", ticket.Status, board.StatusInReview)
	}
	// Wrap-up fired BEFORE Move (gate saw in_progress).
	if stub.calls.Load() != 1 {
		t.Errorf("TicketDone calls = %d, want 1 (wrap-up must have fired)", stub.calls.Load())
	}
	if _, ok := m.panes[ticket.ID]; ok {
		t.Errorf("panes[%s] still present after quick-move", ticket.ID)
	}
	if ticket.AgentStatus != board.AgentCompleted {
		t.Errorf("AgentStatus = %v, want %v", ticket.AgentStatus, board.AgentCompleted)
	}
}

// TestQuickMoveTicketBackward_DoneToInReview_NoWrapUp asserts that the
// backward path doesn't fire wrap-up on done → in_review. The session
// is already gone (we wrapped up when entering in_review or done the
// first time); the helper's pre-Move-status gate ensures we don't try
// to re-kill it.
func TestQuickMoveTicketBackward_DoneToInReview_NoWrapUp(t *testing.T) {
	m, ticket, stub := newWrapUpModel(t, board.StatusDone)

	_, _ = m.quickMoveTicketBackward()

	if ticket.Status != board.StatusInReview {
		t.Errorf("Status = %v, want %v", ticket.Status, board.StatusInReview)
	}
	if stub.calls.Load() != 0 {
		t.Errorf("TicketDone calls = %d, want 0 (no wrap-up on done→in_review)", stub.calls.Load())
	}
}

// TestDropTicket_InProgressToInReview_WrapsUp covers the drag-drop
// path. dropTicket reads dragSourceColumn / dragTargetColumn so the
// fixture points dragTarget at the in-review column before invoking.
func TestDropTicket_InProgressToInReview_WrapsUp(t *testing.T) {
	m, ticket, stub := newWrapUpModel(t, board.StatusInProgress)
	// Find the in-review column index.
	for colIdx, col := range m.columns {
		if col.Status == board.StatusInReview {
			m.dragTargetColumn = colIdx
			break
		}
	}
	m.dragging = true

	_, cmd := m.dropTicket()
	if cmd != nil {
		_ = cmd()
	}

	if ticket.Status != board.StatusInReview {
		t.Errorf("Status = %v, want %v", ticket.Status, board.StatusInReview)
	}
	if stub.calls.Load() != 1 {
		t.Errorf("TicketDone calls = %d, want 1", stub.calls.Load())
	}
	if _, ok := m.panes[ticket.ID]; ok {
		t.Errorf("panes[%s] still present after drop", ticket.ID)
	}
	if ticket.AgentStatus != board.AgentCompleted {
		t.Errorf("AgentStatus = %v, want %v", ticket.AgentStatus, board.AgentCompleted)
	}
}

// TestActivityEvent_AppliesDaemonResolvedStatus pins the
// daemon-authoritative status path: the daemon resolves working/waiting
// from its OWN live PTY grid (which it has for every owned session,
// attached or not) and ships the verdict in SessionEvent.Status on its
// activity heartbeats. The UI must apply that verdict to the ticket. This
// is what lets a bg-spawned unattached session show the truth — the
// daemon sees a prompt on the grid, reports "waiting", and the card
// follows (and reports "working" when the grid shows an active turn).
func TestActivityEvent_AppliesDaemonResolvedStatus(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	const tid board.TicketID = "activity-status-1"
	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	now := time.Now()
	ticket := &board.Ticket{
		ID:             tid,
		Title:          "t",
		ProjectID:      "test",
		Status:         board.StatusInProgress,
		AgentStatus:    board.AgentWorking, // what the daemon "started" event set
		AgentSpawnedAt: &now,
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	m := &Model{
		globalStore:     globalStore,
		daemonOwned:     map[board.TicketID]struct{}{tid: {}},
		daemonViewing:   map[board.TicketID]int{},
		lastPTYActivity: map[board.TicketID]time.Time{},
		panes:           map[board.TicketID]*daemonclient.PaneView{},
	}

	_, _ = m.handleDaemonSessionEvent(daemonSessionEventMsg{
		Event: daemon.SessionEvent{
			Event:          "activity",
			TicketID:       string(tid),
			Status:         "waiting",
			LastActivityAt: now,
		},
	})

	if ticket.AgentStatus != board.AgentWaiting {
		t.Errorf("AgentStatus = %v, want %v (daemon-resolved status on the activity event must be applied)",
			ticket.AgentStatus, board.AgentWaiting)
	}
}

// TestActivityEvent_EmptyStatusDoesNotClobber guards the fallback path:
// an activity event with no resolved Status (older daemon, or an opencode
// session the daemon doesn't classify) must NOT reset the ticket's
// AgentStatus — the file-poll remains the source for those.
func TestActivityEvent_EmptyStatusDoesNotClobber(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	const tid board.TicketID = "activity-status-2"
	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	ticket := &board.Ticket{
		ID: tid, Title: "t", ProjectID: "test",
		Status:      board.StatusInProgress,
		AgentStatus: board.AgentWorking,
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	m := &Model{
		globalStore:     globalStore,
		daemonOwned:     map[board.TicketID]struct{}{tid: {}},
		daemonViewing:   map[board.TicketID]int{},
		lastPTYActivity: map[board.TicketID]time.Time{},
		panes:           map[board.TicketID]*daemonclient.PaneView{},
	}

	_, _ = m.handleDaemonSessionEvent(daemonSessionEventMsg{
		Event: daemon.SessionEvent{
			Event:          "activity",
			TicketID:       string(tid),
			Status:         "",
			LastActivityAt: time.Now(),
		},
	})

	if ticket.AgentStatus != board.AgentWorking {
		t.Errorf("AgentStatus = %v, want %v (empty Status must not clobber)", ticket.AgentStatus, board.AgentWorking)
	}
}

// TestActivityEvent_DoesNotDowngradeCompleted mirrors the file-poll's
// terminal-state guard: a "completed" ticket must not be knocked back to
// working/waiting by a late activity event racing the wrap-up.
func TestActivityEvent_DoesNotDowngradeCompleted(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	const tid board.TicketID = "activity-status-3"
	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	ticket := &board.Ticket{
		ID: tid, Title: "t", ProjectID: "test",
		Status:      board.StatusInProgress,
		AgentStatus: board.AgentCompleted,
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	m := &Model{
		globalStore:     globalStore,
		daemonOwned:     map[board.TicketID]struct{}{tid: {}},
		daemonViewing:   map[board.TicketID]int{},
		lastPTYActivity: map[board.TicketID]time.Time{},
		panes:           map[board.TicketID]*daemonclient.PaneView{},
	}

	_, _ = m.handleDaemonSessionEvent(daemonSessionEventMsg{
		Event: daemon.SessionEvent{
			Event:    "activity",
			TicketID: string(tid),
			Status:   "working",
		},
	})

	if ticket.AgentStatus != board.AgentCompleted {
		t.Errorf("AgentStatus = %v, want %v (terminal state must not be downgraded)", ticket.AgentStatus, board.AgentCompleted)
	}
}

// TestExitedEvent_PreservesResidueOnExpected pins that an Expected=true
// "exited" event (the clean wrap-up signal from handleTicketDone) must
// NOT clear AgentSessionID or AgentSpawnedAt. The JSONL transcript
// outlives the PTY; pulling the ticket back from done must resume the
// same session, not spawn fresh. AgentStatus still transitions to
// AgentCompleted (the only meaningful difference vs unexpected exit).
func TestExitedEvent_PreservesResidueOnExpected(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	const tid board.TicketID = "exit-expected-1"
	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	now := time.Now()
	const wantUUID = "uuid-to-preserve"
	ticket := &board.Ticket{
		ID:             tid,
		Title:          "expected exit",
		ProjectID:      "test",
		Status:         board.StatusInProgress,
		AgentStatus:    board.AgentWorking,
		AgentSessionID: wantUUID,
		AgentSpawnedAt: &now,
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	m := &Model{
		globalStore:     globalStore,
		daemonOwned:     map[board.TicketID]struct{}{tid: {}},
		daemonViewing:   map[board.TicketID]int{},
		lastPTYActivity: map[board.TicketID]time.Time{},
		panes:           map[board.TicketID]*daemonclient.PaneView{},
	}

	_, _ = m.handleDaemonSessionEvent(daemonSessionEventMsg{
		Event: daemon.SessionEvent{
			Event:    "exited",
			TicketID: string(tid),
			Expected: true,
		},
	})

	if ticket.AgentSessionID != wantUUID {
		t.Errorf("AgentSessionID = %q, want %q (must NOT clear on expected exit)",
			ticket.AgentSessionID, wantUUID)
	}
	if ticket.AgentSpawnedAt == nil {
		t.Errorf("AgentSpawnedAt = nil, want non-nil (must NOT clear on expected exit)")
	}
	if ticket.AgentStatus != board.AgentCompleted {
		t.Errorf("AgentStatus = %v, want %v (Expected=true preserves Completed)", ticket.AgentStatus, board.AgentCompleted)
	}
}

// TestExitedEvent_PreservesResidueOnUnexpected guards against the
// daemon-crash-loses-link regression: when the session exits
// unexpectedly (PTY died, daemon hiccuped, agent crashed), the JSONL
// may still be on disk and resumable. The on-disk session linkage
// must survive so a subsequent --resume picks up where the agent
// left off. See commit c718699's UUID-persistence rationale.
func TestExitedEvent_PreservesResidueOnUnexpected(t *testing.T) {
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())

	const tid board.TicketID = "exit-unexpected-1"
	proj := &project.Project{ID: "test", RepoPath: t.TempDir()}
	globalStore := project.NewGlobalTicketStore(nil)
	globalStore.AddProject(proj)

	now := time.Now()
	originalUUID := "uuid-to-preserve"
	ticket := &board.Ticket{
		ID:             tid,
		Title:          "unexpected exit",
		ProjectID:      "test",
		Status:         board.StatusInProgress,
		AgentStatus:    board.AgentWorking,
		AgentSessionID: originalUUID,
		AgentSpawnedAt: &now,
	}
	if err := globalStore.Add(ticket); err != nil {
		t.Fatalf("Add ticket: %v", err)
	}

	m := &Model{
		globalStore:     globalStore,
		daemonOwned:     map[board.TicketID]struct{}{tid: {}},
		daemonViewing:   map[board.TicketID]int{},
		lastPTYActivity: map[board.TicketID]time.Time{},
		panes:           map[board.TicketID]*daemonclient.PaneView{},
	}

	_, _ = m.handleDaemonSessionEvent(daemonSessionEventMsg{
		Event: daemon.SessionEvent{
			Event:    "exited",
			TicketID: string(tid),
			Expected: false,
		},
	})

	if ticket.AgentSessionID != originalUUID {
		t.Errorf("AgentSessionID = %q, want %q (must NOT clear on unexpected exit)",
			ticket.AgentSessionID, originalUUID)
	}
	if ticket.AgentSpawnedAt == nil {
		t.Errorf("AgentSpawnedAt = nil, want non-nil (must NOT clear on unexpected exit)")
	}
	if ticket.AgentStatus != board.AgentNone {
		t.Errorf("AgentStatus = %v, want %v (unexpected exit resets to None)",
			ticket.AgentStatus, board.AgentNone)
	}
}
