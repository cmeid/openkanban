package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/project"
)

// runTicketMove sets the package-level flag vars and calls ticketMoveCmd.RunE.
func runTicketMove(t *testing.T, projectArg, idArg, statusArg string) error {
	t.Helper()
	ticketMoveProject = projectArg
	ticketMoveID = idArg
	ticketMoveStatus = statusArg
	return ticketMoveCmd.RunE(ticketMoveCmd, nil)
}

// --- Flag validation -------------------------------------------------------

func TestTicketMove_MissingProject(t *testing.T) {
	scaffoldTicketDoneEnv(t)
	err := runTicketMove(t, "", "some-id", "done")
	if err == nil || !strings.Contains(err.Error(), "--project") {
		t.Errorf("expected --project error; got %v", err)
	}
}

func TestTicketMove_MissingID(t *testing.T) {
	_, tk, _ := scaffoldTicketDoneEnv(t)
	err := runTicketMove(t, "test-proj", "", string(tk.Status))
	if err == nil || !strings.Contains(err.Error(), "--id") {
		t.Errorf("expected --id error; got %v", err)
	}
}

func TestTicketMove_MissingStatus(t *testing.T) {
	_, tk, _ := scaffoldTicketDoneEnv(t)
	err := runTicketMove(t, "test-proj", string(tk.ID), "")
	if err == nil || !strings.Contains(err.Error(), "--status") {
		t.Errorf("expected --status error; got %v", err)
	}
}

func TestTicketMove_InvalidStatus(t *testing.T) {
	_, tk, _ := scaffoldTicketDoneEnv(t)
	err := runTicketMove(t, "test-proj", string(tk.ID), "not-a-status")
	if err == nil || !strings.Contains(err.Error(), "is not one of") {
		t.Errorf("expected ParseStatus error; got %v", err)
	}
}

// --- Simple status transitions (no daemon) ---------------------------------

func TestTicketMove_BacklogToInProgress_StartsAt(t *testing.T) {
	proj, tk, _ := scaffoldTicketDoneEnv(t)

	// scaffoldTicketDoneEnv leaves the ticket in_progress; reset to backlog.
	store, err := project.LoadTicketStore(proj)
	if err != nil {
		t.Fatalf("LoadTicketStore: %v", err)
	}
	tk.Status = board.StatusBacklog
	tk.StartedAt = nil
	if err := store.SaveTicket(tk); err != nil {
		t.Fatalf("seed Backlog: %v", err)
	}

	if err := runTicketMove(t, "test-proj", string(tk.ID), "in_progress"); err != nil {
		t.Fatalf("runTicketMove: %v", err)
	}

	got := loadTicket(t, proj, tk.ID)
	if got.Status != board.StatusInProgress {
		t.Errorf("Status = %q; want in_progress", got.Status)
	}
	if got.StartedAt == nil {
		t.Error("StartedAt should be stamped after backlog→in_progress")
	}
}

func TestTicketMove_ArchivedTargetAccepted(t *testing.T) {
	proj, tk, _ := scaffoldTicketDoneEnv(t)

	if err := runTicketMove(t, "test-proj", string(tk.ID), "archived"); err != nil {
		t.Fatalf("archived should be a valid target: %v", err)
	}

	got := loadTicket(t, proj, tk.ID)
	if got.Status != board.StatusArchived {
		t.Errorf("Status = %q; want archived", got.Status)
	}
}

func TestTicketMove_IDPrefixResolution(t *testing.T) {
	proj, tk, _ := scaffoldTicketDoneEnv(t)
	prefix := string(tk.ID)[:8]

	if err := runTicketMove(t, "test-proj", prefix, "done"); err != nil {
		t.Fatalf("move via ID prefix: %v", err)
	}
	if got := loadTicket(t, proj, tk.ID); got.Status != board.StatusDone {
		t.Errorf("Status = %q; want done", got.Status)
	}
}

// --- Daemon-integrated tests -----------------------------------------------
//
// Each test that asserts "daemon NOT fired" seeds a live session first and
// verifies it survives the move — a phantom assertion (no session seeded)
// would pass even if TicketDone were erroneously sent, because TicketDone
// is a no-op on miss.

