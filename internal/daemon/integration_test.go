package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
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

// startServerWithOptions is startServer with custom Options (e.g.
// Persistent: true). Returns the server and a channel that Serve's
// error (if any) is sent on.
func startServerWithOptions(t *testing.T, opts Options) (*Server, chan error) {
	t.Helper()
	sock, pid := testEnv(t)

	srv, err := NewServerWithOptions(sock, pid, opts)
	if err != nil {
		t.Fatalf("NewServerWithOptions: %v", err)
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

// TestPrepareExit_OtherTUIClients verifies that PrepareExitResp's
// OtherTUIClients counts TUI-named connections excluding the asker
// and excluding CLI clients. The `daemon stop` / `daemon restart`
// warning UX depends on this.
func TestPrepareExit_OtherTUIClients(t *testing.T) {
	srv, errCh := startServer(t)

	tui1 := dialTestClient(t, srv.SocketPath())
	rTui1 := bufio.NewReader(tui1)
	helloWithName(t, tui1, rTui1, ClientNameTUI)

	tui2 := dialTestClient(t, srv.SocketPath())
	rTui2 := bufio.NewReader(tui2)
	helloWithName(t, tui2, rTui2, ClientNameTUI)

	cli := dialTestClient(t, srv.SocketPath())
	rCli := bufio.NewReader(cli)
	helloWithName(t, cli, rCli, ClientNameCLI)

	// From tui1's PrepareExit, the other TUI count is 1 (tui2 only —
	// self excluded, CLI excluded).
	resp := prepareExit(t, tui1, rTui1)
	if resp.ClientCount != 3 {
		t.Errorf("ClientCount from tui1: got %d want 3", resp.ClientCount)
	}
	if resp.OtherTUIClients != 1 {
		t.Errorf("OtherTUIClients from tui1: got %d want 1 (tui2 only)", resp.OtherTUIClients)
	}

	// From cli's PrepareExit, both TUIs count.
	resp = prepareExit(t, cli, rCli)
	if resp.OtherTUIClients != 2 {
		t.Errorf("OtherTUIClients from cli: got %d want 2 (both TUIs)", resp.OtherTUIClients)
	}

	// Disconnect tui2; tui1's view should then show 0 other TUIs.
	tui2.Close()
	// Brief settle so the daemon updates its clients map.
	time.Sleep(50 * time.Millisecond)
	resp = prepareExit(t, tui1, rTui1)
	if resp.OtherTUIClients != 0 {
		t.Errorf("OtherTUIClients from tui1 after tui2 disconnect: got %d want 0", resp.OtherTUIClients)
	}

	tui1.Close()
	cli.Close()
	waitServerDone(t, errCh, 3*time.Second)
	_ = rTui2 // silence unused if loop short-circuits
}

// TestServerLifecycle_PersistentSurvivesLastDisconnect verifies that
// a daemon running with Options.Persistent stays accepting connections
// after the last client disconnects (the default-mode tests above
// assert the inverse).
func TestServerLifecycle_PersistentSurvivesLastDisconnect(t *testing.T) {
	srv, errCh := startServerWithOptions(t, Options{Persistent: true})

	c1 := dialTestClient(t, srv.SocketPath())
	r1 := bufio.NewReader(c1)
	helloWithName(t, c1, r1, ClientNameTUI)

	// Disconnect; in default mode the daemon would exit here. In
	// persistent mode it must stay up.
	c1.Close()
	time.Sleep(100 * time.Millisecond)

	// Confirm: dial a fresh connection. If the daemon exited, this
	// will fail with ECONNREFUSED / file-not-found.
	c2, err := net.Dial("unix", srv.SocketPath())
	if err != nil {
		t.Fatalf("post-disconnect dial: persistent daemon should still be listening but got: %v", err)
	}
	r2 := bufio.NewReader(c2)
	helloWithName(t, c2, r2, ClientNameTUI)

	// Ensure errCh has not received anything — daemon is still running.
	select {
	case err := <-errCh:
		t.Fatalf("persistent daemon exited unexpectedly: %v", err)
	default:
	}

	// Disconnect c2 and explicitly cancel via Shutdown so the test
	// can wind down (the test's cancel is hooked in t.Cleanup but
	// signaling cleanly is faster than waiting for timeout).
	c2.Close()

	// Trigger shutdown via the listener's accept loop catching the
	// context cancel from t.Cleanup; waitServerDone has its own
	// timeout to bound this.
	srv.initiateShutdown("test cleanup")
	waitServerDone(t, errCh, 3*time.Second)
}

// helloWithName issues a Hello with the given ClientName and validates
// the response is a HelloResp envelope.
func helloWithName(t *testing.T, conn net.Conn, r *bufio.Reader, name string) {
	t.Helper()
	writeReq(t, conn, MsgHelloReq, HelloReq{
		ProtocolVersion: ProtocolVersion,
		BinaryVersion:   "test",
		ClientName:      name,
	})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	typ, _ := readResp(t, r)
	if typ != MsgHelloResp {
		t.Fatalf("hello: got msg %q want %q", typ, MsgHelloResp)
	}
}

// prepareExit issues a PrepareExitReq and returns the decoded response.
func prepareExit(t *testing.T, conn net.Conn, r *bufio.Reader) PrepareExitResp {
	t.Helper()
	writeReq(t, conn, MsgPrepareExitReq, PrepareExitReq{})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	name, raw := readResp(t, r)
	if name != MsgPrepareExitResp {
		t.Fatalf("prepare_exit: got msg %q want %q", name, MsgPrepareExitResp)
	}
	var resp PrepareExitResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode PrepareExitResp: %v", err)
	}
	return resp
}


// cancelExitFor performs a CancelExit RPC. Helper for the exit-intent
// tests.
func cancelExit(t *testing.T, conn net.Conn, r *bufio.Reader) {
	t.Helper()
	writeReq(t, conn, MsgCancelExitReq, CancelExitReq{})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	name, _ := readResp(t, r)
	if name != MsgCancelExitResp {
		t.Fatalf("CancelExit: got msg %q want %q", name, MsgCancelExitResp)
	}
}

// TestServer_PrepareExit_ConcurrentExits fires PrepareExit from N
// clients simultaneously under a starting-gate WaitGroup. The atomic
// exit-intent design guarantees exactly one caller observes
// OtherActiveClients == 0, even when the calls land in the daemon at
// near-identical instants — the clientsMu acquire-and-flip-and-count
// step is serialized. We DON'T assert monotonic decrement across
// callers: interleaving means a later-completing caller may legitimately
// see more "others" than an earlier one (e.g. one caller's RPC finishes
// before another's flip is observed). "Exactly one sees 0" is the
// load-bearing invariant.
func TestServer_PrepareExit_ConcurrentExits(t *testing.T) {
	srv, errCh := startServer(t)

	const n = 5
	conns := make([]net.Conn, n)
	readers := make([]*bufio.Reader, n)
	for i := 0; i < n; i++ {
		conns[i] = dialTestClient(t, srv.SocketPath())
		readers[i] = bufio.NewReader(conns[i])
		helloAndUnpack(t, conns[i], readers[i])
	}

	// Starting gate: all goroutines block on `start` until released.
	var start sync.WaitGroup
	start.Add(1)
	resps := make([]PrepareExitResp, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait()
			resps[i] = prepareExit(t, conns[i], readers[i])
		}(i)
	}
	start.Done()
	wg.Wait()

	zeroSeen := 0
	for i, r := range resps {
		if r.OtherActiveClients == 0 {
			zeroSeen++
		}
		// Every response should also include the deprecated ClientCount
		// (total), which is N here (no one has disconnected yet).
		if r.ClientCount != n {
			t.Errorf("client %d ClientCount: got %d want %d", i, r.ClientCount, n)
		}
	}
	if zeroSeen != 1 {
		t.Errorf("expected exactly one caller to see OtherActiveClients==0; got %d (resps=%+v)",
			zeroSeen, resps)
	}

	// Tear down so the test cleanly completes.
	for _, c := range conns {
		c.Close()
	}
	waitServerDone(t, errCh, 5*time.Second)
}

