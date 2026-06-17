package daemon

import "testing"

func TestKillStats_StartZero(t *testing.T) {
	s := &Server{}
	inflight, failures := s.killStats()
	if inflight != 0 || failures != 0 {
		t.Fatalf("got %d,%d want 0,0", inflight, failures)
	}
}
