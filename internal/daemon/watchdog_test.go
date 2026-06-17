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
}
