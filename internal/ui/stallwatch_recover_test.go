package ui

import (
	"path/filepath"
	"testing"
	"time"
)

// TestStallMonitor_StarvedFiresRecoverOncePerEpisode verifies the watchdog's
// teeth: on a sustained "starved" stall it invokes the recovery closure
// exactly once per episode, and re-arms after the loop makes progress.
//
// Reverting the onTick recover call leaves recovered==0 — red-before-green.
func TestStallMonitor_StarvedFiresRecoverOncePerEpisode(t *testing.T) {
	var push uint64
	m := newStallMonitor(func() (uint64, uint64) { return push, 0 })
	m.dumpPath = filepath.Join(t.TempDir(), "stall.log") // not the real ~/.cache

	var recovered int
	m.setRecover(func() { recovered++ })

	sec := int64(time.Second)
	t0 := int64(1_000_000_000)

	// Seed baseline (phase idle, updateSeq 0). No stall yet.
	m.onTick(t0)
	if recovered != 0 {
		t.Fatalf("recovered=%d after seed, want 0", recovered)
	}

	// Events keep arriving (push advances) but Update never runs → starved.
	push = 5
	m.onTick(t0 + 4*sec) // 4s > 3s threshold
	if recovered != 1 {
		t.Fatalf("recovered=%d after starved stall, want 1", recovered)
	}

	// Same episode must not fire again.
	push = 9
	m.onTick(t0 + 5*sec)
	if recovered != 1 {
		t.Fatalf("recovered=%d (re-fired same episode), want 1", recovered)
	}

	// Progress re-arms the episode; a fresh starved stall fires again.
	m.updateSeq.Add(1)
	m.onTick(t0 + 6*sec) // observes progress
	push = 20
	m.onTick(t0 + 10*sec) // new starved stall
	if recovered != 2 {
		t.Fatalf("recovered=%d after second episode, want 2", recovered)
	}
}

// TestStallMonitor_InCallDoesNotFireRecover verifies recovery is gated to the
// "starved" shape only — an "in-call" stall (Update/View blocked) still dumps
// for diagnostics but does NOT auto-detach (program.Send wouldn't be processed
// while the loop is mid-call, and an in-call stall can be a transient slow
// render rather than a wedge).
func TestStallMonitor_InCallDoesNotFireRecover(t *testing.T) {
	m := newStallMonitor(func() (uint64, uint64) { return 0, 0 })
	m.dumpPath = filepath.Join(t.TempDir(), "stall.log")

	var recovered int
	m.setRecover(func() { recovered++ })

	sec := int64(time.Second)
	t0 := int64(1_000_000_000)

	m.onTick(t0) // seed
	// Enter and stay in Update (in-call).
	m.phase.Store(phaseUpdate)
	m.phaseStartNanos.Store(t0)
	m.onTick(t0 + 4*sec) // in-call stall (>3s in phase)

	if recovered != 0 {
		t.Fatalf("recovered=%d on in-call stall, want 0 (only starved recovers)", recovered)
	}
}
