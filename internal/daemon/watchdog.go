package daemon

import (
	"log"
	"runtime"
	"time"
)

const (
	// wedgeCheckInterval is how often the watchdog samples dispatch stats.
	wedgeCheckInterval = 5 * time.Second
	// wedgeSeconds: inflight work that completes nothing for this long is a
	// wedge. Generous so a slow-but-progressing daemon is never killed.
	wedgeSeconds = 90
	// staleWedgeSeconds: a stale binary (pendingRestart) that also stops
	// completing work exits sooner — it has nothing to lose.
	staleWedgeSeconds = 45
	// shutdownCompletionDeadline bounds how long the daemon may take to
	// actually exit AFTER shutdown is initiated. Healthy shutdown is
	// sub-second (close client conns -> wg.Wait returns) plus per-session
	// kill grace in cleanup(); this is generously above that. Past it,
	// shutdown is wedged (the zombie-daemon failure mode: socket unlinked,
	// process never exits), so the watchdog force-exits to let launchd
	// respawn rather than leave an invisible-but-alive daemon. It cannot
	// false-fire during a legitimate default-mode awaitSessionDrain: that
	// path keeps the daemon alive WITHOUT closing s.shutdown, so this
	// deadline only starts once initiateShutdown has actually fired, by
	// which point exit should be prompt.
	shutdownCompletionDeadline = 30 * time.Second
)

// awaitCompletionOrExit blocks until done is closed (clean shutdown
// completion) or deadline elapses, whichever comes first. On deadline it
// invokes onTimeout. Pure + injectable so the force-exit decision is
// unit-tested without a real os.Exit, mirroring wedgeMonitor.evaluate.
func awaitCompletionOrExit(done <-chan struct{}, deadline time.Duration, onTimeout func()) {
	select {
	case <-done:
	case <-time.After(deadline):
		onTimeout()
	}
}

type wedgeSample struct {
	seq            uint64
	inflight       int64
	pendingRestart bool
	nowNanos       int64
}

// wedgeMonitor decides, from successive dispatch-stat samples, whether the
// daemon is wedged (work queued, nothing completing) long enough to warrant
// a self-restart. Pure + injectable-time so the decision is unit-tested
// without a real os.Exit.
type wedgeMonitor struct {
	wedgeNanos      int64
	staleNanos      int64
	lastSeq         uint64
	lastSeqChangeNs int64
	primed          bool
}

func newWedgeMonitor(wedgeSeconds, staleWedgeSeconds int64) *wedgeMonitor {
	return &wedgeMonitor{
		wedgeNanos: wedgeSeconds * int64(time.Second),
		staleNanos: staleWedgeSeconds * int64(time.Second),
	}
}

// evaluate returns (exit, reason). exit=true means: force-restart now.
func (w *wedgeMonitor) evaluate(s wedgeSample) (bool, string) {
	if !w.primed || s.seq != w.lastSeq {
		w.primed = true
		w.lastSeq = s.seq
		w.lastSeqChangeNs = s.nowNanos
		return false, ""
	}
	// seq frozen since lastSeqChangeNs. Only a wedge if work is queued.
	if s.inflight <= 0 {
		return false, ""
	}
	frozen := s.nowNanos - w.lastSeqChangeNs
	if s.pendingRestart && frozen > w.staleNanos {
		return true, "stale binary wedged (no dispatch completion)"
	}
	if frozen > w.wedgeNanos {
		return true, "dispatch wedged (no completion with work in flight)"
	}
	return false, ""
}

// runWedgeWatchdog samples dispatch stats on a ticker. If evaluate says the
// daemon looks wedged (short handlers in flight, dispatchSeq frozen past the
// threshold) it REPORTS the suspicion — sets s.suspectedWedged (answered on
// hello), emits a daemon_wedged event to connected TUIs, and dumps goroutines
// to the log — but does NOT force-exit. A dispatch wedge does not stop the PTY
// pumps, so self-restarting would only destroy live agent sessions (the
// 2026-06-22 false-positive that killed 11 sessions). Recovery is
// operator-driven from a TUI; with no TUI connected the suspicion is harmless.
// The suspicion is cleared when dispatch resumes. Exits cleanly when the
// shutdown channel closes — and then hands off to awaitShutdownCompletion,
// which IS still allowed to force-exit a hung *shutdown* (a different, genuinely
// unrecoverable zombie state).
func (s *Server) runWedgeWatchdog() {
	mon := newWedgeMonitor(wedgeSeconds, staleWedgeSeconds)
	ticker := time.NewTicker(wedgeCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.shutdown:
			// Shutdown initiated. Don't abandon the post — switch from
			// dispatch-wedge detection to a shutdown-completion backstop.
			// Returning here is what previously dismantled the safety net
			// the moment it was most needed (a hung shutdown left an
			// un-recoverable zombie that even launchd couldn't respawn,
			// since the process never exited). If Serve returns within the
			// deadline this is a no-op; otherwise we force-exit.
			s.awaitShutdownCompletion()
			return
		case <-ticker.C:
			seq, inflight := s.wedgeStats()
			s.stalenessMu.Lock()
			pending := s.pendingRestart
			s.stalenessMu.Unlock()
			suspect, reason := mon.evaluate(wedgeSample{
				seq:            seq,
				inflight:       inflight,
				pendingRestart: pending,
				nowNanos:       time.Now().UnixNano(),
			})
			if !suspect {
				// Dispatch is progressing (or idle) — clear any prior
				// suspicion so a newly dialing TUI isn't wrongly warned.
				if s.suspectedWedged.Swap(false) {
					log.Printf("openkanbankd: wedge watchdog: dispatch resumed; clearing suspected-wedge")
				}
				continue
			}
			// Suspected wedge. REPORT, do not self-destruct: emit + dump once
			// per episode and let an operator decide via a TUI. The flag is
			// also answered on hello so a freshly dialing TUI is told.
			if !s.suspectedWedged.Swap(true) {
				buf := make([]byte, 1<<20)
				n := runtime.Stack(buf, true)
				log.Printf("openkanbankd: WEDGE WATCHDOG suspects a wedge (%s); inflight=%d seq=%d. goroutine dump:\n%s",
					reason, inflight, seq, buf[:n])
				s.emitEvent(SessionEvent{Event: "daemon_wedged", Reason: reason})
			}
		}
	}
}

// awaitShutdownCompletion is the watchdog's post-shutdown phase: once
// shutdown is initiated the process must exit promptly, so it waits for
// Serve to return (s.serveDone) and, if that doesn't happen within
// shutdownCompletionDeadline, dumps goroutines and force-exits so launchd
// respawns. This is the backstop for a hung shutdown — the failure mode
// that left the field daemon a zombie (socket unlinked, process alive,
// SIGUSR1 handler already gone, launchd unable to respawn a
// still-"healthy" process).
func (s *Server) awaitShutdownCompletion() {
	deadline := s.shutdownDeadline
	if deadline <= 0 {
		deadline = shutdownCompletionDeadline
	}
	awaitCompletionOrExit(s.serveDone, deadline, func() {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		log.Printf("openkanbankd: SHUTDOWN WATCHDOG firing — shutdown did not complete within %s. goroutine dump:\n%s",
			deadline, buf[:n])
		log.Printf("openkanbankd: exiting(1) for supervisor respawn (hung shutdown)")
		s.forceExit(1)
	})
}
