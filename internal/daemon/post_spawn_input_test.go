package daemon

import (
	"bufio"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/terminal"
)

// TestHandleSpawn_PostSpawnInput_EchoesThroughPTY asserts the
// end-to-end SpawnReq.PostSpawnInput contract: a non-empty value
// gets written to the spawned child's PTY ~postSpawnInputDelay
// after handleSpawn registers the session.
//
// `cat` is used as a hermetic fixture — it echoes whatever lands on
// stdin back to stdout, so an OutputEvent carrying the post-spawn
// payload proves the timer actually wrote to the PTY master.
//
// We Subscribe to the session's underlying Pane (via the daemon's
// session registry) rather than attaching as a binary-mode client,
// which keeps the test focused on the spawn-side timer behavior.
func TestHandleSpawn_PostSpawnInput_EchoesThroughPTY(t *testing.T) {
	srv, errCh := startServer(t)

	conn := dialTestClient(t, srv.SocketPath())
	r := bufio.NewReader(conn)
	helloAndUnpack(t, conn, r)

	const payload = "hello-daemon-post-spawn\n"
	writeReq(t, conn, MsgSpawnReq, SpawnReq{
		TicketID:       "TEST-PSI-1",
		SessionName:    "post-spawn-input",
		Command:        "/bin/cat",
		Cols:           80,
		Rows:           24,
		Scrollback:     1000,
		PostSpawnInput: payload,
	})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	name, raw := readResp(t, r)
	if name != MsgSpawnResp {
		t.Fatalf("spawn: got %q want %q (payload=%s)", name, MsgSpawnResp, string(raw))
	}
	var spawn SpawnResp
	if err := json.Unmarshal(raw, &spawn); err != nil {
		t.Fatalf("decode SpawnResp: %v", err)
	}
	conn.SetReadDeadline(time.Time{})

	// Fetch the session and subscribe to its pane so we can observe
	// the bytes cat echoes back. Subscribe BEFORE the timer fires so
	// we don't miss the OutputEvent.
	srv.sessionsMu.RLock()
	sess, ok := srv.sessions[spawn.SessionID]
	srv.sessionsMu.RUnlock()
	if !ok {
		t.Fatalf("daemon lost session %s right after spawn", spawn.SessionID)
	}
	pane := sess.Pane()
	if pane == nil {
		t.Fatalf("session %s has nil pane", spawn.SessionID)
	}
	sub, unsub := pane.Subscribe()
	defer unsub()

	// 700ms budget: 500ms timer + slack for the OS to round-trip
	// through the PTY layer + cat's stdin/stdout pipeline.
	collected := drainUntilContains(t, sub, "hello-daemon-post-spawn", 1500*time.Millisecond)
	if !strings.Contains(collected, "hello-daemon-post-spawn") {
		t.Fatalf("PTY output never contained post-spawn payload; got %q", collected)
	}

	// Best-effort kill to free the pane.
	writeReq(t, conn, MsgKillReq, KillReq{SessionID: spawn.SessionID, GraceSeconds: 1})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = readResp(t, r)
	conn.SetReadDeadline(time.Time{})

	conn.Close()
	waitServerDone(t, errCh, 3*time.Second)
}

// TestHandleSpawn_PostSpawnInput_EmptyIsNoOp asserts the negative
// branch: an empty PostSpawnInput must not produce a spurious write.
// We spawn `cat` and read everything it emits for 700ms; cat with
// no stdin produces no output, so the buffer should be empty.
//
// This guards against a future regression where the daemon
// unconditionally schedules a timer that writes "" to the PTY (a
// zero-byte write is technically harmless on a Linux PTY but would
// be wrong on principle — and the empty-data guard lives in
// Pane.SchedulePostSpawnInput).
func TestHandleSpawn_PostSpawnInput_EmptyIsNoOp(t *testing.T) {
	srv, errCh := startServer(t)

	conn := dialTestClient(t, srv.SocketPath())
	r := bufio.NewReader(conn)
	helloAndUnpack(t, conn, r)

	writeReq(t, conn, MsgSpawnReq, SpawnReq{
		TicketID:    "TEST-PSI-2",
		SessionName: "post-spawn-input-empty",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
		// PostSpawnInput intentionally omitted.
	})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	name, raw := readResp(t, r)
	if name != MsgSpawnResp {
		t.Fatalf("spawn: got %q want %q (payload=%s)", name, MsgSpawnResp, string(raw))
	}
	var spawn SpawnResp
	if err := json.Unmarshal(raw, &spawn); err != nil {
		t.Fatalf("decode SpawnResp: %v", err)
	}
	conn.SetReadDeadline(time.Time{})

	srv.sessionsMu.RLock()
	sess, ok := srv.sessions[spawn.SessionID]
	srv.sessionsMu.RUnlock()
	if !ok {
		t.Fatalf("daemon lost session %s right after spawn", spawn.SessionID)
	}
	pane := sess.Pane()
	if pane == nil {
		t.Fatalf("session %s has nil pane", spawn.SessionID)
	}
	sub, unsub := pane.Subscribe()
	defer unsub()

	// Drain for 700ms (post-spawn delay + slack). With no input fed
	// to cat, we expect no OutputEvent at all.
	got := drainFor(t, sub, 700*time.Millisecond)
	if len(got) != 0 {
		t.Errorf("expected no PTY output for empty PostSpawnInput; got %q (%d bytes)", got, len(got))
	}

	writeReq(t, conn, MsgKillReq, KillReq{SessionID: spawn.SessionID, GraceSeconds: 1})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = readResp(t, r)
	conn.SetReadDeadline(time.Time{})

	conn.Close()
	waitServerDone(t, errCh, 3*time.Second)
}

// drainUntilContains reads OutputEvents from ch until the
// concatenated bytes contain needle or deadline elapses. Returns
// whatever was collected.
func drainUntilContains(t *testing.T, ch <-chan terminal.Event, needle string, deadline time.Duration) string {
	t.Helper()
	timeout := time.After(deadline)
	var buf []byte
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return string(buf)
			}
			if oe, ok := ev.(terminal.OutputEvent); ok {
				buf = append(buf, oe.Data...)
				if strings.Contains(string(buf), needle) {
					return string(buf)
				}
			}
		case <-timeout:
			return string(buf)
		}
	}
}

// drainFor reads OutputEvents for exactly the given duration and
// returns whatever was collected. Used to assert "no output appeared
// within the window".
func drainFor(t *testing.T, ch <-chan terminal.Event, deadline time.Duration) string {
	t.Helper()
	timeout := time.After(deadline)
	var buf []byte
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return string(buf)
			}
			if oe, ok := ev.(terminal.OutputEvent); ok {
				buf = append(buf, oe.Data...)
			}
		case <-timeout:
			return string(buf)
		}
	}
}
