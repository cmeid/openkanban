//go:build !windows

package terminal

import (
	"errors"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// TestSessionInfo_LockFree proves Task 1: the Info-reachable accessors
// (Size / Running / PID) must not take p.mu, so a stuck pane holding
// p.mu (e.g. a teardown wedged on a syscall) can never freeze the
// daemon's handleList → Session.Info path.
//
// No PTY is forked: we hold p.mu directly for a window and assert each
// accessor returns within a tight deadline. Red iff Task 1 is reverted
// (the accessors lock p.mu and block for the full hold).
func TestSessionInfo_LockFree(t *testing.T) {
	p := New("lockfree", 80, 24, 100)

	// Seed the atomics the way a started pane would, so the accessors
	// return meaningful values without a real fork.
	p.runningAtomic.Store(true)
	p.pid.Store(4242)
	p.dims.Store(packDims(120, 40))

	held := make(chan struct{})
	released := make(chan struct{})
	go func() {
		p.mu.Lock()
		close(held)
		<-released
		p.mu.Unlock()
	}()
	<-held
	defer close(released)

	const deadline = 500 * time.Millisecond

	assertFast := func(name string, fn func()) {
		t.Helper()
		done := make(chan struct{})
		go func() {
			fn()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(deadline):
			t.Fatalf("%s blocked while p.mu was held — accessor is not lock-free", name)
		}
	}

	var (
		gotW, gotH int
		gotRun     bool
		gotPID     int
	)
	assertFast("Size", func() { gotW, gotH = p.Size() })
	assertFast("Running", func() { gotRun = p.Running() })
	assertFast("PID", func() { gotPID = p.PID() })

	if gotW != 120 || gotH != 40 {
		t.Errorf("Size: got %dx%d want 120x40", gotW, gotH)
	}
	if !gotRun {
		t.Errorf("Running: got false want true")
	}
	if gotPID != 4242 {
		t.Errorf("PID: got %d want 4242", gotPID)
	}
}

// --- helpers shared by the wedge tests below (Tasks 2 & 3) ---

// floodToBackpressure spawns a provably-non-draining child (sleep, which
// never reads stdin) on the pane, then writes chunks until the pane is
// SATURATED: WriteInput returns ErrInputBackpressure on a sustained run
// of consecutive attempts (no interleaved success). Sustained
// backpressure is the proof that the single writer goroutine is
// hard-parked INSIDE f.Write on a full kernel PTY buffer and the bounded
// inputCh (cap 256) stays full — not merely transiting its select. That
// hard-park is the precondition TestStop_ReturnsWhenWriterBlocked needs
// to bite: a writer in select could escape via inputStop, but one parked
// in f.Write can only be released by closing the fd (the close-before-
// wait fix) or collapsing the slave (group-kill). The loop is capped so
// a regression that never backpressures fails loudly rather than
// spinning forever.
func floodToBackpressure(t *testing.T, p *Pane) {
	t.Helper()
	// Put the slave tty in raw, no-echo mode BEFORE the long sleep. In the
	// default cooked+echo mode the line discipline drains our input (echoing
	// it, which the pane's read loop then consumes) — so master writes keep
	// succeeding and the writer never parks, making saturation racy. Raw mode
	// stops that drain: input accumulates until the kernel buffer fills and
	// f.Write hard-blocks. The child still never reads stdin.
	if err := p.StartHeadless("/bin/sh", []string{"-c", "stty raw -echo </dev/tty >/dev/tty 2>/dev/null; exec sleep 600"}, nil); err != nil {
		t.Fatalf("StartHeadless: %v", err)
	}
	// Give the child a moment to apply the stty before we flood.
	time.Sleep(100 * time.Millisecond)
	chunk := make([]byte, 4096)
	// Generous ceiling: with input no longer drained the buffers MUST fill and
	// the writer MUST park; the cap only guards against a regression that
	// never backpressures. Each iteration is a cheap channel op.
	const maxChunks = 1 << 18
	const sustained = 64 // backpressure dominance proving the writer is parked
	streak := 0
	for i := 0; i < maxChunks; i++ {
		_, err := p.WriteInput(chunk)
		if errors.Is(err, ErrInputBackpressure) {
			streak++
			if streak >= sustained {
				return // writer hard-parked in f.Write; channel saturated
			}
			continue
		}
		if err != nil {
			t.Fatalf("WriteInput returned unexpected error before backpressure: %v", err)
		}
		// Interleaved success: the writer freed a slot. A lone success during
		// the fill phase must not zero hard-won progress (that was the old
		// flake), so penalize instead of reset — once the buffers fill and the
		// writer parks, backpressure is uninterrupted and the streak climbs
		// cleanly. Yield so the writer gets scheduled to drain and park.
		if streak > 4 {
			streak -= 4
		} else {
			streak = 0
		}
		runtime.Gosched()
	}
	t.Fatalf("never reached sustained ErrInputBackpressure after %d chunks — writer is draining or unbounded", maxChunks)
}

func mustReturnWithin(t *testing.T, name string, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s", name, d)
	}
}

