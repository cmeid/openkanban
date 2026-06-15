package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	xvt "github.com/charmbracelet/x/vt"

	"github.com/techdufus/openkanban/internal/terminal"
)

// spawnHelper spawns a session on conn using the standard Hello +
// Spawn dance. Returns the session ID.
func spawnHelper(t *testing.T, conn net.Conn, r *bufio.Reader, req SpawnReq) string {
	t.Helper()
	writeReq(t, conn, MsgSpawnReq, req)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	name, raw := readResp(t, r)
	conn.SetReadDeadline(time.Time{})
	if name != MsgSpawnResp {
		t.Fatalf("spawn: got %q want %q", name, MsgSpawnResp)
	}
	var resp SpawnResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode SpawnResp: %v", err)
	}
	if resp.SessionID == "" {
		t.Fatalf("SpawnResp.SessionID empty")
	}
	return resp.SessionID
}

// sendAttach writes an AttachReq on conn and returns the parsed
// envelope. On success returns ("attach.resp", AttachResp); on
// rejection ("error.resp", ErrorResp).
func sendAttach(t *testing.T, conn net.Conn, r *bufio.Reader, req AttachReq) (string, json.RawMessage) {
	t.Helper()
	writeReq(t, conn, MsgAttachReq, req)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	return readResp(t, r)
}

// readBinaryFrames keeps reading frames from r until it has either
// accumulated `want` bytes in OutputEvent payloads, observed
// TypeDetach, or timed out. Returns the accumulated bytes plus a
// detach flag.
func readBinaryFrames(t *testing.T, conn net.Conn, r *bufio.Reader, want int, deadline time.Duration) ([]byte, bool) {
	t.Helper()
	var collected []byte
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		typ, payload, err := ReadFrame(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return collected, true
			}
			// Timeout on individual read is fine — we're polling.
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				if len(collected) >= want {
					return collected, false
				}
				continue
			}
			t.Logf("readBinaryFrames: ReadFrame err: %v", err)
			return collected, false
		}
		switch typ {
		case TypePTYOutput:
			collected = append(collected, payload...)
			if want > 0 && len(collected) >= want {
				return collected, false
			}
		case TypeDetach:
			return collected, true
		default:
			t.Logf("readBinaryFrames: unexpected frame type 0x%02x len=%d", typ, len(payload))
		}
	}
	return collected, false
}

// readSnapshotBytes consumes exactly snapshotSize bytes from the
// connection (across one or more TypePTYOutput frames). Returns the
// concatenated bytes.
func readSnapshotBytes(t *testing.T, conn net.Conn, r *bufio.Reader, snapshotSize int) []byte {
	t.Helper()
	var collected []byte
	for len(collected) < snapshotSize {
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		typ, payload, err := ReadFrame(r)
		if err != nil {
			t.Fatalf("snapshot ReadFrame after %d/%d bytes: %v", len(collected), snapshotSize, err)
		}
		if typ != TypePTYOutput {
			t.Fatalf("snapshot: expected TypePTYOutput, got 0x%02x", typ)
		}
		collected = append(collected, payload...)
	}
	conn.SetReadDeadline(time.Time{})
	if len(collected) != snapshotSize {
		t.Fatalf("snapshot: got %d bytes, want exactly %d", len(collected), snapshotSize)
	}
	return collected
}

// attachAndUnpack performs the AttachReq + AttachResp + snapshot read.
// Returns the AttachResp and the snapshot bytes. Fatal on error.
func attachAndUnpack(t *testing.T, conn net.Conn, r *bufio.Reader, req AttachReq) (AttachResp, []byte) {
	t.Helper()
	name, raw := sendAttach(t, conn, r, req)
	if name != MsgAttachResp {
		t.Fatalf("attach: got msg %q want %q (payload=%s)", name, MsgAttachResp, string(raw))
	}
	var resp AttachResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode AttachResp: %v", err)
	}
	snapshot := readSnapshotBytes(t, conn, r, resp.SnapshotSize)
	return resp, snapshot
}

