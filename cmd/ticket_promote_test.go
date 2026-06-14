package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/board"
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

	if err := ticketInReviewCmd.RunE(ticketInReviewCmd, nil); err != nil {
		t.Fatalf("ticketInReviewCmd.RunE: %v", err)
	}

	got := loadTicket(t, proj, tk.ID)
	if got.Status != board.StatusInReview {
		t.Errorf("Status = %q; want %q", got.Status, board.StatusInReview)
	}
	// AgentStatus must be untouched — review-promotion leaves the
	// live PTY alive and reflecting its current activity.
	if got.AgentStatus == board.AgentCompleted {
		t.Error("AgentStatus = completed; in-review must not flip agent_status")
	}
	// CompletedAt must NOT be set — we're not done.
	if got.CompletedAt != nil {
		t.Errorf("CompletedAt = %v; want nil on in_review", got.CompletedAt)
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
