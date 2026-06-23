package daemon

import "testing"

// TestWedgeStats_ExcludesParkedAttaches pins the 2026-06-22 false-positive fix:
// the wedge watchdog must sample only short (deadline-wrapped) handlers, never
// the by-design-blocking handleAttach/handleShutdown handlers. Two idle parked
// attaches (counted in s.inflight) must NOT register as wedge work, or an idle
// daemon with attached-but-quiet sessions reads as wedged and gets killed.
func TestWedgeStats_ExcludesParkedAttaches(t *testing.T) {
	s := &Server{}

	// Simulate two parked handleAttach handlers: they bump the general
	// inflight gauge but are NOT deadline-wrapped, so wedgeStats must report 0.
	s.inflight.Add(2)
	if _, wi := s.wedgeStats(); wi != 0 {
		t.Fatalf("wedgeStats inflight=%d, want 0 — parked attaches must not count as wedge work", wi)
	}

	// A short (deadline-wrapped) handler IS counted while it runs.
	entered := make(chan struct{})
	release := make(chan struct{})
	go s.runHandlerWithDeadline("test", func() {
		close(entered)
		<-release
	})
	<-entered
	if _, wi := s.wedgeStats(); wi != 1 {
		t.Fatalf("wedgeStats inflight=%d during a short handler, want 1", wi)
	}
	close(release)
}
