package cmd

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/daemon"
)

// wedgedListener binds a Unix socket that accepts connections and holds
// them open forever WITHOUT ever reading or replying — modelling a daemon
// that is alive (accept() succeeds) but wedged. Holding every accepted
// conn open for the test's lifetime is essential: if they were closed or
// GC'd the client would observe io.EOF ("daemon closed the connection"),
// a false-green that masks the very hang under test (and would also
// "pass" if the deadline fix were reverted). Uses /tmp because macOS caps
// AF_UNIX paths at 104 bytes and $TMPDIR can blow that.
func wedgedListener(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "okwedge-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen %s: %v", sock, err)
	}

	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c) // hold open; never read or write
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range held {
			_ = c.Close()
		}
		mu.Unlock()
	})

	t.Setenv("OPENKANBAN_DAEMON_SOCK", sock)
	// Belt-and-suspenders: never autostart a real daemon during the test.
	t.Setenv("OPENKANBAN_DAEMON_BINARY", "/usr/bin/true")
	return sock
}

// TestDaemonCloseRun_Unresponsive drives the cmd-path frame I/O against a
// wedged daemon with a short deadline, proving daemonCloseRun returns
// ErrDaemonUnresponsive promptly rather than hanging. Red-on-revert: undo
// the exchange()→*FrameCtx swap and this trips the time.After guard.
func TestDaemonCloseRun_Unresponsive(t *testing.T) {
	wedgedListener(t)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := daemonCloseRun(ctx, "anything", daemonCloseDefaultGrace, false)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, daemon.ErrDaemonUnresponsive) {
			t.Fatalf("want ErrDaemonUnresponsive, got %v", err)
		}
		if d := time.Since(start); d < 200*time.Millisecond {
			t.Errorf("returned too early (%v): the deadline wasn't the cause (early EOF/refused?)", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemonCloseRun hung against a wedged daemon — frame I/O is not deadline-bounded")
	}
}

// TestDaemonListCmd_Unresponsive exercises the full RunE wiring (the
// rpcTimeout-derived context + the mapDaemonErr guidance message) against
// a wedged daemon. The elapsed >= rpcTimeout/2 floor guards against an
// early-EOF false-green: a clean error would return in well under a
// second, whereas the real deadline fires at ~rpcTimeout.
func TestDaemonListCmd_Unresponsive(t *testing.T) {
	wedgedListener(t)

	// Mirror what cobra's Execute() does: a directly-invoked RunE has a nil
	// command context, whereas Execute() seeds it with context.Background().
	daemonListCmd.SetContext(context.Background())

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- daemonListCmd.RunE(daemonListCmd, nil) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error against a wedged daemon, got nil")
		}
		if !strings.Contains(err.Error(), "unresponsive") {
			t.Fatalf("want the guided 'unresponsive' message, got %v", err)
		}
		if d := time.Since(start); d < rpcTimeout/2 {
			t.Errorf("returned in %v (< rpcTimeout/2): looks like an early EOF, not the read deadline", d)
		}
	case <-time.After(rpcTimeout + 3*time.Second):
		t.Fatal("daemon list hung against a wedged daemon")
	}
}
