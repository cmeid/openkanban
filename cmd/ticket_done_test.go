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
	// Clear agent session env vars so tests start in "human context". Tests
	// that simulate agent context re-set these explicitly after scaffolding.
	t.Setenv("OPENKANBAN_SESSION", "")
	t.Setenv("OPENKANBAN_TICKET_ID", "")

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

	ticketDoneForce = true
	t.Cleanup(func() { ticketDoneForce = false })

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

// TestTicketDone_DaemonUp_LeavesSessionAlive is the CLI half of the
// preservation guarantee. `openkanban ticket done` is the agent saying
// "I'm handing this back" — a status change like any other, NOT the
// agent's own exit (the process keeps running after the command
// returns). So the daemon-owned session must still be there afterwards,
// ready for the user to press Enter on the done card and read the
// transcript.
//
// This inverts the old contract: the command used to send a TicketDone
// RPC that killed the PTY, and this test used to poll until the session
// disappeared.
func TestTicketDone_DaemonUp_LeavesSessionAlive(t *testing.T) {
	sock, _ := daemonTestEnv(t)
	startDaemonServer(t, sock)

	proj, tk, _ := scaffoldTicketDoneEnv(t)
	t.Setenv("OPENKANBAN_TICKET_ID", string(tk.ID))
	t.Setenv("OPENKANBAN_SESSION", "test-session")

	// Pre-stage a daemon-owned session for the ticket. Seeding it is what
	// makes the survival assertion non-vacuous.
	spawnDaemonSessionForTicket(t, string(tk.ID))

	ticketDoneForce = true
	t.Cleanup(func() { ticketDoneForce = false })

	stderr := captureStderr(t, func() {
		if err := ticketDoneCmd.RunE(ticketDoneCmd, nil); err != nil {
			t.Fatalf("ticketDoneCmd.RunE: %v", err)
		}
	})

	// The daemon isn't contacted at all any more, so nothing should be
	// reported about it either way.
	if strings.Contains(stderr, "openkanbankd:") {
		t.Errorf("stderr mentions openkanbankd; the daemon must not be contacted: %q", stderr)
	}

	// Ticket .md write is still authoritative.
	got := loadTicket(t, proj, tk.ID)
	if got.Status != board.StatusDone {
		t.Errorf("Status = %q; want %q", got.Status, board.StatusDone)
	}
	if got.AgentStatus != board.AgentCompleted {
		t.Errorf("AgentStatus = %q; want %q", got.AgentStatus, board.AgentCompleted)
	}

	requireSessionSurvives(t, string(tk.ID), 500*time.Millisecond)
}

// TestTicketDone_DaemonUp_NoSession_DoesNotContactDaemon covers the case
// where the daemon is up but holds no session for this ticket. There is
// nothing to warn about now that the RPC is gone, so stderr must be
// clean — the old "no live session for ticket …" line was a symptom of
// the teardown call this change removed.
func TestTicketDone_DaemonUp_NoSession_DoesNotContactDaemon(t *testing.T) {
	sock, _ := daemonTestEnv(t)
	startDaemonServer(t, sock)
	// Hold the daemon open so "no warning" can't be confused with "the
	// daemon went away".
	holdDaemonOpen(t)

	proj, tk, _ := scaffoldTicketDoneEnv(t)
	t.Setenv("OPENKANBAN_TICKET_ID", string(tk.ID))
	t.Setenv("OPENKANBAN_SESSION", "test-session")

	ticketDoneForce = true
	t.Cleanup(func() { ticketDoneForce = false })

	var runErr error
	stderr := captureStderr(t, func() {
		runErr = ticketDoneCmd.RunE(ticketDoneCmd, nil)
	})
	if runErr != nil {
		t.Fatalf("ticketDoneCmd.RunE: %v", runErr)
	}
	if strings.Contains(stderr, "openkanbankd:") {
		t.Errorf("stderr mentions openkanbankd; the daemon must not be contacted: %q", stderr)
	}
	got := loadTicket(t, proj, tk.ID)
	if got.Status != board.StatusDone {
		t.Errorf("Status = %q; want %q", got.Status, board.StatusDone)
	}
}

// TestTicketDone_DaemonDown_Exit0Quietly verifies that with no daemon
// running the CLI still completes (.md write done) and stays silent. A
// scripted invocation must never autostart a daemon, and it no longer
// needs one at all — so the old "openkanbankd: <dial error>" warning
// line must be gone too. We intentionally do NOT call startDaemonServer.
func TestTicketDone_DaemonDown_Exit0Quietly(t *testing.T) {
	daemonTestEnv(t)
	// NOT starting the daemon.

	proj, tk, _ := scaffoldTicketDoneEnv(t)
	t.Setenv("OPENKANBAN_TICKET_ID", string(tk.ID))
	t.Setenv("OPENKANBAN_SESSION", "test-session")

	ticketDoneForce = true
	t.Cleanup(func() { ticketDoneForce = false })

	var runErr error
	stderr := captureStderr(t, func() {
		runErr = ticketDoneCmd.RunE(ticketDoneCmd, nil)
	})
	if runErr != nil {
		t.Fatalf("ticketDoneCmd.RunE: %v", runErr)
	}
	if strings.Contains(stderr, "openkanbankd:") {
		t.Errorf("stderr mentions openkanbankd with no daemon running; want silence: %q", stderr)
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

	ticketDoneForce = true
	t.Cleanup(func() { ticketDoneForce = false })

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

// TestTicketDone_RefusedFromAgentSession verifies the hard gate: when
// run from inside an agent session (env vars set) without --force, the
// command returns an error and the ticket status is unchanged on disk.
// Red-before-green: commenting out the guardAgentStatusChange call in
// wrapUpSessionTicketAt must make this test fail.
func TestTicketDone_RefusedFromAgentSession(t *testing.T) {
	proj, tk, _ := scaffoldTicketDoneEnv(t) // seeds ticket as in_progress

	t.Setenv("OPENKANBAN_TICKET_ID", string(tk.ID))
	t.Setenv("OPENKANBAN_SESSION", "agent-session")
	// ticketDoneForce deliberately left false (the default).

	err := ticketDoneCmd.RunE(ticketDoneCmd, nil)
	if err == nil {
		t.Fatal("expected error from agent context without --force; got nil")
	}

	// Ticket must remain in_progress on disk — the gate must not have
	// allowed the status mutation through.
	got := loadTicket(t, proj, tk.ID)
	if got.Status != board.StatusInProgress {
		t.Errorf("Status = %q after refused gate; want in_progress (ticket must be unchanged)", got.Status)
	}
}

// TestTicketDone_AllowedWithForce verifies that --force lets the
// command through from an agent session.
func TestTicketDone_AllowedWithForce(t *testing.T) {
	proj, tk, _ := scaffoldTicketDoneEnv(t)

	t.Setenv("OPENKANBAN_TICKET_ID", string(tk.ID))
	t.Setenv("OPENKANBAN_SESSION", "agent-session")

	ticketDoneForce = true
	t.Cleanup(func() { ticketDoneForce = false })

	if err := ticketDoneCmd.RunE(ticketDoneCmd, nil); err != nil {
		t.Fatalf("ticketDoneCmd.RunE with --force: %v", err)
	}

	got := loadTicket(t, proj, tk.ID)
	if got.Status != board.StatusDone {
		t.Errorf("Status = %q; want done", got.Status)
	}
}