// writeBinaryFrame writes a single binary frame on conn.
func writeBinaryFrame(t *testing.T, conn net.Conn, typ byte, payload []byte) {
	t.Helper()
	if err := WriteFrame(conn, typ, payload); err != nil {
		t.Fatalf("WriteFrame type 0x%02x: %v", typ, err)
	}
}

// TestAttach_Echo spawns /bin/cat, attaches, types "hello\n" via
// TypePTYInput, and asserts the echoed "hello" comes back on
// TypePTYOutput.
func TestAttach_Echo(t *testing.T) {
	srv, errCh := startServer(t)

	conn := dialTestClient(t, srv.SocketPath())
	r := bufio.NewReader(conn)
	helloAndUnpack(t, conn, r)

	sessID := spawnHelper(t, conn, r, SpawnReq{
		TicketID:    "ECHO-1",
		SessionName: "echo-test",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})

	resp, snapshot := attachAndUnpack(t, conn, r, AttachReq{
		SessionID: sessID,
		Cols:      80,
		Rows:      24,
	})
	if resp.ClientID == 0 {
		t.Errorf("AttachResp.ClientID is 0")
	}
	if len(snapshot) == 0 {
		t.Errorf("snapshot empty: expected at least the RIS/CUP prologue")
	}

	// Now in binary mode. Send "hello\n" as TypePTYInput; /bin/cat
	// will echo "hello\n" back. The PTY may also echo input itself
	// (line discipline is on by default), so we expect "hello" twice
	// in some order — we just look for it once.
	writeBinaryFrame(t, conn, TypePTYInput, []byte("hello\n"))

	bytesGot, _ := readBinaryFrames(t, conn, r, 5, 3*time.Second)
	if !bytes.Contains(bytesGot, []byte("hello")) {
		t.Errorf("expected echo bytes to contain %q, got %q", "hello", bytesGot)
	}

	// Clean up: signal detach so the server breaks out of binary mode.
	writeBinaryFrame(t, conn, TypeDetach, nil)

	// Kill the cat session so the daemon shutdown path is clean.
	// We can't write JSON on this conn anymore (it's been in binary
	// mode), but the server transitions back to JSON-read after our
	// detach. Give it a moment, then issue Kill via a fresh conn so
	// last-client-disconnect still works.
	conn2 := dialTestClient(t, srv.SocketPath())
	r2 := bufio.NewReader(conn2)
	helloAndUnpack(t, conn2, r2)
	writeReq(t, conn2, MsgKillReq, KillReq{SessionID: sessID, GraceSeconds: 1})
	conn2.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = readResp(t, r2)
	conn2.SetReadDeadline(time.Time{})
	conn2.Close()

	conn.Close()
	waitServerDone(t, errCh, 5*time.Second)
}

// TestAttach_AlreadyAttached spawns a session, attaches one client,
// then tries to attach a second client and expects an
// "already_attached" error.
func TestAttach_AlreadyAttached(t *testing.T) {
	srv, errCh := startServer(t)

	c1 := dialTestClient(t, srv.SocketPath())
	r1 := bufio.NewReader(c1)
	helloAndUnpack(t, c1, r1)

	sessID := spawnHelper(t, c1, r1, SpawnReq{
		TicketID:    "ATT-1",
		SessionName: "attach-test",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})

	// c1 attaches successfully.
	_, _ = attachAndUnpack(t, c1, r1, AttachReq{
		SessionID: sessID,
		Cols:      80,
		Rows:      24,
	})

	// c2 connects and tries to attach to the same session.
	c2 := dialTestClient(t, srv.SocketPath())
	r2 := bufio.NewReader(c2)
	helloAndUnpack(t, c2, r2)

	name, raw := sendAttach(t, c2, r2, AttachReq{
		SessionID: sessID,
		Cols:      80,
		Rows:      24,
	})
	if name != MsgErrorResp {
		t.Fatalf("second attach: got %q want %q", name, MsgErrorResp)
	}
	var errResp ErrorResp
	if err := json.Unmarshal(raw, &errResp); err != nil {
		t.Fatalf("decode ErrorResp: %v", err)
	}
	if errResp.Code != "already_attached" {
		t.Errorf("ErrorResp.Code: got %q want %q", errResp.Code, "already_attached")
	}
	// Takeover semantics are exercised separately in
	// TestAttach_Takeover_* — here we only assert the Takeover=false
	// refusal path.

	// Drop c2 first, then c1 (with detach), then clean up the session.
	c2.Close()
	writeBinaryFrame(t, c1, TypeDetach, nil)

	// Cleanup via fresh conn.
	c3 := dialTestClient(t, srv.SocketPath())
	r3 := bufio.NewReader(c3)
	helloAndUnpack(t, c3, r3)
	writeReq(t, c3, MsgKillReq, KillReq{SessionID: sessID, GraceSeconds: 1})
	c3.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = readResp(t, r3)
	c3.SetReadDeadline(time.Time{})
	c3.Close()

	c1.Close()
	waitServerDone(t, errCh, 5*time.Second)
}

