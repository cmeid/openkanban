package terminal

import (
	"os"
	"testing"
	"time"
)

// TestPane_WriteInputDoesNotHoldLockAcrossBlockingWrite is the regression
// guard for the stuck-session deadlock captured 2026-06-17
// (.stuck-session-capture/ANALYSIS.md, goroutine 342).
//
// WriteInput must NOT hold p.mu across the blocking p.pty.Write. When the
// child (claude) stalls draining its PTY input — e.g. busy ingesting a huge
// paste — the write() blocks on a full kernel buffer. Holding p.mu across it
// pins the lock, which in production starves handleOutput (the PTY->emulator
// output drain) and cascades into the daemon via handleList -> Session.Info
// -> Pane.Size; that full cascade is documented in
// .stuck-session-capture/ANALYSIS.md.
//
// This test asserts the single load-bearing property of the fix in isolation
// — while a WriteInput is blocked in the PTY write, p.mu is NOT held, so a
// concurrent p.mu-taking call (Size) still returns. It deliberately does not
// reconstruct the drain/daemon cascade, only the lock-release contract the
// cascade depends on.
//
// Setup: point p.pty at the write end of an os.Pipe with no reader, then
// WriteInput a payload larger than the pipe buffer so the underlying write
// blocks indefinitely. Assert that Size() (which takes p.mu) still returns
// promptly. Pre-fix Size blocks forever; post-fix WriteInput released p.mu
// before the write.
func TestPane_WriteInputDoesNotHoldLockAcrossBlockingWrite(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	// Closing the read end makes the blocked write fail with EPIPE, freeing
	// the leaked WriteInput goroutine when the test ends.
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })

	p := &Pane{pty: w, running: true}

	// Larger than the pipe buffer (~64KB) with no reader draining r => the
	// underlying write() blocks.
	go func() { _, _ = p.WriteInput(make([]byte, 1<<20)) }()

	// Give the writer time to enter (and block in) the syscall while — pre-fix
	// — holding p.mu.
	time.Sleep(200 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		p.Size() // takes p.mu
		close(done)
	}()

	select {
	case <-done:
		// Size acquired p.mu => WriteInput is not holding it across the write.
	case <-time.After(2 * time.Second):
		t.Fatal("Pane.Size blocked >2s while WriteInput's PTY write was blocked — " +
			"WriteInput holds p.mu across the blocking write (the deadlock seed)")
	}
}
