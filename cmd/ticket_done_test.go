package cmd

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
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

// captureStderr swaps os.Stderr with an os.Pipe for the duration of fn
// and returns whatever was written to it. Used by the ticket-done tests
// to inspect the "openkanbankd: ..." warning lines without coupling to
// log.Printf or fmt destinations elsewhere.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	origStderr := os.Stderr
	os.Stderr = w

	var buf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&buf, r)
	}()

	fn()

	_ = w.Close()
	os.Stderr = origStderr
	wg.Wait()
	_ = r.Close()
	return buf.String()
}

// spawnDaemonSessionForTicket spawns a /bin/cat session bound to the
// given ticketID. Holds a long-lived client open via t.Cleanup so the
// daemon doesn't shut down between this call and the RPC under test.
// Returns the daemon-internal session ID.
func spawnDaemonSessionForTicket(t *testing.T, ticketID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := daemonclient.New(ctx)
	if err != nil {
		t.Fatalf("daemonclient.New: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	resp, err := c.Spawn(ctx, daemon.SpawnReq{
		TicketID:    ticketID,
		SessionName: "test-session",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	return resp.SessionID
}

// TestTicketDone_DaemonUp_OwnsTicket_SendsTicketDoneReq verifies that
// when the daemon is up and owns a session for the ticket, the CLI
// successfully delivers a TicketDoneReq and the daemon responds by
// killing the session. The List RPC after the run shows no remaining
// sessions for the ticket.
func TestTicketDone_DaemonUp_OwnsTicket_SendsTicketDoneReq(t *testing.T) {
	sock, _ := daemonTestEnv(t)
	startDaemonServer(t, sock)

	proj, tk, _ := scaffoldTicketDoneEnv(t)
	t.Setenv("OPENKANBAN_TICKET_ID", string(tk.ID))
	t.Setenv("OPENKANBAN_SESSION", "test-session")

	// Pre-stage a daemon-owned session for the ticket.
	daemonSessionID := spawnDaemonSessionForTicket(t, string(tk.ID))

	stderr := captureStderr(t, func() {
		if err := ticketDoneCmd.RunE(ticketDoneCmd, nil); err != nil {
			t.Fatalf("ticketDoneCmd.RunE: %v", err)
		}
	})

	// On success there's no warning line (Killed=true).
	if strings.Contains(stderr, "openkanbankd:") {
		t.Errorf("unexpected openkanbankd warning on happy path: %q", stderr)
	}

	// Ticket .md write should have happened (existing behavior).
	got := loadTicket(t, proj, tk.ID)
	if got.Status != board.StatusDone {
		t.Errorf("Status = %q; want %q", got.Status, board.StatusDone)
	}

	// And the daemon should no longer hold that session. Use a fresh
	// short-lived client to ask.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := daemonclient.New(ctx)
	if err != nil {
		t.Fatalf("daemonclient.New (post-check): %v", err)
	}
	defer c.Close()

	// Give the kill goroutine a moment to do its work.
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
	t.Errorf("daemon still holds session %s after ticket-done", daemonSessionID)
}

// TestTicketDone_DaemonUp_DoesNotOwn_StderrWarningExit0 verifies the
// soft-no-op path: when the daemon is up but doesn't have a session
// for the ticket, the CLI prints a stderr warning and exits 0. The
// on-disk .md write still happens.
func TestTicketDone_DaemonUp_DoesNotOwn_StderrWarningExit0(t *testing.T) {
	sock, _ := daemonTestEnv(t)
	startDaemonServer(t, sock)
	// Hold the daemon open so it doesn't shut down between our
	// scaffolding and the CLI's probe.
	holdDaemonOpen(t)

	proj, tk, _ := scaffoldTicketDoneEnv(t)
	t.Setenv("OPENKANBAN_TICKET_ID", string(tk.ID))
	t.Setenv("OPENKANBAN_SESSION", "test-session")

	var runErr error
	stderr := captureStderr(t, func() {
		runErr = ticketDoneCmd.RunE(ticketDoneCmd, nil)
	})
	if runErr != nil {
		t.Fatalf("ticketDoneCmd.RunE: %v", runErr)
	}
	if !strings.Contains(stderr, "openkanbankd:") {
		t.Errorf("expected stderr to mention openkanbankd; got %q", stderr)
	}
	if !strings.Contains(stderr, "no live session") {
		t.Errorf("expected stderr to say 'no live session'; got %q", stderr)
	}
	got := loadTicket(t, proj, tk.ID)
	if got.Status != board.StatusDone {
		t.Errorf("Status = %q; want %q", got.Status, board.StatusDone)
	}
}

// TestTicketDone_DaemonDown_StderrWarningExit0 verifies that with no
// daemon running, the CLI completes successfully (.md write done) but
// emits a stderr warning line. We intentionally do NOT call
// startDaemonServer.
func TestTicketDone_DaemonDown_StderrWarningExit0(t *testing.T) {
	daemonTestEnv(t)
	// NOT starting the daemon.

	proj, tk, _ := scaffoldTicketDoneEnv(t)
	t.Setenv("OPENKANBAN_TICKET_ID", string(tk.ID))
	t.Setenv("OPENKANBAN_SESSION", "test-session")

	var runErr error
	stderr := captureStderr(t, func() {
		runErr = ticketDoneCmd.RunE(ticketDoneCmd, nil)
	})
	if runErr != nil {
		t.Fatalf("ticketDoneCmd.RunE: %v", runErr)
	}
	if !strings.Contains(stderr, "openkanbankd:") {
		t.Errorf("expected stderr to mention openkanbankd; got %q", stderr)
	}
	got := loadTicket(t, proj, tk.ID)
	if got.Status != board.StatusDone {
		t.Errorf("Status = %q; want %q", got.Status, board.StatusDone)
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
