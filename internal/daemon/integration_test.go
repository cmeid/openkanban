package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// --- Test scaffolding ---

// testEnv pins per-test paths via the env-var overrides so concurrent
// tests don't collide on the shared ~/.cache/openkanban/ files.
//
// macOS imposes a 104-byte limit on AF_UNIX paths, and t.TempDir()
// produces paths like /var/folders/.../TestName1234567890/001/ that
// frequently blow that budget once you append daemon.sock. We use
// os.MkdirTemp("", "okbd-") (which is rooted at $TMPDIR but with a
// short prefix) and t.Cleanup to remove it; the short prefix keeps
// us inside the 104-byte budget on macOS for any reasonable test
// name.
func testEnv(t *testing.T) (sockPath, pidPath string) {
	t.Helper()
	// /tmp is a symlink to /private/tmp on macOS but pathconf'd for
	// short AF_UNIX paths. Using it directly (rather than $TMPDIR /
	// t.TempDir()) sidesteps the 104-byte limit for socket paths.
	dir, err := os.MkdirTemp("/tmp", "okbd-")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sockPath = filepath.Join(dir, "d.sock")
	pidPath = filepath.Join(dir, "d.pid")
	logPath := filepath.Join(dir, "d.log")

	t.Setenv(EnvSocket, sockPath)
	t.Setenv(EnvPid, pidPath)
	t.Setenv(EnvLog, logPath)
	return sockPath, pidPath
}

// startServer constructs a Server on the test's socket/pid paths and
// runs Serve in a goroutine. Returns the server and a channel that
// the Serve error (if any) is sent on.
//
// NewServer binds the listener synchronously before returning, so
// callers may dial immediately. We do NOT probe the listener with a
// throwaway dial here because that would consume a ClientID and skew
// the ClientCount the tests assert on.
func startServer(t *testing.T) (*Server, chan error) {
	t.Helper()
	sock, pid := testEnv(t)

	srv, err := NewServer(sock, pid)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ctx)
	}()
	return srv, errCh
}

// dialTestClient opens a fresh connection to sock.
func dialTestClient(t *testing.T, sock string) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// writeReq encodes payload as a TypeJSONReq envelope and writes it.
func writeReq(t *testing.T, conn net.Conn, msgType string, payload any) {
	t.Helper()
	raw, err := EncodeMsg(msgType, payload)
	if err != nil {
		t.Fatalf("EncodeMsg: %v", err)
	}
	if err := WriteFrame(conn, TypeJSONReq, raw); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
}

// readResp reads one frame and decodes it as an envelope. Returns the
// type name and the raw payload. Fatal on read or decode failure.
func readResp(t *testing.T, r *bufio.Reader) (string, json.RawMessage) {
	t.Helper()
	if err := setReadDeadline(r, 3*time.Second); err != nil {
		t.Fatalf("setReadDeadline: %v", err)
	}
	typ, payload, err := ReadFrame(r)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if typ != TypeJSONResp {
		t.Fatalf("frame type: got 0x%02x want TypeJSONResp", typ)
	}
	name, raw, err := DecodeEnvelope(payload)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	return name, raw
}

// setReadDeadline is a no-op when r isn't an io.Conn-backed Reader.
// We just keep it as a hook for symmetry; the underlying conn is
// driven via direct deadlines in the helpers below.
func setReadDeadline(*bufio.Reader, time.Duration) error { return nil }

// helloAndUnpack does the obligatory Hello handshake and returns the
// daemon's ClientID for the connection.
func helloAndUnpack(t *testing.T, conn net.Conn, r *bufio.Reader) uint16 {
	t.Helper()
	writeReq(t, conn, MsgHelloReq, HelloReq{
		ProtocolVersion: ProtocolVersion,
		BinaryVersion:   "test",
		ClientName:      "integration-test",
	})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	name, raw := readResp(t, r)
	if name != MsgHelloResp {
		t.Fatalf("hello: got msg %q want %q", name, MsgHelloResp)
	}
	var resp HelloResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode HelloResp: %v", err)
	}
	if resp.ProtocolVersion != ProtocolVersion {
		t.Errorf("ProtocolVersion: got %d want %d", resp.ProtocolVersion, ProtocolVersion)
	}
	return resp.ClientID
}

// waitServerDone waits up to timeout for srv to return from Serve.
func waitServerDone(t *testing.T, errCh chan error, timeout time.Duration) {
	t.Helper()
	select {
	case err := <-errCh:
		if err != nil {
			t.Logf("server exited with error: %v", err)
		}
	case <-time.After(timeout):
		t.Fatalf("server did not exit within %s", timeout)
	}
}

// --- Tests ---

