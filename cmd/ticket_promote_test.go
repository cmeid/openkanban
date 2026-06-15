package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

func TestTicketInProgress_FromBacklog(t *testing.T) {
	proj, tk, _ := scaffoldTicketDoneEnv(t)

	// scaffoldTicketDoneEnv leaves the ticket in StatusInProgress —
	// reset to Backlog so this test actually exercises the transition.
	store, err := project.LoadTicketStore(proj)
	if err != nil {
		t.Fatalf("LoadTicketStore: %v", err)
	}
	tk.Status = board.StatusBacklog
	tk.StartedAt = nil
	if err := store.SaveTicket(tk); err != nil {
		t.Fatalf("seed Backlog: %v", err)
	}

	t.Setenv("OPENKANBAN_TICKET_ID", string(tk.ID))

	if err := ticketInProgressCmd.RunE(ticketInProgressCmd, nil); err != nil {
		t.Fatalf("ticketInProgressCmd.RunE: %v", err)
	}

	got := loadTicket(t, proj, tk.ID)
	if got.Status != board.StatusInProgress {
		t.Errorf("Status = %q; want %q", got.Status, board.StatusInProgress)
	}
	if got.StartedAt == nil {
		t.Error("StartedAt is nil after promotion to in_progress")
	}
}

func TestTicketInProgress_IdempotentNoRestartStamp(t *testing.T) {
	proj, tk, _ := scaffoldTicketDoneEnv(t)
	// scaffold already set StartedAt; capture and verify it doesn't move.
	originalStart := *loadTicket(t, proj, tk.ID).StartedAt

	t.Setenv("OPENKANBAN_TICKET_ID", string(tk.ID))

	// Sleep a beat so a re-stamp would be observable.
	time.Sleep(5 * time.Millisecond)

	if err := ticketInProgressCmd.RunE(ticketInProgressCmd, nil); err != nil {
		t.Fatalf("ticketInProgressCmd.RunE: %v", err)
	}

	got := loadTicket(t, proj, tk.ID)
	if got.Status != board.StatusInProgress {
		t.Errorf("Status = %q; want %q", got.Status, board.StatusInProgress)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(originalStart) {
		t.Errorf("StartedAt drifted on idempotent invocation: got %v, want %v",
			got.StartedAt, originalStart)
	}
}

func TestTicketInReview_FromInProgress(t *testing.T) {
	proj, tk, _ := scaffoldTicketDoneEnv(t)
	t.Setenv("OPENKANBAN_TICKET_ID", string(tk.ID))
	t.Setenv("OPENKANBAN_SESSION", "test-session")

	if err := ticketInReviewCmd.RunE(ticketInReviewCmd, nil); err != nil {
		t.Fatalf("ticketInReviewCmd.RunE: %v", err)
	}

	got := loadTicket(t, proj, tk.ID)
	if got.Status != board.StatusInReview {
		t.Errorf("Status = %q; want %q", got.Status, board.StatusInReview)
	}
	// AgentStatus must flip to completed — in-review now mirrors done's
	// "/quit equivalent" motion, so the badge marks the agent as having
	// wrapped up its work even though the column says In Review.
	if got.AgentStatus != board.AgentCompleted {
		t.Errorf("AgentStatus = %q; want %q", got.AgentStatus, board.AgentCompleted)
	}
	// CompletedAt must NOT be set — we're not done.
	if got.CompletedAt != nil {
		t.Errorf("CompletedAt = %v; want nil on in_review", got.CompletedAt)
	}

	// Status file must be written so TUIs that aren't subscribed to
	// the daemon push channel still see the completion via the poll.
	home := os.Getenv("HOME")
	body, err := os.ReadFile(filepath.Join(home, ".cache", "openkanban-status", "test-session.status"))
	if err != nil {
		t.Fatalf("status file missing: %v", err)
	}
	if string(body) != "completed\n" {
		t.Errorf("status file body = %q; want %q", body, "completed\n")
	}
}

func TestTicketInReview_IdempotentOnSecondInvocation(t *testing.T) {
	proj, tk, _ := scaffoldTicketDoneEnv(t)
	t.Setenv("OPENKANBAN_TICKET_ID", string(tk.ID))

	if err := ticketInReviewCmd.RunE(ticketInReviewCmd, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	firstUpdate := loadTicket(t, proj, tk.ID).UpdatedAt

	time.Sleep(5 * time.Millisecond)

	if err := ticketInReviewCmd.RunE(ticketInReviewCmd, nil); err != nil {
		t.Fatalf("second run: %v", err)
	}
	secondUpdate := loadTicket(t, proj, tk.ID).UpdatedAt

	// Idempotent: UpdatedAt should NOT advance on a no-op promotion.
	if !secondUpdate.Equal(firstUpdate) {
		t.Errorf("UpdatedAt drifted on idempotent invocation: %v -> %v",
			firstUpdate, secondUpdate)
	}
}

// TestTicketInReview_DaemonUp_OwnsTicket_SendsTicketDoneReq verifies
// the daemon-side termination path: when a live session is bound to
// the ticket, `ticket in-review` delivers the TicketDoneReq RPC and
// the daemon kills the session. Mirrors the equivalent done test.
func TestTicketInReview_DaemonUp_OwnsTicket_SendsTicketDoneReq(t *testing.T) {
	sock, _ := daemonTestEnv(t)
	startDaemonServer(t, sock)

	proj, tk, _ := scaffoldTicketDoneEnv(t)
	t.Setenv("OPENKANBAN_TICKET_ID", string(tk.ID))
	t.Setenv("OPENKANBAN_SESSION", "test-session")

	daemonSessionID := spawnDaemonSessionForTicket(t, string(tk.ID))

	stderr := captureStderr(t, func() {
		if err := ticketInReviewCmd.RunE(ticketInReviewCmd, nil); err != nil {
			t.Fatalf("ticketInReviewCmd.RunE: %v", err)
		}
	})

	if strings.Contains(stderr, "openkanbankd:") {
		t.Errorf("unexpected openkanbankd warning on happy path: %q", stderr)
	}

	got := loadTicket(t, proj, tk.ID)
	if got.Status != board.StatusInReview {
		t.Errorf("Status = %q; want %q", got.Status, board.StatusInReview)
	}
	if got.AgentStatus != board.AgentCompleted {
		t.Errorf("AgentStatus = %q; want %q", got.AgentStatus, board.AgentCompleted)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := daemonclient.New(ctx)
	if err != nil {
		t.Fatalf("daemonclient.New (post-check): %v", err)
	}
	defer c.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		list, err := c.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		stillThere := false
		for _, s := range list.Sessions {
			if s.SessionID == daemonSessionID {
				stillThere = true
				break
			}
		}
		if !stillThere {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("daemon still holds session %s after ticket-in-review", daemonSessionID)
}

func TestPromoteSessionTicket_MissingEnvVar(t *testing.T) {
	scaffoldTicketDoneEnv(t)
	// Make sure no prior test bleed sets the env.
	t.Setenv("OPENKANBAN_TICKET_ID", "")

	err := promoteSessionTicketTo(board.StatusInReview)
	if err == nil {
		t.Fatal("expected error when OPENKANBAN_TICKET_ID is unset, got nil")
	}
	if !strings.Contains(err.Error(), "OPENKANBAN_TICKET_ID") {
		t.Errorf("error %q should mention the env var", err)
	}
}

func TestPromoteSessionTicket_UnknownTicket(t *testing.T) {
	scaffoldTicketDoneEnv(t)
	t.Setenv("OPENKANBAN_TICKET_ID", "00000000-0000-0000-0000-000000000000")

	err := promoteSessionTicketTo(board.StatusInReview)
	if err == nil {
		t.Fatal("expected error for unknown ticket id, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q should mention 'not found'", err)
	}
}