// TestWriteInput_DoesNotBlockOnFullChild proves Task 2: WriteInput must
// not hold p.mu across the blocking PTY write. With a writer parked
// inside f.Write and the input channel full, WriteInput returns
// ErrInputBackpressure (bounded backpressure, not an infinite block),
// AND — the load-bearing assertion — the concurrent accessors
// Size/Running/PID and a full Stop() all return promptly. Red iff Task 2
// is reverted (WriteInput holds p.mu across the blocked write → every
// concurrent accessor hangs behind it).
func TestWriteInput_DoesNotBlockOnFullChild(t *testing.T) {
	p := New("wedge-writeinput", 80, 24, 100)
	floodToBackpressure(t, p)

	// A writer goroutine is now parked inside f.Write and inputCh is
	// full. A further WriteInput must return promptly (backpressure),
	// not block on p.mu.
	mustReturnWithin(t, "WriteInput under backpressure", 2*time.Second, func() {
		_, _ = p.WriteInput([]byte("more"))
	})

	// The cascade-breaking assertions: the Info accessors must stay
	// responsive even with a writer wedged.
	mustReturnWithin(t, "Size under wedge", 2*time.Second, func() { _, _ = p.Size() })
	mustReturnWithin(t, "Running under wedge", 2*time.Second, func() { _ = p.Running() })
	mustReturnWithin(t, "PID under wedge", 2*time.Second, func() { _ = p.PID() })

	// And teardown must complete despite the parked writer.
	mustReturnWithin(t, "Stop under wedge", 5*time.Second, func() { _ = p.Stop() })
}

