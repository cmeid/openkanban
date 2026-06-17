package daemon

import "testing"

func TestHandleHealth_ReportsCounts(t *testing.T) {
	s := &Server{reg: newSessionRegistry()}
	s.reg.store("a", &Session{id: "a"})
	resp := s.handleHealth(&clientConn{id: 1}, HealthReq{})
	if resp.Sessions != 1 {
		t.Fatalf("Sessions=%d want 1", resp.Sessions)
	}
	if resp.Goroutines <= 0 || resp.PID <= 0 {
		t.Fatalf("Goroutines=%d PID=%d want positive", resp.Goroutines, resp.PID)
	}
}
