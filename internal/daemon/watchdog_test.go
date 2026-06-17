package daemon

import "testing"

func TestDispatchStats_StartZero(t *testing.T) {
	s := &Server{}
	seq, inflight := s.dispatchStats()
	if seq != 0 || inflight != 0 {
		t.Fatalf("got seq=%d inflight=%d want 0,0", seq, inflight)
	}
}
