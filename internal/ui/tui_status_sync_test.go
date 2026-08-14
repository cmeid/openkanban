package ui

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

// ticketDoneStubAPI is a minimal daemonAPI focused on the status-move
// path. It records every TicketDone invocation so tests can assert the
// daemon notification did NOT fire (status moves must never end a
// session) — and, in the control subtest, that it still fires for
// ticket DELETE. Every other method falls through to daemonAPINoop's
// zero-value returns.
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

// newStatusMoveModel builds a minimal Model wired with the column
// stack, pane map, and global store needed to exercise a board-driven
// status move. The seeded ticket starts at `status` with a daemon-owned
// pane entry AND a linked session (AgentSessionID + AgentSpawnedAt), so
// the preservation assertions have something they could actually lose —
// a fixture with no session residue would pass even against the old
// teardown code.
//
// live controls the pane's cached Running() flag, which is the gate
// stampTerminalAgentStatus reads: a still-working agent must NOT be
// stamped Completed (that badge is terminal against both status
// sources and would freeze), while a ticket with no live session must
// be. Either way the PaneView is built with no daemon client and
// sessionID="" so Stop()/Close() short-circuit and no socket is needed.
func newStatusMoveModel(t *testing.T, status board.TicketStatus, live bool) (*Model, *board.Ticket, *ticketDoneStubAPI) {
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

	// PaneView with empty sessionID — Stop() short-circuits, so no real
	// daemon is needed. A non-nil SessionInfo with Running=true puts the
	// view in PaneViewUnattached, which is what makes Running() report
	// true (see PaneView.Running's state table).
	var info *daemon.SessionInfo
	if live {
		info = &daemon.SessionInfo{Running: true}
	}
	pv := daemonclient.NewPaneView(nil, string(ticket.ID), "", info)
	if pv.Running() != live {
		t.Fatalf("fixture: pane Running() = %v, want %v", pv.Running(), live)
	}

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
		config:          &config.Config{Agents: map[string]config.AgentConfig{}},
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

// TestStatusMove_PreservesSessionAndSkipsTicketDone is the core
// preservation guarantee: a board-driven status move must never end the
// ticket's session. It replaces a family of tests that pinned the
// opposite — the removed wrapUpSessionForTicket stopped the pane and
// sent TicketDone on every exit from in_progress.
//
// The table covers every destination, for both a live and a finished
// session. In all cases the pane stays in m.panes, no TicketDone RPC is
// sent, the durable resume residue (AgentSessionID / AgentSpawnedAt) is
// untouched, and the user is not ejected from the session view.
//
// The final "control" subtest deletes a ticket on the SAME fixture and
// asserts TicketDone fires exactly once. Without it every "want 0 calls"
// assertion above would also pass if the stub were simply never wired to
// m.daemon — a false green that survives reverting the fix.
func TestStatusMove_PreservesSessionAndSkipsTicketDone(t *testing.T) {
	targets := []board.TicketStatus{
		board.StatusInReview,
		board.StatusDone,
		board.StatusBacklog,
		board.StatusNext,
		board.StatusArchived,
	}
	for _, live := range []bool{true, false} {
		for _, target := range targets {
			name := string(target) + "/finished"
			if live {
				name = string(target) + "/live"
			}
			t.Run(name, func(t *testing.T) {
				m, ticket, stub := newStatusMoveModel(t, board.StatusInProgress, live)
				wantSession := ticket.AgentSessionID
				wantSpawned := ticket.AgentSpawnedAt
				if wantSession == "" || wantSpawned == nil {
					t.Fatal("fixture: ticket must carry session residue, or the preservation assertions are vacuous")
				}

				m.stampTerminalAgentStatus(ticket, target)

				if _, ok := m.panes[ticket.ID]; !ok {
					t.Errorf("panes[%s] removed by a status move; the session must survive", ticket.ID)
				}
				if got := stub.calls.Load(); got != 0 {
					t.Errorf("TicketDone calls = %d, want 0 (a status move must not end the session)", got)
				}
				if ticket.AgentSessionID != wantSession {
					t.Errorf("AgentSessionID = %q, want %q (the resume key must survive)", ticket.AgentSessionID, wantSession)
				}
				if ticket.AgentSpawnedAt != wantSpawned {
					t.Errorf("AgentSpawnedAt = %v, want %v (unchanged)", ticket.AgentSpawnedAt, wantSpawned)
				}
				if m.focusedPane != ticket.ID || m.mode != ModeAgentView {
					t.Errorf("focus unwound (focusedPane=%q mode=%v); a status move must not eject the user from the session",
						m.focusedPane, m.mode)
				}
			})
		}
	}

	// Control: proves the stub is actually reachable from this fixture.
	t.Run("control/ticket delete still notifies daemon", func(t *testing.T) {
		m, ticket, stub := newStatusMoveModel(t, board.StatusInProgress, true)

		m.performTicketCleanup(ticket)

		if got := stub.calls.Load(); got != 1 {
			t.Fatalf("TicketDone calls on ticket DELETE = %d, want 1 — the stub isn't wired, so the want-0 assertions above prove nothing", got)
		}
	})
}

// TestStampTerminalAgentStatus_BadgeGate pins the only bookkeeping the
// helper still does, and its gate. Leaving in_progress for a terminal
// column stamps AgentCompleted so the card renders as finished without
// waiting on a daemon event — but ONLY when nothing is live.
// AgentCompleted is terminal against both status sources (the poll
// handler and applyDaemonStatus each refuse to downgrade it), so
// stamping it over a still-working agent would freeze the badge until
// the ticket returned to an active column.
func TestStampTerminalAgentStatus_BadgeGate(t *testing.T) {
	t.Run("live session is not stamped", func(t *testing.T) {
		m, ticket, _ := newStatusMoveModel(t, board.StatusInProgress, true)
		// Precondition: the seeded badge must differ from Completed, or
		// "it still isn't Completed" is a tautology.
		if ticket.AgentStatus != board.AgentWorking {
			t.Fatalf("fixture: AgentStatus = %v, want %v", ticket.AgentStatus, board.AgentWorking)
		}

		m.stampTerminalAgentStatus(ticket, board.StatusInReview)

		if ticket.AgentStatus == board.AgentCompleted {
			t.Fatalf("AgentStatus = %v; a live agent must not be stamped Completed", ticket.AgentStatus)
		}
		// The load-bearing consequence: the daemon can still drive the
		// badge. Assert on applyDaemonStatus's RETURN value — reading
		// AgentStatus after the write would pass even if the terminal
		// guard had swallowed the update. "waiting" (not "working") so
		// the call is a real transition rather than a no-op.
		if !m.applyDaemonStatus(ticket, string(board.AgentWaiting)) {
			t.Error("applyDaemonStatus(waiting) = false; the live agent's badge is frozen")
		}
	})

	t.Run("no live session is stamped", func(t *testing.T) {
		m, ticket, _ := newStatusMoveModel(t, board.StatusInProgress, false)

		m.stampTerminalAgentStatus(ticket, board.StatusDone)

		if ticket.AgentStatus != board.AgentCompleted {
			t.Errorf("AgentStatus = %v, want %v (a finished ticket should still get its badge)",
				ticket.AgentStatus, board.AgentCompleted)
		}
	})

	t.Run("not leaving in_progress is a no-op", func(t *testing.T) {
		m, ticket, _ := newStatusMoveModel(t, board.StatusBacklog, false)

		m.stampTerminalAgentStatus(ticket, board.StatusInProgress)

		if ticket.AgentStatus != board.AgentWorking {
			t.Errorf("AgentStatus = %v, want %v (unchanged)", ticket.AgentStatus, board.AgentWorking)
		}
	})

	t.Run("non-terminal destination is a no-op", func(t *testing.T) {
		m, ticket, _ := newStatusMoveModel(t, board.StatusInProgress, false)

		m.stampTerminalAgentStatus(ticket, board.StatusBacklog)

		if ticket.AgentStatus != board.AgentWorking {
			t.Errorf("AgentStatus = %v, want %v (unchanged)", ticket.AgentStatus, board.AgentWorking)
		}
	})
}

// TestQuickMoveTicket_InProgressToInReview_PreservesSession pins the
// forward quick-move wiring. The stamp still runs BEFORE
// globalStore.Move — its gate reads the pre-move status, which Move
// mutates in place — but the path no longer returns a teardown Cmd.
func TestQuickMoveTicket_InProgressToInReview_PreservesSession(t *testing.T) {
	m, ticket, stub := newStatusMoveModel(t, board.StatusInProgress, false)

	_, cmd := m.quickMoveTicket()

	if cmd != nil {
		t.Error("quickMoveTicket returned a Cmd; the async teardown Cmd should be gone")
	}
	if ticket.Status != board.StatusInReview {
		t.Errorf("Status = %v, want %v", ticket.Status, board.StatusInReview)
	}
	if _, ok := m.panes[ticket.ID]; !ok {
		t.Errorf("panes[%s] removed by quick-move; the session must survive", ticket.ID)
	}
	if got := stub.calls.Load(); got != 0 {
		t.Errorf("TicketDone calls = %d, want 0", got)
	}
	if ticket.AgentSessionID == "" {
		t.Error("AgentSessionID cleared by quick-move; the resume key must survive")
	}
	// Proves the stamp ran pre-Move: post-Move the gate would see
	// in_review and skip.
	if ticket.AgentStatus != board.AgentCompleted {
		t.Errorf("AgentStatus = %v, want %v (stamp must run before Move)", ticket.AgentStatus, board.AgentCompleted)
	}
}

// TestQuickMoveTicketBackward_DoneToInReview_PreservesSession asserts the
// backward path is inert with respect to the session too. The stamp's
// pre-move-status gate makes it a no-op here (the ticket isn't leaving
// in_progress), and nothing else touches the pane.
func TestQuickMoveTicketBackward_DoneToInReview_PreservesSession(t *testing.T) {
	m, ticket, stub := newStatusMoveModel(t, board.StatusDone, true)

	_, cmd := m.quickMoveTicketBackward()

	if cmd != nil {
		t.Error("quickMoveTicketBackward returned a Cmd; the async teardown Cmd should be gone")
	}
	if ticket.Status != board.StatusInReview {
		t.Errorf("Status = %v, want %v", ticket.Status, board.StatusInReview)
	}
	if got := stub.calls.Load(); got != 0 {
		t.Errorf("TicketDone calls = %d, want 0 (no teardown on done→in_review)", got)
	}
	if _, ok := m.panes[ticket.ID]; !ok {
		t.Errorf("panes[%s] removed by backward move", ticket.ID)
	}
}

// TestQuickMoveTicketBackward_InReviewToInProgress_ClearsDoneBadge pins the
// reported bug end-to-end through the TUI: a ticket pulled from in_review
// back to in_progress carries a stale AgentCompleted badge ("✓ done") that
// must clear once it re-enters the active column. The TUI backward path
// routes through globalStore.Move → board.SetStatus, where the fix lives.
//
// This is also the self-healing half of the preserved-session design: the
// badge a terminal move stamps is demoted on the way back, so a session
// that keeps running doesn't stay frozen at "Completed".
func TestQuickMoveTicketBackward_InReviewToInProgress_ClearsDoneBadge(t *testing.T) {
	m, ticket, _ := newStatusMoveModel(t, board.StatusInReview, false)
	// Seed the stale done badge a finished session leaves behind.
	ticket.SetAgentStatus(board.AgentCompleted)
	someTime := time.Now().Add(-time.Hour)
	ticket.CompletedAt = &someTime

	// Fail-loud precondition: the post-assertion is vacuous unless the
	// stale badge is actually present before the move (AgentNone is the
	// zero-value default).
	if ticket.AgentStatus != board.AgentCompleted {
		t.Fatalf("precondition: want AgentStatus %q, got %q", board.AgentCompleted, ticket.AgentStatus)
	}

	_, _ = m.quickMoveTicketBackward()

	if ticket.Status != board.StatusInProgress {
		t.Errorf("Status = %v, want %v", ticket.Status, board.StatusInProgress)
	}
	if ticket.AgentStatus != board.AgentNone {
		t.Errorf("AgentStatus = %q, want %q (stale done badge must clear)", ticket.AgentStatus, board.AgentNone)
	}
	if ticket.CompletedAt != nil {
		t.Errorf("CompletedAt = %v, want nil", *ticket.CompletedAt)
	}
	// Pulling a ticket back must not cost the session either.
	if _, ok := m.panes[ticket.ID]; !ok {
		t.Errorf("panes[%s] removed by in_review→in_progress", ticket.ID)
	}
	if ticket.AgentSessionID == "" {
		t.Error("AgentSessionID cleared by in_review→in_progress; the resume key must survive")
	}
}

// TestDropTicket_InProgressToInReview_PreservesSession covers the
// drag-drop path. dropTicket reads dragSourceColumn / dragTargetColumn so
// the fixture points dragTarget at the in-review column before invoking.
func TestDropTicket_InProgressToInReview_PreservesSession(t *testing.T) {
	m, ticket, stub := newStatusMoveModel(t, board.StatusInProgress, false)
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
		t.Error("dropTicket returned a Cmd; the async teardown Cmd should be gone")
	}
	if ticket.Status != board.StatusInReview {
		t.Errorf("Status = %v, want %v", ticket.Status, board.StatusInReview)
	}
	if got := stub.calls.Load(); got != 0 {
		t.Errorf("TicketDone calls = %d, want 0", got)
	}
	if _, ok := m.panes[ticket.ID]; !ok {
		t.Errorf("panes[%s] removed by drop; the session must survive", ticket.ID)
	}
	if ticket.AgentStatus != board.AgentCompleted {
		t.Errorf("AgentStatus = %v, want %v", ticket.AgentStatus, board.AgentCompleted)
	}
}

// TestStatusMove_NilDaemonAPI_DoesNotPanic exercises the
// daemon-unreachable case: the TUI started before the daemon was up, so
// m.daemon is nil. The stamp is pure local bookkeeping, so it must still
// land.
func TestStatusMove_NilDaemonAPI_DoesNotPanic(t *testing.T) {
	m, ticket, _ := newStatusMoveModel(t, board.StatusInProgress, false)
	m.daemon = nil

	m.stampTerminalAgentStatus(ticket, board.StatusInReview)

	if _, ok := m.panes[ticket.ID]; !ok {
		t.Errorf("panes[%s] removed with nil m.daemon", ticket.ID)
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
