package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

// These tests use net.Pipe, whose endpoints honor SetReadDeadline /
// SetWriteDeadline and return os.ErrDeadlineExceeded on expiry — a real
// net.Conn whose I/O genuinely blocks, unlike a buffered socketpair where
// a small frame would slip into the kernel buffer and never block.

func TestWriteFrameCtx_DeadlineUnresponsive(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	// peer never reads, so the (synchronous, unbuffered) write blocks
	// until the deadline fires.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- WriteFrameCtx(ctx, client, TypeJSONReq, []byte("hello")) }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrDaemonUnresponsive) {
			t.Fatalf("want ErrDaemonUnresponsive, got %v", err)
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("want the original os.ErrDeadlineExceeded to remain recoverable, got %v", err)
		}
		if d := time.Since(start); d < 80*time.Millisecond {
			t.Errorf("returned too early (%v) — the write deadline wasn't actually applied", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WriteFrameCtx hung — write deadline not applied")
	}
}

func TestReadFrameCtx_DeadlineUnresponsive(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	// peer never writes, so the read blocks until the deadline fires.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, _, err := ReadFrameCtx(ctx, client, client)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrDaemonUnresponsive) {
			t.Fatalf("want ErrDaemonUnresponsive, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReadFrameCtx hung — read deadline not applied")
	}
}

func TestWriteFrameCtx_NoDeadlinePassthrough(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	go func() { _, _ = io.Copy(io.Discard, peer) }()

	// No deadline on the context → behavior must be identical to a bare
	// WriteFrame (no deadline machinery, no normalization).
	if err := WriteFrameCtx(context.Background(), client, TypeJSONReq, []byte("hi")); err != nil {
		t.Fatalf("WriteFrameCtx with no deadline: %v", err)
	}
}

func TestWriteFrameCtx_ClearsDeadline(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	go func() { _, _ = io.Copy(io.Discard, peer) }()

	// First write under a short deadline succeeds (peer is draining).
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if err := WriteFrameCtx(ctx, client, TypeJSONReq, []byte("one")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	cancel()

	// Let the first call's deadline pass. If WriteFrameCtx failed to clear
	// the write deadline on the shared conn, it is now in the past and the
	// next (no-deadline) write would fail immediately — the poisoned-conn
	// regression the defer-clear exists to prevent.
	time.Sleep(150 * time.Millisecond)
	if err := WriteFrameCtx(context.Background(), client, TypeJSONReq, []byte("two")); err != nil {
		t.Fatalf("second write after the first deadline elapsed — stale deadline not cleared: %v", err)
	}
}
