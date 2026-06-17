package daemon

import "testing"

func TestDispatchStats_StartZero(t *testing.T) {
	s := &Server{}
	seq, inflight := s.dispatchStats()
	if seq != 0 || inflight != 0 {
		t.Fatalf("got seq=%d inflight=%d want 0,0", seq, inflight)
	}
}

func TestWedge_NoExitWhenProgressing(t *testing.T) {
	w := newWedgeMonitor(60, 30) // wedgeSeconds, staleWedgeSeconds
	sec := int64(1e9)
	// seq advances each tick → never a wedge even with inflight>0.
	w.evaluate(wedgeSample{seq: 1, inflight: 2, nowNanos: 10 * sec})
	exit, _ := w.evaluate(wedgeSample{seq: 2, inflight: 2, nowNanos: 80 * sec})
	if exit {
		t.Fatal("exit fired while dispatchSeq was advancing")
	}
}

func TestWedge_ExitOnFrozenSeqWithInflight(t *testing.T) {
	w := newWedgeMonitor(60, 30)
	sec := int64(1e9)
	w.evaluate(wedgeSample{seq: 5, inflight: 3, nowNanos: 10 * sec}) // baseline
	exit, reason := w.evaluate(wedgeSample{seq: 5, inflight: 3, nowNanos: 71 * sec})
	if !exit {
		t.Fatalf("no exit after %ds frozen with inflight>0", 61)
	}
	if reason == "" {
		t.Fatal("exit reason empty")
	}
}

func TestWedge_NoExitWhenIdle(t *testing.T) {
	w := newWedgeMonitor(60, 30)
	sec := int64(1e9)
	w.evaluate(wedgeSample{seq: 5, inflight: 0, nowNanos: 10 * sec})
	exit, _ := w.evaluate(wedgeSample{seq: 5, inflight: 0, nowNanos: 200 * sec})
	if exit {
		t.Fatal("exit fired on an idle daemon (inflight==0)")
	}
}

func TestWedge_StaleBinaryFrozenExitsSooner(t *testing.T) {
	w := newWedgeMonitor(60, 30)
	sec := int64(1e9)
	w.evaluate(wedgeSample{seq: 5, inflight: 1, pendingRestart: true, nowNanos: 10 * sec})
	exit, _ := w.evaluate(wedgeSample{seq: 5, inflight: 1, pendingRestart: true, nowNanos: 41 * sec})
	if !exit {
		t.Fatal("stale+frozen did not exit at the shorter threshold")
	}
	// Symmetric: same elapsed time, but NOT stale → must NOT exit (proves
	// the early exit was due to the stale threshold, not the wedge one).
	w2 := newWedgeMonitor(60, 30)
	w2.evaluate(wedgeSample{seq: 5, inflight: 1, nowNanos: 10 * sec})
	if exit, _ := w2.evaluate(wedgeSample{seq: 5, inflight: 1, nowNanos: 41 * sec}); exit {
		t.Fatal("non-stale path exited at 31s; should require the full 60s")
	}
}

func TestWedge_ProdThresholds(t *testing.T) {
	sec := int64(1e9)
	// Non-stale: must NOT exit at 89s frozen, MUST exit just past 90s.
	w := newWedgeMonitor(90, 45)
	w.evaluate(wedgeSample{seq: 1, inflight: 2, nowNanos: 10 * sec})
	if exit, _ := w.evaluate(wedgeSample{seq: 1, inflight: 2, nowNanos: 99 * sec}); exit {
		t.Fatal("exited at 89s frozen; non-stale threshold must be 90s")
	}
	if exit, _ := w.evaluate(wedgeSample{seq: 1, inflight: 2, nowNanos: 101 * sec}); !exit {
		t.Fatal("did not exit at 91s frozen; non-stale threshold must be 90s")
	}
	// Stale: must exit at the shorter 45s threshold, not wait for 90s.
	w2 := newWedgeMonitor(90, 45)
	w2.evaluate(wedgeSample{seq: 1, inflight: 1, pendingRestart: true, nowNanos: 10 * sec})
	if exit, _ := w2.evaluate(wedgeSample{seq: 1, inflight: 1, pendingRestart: true, nowNanos: 56 * sec}); !exit {
		t.Fatal("stale path did not exit at 46s; stale threshold must be 45s")
	}
}
