package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
)

// spawnDaemonSessionWithTicket is a daemon_close_test.go-local variant
// of spawnDaemonSessionForUUID that lets us specify the TicketID (and
// also returns a unique SessionName). The shared helper hard-codes
// TicketID="T-DAEMON-OWNS" which would collide if a single test spawns
// multiple sessions and wants the daemon to keep both (the daemon
// dedups per TicketID — `handleSpawn` returns the existing SessionID
// on a second spawn for the same ticket).
//
// Returns the daemon-assigned SessionID. The underlying client is held
// open via t.Cleanup so the daemon doesn't shut down between RPCs.
func spawnDaemonSessionWithTicket(t *testing.T, sock, ticketID, sessionName, uuid string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := daemonclient.New(ctx)
	if err != nil {
		t.Fatalf("daemonclient.New: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	resp, err := c.Spawn(ctx, daemon.SpawnReq{
		TicketID:         ticketID,
		SessionName:      sessionName,
		Command:          "/bin/cat",
		Cols:             80,
		Rows:             24,
		Scrollback:       1000,
		AgentSessionUUID: uuid,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	return resp.SessionID
}

// listDaemonSessions is a short helper that opens a fresh client, runs
// List, and closes the client. Used for end-of-test assertions.
func listDaemonSessions(t *testing.T) []daemon.SessionInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := daemonclient.New(ctx)
	if err != nil {
		t.Fatalf("daemonclient.New: %v", err)
	}
	defer c.Close()
	resp, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return resp.Sessions
}

// waitForSessionCount polls List until the daemon reports exactly want
// sessions or the deadline expires. Kill is asynchronous w.r.t. the
// RPC return (the daemon spawns a goroutine to SIGTERM-then-SIGKILL),
// so a tight assertion right after the close call would flake.
func waitForSessionCount(t *testing.T, want int) []daemon.SessionInfo {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var sessions []daemon.SessionInfo
	for time.Now().Before(deadline) {
		sessions = listDaemonSessions(t)
		if len(sessions) == want {
			return sessions
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("waitForSessionCount: got %d sessions, want %d", len(sessions), want)
	return sessions
}

// --- Integration tests for daemon close -----------------------------------

// TestDaemonClose_BySessionIDPrefix spawns 2 sessions, closes one by
// its 8-char SessionID prefix, and asserts the OTHER session survives.
// This exercises the SessionID-prefix resolution path and verifies the
// kill RPC actually fired.
func TestDaemonClose_BySessionIDPrefix(t *testing.T) {
	sock, _ := daemonTestEnv(t)
	startDaemonServer(t, sock)

	sessA := spawnDaemonSessionWithTicket(t, sock, "T-A", "sess-a", "")
	sessB := spawnDaemonSessionWithTicket(t, sock, "T-B", "sess-b", "")

	// Use the 8-char prefix that `daemon list` would display.
	prefix := sessA[:8]
	plan, err := daemonCloseRun(context.Background(), prefix, 0, true)
	if err != nil {
		t.Fatalf("daemonCloseRun: %v", err)
	}
	if plan.Kind != "kill" {
		t.Fatalf("plan.Kind = %q want %q", plan.Kind, "kill")
	}
	if len(plan.Sessions) != 1 || plan.Sessions[0].SessionID != sessA {
		t.Errorf("plan.Sessions = %+v, want [%s]", plan.Sessions, sessA)
	}

	sessions := waitForSessionCount(t, 1)
	if sessions[0].SessionID != sessB {
		t.Errorf("survivor = %s, want %s (sessA should be gone)", sessions[0].SessionID, sessB)
	}
}

// TestDaemonClose_ByTicketID spawns 1 session for ticket T1 and closes
// it by passing the TicketID. Exercises the TicketDone fallback path.
func TestDaemonClose_ByTicketID(t *testing.T) {
	sock, _ := daemonTestEnv(t)
	startDaemonServer(t, sock)

	const ticketID = "T-CLOSE-BY-TICKET"
	sess := spawnDaemonSessionWithTicket(t, sock, ticketID, "sess-x", "")

	plan, err := daemonCloseRun(context.Background(), ticketID, 0, true)
	if err != nil {
		t.Fatalf("daemonCloseRun: %v", err)
	}
	if plan.Kind != "ticket_done" {
		t.Fatalf("plan.Kind = %q want %q", plan.Kind, "ticket_done")
	}
	if plan.TicketID != ticketID {
		t.Errorf("plan.TicketID = %q want %q", plan.TicketID, ticketID)
	}
	if len(plan.Sessions) != 1 || plan.Sessions[0].SessionID != sess {
		t.Errorf("plan.Sessions = %+v, want [%s]", plan.Sessions, sess)
	}

	waitForSessionCount(t, 0)
}

// TestDaemonClose_AmbiguousPrefix asserts an arg that prefixes multiple
// SessionIDs returns an error listing the candidates. SessionIDs are
// random 16-char hex; engineering a 4+ char collision by spawning is
// theoretically possible but in practice flaky. Instead we craft a list
// of synthetic SessionInfos with a known shared prefix and call the
// pure resolver function directly — no daemon needed.
func TestDaemonClose_AmbiguousPrefix(t *testing.T) {
	sessions := []daemon.SessionInfo{
		{SessionID: "abcd1111aaaaaaaa", TicketID: "T-1", PID: 1001},
		{SessionID: "abcd2222bbbbbbbb", TicketID: "T-2", PID: 1002},
	}
	_, err := resolveDaemonCloseArg("abcd", sessions)
	if err == nil {
		t.Fatalf("resolveDaemonCloseArg: want error, got nil")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error %q should mention 'ambiguous'", err.Error())
	}
	// Both candidates' short form should appear in the message so the
	// user can pick a longer prefix.
	for _, want := range []string{"abcd1111", "abcd2222"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should list candidate %q", err.Error(), want)
		}
	}
}

// TestDaemonClose_PrefixTooShort asserts a prefix shorter than
// minSessionPrefixLen is rejected — falls through to the "no match"
// branch (an empty TicketID match list yields the standard not-found
// error), preventing a 1-char "a" from killing the lexically-first
// session.
func TestDaemonClose_PrefixTooShort(t *testing.T) {
	sessions := []daemon.SessionInfo{
		{SessionID: "abcd1111aaaaaaaa", TicketID: "T-1", PID: 1001},
		{SessionID: "abef2222bbbbbbbb", TicketID: "T-2", PID: 1002},
	}
	// "ab" is a SessionID prefix of both but below minSessionPrefixLen.
	// With no ticket "ab" the resolver should report no match.
	_, err := resolveDaemonCloseArg("ab", sessions)
	if err == nil {
		t.Fatalf("resolveDaemonCloseArg(\"ab\"): want error, got nil")
	}
	if !strings.Contains(err.Error(), "no session") {
		t.Errorf("error %q should say 'no session'", err.Error())
	}
}

// TestDaemonClose_NoMatch asserts a bogus arg with no session/ticket
// match returns a clean error mentioning the listed session count.
func TestDaemonClose_NoMatch(t *testing.T) {
	sock, _ := daemonTestEnv(t)
	startDaemonServer(t, sock)

	spawnDaemonSessionWithTicket(t, sock, "T-X", "sess-x", "")

	_, err := daemonCloseRun(context.Background(), "no-such-id-anywhere", 0, true)
	if err == nil {
		t.Fatalf("daemonCloseRun: want error, got nil")
	}
	if !strings.Contains(err.Error(), "no session") {
		t.Errorf("error %q should say 'no session'", err.Error())
	}
	// The session we spawned must still be alive.
	if got := len(listDaemonSessions(t)); got != 1 {
		t.Errorf("session count after failed close = %d, want 1", got)
	}
}

// TestDaemonClose_NoDaemon asserts a clean error when the daemon socket
// is missing entirely. We point OPENKANBAN_DAEMON_SOCK at a path that
// doesn't exist and skip startDaemonServer.
func TestDaemonClose_NoDaemon(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "okbt-noddmn-")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sock := filepath.Join(dir, "nonexistent.sock")
	t.Setenv("OPENKANBAN_DAEMON_SOCK", sock)
	t.Setenv("OPENKANBAN_DAEMON_PID", filepath.Join(dir, "d.pid"))
	t.Setenv("OPENKANBAN_DAEMON_LOG", filepath.Join(dir, "d.log"))
	// Prevent autostart from forking a real daemon under us.
	t.Setenv("OPENKANBAN_DAEMON_BINARY", "/usr/bin/true")
	t.Setenv("HOME", dir)

	_, err = daemonCloseRun(context.Background(), "any-id", 0, true)
	if err == nil {
		t.Fatalf("daemonCloseRun: want error when daemon is down, got nil")
	}
	if !strings.Contains(err.Error(), "openkanbankd is not running") {
		t.Errorf("error %q should mention 'openkanbankd is not running'", err.Error())
	}
}

// TestDaemonClose_ExactSessionID asserts the exact-SessionID path
// (precedence rank 1) — passing the full SessionID resolves to a single
// kill plan even if the same string is ALSO a 16-char prefix.
func TestDaemonClose_ExactSessionID(t *testing.T) {
	sock, _ := daemonTestEnv(t)
	startDaemonServer(t, sock)

	sess := spawnDaemonSessionWithTicket(t, sock, "T-EXACT", "sess-exact", "")

	plan, err := daemonCloseRun(context.Background(), sess, 0, true)
	if err != nil {
		t.Fatalf("daemonCloseRun: %v", err)
	}
	if plan.Kind != "kill" {
		t.Fatalf("plan.Kind = %q want %q", plan.Kind, "kill")
	}
	if plan.Sessions[0].SessionID != sess {
		t.Errorf("plan.Sessions[0].SessionID = %q want %q", plan.Sessions[0].SessionID, sess)
	}
	waitForSessionCount(t, 0)
}
