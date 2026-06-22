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