// TestAttach_DetachCleansUp verifies that after the client sends
// TypeDetach the session's attached field is cleared and a second
// attach (from any client) succeeds.
func TestAttach_DetachCleansUp(t *testing.T) {
	srv, errCh := startServer(t)

	c1 := dialTestClient(t, srv.SocketPath())
	r1 := bufio.NewReader(c1)
	helloAndUnpack(t, c1, r1)

	sessID := spawnHelper(t, c1, r1, SpawnReq{
		TicketID:    "DET-1",
		SessionName: "detach-test",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})

	_, _ = attachAndUnpack(t, c1, r1, AttachReq{
		SessionID: sessID,
		Cols:      80,
		Rows:      24,
	})

	// Send TypeDetach. After the server processes it, the conn is
	// back in JSON-read mode but we don't need to use it for that
	// anymore — we use a fresh conn to verify state.
	writeBinaryFrame(t, c1, TypeDetach, nil)

	// Poll: wait for sess.attached to be cleared. The detach is
	// asynchronous wrt our test goroutine.
	srv.sessionsMu.RLock()
	sess := srv.sessions[sessID]
	srv.sessionsMu.RUnlock()
	if sess == nil {
		t.Fatalf("session not registered post-spawn")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sess.attachMu.Lock()
		attached := sess.attached
		sess.attachMu.Unlock()
		if attached == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	sess.attachMu.Lock()
	if sess.attached != nil {
		sess.attachMu.Unlock()
		t.Fatalf("session.attached still set 2s after detach")
	}
	sess.attachMu.Unlock()

	// Now a fresh attach (on a NEW conn so we don't have to worry
	// about whether the old conn's JSON loop has resynchronized).
	c2 := dialTestClient(t, srv.SocketPath())
	r2 := bufio.NewReader(c2)
	helloAndUnpack(t, c2, r2)

	name, _ := sendAttach(t, c2, r2, AttachReq{
		SessionID: sessID,
		Cols:      80,
		Rows:      24,
	})
	if name != MsgAttachResp {
		t.Fatalf("re-attach: got %q want %q", name, MsgAttachResp)
	}
	// Don't bother reading the snapshot bytes — we proved the resp
	// arrived. Just clean up.
	writeBinaryFrame(t, c2, TypeDetach, nil)

	// Kill the session.
	c3 := dialTestClient(t, srv.SocketPath())
	r3 := bufio.NewReader(c3)
	helloAndUnpack(t, c3, r3)
	writeReq(t, c3, MsgKillReq, KillReq{SessionID: sessID, GraceSeconds: 1})
	c3.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = readResp(t, r3)
	c3.SetReadDeadline(time.Time{})

	c1.Close()
	c2.Close()
	c3.Close()
	waitServerDone(t, errCh, 5*time.Second)
}

// TestAttach_Resize sends a TypeResize frame and verifies the pane
// picks up the new dimensions.
func TestAttach_Resize(t *testing.T) {
	srv, errCh := startServer(t)

	conn := dialTestClient(t, srv.SocketPath())
	r := bufio.NewReader(conn)
	helloAndUnpack(t, conn, r)

	sessID := spawnHelper(t, conn, r, SpawnReq{
		TicketID:    "RES-1",
		SessionName: "resize-test",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})

	_, _ = attachAndUnpack(t, conn, r, AttachReq{
		SessionID: sessID,
		Cols:      80,
		Rows:      24,
	})

	// Resize to 120x40.
	writeBinaryFrame(t, conn, TypeResize, EncodeResize(120, 40, 0))

	// SetSize on the pane is synchronous wrt the daemon's binaryLoop
	// goroutine. Poll briefly to give that goroutine time to process
	// the frame.
	srv.sessionsMu.RLock()
	sess := srv.sessions[sessID]
	srv.sessionsMu.RUnlock()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w, h := sess.pane.Size()
		if w == 120 && h == 40 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	w, h := sess.pane.Size()
	if w != 120 || h != 40 {
		t.Errorf("post-resize Size: got %dx%d, want 120x40", w, h)
	}

	writeBinaryFrame(t, conn, TypeDetach, nil)

	// Cleanup
	c2 := dialTestClient(t, srv.SocketPath())
	r2 := bufio.NewReader(c2)
	helloAndUnpack(t, c2, r2)
	writeReq(t, c2, MsgKillReq, KillReq{SessionID: sessID, GraceSeconds: 1})
	c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = readResp(t, r2)
	c2.SetReadDeadline(time.Time{})
	c2.Close()

	conn.Close()
	waitServerDone(t, errCh, 5*time.Second)
}

// TestAttach_SnapshotOnReAttach verifies that detach + re-attach
// returns a snapshot reflecting the current state of the pane. The
// test feeds known bytes ("abc\r\nxyz") into the pane via input, then
// reads them back via cat's echo. After detach, it re-attaches and
// asserts the snapshot's bytes, when fed into a fresh emulator,
// reproduce the same visible text.
func TestAttach_SnapshotOnReAttach(t *testing.T) {
	srv, errCh := startServer(t)

	conn := dialTestClient(t, srv.SocketPath())
	r := bufio.NewReader(conn)
	helloAndUnpack(t, conn, r)

	sessID := spawnHelper(t, conn, r, SpawnReq{
		TicketID:    "SNAP-1",
		SessionName: "snap-test",
		Command:     "/bin/sh",
		Args:        []string{"-c", "printf 'abc\\nxyz\\n'; sleep 10"},
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})

	_, snapshot := attachAndUnpack(t, conn, r, AttachReq{
		SessionID: sessID,
		Cols:      80,
		Rows:      24,
	})
	_ = snapshot

	// Drain output until we see "xyz" or time out. The shell prints
	// "abc" and "xyz" on separate lines.
	bytesGot, _ := readBinaryFrames(t, conn, r, 6, 3*time.Second)
	if !bytes.Contains(bytesGot, []byte("xyz")) {
		t.Logf("first attach drain bytes: %q", bytesGot)
	}

	// Detach cleanly.
	writeBinaryFrame(t, conn, TypeDetach, nil)

	// Wait for the daemon to release sess.attached.
	srv.sessionsMu.RLock()
	sess := srv.sessions[sessID]
	srv.sessionsMu.RUnlock()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sess.attachMu.Lock()
		attached := sess.attached
		sess.attachMu.Unlock()
		if attached == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Re-attach on a fresh conn.
	conn2 := dialTestClient(t, srv.SocketPath())
	r2 := bufio.NewReader(conn2)
	helloAndUnpack(t, conn2, r2)

	_, snap2 := attachAndUnpack(t, conn2, r2, AttachReq{
		SessionID: sessID,
		Cols:      80,
		Rows:      24,
	})

	// Feed snap2 into a fresh emulator and verify "abc" and "xyz"
	// appear in the resulting grid. We DON'T require them to be on
	// specific rows because cat's echo + the explicit \n produces a
	// slightly different layout depending on whether the shell's
	// printf flushed in one or two chunks.
	em := xvt.NewSafeEmulator(80, 24)
	if _, err := em.Write(snap2); err != nil {
		t.Fatalf("emulator write snapshot: %v", err)
	}

	if !gridContains(em, "abc") {
		t.Errorf("re-attach snapshot grid missing %q\nfull grid:\n%s", "abc", gridDump(em))
	}
	if !gridContains(em, "xyz") {
		t.Errorf("re-attach snapshot grid missing %q\nfull grid:\n%s", "xyz", gridDump(em))
	}

	writeBinaryFrame(t, conn2, TypeDetach, nil)

	// Kill session.
	c3 := dialTestClient(t, srv.SocketPath())
	r3 := bufio.NewReader(c3)
	helloAndUnpack(t, c3, r3)
	writeReq(t, c3, MsgKillReq, KillReq{SessionID: sessID, GraceSeconds: 1})
	c3.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = readResp(t, r3)
	c3.SetReadDeadline(time.Time{})
	c3.Close()

	conn.Close()
	conn2.Close()
	waitServerDone(t, errCh, 5*time.Second)
}

// gridContains reports whether substr appears on any row of em.
func gridContains(em *xvt.SafeEmulator, substr string) bool {
	rows := em.Height()
	cols := em.Width()
	for row := 0; row < rows; row++ {
		var sb strings.Builder
		for col := 0; col < cols; col++ {
			ch := terminal.CellToGlyph(em.CellAt(col, row)).Char
			if ch == 0 {
				ch = ' '
			}
			sb.WriteRune(ch)
		}
		if strings.Contains(sb.String(), substr) {
			return true
		}
	}
	return false
}

// gridDump returns a printable dump of em's grid for diagnostics.
func gridDump(em *xvt.SafeEmulator) string {
	var sb strings.Builder
	rows := em.Height()
	cols := em.Width()
	for row := 0; row < rows; row++ {
		for col := 0; col < cols; col++ {
			ch := terminal.CellToGlyph(em.CellAt(col, row)).Char
			if ch == 0 {
				ch = ' '
			}
			sb.WriteRune(ch)
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// waitForDetachFrame reads frames on conn until it sees TypeDetach,
// the conn closes, or the deadline elapses. Returns true if TypeDetach
// was observed. Useful for assertions in takeover tests where the
// displaced client must receive a TypeDetach frame regardless of how
// much fan-out output preceded it.
func waitForDetachFrame(t *testing.T, conn net.Conn, r *bufio.Reader, deadline time.Duration) bool {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		typ, _, err := ReadFrame(r)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			if errors.Is(err, io.EOF) {
				return false
			}
			t.Logf("waitForDetachFrame: ReadFrame err: %v", err)
			return false
		}
		if typ == TypeDetach {
			conn.SetReadDeadline(time.Time{})
			return true
		}
		// TypePTYOutput and any other frame: drain and keep looking.
	}
	conn.SetReadDeadline(time.Time{})
	return false
}

// TestAttach_Takeover_DisplacesOldClient verifies the core takeover
// semantics: when client B attaches with Takeover=true to a session
// already held by client A, A receives a TypeDetach frame, B's attach
// succeeds with a non-empty snapshot, the underlying agent process
// keeps running, and B continues receiving output.
func TestAttach_Takeover_DisplacesOldClient(t *testing.T) {
	srv, errCh := startServer(t)

	c1 := dialTestClient(t, srv.SocketPath())
	r1 := bufio.NewReader(c1)
	helloAndUnpack(t, c1, r1)

	sessID := spawnHelper(t, c1, r1, SpawnReq{
		TicketID:    "TKO-1",
		SessionName: "takeover-test",
		Command:     "/bin/sh",
		Args:        []string{"-c", "while true; do echo tick; sleep 0.05; done"},
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})

	// C1 attaches and reads a couple of output frames so we know the
	// agent loop is running.
	_, _ = attachAndUnpack(t, c1, r1, AttachReq{
		SessionID: sessID,
		Cols:      80,
		Rows:      24,
	})
	if got, _ := readBinaryFrames(t, c1, r1, 4, 2*time.Second); len(got) == 0 {
		t.Fatalf("c1 received no output before takeover")
	}

	// C2 attaches with Takeover=true.
	c2 := dialTestClient(t, srv.SocketPath())
	r2 := bufio.NewReader(c2)
	helloAndUnpack(t, c2, r2)

	resp2, snap2 := attachAndUnpack(t, c2, r2, AttachReq{
		SessionID: sessID,
		Cols:      80,
		Rows:      24,
		Takeover:  true,
	})
	if resp2.ClientID == 0 {
		t.Errorf("takeover AttachResp.ClientID is 0")
	}
	if len(snap2) == 0 {
		t.Errorf("takeover snapshot empty: expected at least the RIS/CUP prologue")
	}

	// C1 must have received a TypeDetach frame.
	if !waitForDetachFrame(t, c1, r1, 2*time.Second) {
		t.Fatalf("c1 did not receive TypeDetach within 2s of takeover")
	}

	// Agent must still be running — takeover changes wire state only.
	srv.sessionsMu.RLock()
	sess := srv.sessions[sessID]
	srv.sessionsMu.RUnlock()
	if sess == nil {
		t.Fatalf("session not registered post-spawn")
	}
	if !sess.Running() {
		t.Errorf("agent stopped running after takeover")
	}

	// C2 should keep receiving output: the new fan-out is wired to
	// the same pane, which still has a live child.
	if got, _ := readBinaryFrames(t, c2, r2, 6, 2*time.Second); !bytes.Contains(got, []byte("tick")) {
		t.Errorf("c2 expected to receive 'tick' output after takeover, got %q", got)
	}

	// Cleanup.
	writeBinaryFrame(t, c2, TypeDetach, nil)

	c3 := dialTestClient(t, srv.SocketPath())
	r3 := bufio.NewReader(c3)
	helloAndUnpack(t, c3, r3)
	writeReq(t, c3, MsgKillReq, KillReq{SessionID: sessID, GraceSeconds: 1})
	c3.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = readResp(t, r3)
	c3.SetReadDeadline(time.Time{})
	c3.Close()

	c1.Close()
	c2.Close()
	waitServerDone(t, errCh, 5*time.Second)
}

// TestAttach_Takeover_AgentUnaffected proves the agent process keeps
// its stdin/stdout intact across a takeover by using /bin/cat as a
// simple echo: C1 writes "first" and reads it back, then C2 takes
// over, writes "second", and reads "second" back.
func TestAttach_Takeover_AgentUnaffected(t *testing.T) {
	srv, errCh := startServer(t)

	c1 := dialTestClient(t, srv.SocketPath())
	r1 := bufio.NewReader(c1)
	helloAndUnpack(t, c1, r1)

	sessID := spawnHelper(t, c1, r1, SpawnReq{
		TicketID:    "TKO-2",
		SessionName: "takeover-cat",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})

	_, _ = attachAndUnpack(t, c1, r1, AttachReq{
		SessionID: sessID,
		Cols:      80,
		Rows:      24,
	})

	// C1 writes "first" and observes the echo.
	writeBinaryFrame(t, c1, TypePTYInput, []byte("first\n"))
	if got, _ := readBinaryFrames(t, c1, r1, 5, 2*time.Second); !bytes.Contains(got, []byte("first")) {
		t.Fatalf("c1 did not see 'first' echo: got %q", got)
	}

	// C2 takes over.
	c2 := dialTestClient(t, srv.SocketPath())
	r2 := bufio.NewReader(c2)
	helloAndUnpack(t, c2, r2)
	_, _ = attachAndUnpack(t, c2, r2, AttachReq{
		SessionID: sessID,
		Cols:      80,
		Rows:      24,
		Takeover:  true,
	})

	// Drain C1's pending detach frame so handleConn can resume JSON
	// reads on c1 cleanly.
	if !waitForDetachFrame(t, c1, r1, 2*time.Second) {
		t.Fatalf("c1 did not receive TypeDetach")
	}

	// C2 writes "second" and reads it back. This proves cat's
	// stdin/stdout are intact — i.e. the agent process was not
	// touched by the takeover.
	writeBinaryFrame(t, c2, TypePTYInput, []byte("second\n"))
	if got, _ := readBinaryFrames(t, c2, r2, 6, 2*time.Second); !bytes.Contains(got, []byte("second")) {
		t.Errorf("c2 did not see 'second' echo (agent broken by takeover): got %q", got)
	}

	// Cleanup.
	writeBinaryFrame(t, c2, TypeDetach, nil)

	c3 := dialTestClient(t, srv.SocketPath())
	r3 := bufio.NewReader(c3)
	helloAndUnpack(t, c3, r3)
	writeReq(t, c3, MsgKillReq, KillReq{SessionID: sessID, GraceSeconds: 1})
	c3.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = readResp(t, r3)
	c3.SetReadDeadline(time.Time{})
	c3.Close()

	c1.Close()
	c2.Close()
	waitServerDone(t, errCh, 5*time.Second)
}

// TestAttach_SnapshotIncludesScrollback verifies that when a session
// has produced scrollback history before a client attaches, the
// snapshot byte stream carries that history (in addition to the live
// grid redraw). The assertion feeds the snapshot bytes into a
// scrollback-driving consumer that mirrors what the real client does
// during snapshot apply, and asserts the resulting ring is non-empty.
func TestAttach_SnapshotIncludesScrollback(t *testing.T) {
	srv, errCh := startServer(t)

	conn := dialTestClient(t, srv.SocketPath())
	r := bufio.NewReader(conn)
	helloAndUnpack(t, conn, r)

	// Spawn a shell that emits 60 lines (well past the 24-row grid)
	// then sleeps so the session is still alive when we re-attach.
	sessID := spawnHelper(t, conn, r, SpawnReq{
		TicketID:    "SCROLL-1",
		SessionName: "scrollback-attach",
		Command:     "/bin/sh",
		Args:        []string{"-c", "for i in $(seq 1 60); do echo line $i; done; sleep 30"},
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})

	// Attach once to start draining the PTY (the pane only populates
	// scrollback during its own handleOutput; the read loop runs as
	// soon as anyone subscribes — and Attach is what makes a
	// subscriber appear from the server's perspective).
	_, _ = attachAndUnpack(t, conn, r, AttachReq{
		SessionID: sessID,
		Cols:      80,
		Rows:      24,
	})

	// Drain output until we see "line 60" or time out — that tells us
	// the shell has finished emitting all 60 lines and the pane has
	// scrolled the early ones into scrollback.
	bytesGot, _ := readBinaryFrames(t, conn, r, 600, 5*time.Second)
	if !bytes.Contains(bytesGot, []byte("line 60")) {
		t.Logf("first attach drain bytes (last 200): %q", tail(bytesGot, 200))
	}

	writeBinaryFrame(t, conn, TypeDetach, nil)

	// Wait for daemon to release sess.attached so the next attach
	// proceeds without an already_attached error.
	srv.sessionsMu.RLock()
	sess := srv.sessions[sessID]
	srv.sessionsMu.RUnlock()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sess.attachMu.Lock()
		attached := sess.attached
		sess.attachMu.Unlock()
		if attached == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Confirm the pane's scrollback ring really has lines in it. If
	// not, the rest of the test is meaningless.
	if got := sess.pane.ScrollbackLen(); got == 0 {
		t.Fatalf("pane scrollback empty before re-attach; expected >0 lines after 60-line shell output")
	}

	// Re-attach on a fresh conn so the snapshot includes scrollback.
	conn2 := dialTestClient(t, srv.SocketPath())
	r2 := bufio.NewReader(conn2)
	helloAndUnpack(t, conn2, r2)

	_, snap2 := attachAndUnpack(t, conn2, r2, AttachReq{
		SessionID: sessID,
		Cols:      80,
		Rows:      24,
	})

	// Floor check: just the redraw of an 80x24 grid is roughly
	// (80 cols * ~few bytes + per-row CUP overhead + prologue) ≈
	// well under 10 KiB for an empty-ish grid. With 36 history rows
	// (60 emitted - 24 visible) at ~10 bytes per row plus the
	// redraw, the snapshot must comfortably exceed the redraw-only
	// floor. Assert > 1 KiB as a loose lower bound; the structural
	// assertion below is stronger.
	if len(snap2) < 1024 {
		t.Errorf("re-attach snapshot suspiciously small: %d bytes (expected >1KiB given 36 scrollback rows)", len(snap2))
	}

	// Structural assertion: the snapshot bytes must contain at least
	// one of the early history lines that have scrolled off the top
	// of the live grid. "line 1" through "line 36" are no longer
	// visible on a 24-row screen but should appear in the serialized
	// scrollback portion of the snapshot.
	foundHistory := false
	for i := 1; i <= 36; i++ {
		needle := []byte(fmt.Sprintf("line %d", i))
		if bytes.Contains(snap2, needle) {
			foundHistory = true
			break
		}
	}
	if !foundHistory {
		t.Errorf("re-attach snapshot does not contain any of lines 1-36 (scrollback history); snapshot size=%d", len(snap2))
	}

	writeBinaryFrame(t, conn2, TypeDetach, nil)

	c3 := dialTestClient(t, srv.SocketPath())
	r3 := bufio.NewReader(c3)
	helloAndUnpack(t, c3, r3)
	writeReq(t, c3, MsgKillReq, KillReq{SessionID: sessID, GraceSeconds: 1})
	c3.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = readResp(t, r3)
	c3.SetReadDeadline(time.Time{})
	c3.Close()

	conn.Close()
	conn2.Close()
	waitServerDone(t, errCh, 5*time.Second)
}

// tail returns the last n bytes of s (or all of s if shorter), for
// diagnostic logging on long byte streams.
func tail(s []byte, n int) []byte {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// TestAttach_Takeover_OldClientConnStaysAlive proves that a takeover
// only ends the displaced client's binary loop — its underlying conn
// is left open so it can resume JSON-mode RPCs. After being displaced,
// C1 sends a List request and the daemon answers it on the same conn.
func TestAttach_Takeover_OldClientConnStaysAlive(t *testing.T) {
	srv, errCh := startServer(t)

	c1 := dialTestClient(t, srv.SocketPath())
	r1 := bufio.NewReader(c1)
	helloAndUnpack(t, c1, r1)

	sessID := spawnHelper(t, c1, r1, SpawnReq{
		TicketID:    "TKO-3",
		SessionName: "takeover-conn-alive",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})

	_, _ = attachAndUnpack(t, c1, r1, AttachReq{
		SessionID: sessID,
		Cols:      80,
		Rows:      24,
	})

	// C2 takes over.
	c2 := dialTestClient(t, srv.SocketPath())
	r2 := bufio.NewReader(c2)
	helloAndUnpack(t, c2, r2)
	_, _ = attachAndUnpack(t, c2, r2, AttachReq{
		SessionID: sessID,
		Cols:      80,
		Rows:      24,
		Takeover:  true,
	})

	// C1 receives TypeDetach.
	if !waitForDetachFrame(t, c1, r1, 2*time.Second) {
		t.Fatalf("c1 did not receive TypeDetach after takeover")
	}

	// C1's conn must still be alive: send a List request and expect
	// a List response back. handleConn's outer JSON read loop should
	// have resumed reading from c1.r as soon as binaryLoop returned.
	writeReq(t, c1, MsgListReq, ListReq{})
	c1.SetReadDeadline(time.Now().Add(3 * time.Second))
	name, raw := readResp(t, r1)
	c1.SetReadDeadline(time.Time{})
	if name != MsgListResp {
		t.Fatalf("post-takeover List on c1: got %q want %q (conn broken?)", name, MsgListResp)
	}
	var lr ListResp
	if err := json.Unmarshal(raw, &lr); err != nil {
		t.Fatalf("decode ListResp: %v", err)
	}
	if len(lr.Sessions) == 0 {
		t.Errorf("post-takeover List: expected at least the takeover session, got 0")
	}

	// Cleanup.
	writeBinaryFrame(t, c2, TypeDetach, nil)

	c3 := dialTestClient(t, srv.SocketPath())
	r3 := bufio.NewReader(c3)
	helloAndUnpack(t, c3, r3)
	writeReq(t, c3, MsgKillReq, KillReq{SessionID: sessID, GraceSeconds: 1})
	c3.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = readResp(t, r3)
	c3.SetReadDeadline(time.Time{})
	c3.Close()

	c1.Close()
	c2.Close()
	waitServerDone(t, errCh, 5*time.Second)
}