// TestServer_CancelExit_ReversesFlag asserts that calling CancelExit
// reverses the exiting flag set by an earlier PrepareExit — a peer's
// next PrepareExit sees the original client as active again.
func TestServer_CancelExit_ReversesFlag(t *testing.T) {
	srv, errCh := startServer(t)

	a := dialTestClient(t, srv.SocketPath())
	rA := bufio.NewReader(a)
	helloAndUnpack(t, a, rA)

	b := dialTestClient(t, srv.SocketPath())
	rB := bufio.NewReader(b)
	helloAndUnpack(t, b, rB)

	// A prepares to exit; from A's POV B is still active (1).
	respA := prepareExit(t, a, rA)
	if respA.OtherActiveClients != 1 {
		t.Errorf("A PrepareExit OtherActiveClients: got %d want 1", respA.OtherActiveClients)
	}

	// B prepares to exit; from B's POV A has already flipped to exiting,
	// so the count of *active* others is 0.
	respB := prepareExit(t, b, rB)
	if respB.OtherActiveClients != 0 {
		t.Errorf("B PrepareExit OtherActiveClients (A exiting): got %d want 0", respB.OtherActiveClients)
	}

	// A cancels its exit; B's next PrepareExit should see A back to active.
	cancelExit(t, a, rA)
	respB2 := prepareExit(t, b, rB)
	if respB2.OtherActiveClients != 1 {
		t.Errorf("B PrepareExit after A CancelExit: got OtherActiveClients=%d want 1", respB2.OtherActiveClients)
	}

	a.Close()
	b.Close()
	waitServerDone(t, errCh, 3*time.Second)
}