func TestServerLifecycle_ListEmpty(t *testing.T) {
	srv, errCh := startServer(t)

	conn := dialTestClient(t, srv.SocketPath())
	r := bufio.NewReader(conn)

	helloAndUnpack(t, conn, r)

	// Subscribe is currently a no-op but exercise the path.
	writeReq(t, conn, MsgSubscribeReq, SubscribeReq{})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	name, _ := readResp(t, r)
	if name != MsgSubscribeResp {
		t.Errorf("subscribe: got %q want %q", name, MsgSubscribeResp)
	}

	writeReq(t, conn, MsgListReq, ListReq{})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	name, raw := readResp(t, r)
	if name != MsgListResp {
		t.Errorf("list: got msg %q want %q", name, MsgListResp)
	}
	var list ListResp
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode ListResp: %v", err)
	}
	if len(list.Sessions) != 0 {
		t.Errorf("list with no spawns: got %d sessions, want 0", len(list.Sessions))
	}
	conn.SetReadDeadline(time.Time{})

	// Disconnect → daemon shuts down because the last client left.
	conn.Close()
	waitServerDone(t, errCh, 3*time.Second)
}

func TestServerLifecycle_SpawnEcho(t *testing.T) {
	srv, errCh := startServer(t)

	conn := dialTestClient(t, srv.SocketPath())
	r := bufio.NewReader(conn)
	helloAndUnpack(t, conn, r)

	writeReq(t, conn, MsgSpawnReq, SpawnReq{
		TicketID:    "TEST-1",
		SessionName: "spawn-echo",
		Command:     "/bin/echo",
		Args:        []string{"hi"},
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	name, raw := readResp(t, r)
	if name != MsgSpawnResp {
		t.Fatalf("spawn: got %q want %q, payload=%s", name, MsgSpawnResp, string(raw))
	}
	var spawn SpawnResp
	if err := json.Unmarshal(raw, &spawn); err != nil {
		t.Fatalf("decode SpawnResp: %v", err)
	}
	if spawn.SessionID == "" {
		t.Errorf("SpawnResp.SessionID empty")
	}
	if spawn.PID == 0 {
		t.Errorf("SpawnResp.PID is 0")
	}

	// List should include the session (it may or may not have
	// already exited; the daemon does not currently auto-remove on
	// exit — that's an explicit Kill).
	writeReq(t, conn, MsgListReq, ListReq{})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	name, raw = readResp(t, r)
	if name != MsgListResp {
		t.Fatalf("list: got %q want %q", name, MsgListResp)
	}
	var list ListResp
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode ListResp: %v", err)
	}
	found := false
	for _, s := range list.Sessions {
		if s.SessionID == spawn.SessionID {
			found = true
			if s.TicketID != "TEST-1" {
				t.Errorf("TicketID: got %q want %q", s.TicketID, "TEST-1")
			}
		}
	}
	if !found {
		t.Errorf("spawned session %s not in list (got %d sessions)", spawn.SessionID, len(list.Sessions))
	}

	// Best-effort Kill regardless of whether echo already exited.
	writeReq(t, conn, MsgKillReq, KillReq{SessionID: spawn.SessionID, GraceSeconds: 1})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	name, _ = readResp(t, r)
	if name != MsgKillResp && name != MsgErrorResp {
		t.Errorf("kill: got %q, want KillResp or ErrorResp", name)
	}
	conn.SetReadDeadline(time.Time{})

	conn.Close()
	waitServerDone(t, errCh, 3*time.Second)
}

func TestServerLifecycle_TwoClients(t *testing.T) {
	srv, errCh := startServer(t)

	c1 := dialTestClient(t, srv.SocketPath())
	r1 := bufio.NewReader(c1)
	helloAndUnpack(t, c1, r1)

	c2 := dialTestClient(t, srv.SocketPath())
	r2 := bufio.NewReader(c2)
	// Second client's hello should observe ClientCount=2.
	writeReq(t, c2, MsgHelloReq, HelloReq{
		ProtocolVersion: ProtocolVersion,
		BinaryVersion:   "test",
		ClientName:      "c2",
	})
	c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	name, raw := readResp(t, r2)
	if name != MsgHelloResp {
		t.Fatalf("c2 hello: got %q want %q", name, MsgHelloResp)
	}
	var hr HelloResp
	if err := json.Unmarshal(raw, &hr); err != nil {
		t.Fatalf("decode HelloResp: %v", err)
	}
	if hr.ClientCount != 2 {
		t.Errorf("ClientCount on second connect: got %d want 2", hr.ClientCount)
	}
	c2.SetReadDeadline(time.Time{})

	// c1 disconnects; daemon must still be running because c2 is
	// holding it open.
	c1.Close()

	// Brief settle, then issue a List on c2 to confirm liveness.
	time.Sleep(50 * time.Millisecond)
	writeReq(t, c2, MsgListReq, ListReq{})
	c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	name, _ = readResp(t, r2)
	if name != MsgListResp {
		t.Errorf("post-c1-disconnect list on c2: got %q want %q", name, MsgListResp)
	}
	c2.SetReadDeadline(time.Time{})

	// Now c2 disconnects → daemon exits.
	c2.Close()
	waitServerDone(t, errCh, 3*time.Second)
}

func TestServerLifecycle_PidLock(t *testing.T) {
	t.Parallel()

	// See note on testEnv re: AF_UNIX path length on macOS.
	dir, err := os.MkdirTemp("/tmp", "okbd-")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	pid := filepath.Join(dir, "d.pid")

	srv, err := NewServer(sock, pid)
	if err != nil {
		t.Fatalf("first NewServer: %v", err)
	}
	t.Cleanup(func() {
		// Pull the listener down so the file is freed.
		if srv.ln != nil {
			srv.ln.Close()
		}
		if srv.pidlock != nil {
			srv.pidlock.Release()
		}
	})

	sock2 := filepath.Join(dir, "daemon2.sock")
	_, err = NewServer(sock2, pid)
	if err == nil {
		t.Fatal("second NewServer: expected ErrAlreadyLocked, got nil")
	}
	var already *ErrAlreadyLocked
	if !errors.As(err, &already) {
		t.Fatalf("second NewServer: got %T (%v), want *ErrAlreadyLocked", err, err)
	}
}

func TestServerLifecycle_AttachSessionNotFound(t *testing.T) {
	srv, errCh := startServer(t)

	conn := dialTestClient(t, srv.SocketPath())
	r := bufio.NewReader(conn)
	helloAndUnpack(t, conn, r)

	writeReq(t, conn, MsgAttachReq, AttachReq{SessionID: "doesnt-matter", Cols: 80, Rows: 24})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	name, raw := readResp(t, r)
	if name != MsgErrorResp {
		t.Fatalf("attach: got %q want %q", name, MsgErrorResp)
	}
	var errResp ErrorResp
	if err := json.Unmarshal(raw, &errResp); err != nil {
		t.Fatalf("decode ErrorResp: %v", err)
	}
	if errResp.Code != "session_not_found" {
		t.Errorf("ErrorResp.Code: got %q want %q", errResp.Code, "session_not_found")
	}
	conn.SetReadDeadline(time.Time{})

	conn.Close()
	waitServerDone(t, errCh, 3*time.Second)
}

func TestServerLifecycle_LastClientWithLiveSessions(t *testing.T) {
	srv, errCh := startServer(t)

	conn := dialTestClient(t, srv.SocketPath())
	r := bufio.NewReader(conn)
	helloAndUnpack(t, conn, r)

	// Spawn a long-running session.
	writeReq(t, conn, MsgSpawnReq, SpawnReq{
		TicketID:    "TEST-LIVE",
		SessionName: "live-session",
		Command:     "/bin/sleep",
		Args:        []string{"30"},
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	name, raw := readResp(t, r)
	if name != MsgSpawnResp {
		t.Fatalf("spawn: got %q want %q", name, MsgSpawnResp)
	}
	var spawn SpawnResp
	if err := json.Unmarshal(raw, &spawn); err != nil {
		t.Fatalf("decode SpawnResp: %v", err)
	}
	conn.SetReadDeadline(time.Time{})

	// Reach into the server (same package) to grab a handle on the
	// session so we can verify the defensive kill drove it to
	// not-running.
	srv.sessionsMu.RLock()
	sess := srv.sessions[spawn.SessionID]
	srv.sessionsMu.RUnlock()
	if sess == nil {
		t.Fatalf("session %s not registered on server", spawn.SessionID)
	}
	if !sess.Running() {
		t.Fatalf("sleep session reported not running immediately after spawn")
	}

	// Drop the client without killing the session — this simulates
	// the exit-guard being bypassed.
	conn.Close()

	waitServerDone(t, errCh, 8*time.Second)

	// The defensive cleanup should have killed the session.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !sess.Running() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("session %s still running after last-client shutdown", spawn.SessionID)
}

// --- Concurrency smoke ---

// TestServer_ConcurrentClients fires several short-lived clients in
// parallel and checks the daemon survives until the last disconnects.
// This catches obvious races in clientsMu / sessionsMu bookkeeping
// under -race.
func TestServer_ConcurrentClients(t *testing.T) {
	srv, errCh := startServer(t)

	const n = 8
	var wg sync.WaitGroup
	// Hold one connection open for the full duration so the test
	// controls the daemon shutdown.
	holder := dialTestClient(t, srv.SocketPath())
	rHolder := bufio.NewReader(holder)
	helloAndUnpack(t, holder, rHolder)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn, err := net.Dial("unix", srv.SocketPath())
			if err != nil {
				t.Errorf("dial %d: %v", i, err)
				return
			}
			defer conn.Close()
			r := bufio.NewReader(conn)
			writeReq(t, conn, MsgHelloReq, HelloReq{
				ProtocolVersion: ProtocolVersion,
				BinaryVersion:   "test",
				ClientName:      fmt.Sprintf("c%d", i),
			})
			conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			typ, payload, err := ReadFrame(r)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					t.Errorf("c%d ReadFrame: %v", i, err)
				}
				return
			}
			if typ != TypeJSONResp {
				t.Errorf("c%d unexpected frame type 0x%02x", i, typ)
				return
			}
			name, _, _ := DecodeEnvelope(payload)
			if name != MsgHelloResp {
				t.Errorf("c%d got msg %q", i, name)
			}
		}(i)
	}
	wg.Wait()

	holder.Close()
	waitServerDone(t, errCh, 5*time.Second)
}