func TestTicketMove_BacklogToInProgress_DaemonSessionSurvives(t *testing.T) {
	sock, _ := daemonTestEnv(t)
	startDaemonServer(t, sock)
	proj, tk, _ := scaffoldTicketDoneEnv(t)

	// Reset ticket to backlog.
	store, err := project.LoadTicketStore(proj)
	if err != nil {
		t.Fatalf("LoadTicketStore: %v", err)
	}
	tk.Status = board.StatusBacklog
	tk.StartedAt = nil
	if err := store.SaveTicket(tk); err != nil {
		t.Fatalf("seed Backlog: %v", err)
	}

	// Seed a live daemon session. If the move fires TicketDone, the
	// session will disappear; survival proves TicketDone was NOT sent.
	spawnDaemonSessionForTicket(t, string(tk.ID))

	if err := runTicketMove(t, "test-proj", string(tk.ID), "in_progress"); err != nil {
		t.Fatalf("runTicketMove: %v", err)
	}

	got := loadTicket(t, proj, tk.ID)
	if got.Status != board.StatusInProgress {
		t.Errorf("Status = %q; want in_progress", got.Status)
	}
	// REVERT PROOF: if the teardown gate were widened to fire on all moves
	// (not just exits from in_progress), this assertion would fail because
	// the session would be killed by the spurious TicketDone.
	if !daemonHasSessionForTicket(t, string(tk.ID)) {
		t.Error("daemon session was unexpectedly killed by backlog→in_progress move")
	}
}

