package daemonclient

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/daemon"
)

// TestClientDo_WriteDeadlineUnresponsive drives Client.do() (the real call
// site, not WriteFrameCtx in isolation) with a net.Pipe whose peer never
// reads, so the request write blocks until the context deadline. Proves
// do() bounds the write and tears the client down on failure. Red-on-
// revert: restore the bare daemon.WriteFrame at the do() write and this
// trips the time.After guard.
func TestClientDo_WriteDeadlineUnresponsive(t *testing.T) {
	clientEnd, peer := net.Pipe()
	defer peer.Close() // never read → the client's frame write blocks

	c := &Client{
		conn:    clientEnd,
		r:       bufio.NewReader(clientEnd),
		closeCh: make(chan struct{}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		var resp daemon.ListResp
		done <- c.do(ctx, daemon.MsgListReq, daemon.ListReq{}, daemon.MsgListResp, &resp)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, daemon.ErrDaemonUnresponsive) {
			t.Fatalf("want ErrDaemonUnresponsive, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("c.do hung — the RPC write is not bounded by the context deadline")
	}

	if !c.closed.Load() {
		t.Error("expected the write failure to signalDisconnect and mark the client closed")
	}
}

// wedgedListener: see cmd/daemon_unresponsive_test.go for the rationale.
// Duplicated here because test helpers don't cross package boundaries.
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
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
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
	t.Setenv("OPENKANBAN_DAEMON_BINARY", "/usr/bin/true")
	return sock
}

// TestPaneViewAttach_Unresponsive points the attach path at a wedged
// daemon. The hello write slips into the socket buffer, but the hello
// response read blocks until the handshake deadline (honoring the short
// ctx) fires. Proves attach returns ErrDaemonUnresponsive instead of
// stranding the PaneView. Red-on-revert: drop the handshake SetDeadline
// and this trips the time.After guard.
func TestPaneViewAttach_Unresponsive(t *testing.T) {
	wedgedListener(t)

	pv := NewPaneView(nil, "T-UNRESP", "sess-unresp", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- pv.attach(ctx, false) }()

	select {
	case err := <-done:
		if !errors.Is(err, daemon.ErrDaemonUnresponsive) {
			t.Fatalf("want ErrDaemonUnresponsive, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("attach hung against a wedged daemon — the handshake is not deadline-bounded")
	}

	if pv.state == PaneViewAttached {
		t.Error("attach must not reach PaneViewAttached against a wedged daemon")
	}
}
