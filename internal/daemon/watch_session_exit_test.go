package daemon

import (
	"bufio"
	"encoding/json"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// syncBuffer is defined in integration_test.go — reuse it.

// TestWatchSessionExit_PanicSafety asserts that a panic inside the
// "exited" emit step does NOT crash the daemon and does NOT leave
// the session leaked in the registry. The deferred cleanup runs
// removeSession() BEFORE emit(), so registry cleanup is preserved
// even when emit panics; the inner recover swallows the panic so
// the goroutine returns cleanly and Serve keeps running.
//
// Regression guard for the bug where any panic in a background
// daemon goroutine (broadcastEvents / watchBinaryStaleness /
// watchSessionExit) would crash the whole process — taking every
// live agent PTY with it.
func TestWatchSessionExit_PanicSafety(t *testing.T) {
	// Redirect log output so we can assert the panic was logged.
	// syncBuffer is goroutine-safe so the daemon's handleConn log
	// writes (on client connect/disconnect) don't race with the
	// test's reads at the end.
	var logBuf syncBuffer
	origWriter := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(origWriter) })

	srv, errCh := startServer(t)

	// Inject panicking emit BEFORE spawning. watchSessionExit reads
	// emitSessionExitFn once at goroutine start; we set it before
	// the spawn so the watcher picks it up.
	var emitCalls atomic.Int32
	srv.emitSessionExitFn = func(ev SessionEvent) {
		emitCalls.Add(1)
		panic("injected: emit failure")
	}

	conn := dialTestClient(t, srv.SocketPath())
	r := bufio.NewReader(conn)
	helloAndUnpack(t, conn, r)

	// Spawn /bin/sleep 0.2 — gives the watcher goroutine time to
	// subscribe to the pane before the child exits (sub-second is
	// fine; /usr/bin/true exits so fast that under -race the child
	// can finish before watchSessionExit's Subscribe is wired up,
	// dropping the ExitEvent on the floor). 0.2s exit then triggers
	// the watcher's deferred cleanup (removeSession then panicking
	// emit).
	writeReq(t, conn, MsgSpawnReq, SpawnReq{
		TicketID:    "PANIC-1",
		SessionName: "panic-safety",
		Command:     "/bin/sleep",
		Args:        []string{"0.2"},
		Cols:        80,
		Rows:        24,
		Scrollback:  100,
	})
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	name, raw := readResp(t, r)
	if name != MsgSpawnResp {
		t.Fatalf("spawn: got %q, payload=%s", name, string(raw))
	}
	var spawn SpawnResp
	if err := json.Unmarshal(raw, &spawn); err != nil {
		t.Fatalf("decode SpawnResp: %v", err)
	}
	conn.SetReadDeadline(time.Time{})

	// Wait for the watcher to process the exit. Polling
	// srv.sessions is the post-condition we care about: the
	// session should disappear despite emit panicking.
	deadline := time.Now().Add(3 * time.Second)
	removed := false
	for time.Now().Before(deadline) {
		if _, stillThere := srv.reg.get(spawn.SessionID); !stillThere {
			removed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !removed {
		t.Errorf("session %s still in registry after watcher cleanup; expected removal even though emit panicked", spawn.SessionID)
	}

	if got := emitCalls.Load(); got != 1 {
		t.Errorf("emit calls: got %d want 1 (cleanup defer must invoke emit exactly once)", got)
	}

	// Daemon must still be running. If the panic escaped, Serve
	// would have returned. Try a fresh RPC.
	c2 := dialTestClient(t, srv.SocketPath())
	r2 := bufio.NewReader(c2)
	helloAndUnpack(t, c2, r2)
	writeReq(t, c2, MsgListReq, ListReq{})
	c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	name, _ = readResp(t, r2)
	if name != MsgListResp {
		t.Errorf("daemon unresponsive after watcher panic: List returned %q", name)
	}

	// Panic should be in the log.
	logOut := logBuf.String()
	if !strings.Contains(logOut, "panic in watchSessionExit cleanup emit") {
		t.Errorf("expected log to contain panic notice; got:\n%s", logOut)
	}
	if !strings.Contains(logOut, "injected: emit failure") {
		t.Errorf("expected log to contain panic value; got:\n%s", logOut)
	}

	conn.Close()
	c2.Close()
	waitServerDone(t, errCh, 3*time.Second)
}
