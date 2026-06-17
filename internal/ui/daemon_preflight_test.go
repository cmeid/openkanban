package ui

import (
	"testing"
	"time"
)

// TestPreflightListSessions_NilClientReturnsEmpty pins the degenerate
// path: a nil client yields an empty snapshot and no error, leaving the
// caller (internal/app) to decide whether a nil client is itself an exit
// condition.
func TestPreflightListSessions_NilClientReturnsEmpty(t *testing.T) {
	got, err := PreflightListSessions(nil)
	if err != nil {
		t.Fatalf("nil client should not error; got %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("nil client should yield an empty snapshot; got %v", got)
	}
}

// TestPreflightBudgetStaysFastFail guards the anti-hang intent. The
// preflight gates launch-vs-exit, so against a wedged daemon (every
// attempt times out) its total wait is the budget below — it must stay
// small so a wedge surfaces a message in seconds, not the ~30s the old
// degrade-friendly reconcile tolerated, and never the unbounded hang this
// work removed. A future bump to the constants that blows this budget
// should fail here and be reconsidered.
func TestPreflightBudgetStaysFastFail(t *testing.T) {
	worst := time.Duration(startupReconcileAttempts) * startupReconcileTimeout
	if worst > 10*time.Second {
		t.Errorf("preflight worst-case wait = %s; want <= 10s (fast-fail a wedged daemon)", worst)
	}
}
