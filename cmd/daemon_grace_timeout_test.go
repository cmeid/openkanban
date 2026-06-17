package cmd

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/daemon"
)

// TestGraceTimeout pins the budget property: for n>=1 the deadline must
// exceed BOTH the worst-case serial drain (n*grace) and rpcTimeout, and
// n==0 collapses to rpcTimeout (fast-fail preserved against a wedged
// daemon). This is the math; the wiring is proven by the integration
// test below.
func TestGraceTimeout(t *testing.T) {
	if got := graceTimeout(0, 3*time.Second); got != rpcTimeout {
		t.Errorf("graceTimeout(0) = %v, want rpcTimeout %v", got, rpcTimeout)
	}
	for _, n := range []int{1, 6} {
		g := 3 * time.Second
		got := graceTimeout(n, g)
		if worst := time.Duration(n) * g; got <= worst {
			t.Errorf("graceTimeout(%d, %v) = %v, must exceed worst-case drain %v", n, g, got, worst)
		}
		if got <= rpcTimeout {
			t.Errorf("graceTimeout(%d, %v) = %v, must exceed rpcTimeout %v", n, g, got, rpcTimeout)
		}
	}
}

// slowKillDaemon binds a Unix socket and serves exactly the hello → list →
// kill sequence `daemon close <id>` issues, replying to hello/list
// instantly but DELAYING the kill response by killDelay — modelling a
// HEALTHY daemon whose handleKill blocks on sess.Kill(grace) before it
// replies. (Distinct from wedgedListener, which never replies at all.)
func slowKillDaemon(t *testing.T, sessionID string, killDelay time.Duration) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "okslow-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen %s: %v", sock, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	t.Setenv("OPENKANBAN_DAEMON_SOCK", sock)
	t.Setenv("OPENKANBAN_DAEMON_BINARY", "/usr/bin/true")

	writeResp := func(conn net.Conn, name string, payload any) error {
		raw, err := daemon.EncodeMsg(name, payload)
		if err != nil {
			return err
		}
		return daemon.WriteFrame(conn, daemon.TypeJSONResp, raw)
	}

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)

		// hello (instant)
		if _, _, err := daemon.ReadFrame(r); err != nil {
			return
		}
		if err := writeResp(conn, daemon.MsgHelloResp, daemon.HelloResp{
			ProtocolVersion: daemon.ProtocolVersion,
			BinaryVersion:   "fake",
		}); err != nil {
			return
		}
		// list (instant) — one session matching the requested id
		if _, _, err := daemon.ReadFrame(r); err != nil {
			return
		}
		if err := writeResp(conn, daemon.MsgListResp, daemon.ListResp{
			Sessions: []daemon.SessionInfo{{SessionID: sessionID, TicketID: "T-SLOW", Running: true}},
		}); err != nil {
			return
		}
		// kill (delayed — the by-design grace block)
		if _, _, err := daemon.ReadFrame(r); err != nil {
			return
		}
		time.Sleep(killDelay)
		_ = writeResp(conn, daemon.MsgKillResp, daemon.KillResp{})
	}()
}

// TestDaemonCloseRun_SlowKillNotUnresponsive proves the wiring: a healthy
// daemon whose kill reply lands AFTER rpcTimeout (but within the
// grace-scaled budget) must NOT be misreported as unresponsive.
// Red-on-revert: point the kill exchange back at the fast ctx (rpcTimeout)
// and this fails with ErrDaemonUnresponsive.
func TestDaemonCloseRun_SlowKillNotUnresponsive(t *testing.T) {
	// Shrink the fast-RPC budget so the test is quick. The kill budget is
	// graceTimeout(1, 3s) = 200ms + 4s, comfortably above the 600ms delay.
	orig := rpcTimeout
	rpcTimeout = 200 * time.Millisecond
	t.Cleanup(func() { rpcTimeout = orig })

	const sid = "abcd1234abcd1234"
	slowKillDaemon(t, sid, 600*time.Millisecond) // > rpcTimeout, << graceTimeout(1,3s)

	start := time.Now()
	plan, err := daemonCloseRun(context.Background(), sid, daemonCloseDefaultGrace, true)
	if err != nil {
		t.Fatalf("a slow-but-healthy kill must succeed, got: %v", err)
	}
	if plan.Kind != "kill" {
		t.Fatalf("plan.Kind = %q, want kill", plan.Kind)
	}
	if d := time.Since(start); d < 500*time.Millisecond {
		t.Errorf("returned in %v — the kill delay wasn't awaited, so the slow path wasn't exercised", d)
	}
}