// TestStop_ReturnsWhenWriterBlocked guards teardown LIVENESS: with a
// writer goroutine provably parked inside f.Write (we flood the
// non-draining child to sustained ErrInputBackpressure first — otherwise
// no writer is blocked and the test couldn't bite), Stop() must still
// return within a deadline.
//
// NOTE on what this does and does NOT isolate: teardown has THREE
// independent unblock paths — (1) f.Close() before the waits
// (close-before-wait), (2) the group SIGKILL collapsing the slave →
// f.Write returns EIO, and (3) closing inputStop catching the writer in
// its select. This test stays green if ANY one path works, so reverting
// close-before-wait ALONE does not make it hang (the group-kill path
// still saves it). The close-before-wait ordering's necessity is proven
// separately by reverting it AND neutering the other two paths, after
// which this exact assertion hangs (see the plan's red-before-green
// report). This redundancy is by design — teardown must not rest on one
// platform assumption.
func TestStop_ReturnsWhenWriterBlocked(t *testing.T) {
	p := New("wedge-stop", 80, 24, 100)
	floodToBackpressure(t, p)

	mustReturnWithin(t, "Stop with writer parked", 5*time.Second, func() {
		if err := p.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
}

// TestStop_KillsProcessGroup proves Task 3's process-group kill. The
// child backgrounds two long sleeps in its own session/pgroup; Stop()
// must SIGKILL the whole group so the backgrounded sleeps die too.
//
// The child traps and IGNORES SIGHUP (`trap '' HUP`) on purpose: closing
// the PTY master delivers SIGHUP to the session, which would otherwise
// reap the backgrounded sleeps on its own and mask a reverted group-kill
// (making this test vacuous). By ignoring SIGHUP, the ONLY thing that
// reaps the sleeps is an explicit group SIGKILL — so reverting group-kill
// to a single-process kill leaves them alive and the test fails. Verified
// red-before-green on darwin.
func TestStop_KillsProcessGroup(t *testing.T) {
	p := New("groupkill", 80, 24, 100)
	if err := p.StartHeadless("/bin/sh", []string{"-c", "trap '' HUP; sleep 600 & sleep 600 & wait"}, nil); err != nil {
		t.Fatalf("StartHeadless: %v", err)
	}

	pid := p.PID()
	if pid <= 0 {
		t.Fatalf("PID: got %d", pid)
	}
	// creack/pty sets Setsid, so the child leads its own process group
	// with pgid == pid. Give the backgrounded children a moment to fork.
	time.Sleep(200 * time.Millisecond)

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Poll until kill(-pgid, 0) reports that no *live signalable*
	// process remains in the group. The error code differs by platform
	// AFTER a successful group SIGKILL:
	//   - Linux: ESRCH (no such process group).
	//   - darwin: EPERM — the SIGKILLed members become zombies
	//     reparented to init (pid 1); a probe of a group that holds only
	//     init-owned zombies returns EPERM, not ESRCH.
	// Either non-nil error means the group-kill worked. The discriminator
	// vs. the reverted single-process kill is decisive: a single-process
	// kill leaves the backgrounded sleeps ALIVE in the group, so
	// kill(-pgid, 0) keeps returning nil indefinitely (the test then
	// times out). Verified empirically on this darwin host.
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(-pid, 0)
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
			return // no live signalable member — group reaped
		}
		if err != nil {
			// Any other non-nil error is also "not a live group".
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process group %d still has live members after Stop (kill -%d 0 = nil) — group-kill did not fire", pid, pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestStop_Idempotent proves Fix 1: a second Stop() on an already-stopped
// pane must return promptly and must NOT re-issue signalGroup(SIGKILL)
// (which risks hitting a recycled pgid). teardownOnce guarantees the
// group signal fires at most once; pgid is zeroed after the first fire so
// the ≤0 guard in signalGroup catches any late caller.
//
// Red-before-green proof: revert teardownOnce + pgid.Store(0) → the second
// Stop() executes a full second teardown including signalGroup, which may
// hit a recycled pgid. The timing assertion below may not always catch
// that (it depends on pgid reuse timing), but the zero-pgid guard is
// directly observable: after Stop(), p.pgid.Load() must be 0.
func TestStop_Idempotent(t *testing.T) {
	p := New("idempotent", 80, 24, 100)
	if err := p.StartHeadless("/bin/sh", []string{"-c", "sleep 600"}, nil); err != nil {
		t.Fatalf("StartHeadless: %v", err)
	}

	// First Stop must return promptly.
	mustReturnWithin(t, "first Stop", 5*time.Second, func() {
		if err := p.Stop(); err != nil {
			t.Errorf("first Stop: %v", err)
		}
	})

	// pgid must be zeroed after teardown so signalGroup's guard catches
	// any subsequent call.
	if got := p.pgid.Load(); got != 0 {
		t.Errorf("pgid after Stop: got %d, want 0", got)
	}

	// Second Stop must also return promptly and not panic.
	mustReturnWithin(t, "second Stop", 2*time.Second, func() {
		if err := p.Stop(); err != nil {
			t.Errorf("second Stop: %v", err)
		}
	})
}
