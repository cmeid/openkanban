package daemon

import (
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/terminal"
)

// TestWedgedSession_HealthFastListBounded is the daemon-level regression
// guard for the resilience properties this effort introduced:
//
//   (A) Health must answer immediately even when a session's Info() is
//       wedged — i.e. handleHealth must NOT share any per-session lock
//       (lock-free registry + atomic counters only).
//
//   (B) List that hits a permanently-wedged Info() must be BOUNDED by
//       the handler deadline (runHandlerWithDeadline / Phase C2) rather
//       than hang forever.
//
// Wedge mechanism: holding sess.attachMu before the test body makes
// sess.Info() block at its first statement (s.attachMu.Lock()), without
// requiring a real PTY or pane. A real (unstarted) pane is provided so
// that the abandoned goroutine released by the deferred Unlock can call
// Info() to completion without nil-derefing on pane.Size().
func TestWedgedSession_HealthFastListBounded(t *testing.T) {
	s := &Server{reg: newSessionRegistry()}
	sess := &Session{id: "wedged", ticketID: "t-wedge", pane: terminal.New("wedged", 80, 24, 100)}
	s.reg.store(sess.id, sess)

	// Simulate a permanently wedged Info(): hold attachMu so any call to
	// sess.Info() blocks at its first line. The defer releases it so the
	// leaked list goroutine can unwind after the test exits.
	sess.attachMu.Lock()
	defer sess.attachMu.Unlock()

	// (A) Health answers immediately despite the wedged session.
	// handleHealth reads only atomics and reg.len() — no per-session lock.
	// A timeout here means a global-lock regression was introduced.
	hdone := make(chan HealthResp, 1)
	go func() { hdone <- s.handleHealth(&clientConn{id: 1}, HealthReq{}) }()
	select {
	case h := <-hdone:
		if h.Sessions != 1 {
			t.Fatalf("Health Sessions=%d want 1", h.Sessions)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handleHealth blocked behind a wedged session — global-lock regression")
	}

	// (B) List that hits the wedged Info() is BOUNDED by the handler
	// deadline, not a permanent hang. Shorten the deadline so the test
	// is fast; restore it after.
	old := handlerDeadlineOverride
	handlerDeadlineOverride = 200 * time.Millisecond
	defer func() { handlerDeadlineOverride = old }()

	finished := s.runHandlerWithDeadline("list", func() {
		_ = s.handleList(&clientConn{id: 2}, ListReq{})
	})
	if finished {
		t.Fatal("handleList completed despite a wedged session — expected deadline abandonment")
	}
}
