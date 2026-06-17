package daemon

import (
	"log"
	"os"
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
)

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

// runWedgeWatchdog samples dispatch stats on a ticker and force-restarts the
// daemon if evaluate says it's wedged. Dumps every goroutine's stack to the
// log first (the postmortem), then os.Exit(1) so launchd/systemd respawns —
// picking up a new on-disk binary if one is present. Exits cleanly when the
// shutdown channel closes.
func (s *Server) runWedgeWatchdog() {
	mon := newWedgeMonitor(wedgeSeconds, staleWedgeSeconds)
	ticker := time.NewTicker(wedgeCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.shutdown:
			return
		case <-ticker.C:
			seq, inflight := s.dispatchStats()
			s.stalenessMu.Lock()
			pending := s.pendingRestart
			s.stalenessMu.Unlock()
			exit, reason := mon.evaluate(wedgeSample{
				seq:            seq,
				inflight:       inflight,
				pendingRestart: pending,
				nowNanos:       time.Now().UnixNano(),
			})
			if !exit {
				continue
			}
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			log.Printf("openkanbankd: WEDGE WATCHDOG firing (%s); inflight=%d seq=%d. goroutine dump:\n%s",
				reason, inflight, seq, buf[:n])
			log.Printf("openkanbankd: exiting(1) for supervisor respawn")
			os.Exit(1)
		}
	}
}
