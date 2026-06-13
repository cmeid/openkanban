package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/project"
)

// scaffoldTicketDoneEnv stands up an isolated openkanban config dir
// with a project + one in_progress ticket, and returns the ticket
// (loaded from disk so it's the same shape the CLI will see).
func scaffoldTicketDoneEnv(t *testing.T) (proj *project.Project, ticket *board.Ticket, home string) {
	t.Helper()

	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OPENKANBAN_CONFIG_DIR", filepath.Join(home, ".config", "openkanban"))
	t.Setenv("XDG_CONFIG_HOME", "")

	registry, err := project.LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	proj = project.NewProject("test-proj", filepath.Join(home, "repo"))
	if err := registry.Add(proj); err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	store, err := project.LoadTicketStore(proj)
	if err != nil {
		t.Fatalf("LoadTicketStore: %v", err)
	}
	ticket = board.NewTicket("smoke", proj.ID)
	ticket.SetStatus(board.StatusInProgress)
	if err := store.SaveTicket(ticket); err != nil {
		t.Fatalf("SaveTicket: %v", err)
	}
	return proj, ticket, home
}

func loadTicket(t *testing.T, proj *project.Project, id board.TicketID) *board.Ticket {
	t.Helper()
	store, err := project.LoadTicketStore(proj)
	if err != nil {
		t.Fatalf("re-LoadTicketStore: %v", err)
	}
	tk, err := store.Get(id)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", id, err)
	}
	return tk
}

func TestTicketDone_HappyPath(t *testing.T) {
	proj, tk, _ := scaffoldTicketDoneEnv(t)

	t.Setenv("OPENKANBAN_TICKET_ID", string(tk.ID))
	t.Setenv("OPENKANBAN_SESSION", "test-session")

	if err := ticketDoneCmd.RunE(ticketDoneCmd, nil); err != nil {
		t.Fatalf("ticketDoneCmd.RunE: %v", err)
	}

	got := loadTicket(t, proj, tk.ID)
	if got.Status != board.StatusDone {
		t.Errorf("Status = %q; want %q", got.Status, board.StatusDone)
	}
	if got.AgentStatus != board.AgentCompleted {
		t.Errorf("AgentStatus = %q; want %q", got.AgentStatus, board.AgentCompleted)
	}
	if got.CompletedAt == nil {
		t.Error("CompletedAt should be stamped")
	}

	statusFile := filepath.Join(t.TempDir(), "ignored") // placeholder; real check below
	_ = statusFile
	home := os.Getenv("HOME")
	body, err := os.ReadFile(filepath.Join(home, ".cache", "openkanban-status", "test-session.status"))
	if err != nil {
		t.Fatalf("status file missing: %v", err)
	}
	if string(body) != "completed\n" {
		t.Errorf("status file body = %q; want %q", body, "completed\n")
	}
}

func TestTicketDone_IdempotentDoesNotRestampCompletedAt(t *testing.T) {
	proj, tk, _ := scaffoldTicketDoneEnv(t)

	// Pre-flip the ticket to Done with a known CompletedAt in the past.
	store, err := project.LoadTicketStore(proj)
	if err != nil {
		t.Fatalf("LoadTicketStore: %v", err)
	}
	loaded, err := store.Get(tk.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	past := time.Now().Add(-1 * time.Hour)
	loaded.Status = board.StatusDone
	loaded.CompletedAt = &past
	loaded.AgentStatus = board.AgentCompleted
	if err := store.SaveTicket(loaded); err != nil {
		t.Fatalf("SaveTicket: %v", err)
	}

	t.Setenv("OPENKANBAN_TICKET_ID", string(tk.ID))
	t.Setenv("OPENKANBAN_SESSION", "test-session")

	if err := ticketDoneCmd.RunE(ticketDoneCmd, nil); err != nil {
		t.Fatalf("ticketDoneCmd.RunE: %v", err)
	}

	got := loadTicket(t, proj, tk.ID)
	if got.CompletedAt == nil {
		t.Fatal("CompletedAt should not be nil after idempotent run")
	}
	// Tolerate a small jitter — but it must be the past timestamp, not a fresh now.
	if !got.CompletedAt.Equal(past) {
		t.Errorf("CompletedAt = %v; want preserved past value %v", *got.CompletedAt, past)
	}
}

func TestTicketDone_MissingTicketIDEnv(t *testing.T) {
	_, _, _ = scaffoldTicketDoneEnv(t)
	t.Setenv("OPENKANBAN_TICKET_ID", "")

	if err := ticketDoneCmd.RunE(ticketDoneCmd, nil); err == nil {
		t.Fatal("expected error when OPENKANBAN_TICKET_ID is unset")
	}
}

func TestTicketDone_TicketNotFound(t *testing.T) {
	_, _, _ = scaffoldTicketDoneEnv(t)
	t.Setenv("OPENKANBAN_TICKET_ID", "00000000-0000-0000-0000-000000000000")

	err := ticketDoneCmd.RunE(ticketDoneCmd, nil)
	if err == nil {
		t.Fatal("expected error when ticket id refers to a deleted/missing ticket")
	}
}

func TestTicketDone_NoStatusFileWhenSessionUnset(t *testing.T) {
	proj, tk, home := scaffoldTicketDoneEnv(t)

	t.Setenv("OPENKANBAN_TICKET_ID", string(tk.ID))
	t.Setenv("OPENKANBAN_SESSION", "")

	if err := ticketDoneCmd.RunE(ticketDoneCmd, nil); err != nil {
		t.Fatalf("ticketDoneCmd.RunE: %v", err)
	}

	// Ticket mutation still happened.
	got := loadTicket(t, proj, tk.ID)
	if got.Status != board.StatusDone {
		t.Errorf("Status = %q; want done", got.Status)
	}

	// No status file written (the cache dir might not even exist).
	entries, _ := os.ReadDir(filepath.Join(home, ".cache", "openkanban-status"))
	if len(entries) != 0 {
		t.Errorf("expected no status files; got %d entries", len(entries))
	}
}
