package daemon

import (
	"testing"
	"time"
)

func TestRunHandlerWithDeadline(t *testing.T) {
	s := &Server{}
	if !s.runHandlerWithDeadline("fast", func() {}) {
		t.Fatal("fast handler reported as timed out")
	}
	// A handler that outlives the deadline returns false. Use a tiny
	// override via the package var so the test is fast.
	old := handlerDeadlineOverride
	handlerDeadlineOverride = 50 * time.Millisecond
	defer func() { handlerDeadlineOverride = old }()
	if s.runHandlerWithDeadline("slow", func() { time.Sleep(500 * time.Millisecond) }) {
		t.Fatal("slow handler should have reported timeout")
	}
}
