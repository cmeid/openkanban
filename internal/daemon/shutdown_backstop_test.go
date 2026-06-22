package daemon

import (
	"testing"
	"time"
)

// awaitCompletionOrExit is the core of the shutdown-completion backstop:
// it fires onTimeout iff the done signal (Serve returned) does NOT arrive
// within the deadline. These two tests pin both branches without a real
// os.Exit — mirroring how wedgeMonitor.evaluate is unit-tested apart from
// the watchdog's os.Exit.

// Clean shutdown: Serve returns (done closes) before the deadline, so the
// force-exit path must NOT run. This is every healthy shutdown.
func TestAwaitCompletionOrExit_DoneBeforeDeadline_NoForceExit(t *testing.T) {
	done := make(chan struct{})
	close(done) // Serve already returned

	fired := false
	awaitCompletionOrExit(done, time.Hour, func() { fired = true })

	if fired {
		t.Fatal("onTimeout fired despite a clean (done-before-deadline) shutdown")
	}
}

// Hung shutdown: Serve never returns within the deadline, so the backstop
// MUST fire (in prod: dump goroutines + os.Exit(1) so launchd respawns).
// This is the zombie-daemon failure mode the backstop exists to catch.
func TestAwaitCompletionOrExit_DeadlineBeforeDone_ForcesExit(t *testing.T) {
	done := make(chan struct{}) // never closed — shutdown is wedged

	fired := make(chan struct{})
	go awaitCompletionOrExit(done, 10*time.Millisecond, func() { close(fired) })

	select {
	case <-fired:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("onTimeout did not fire for a hung shutdown (done never closed)")
	}
}

// awaitShutdownCompletion wires serveDone + the deadline + the exit seam.
// A hung shutdown (serveDone never closes) must force-exit via s.exitFunc
// — the seam exists precisely so this is observable in a test instead of
// killing the test binary with a real os.Exit.
func TestAwaitShutdownCompletion_HungShutdown_ForcesExit(t *testing.T) {
	exited := make(chan int, 1)
	srv := &Server{
		serveDone:        make(chan struct{}), // never closed → hung
		shutdownDeadline: 10 * time.Millisecond,
		exitFunc:         func(code int) { exited <- code },
	}

	go srv.awaitShutdownCompletion()

	select {
	case code := <-exited:
		if code != 1 {
			t.Fatalf("forceExit code = %d, want 1", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hung shutdown did not force-exit within timeout")
	}
}

// Conversely, a clean shutdown (serveDone closes) must NOT force-exit,
// even with a tiny-but-unreached deadline window.
func TestAwaitShutdownCompletion_CleanExit_NoForceExit(t *testing.T) {
	exited := make(chan int, 1)
	srv := &Server{
		serveDone:        make(chan struct{}),
		shutdownDeadline: time.Hour,
		exitFunc:         func(code int) { exited <- code },
	}
	close(srv.serveDone) // Serve returned cleanly

	srv.awaitShutdownCompletion() // must return promptly without exiting

	select {
	case code := <-exited:
		t.Fatalf("clean shutdown force-exited with code %d", code)
	default:
	}
}