// TestServerLifecycle_MultiTUI_NoDefensiveKill drives the multi-TUI
// close path end-to-end: client A (with a live session) prepares to
// exit while B is still attached, sees OtherActiveClients > 0,
// silent-quits. B then kills the session and disconnects. When the
// last-client-disconnect handler fires, sessions is empty, so the
// clean shutdown log fires — NOT the "exit-guard was bypassed" warn.
//
// Captures the standard log output for the duration of the test so we
// can assert the warning never appeared.
func TestServerLifecycle_MultiTUI_NoDefensiveKill(t *testing.T) {
	// Capture log output for the duration of this test.
	var logBuf syncBuffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prev) })

	srv, errCh := startServer(t)

	a := dialTestClient(t, srv.SocketPath())
	rA := bufio.NewReader(a)
	helloAndUnpack(t, a, rA)

	b := dialTestClient(t, srv.SocketPath())
	rB := bufio.NewReader(b)
	helloAndUnpack(t, b, rB)

	// A spawns a long-running session. After A leaves, B owns it.
	writeReq(t, a, MsgSpawnReq, SpawnReq{
		TicketID:    "TEST-MTUI",
		SessionName: "mtui-session",
		Command:     "/bin/sleep",
		Args:        []string{"30"},
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})
	a.SetReadDeadline(time.Now().Add(3 * time.Second))
	name, raw := readResp(t, rA)
	if name != MsgSpawnResp {
		t.Fatalf("spawn: got %q want %q", name, MsgSpawnResp)
	}
	var spawn SpawnResp
	if err := json.Unmarshal(raw, &spawn); err != nil {
		t.Fatalf("decode SpawnResp: %v", err)
	}
	a.SetReadDeadline(time.Time{})

	// A prepares to exit while B is still attached. OtherActiveClients
	// should be 1 → the TUI silent-quits.
	respA := prepareExit(t, a, rA)
	if respA.OtherActiveClients != 1 {
		t.Fatalf("A PrepareExit OtherActiveClients: got %d want 1", respA.OtherActiveClients)
	}
	a.Close()

	// Give the daemon a moment to process A's disconnect bookkeeping
	// (clients map shrinks under clientsMu).
	time.Sleep(100 * time.Millisecond)

	// B explicitly kills the session before exiting — the well-behaved
	// last-out path.
	writeReq(t, b, MsgKillReq, KillReq{
		SessionID:    spawn.SessionID,
		GraceSeconds: 1,
	})
	b.SetReadDeadline(time.Now().Add(5 * time.Second))
	name, _ = readResp(t, rB)
	if name != MsgKillResp {
		t.Fatalf("kill: got %q want %q", name, MsgKillResp)
	}
	b.SetReadDeadline(time.Time{})

	// B disconnects. Daemon should observe clients==0, sessions==0,
	// and log the clean-shutdown line, NOT the bypassed warning.
	b.Close()
	waitServerDone(t, errCh, 5*time.Second)

	out := logBuf.String()
	if strings.Contains(out, "exit-guard was bypassed") {
		t.Errorf("unexpected defensive-kill log in clean multi-TUI close:\n%s", out)
	}
	if !strings.Contains(out, "last client disconnected; shutting down") {
		t.Errorf("expected clean-shutdown log line; got:\n%s", out)
	}
}

// syncBuffer is a goroutine-safe in-memory buffer for capturing log
// output during a test. The daemon writes to log.Default() from
// multiple goroutines (read loops, shutdown, the spawned session's
// exit callback), and a bare bytes.Buffer would race under -race.
type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	return len(p), nil
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.buf)
}
