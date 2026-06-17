package daemon

import (
	"testing"
)

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
	// Source must be one of the three canonical values. In test environments
	// OPENKANBAN_DAEMON_SOURCE is unset, so daemonSource() returns "manual".
	validSources := map[string]bool{"tui-fork": true, "launchd": true, "manual": true}
	if !validSources[resp.Source] {
		t.Fatalf("Source=%q want one of tui-fork|launchd|manual", resp.Source)
	}
	if resp.Source != daemonSource() {
		t.Fatalf("Source=%q does not match daemonSource()=%q", resp.Source, daemonSource())
	}
}