func TestTicketMove_InProgressToInReview_AgentCompleted_SessionGone(t *testing.T) {
	sock, _ := daemonTestEnv(t)
	startDaemonServer(t, sock)
	proj, tk, _ := scaffoldTicketDoneEnv(t)

	spawnDaemonSessionForTicket(t, string(tk.ID))

	if err := runTicketMove(t, "test-proj", string(tk.ID), "in_review"); err != nil {
		t.Fatalf("runTicketMove: %v", err)
	}

	got := loadTicket(t, proj, tk.ID)
	if got.Status != board.StatusInReview {
		t.Errorf("Status = %q; want in_review", got.Status)
	}
	if got.AgentStatus != board.AgentCompleted {
		t.Errorf("AgentStatus = %q; want completed", got.AgentStatus)
	}
	// CompletedAt is stamped only by done, not in_review.
	if got.CompletedAt != nil {
		t.Errorf("CompletedAt = %v; want nil for in_review", got.CompletedAt)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !daemonHasSessionForTicket(t, string(tk.ID)) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("daemon session still alive after in_progress→in_review")
}

func TestTicketMove_InProgressToDone_CompletedAt_SessionGone(t *testing.T) {
	sock, _ := daemonTestEnv(t)
	startDaemonServer(t, sock)
	proj, tk, _ := scaffoldTicketDoneEnv(t)

	spawnDaemonSessionForTicket(t, string(tk.ID))

	if err := runTicketMove(t, "test-proj", string(tk.ID), "done"); err != nil {
		t.Fatalf("runTicketMove: %v", err)
	}

	got := loadTicket(t, proj, tk.ID)
	if got.Status != board.StatusDone {
		t.Errorf("Status = %q; want done", got.Status)
	}
	if got.AgentStatus != board.AgentCompleted {
		t.Errorf("AgentStatus = %q; want completed", got.AgentStatus)
	}
	if got.CompletedAt == nil {
		t.Error("CompletedAt should be stamped after →done")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !daemonHasSessionForTicket(t, string(tk.ID)) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("daemon session still alive after in_progress→done")
}

func TestTicketMove_InProgressToBacklog_Requeue_ResetsAgentStatus(t *testing.T) {
	sock, _ := daemonTestEnv(t)
	startDaemonServer(t, sock)
	proj, tk, _ := scaffoldTicketDoneEnv(t)

	// Seed AgentStatus to a non-idle value so the reset is observable.
	// Without this seeding, the assertion "AgentStatus == AgentNone" is
	// vacuous because AgentNone is the zero value — a never-touched ticket
	// reads "none" regardless of whether the reset code executed.
	// REVERT PROOF: revert the AgentStatus-reset block in ticket_move.go
	// and this test fails (AgentStatus stays AgentWorking, not AgentNone).
	store, err := project.LoadTicketStore(proj)
	if err != nil {
		t.Fatalf("LoadTicketStore: %v", err)
	}
	loaded, err := store.Get(tk.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	loaded.SetAgentStatus(board.AgentWorking)
	if err := store.SaveTicket(loaded); err != nil {
		t.Fatalf("SaveTicket seed AgentWorking: %v", err)
	}

	spawnDaemonSessionForTicket(t, string(tk.ID))

	if err := runTicketMove(t, "test-proj", string(tk.ID), "backlog"); err != nil {
		t.Fatalf("runTicketMove: %v", err)
	}

	got := loadTicket(t, proj, tk.ID)
	if got.Status != board.StatusBacklog {
		t.Errorf("Status = %q; want backlog", got.Status)
	}
	if got.AgentStatus != board.AgentNone {
		t.Errorf("AgentStatus = %q; want none (re-queue reset)", got.AgentStatus)
	}
	if got.AgentStatus == board.AgentCompleted {
		t.Error("AgentStatus must NOT be completed on re-queue (sticky-terminal bug)")
	}

	// TicketDone fires on any exit from in_progress — session gone.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !daemonHasSessionForTicket(t, string(tk.ID)) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("daemon session still alive after in_progress→backlog")
}

func TestTicketMove_SameStatus_NoOp_TimestampsUnchanged_SessionSurvives(t *testing.T) {
	sock, _ := daemonTestEnv(t)
	startDaemonServer(t, sock)
	proj, tk, _ := scaffoldTicketDoneEnv(t)

	// Capture timestamps before the move. Sleep a beat so any re-stamp
	// would be observable.
	initial := loadTicket(t, proj, tk.ID)
	before := *initial.StatusChangedAt
	beforeUpdated := initial.UpdatedAt
	time.Sleep(5 * time.Millisecond)

	// Seed a live daemon session. If TicketDone is erroneously fired on
	// a no-op same-status move, the session disappears and we catch it.
	// REVERT PROOF: remove the early-return and StatusChangedAt drifts;
	// remove the teardown gate and the session gets unexpectedly killed.
	spawnDaemonSessionForTicket(t, string(tk.ID))

	if err := runTicketMove(t, "test-proj", string(tk.ID), "in_progress"); err != nil {
		t.Fatalf("same-status runTicketMove: %v", err)
	}

	got := loadTicket(t, proj, tk.ID)

	// StatusChangedAt must not drift on a no-op (SetStatus unconditionally
	// restamps when called, so only the early-return prevents the stamp).
	if !got.StatusChangedAt.Equal(before) {
		t.Errorf("StatusChangedAt drifted on same-status move: was %v, now %v",
			before, *got.StatusChangedAt)
	}
	if !got.UpdatedAt.Equal(beforeUpdated) {
		t.Errorf("UpdatedAt drifted on same-status move: was %v, now %v",
			beforeUpdated, got.UpdatedAt)
	}

	// Daemon session must survive (no teardown on a no-op).
	if !daemonHasSessionForTicket(t, string(tk.ID)) {
		t.Error("daemon session was unexpectedly killed by same-status no-op move")
	}
}

func TestTicketMove_DaemonDown_ExitZero(t *testing.T) {
	daemonTestEnv(t) // isolate socket; no startDaemonServer
	proj, tk, _ := scaffoldTicketDoneEnv(t)

	// in_progress → in_review triggers daemon teardown; with daemon down
	// notifyDaemonTicketDoneCLI returns nil (ErrDaemonUnavailable → nil).
	if err := runTicketMove(t, "test-proj", string(tk.ID), "in_review"); err != nil {
		t.Fatalf("expected exit 0 with daemon down; got: %v", err)
	}

	got := loadTicket(t, proj, tk.ID)
	if got.Status != board.StatusInReview {
		t.Errorf("Status = %q; want in_review", got.Status)
	}
}
