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
// WriteInput must NOT hold p.mu across the blocking PTY write. When the child
// (claude) stalls draining its PTY input — e.g. busy ingesting a huge paste —
// the write() blocks on a full kernel buffer. Holding p.mu across it pins the
// lock, which in production starves handleOutput (the PTY->emulator output
// drain) and cascades into the daemon via handleList -> Session.Info ->
// Pane.Size; that full cascade is documented in .stuck-session-capture/.
//
// This implementation decouples the write onto a dedicated per-pane writer
// goroutine: WriteInput does a non-blocking enqueue and returns, and ONLY the
// writer goroutine ever blocks in the PTY write — never under p.mu. This test
// asserts that load-bearing property in isolation: while the writer goroutine
// is parked in a blocked write, p.mu is acquirable. It deliberately does not
// reconstruct the drain/daemon cascade, only the lock-freedom it depends on.
//
// Setup: point the pane's pty at the write end of an os.Pipe with no reader,
// start the input writer, then WriteInput a payload larger than the pipe
// buffer so the writer goroutine blocks in write() indefinitely. Assert that
// p.mu can still be taken promptly. If WriteInput were reverted to write under
// p.mu (the deadlock seed), the WriteInput goroutine would pin p.mu and this
// assertion would time out.
func TestPane_WriteInputDoesNotHoldLockAcrossBlockingWrite(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	// Closing the pipe makes the blocked write fail (EPIPE/EBADF), so the
	// parked writer goroutine exits when the test ends.
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })

	p := New("test", 80, 24, 1000)
	p.mu.Lock()
	p.pty = w
	p.running = true
	// Wire the writer goroutine (assigns inputCh) and publish runningAtomic
	// last, exactly as Start/StartHeadless do, so WriteInput proceeds.
	p.startInputWriterUnlocked()
	p.stampStartedUnlocked()
	p.mu.Unlock()

	// Payload larger than the pipe buffer (~64KB) with no reader draining r
	// => the writer goroutine blocks in write(). WriteInput itself must
	// return promptly (non-blocking enqueue), so run it in a goroutine only
	// so a regression that writes under p.mu inline can't wedge the test
	// body before the assertion.
	go func() { _, _ = p.WriteInput(make([]byte, 1<<20)) }()

	// Let the writer goroutine dequeue and block in the syscall (or — under a
	// regression — let the inline write pin p.mu).
	time.Sleep(200 * time.Millisecond)

	locked := make(chan struct{})
	go func() {
		p.mu.Lock()
		p.mu.Unlock()
		close(locked)
	}()

	select {
	case <-locked:
		// p.mu acquired => the blocked PTY write is not holding it.
	case <-time.After(2 * time.Second):
		t.Fatal("p.mu held >2s while the PTY write was blocked — " +
			"WriteInput holds p.mu across the blocking write (the deadlock seed)")
	}
}
