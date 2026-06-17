package ui

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestMonitor builds a monitor with an injectable push/drop reader
// and no watchdog goroutine (tests drive evaluate/evaluateAndDump
// directly). thresholdNanos defaults to 3s as in production.
func newTestMonitor(push *uint64, drop *uint64) *stallMonitor {
	m := newStallMonitor(func() (uint64, uint64) { return *push, *drop })
	return m
}

// seedHeartbeat records a non-zero baseline updateSeq and runs one
// evaluate so "flat" on later ticks is an OBSERVED hold, not a
// zero-value coincidence. Returns the baseline time.
func seedHeartbeat(t *testing.T, m *stallMonitor, seq uint64, t0 int64) {
	t.Helper()
	m.updateSeq.Store(seq)
	if kind, ok := m.evaluate(t0); ok {
		t.Fatalf("baseline evaluate should not dump, got kind=%q", kind)
	}
}

func TestStallWatch_InCall(t *testing.T) {
	var push, drop uint64
	m := newTestMonitor(&push, &drop)

	// Seed SPECIFIC published values so a dump fired by the wrong branch
	// (or with default fields) is detectable.
	m.enterUpdate("ui.bigRenderMsg", "AGENT_VIEW", 7, 4)
	// Force the phase-start well into the past: still in-update, 10s ago.
	start := time.Now().Add(-10 * time.Second).UnixNano()
	m.phaseStartNanos.Store(start)

	var buf bytes.Buffer
	now := start + int64(10*time.Second)
	if !m.evaluateAndDump(&buf, now) {
		t.Fatal("expected an in-call dump for a 10s-running Update")
	}
	out := buf.String()
	if !strings.Contains(out, "kind=in-call") {
		t.Errorf("dump kind should be in-call; got:\n%s", out)
	}
	// Seeded fields must appear — guards against a wrong-branch dump that
	// would carry default (empty/zero) values.
	if !strings.Contains(out, `msg="ui.bigRenderMsg"`) {
		t.Errorf("dump missing seeded inflight msg; got:\n%s", out)
	}
	if !strings.Contains(out, "activeSessions=4") {
		t.Errorf("dump missing seeded activeSessions=4; got:\n%s", out)
	}
	if !strings.Contains(out, "goroutine ") {
		t.Errorf("dump missing goroutine stack; got:\n%s", out)
	}
}

// TestStallWatch_StarvedVsIdle is the shared-setup PAIR: identical state
// except the push-counter delta. The ONLY thing separating "dump" from
// "no dump" is whether pushes advanced — so a detector that ignored the
// push counter (and would thus false-positive on a quiet board) cannot
// pass both halves.
func TestStallWatch_StarvedVsIdle(t *testing.T) {
	base := time.Now().UnixNano()
	stalled := base + int64(4*time.Second) // past the 3s threshold

	t.Run("starved_pushes_advancing", func(t *testing.T) {
		var push, drop uint64 = 100, 0
		m := newTestMonitor(&push, &drop)
		// phase idle, heartbeat seeded non-zero and held flat.
		m.phase.Store(phaseIdle)
		seedHeartbeat(t, m, 42, base)

		push += 5 // pushes kept arriving during the stall window

		var buf bytes.Buffer
		if !m.evaluateAndDump(&buf, stalled) {
			t.Fatal("expected a starved dump: Update flat 4s while pushes advanced")
		}
		out := buf.String()
		if !strings.Contains(out, "kind=starved") {
			t.Errorf("dump kind should be starved; got:\n%s", out)
		}
		if !strings.Contains(out, "pushDelta=5") {
			t.Errorf("dump should record pushDelta=5; got:\n%s", out)
		}
	})

	t.Run("idle_pushes_flat", func(t *testing.T) {
		var push, drop uint64 = 100, 0
		m := newTestMonitor(&push, &drop)
		m.phase.Store(phaseIdle)
		seedHeartbeat(t, m, 42, base)

		// push held FLAT — the only difference from the starved case.

		var buf bytes.Buffer
		if m.evaluateAndDump(&buf, stalled) {
			t.Fatalf("genuine idle must NOT dump; got:\n%s", buf.String())
		}
		if buf.Len() != 0 {
			t.Errorf("no bytes should be written on genuine idle; got %d", buf.Len())
		}
	})
}

// TestStallWatch_OnTickWritesFile exercises the PRODUCTION write path
// (onTick opens the dump file and writes), not just the evaluateAndDump
// test seam — proving the file actually gets the dump on a real stall.
func TestStallWatch_OnTickWritesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stall.log")
	t.Setenv("OPENKANBAN_TUI_STALL_LOG", path)

	var push, drop uint64
	m := newStallMonitor(func() (uint64, uint64) { return push, drop })
	if m.dumpPath != path {
		t.Fatalf("dumpPath = %q, want %q (env override not honored)", m.dumpPath, path)
	}
	m.enterUpdate("ui.exitedHandler", "NORMAL", 2, 6)
	start := time.Now().Add(-10 * time.Second).UnixNano()
	m.phaseStartNanos.Store(start)

	m.onTick(start + int64(10*time.Second))

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("dump file not written: %v", err)
	}
	out := string(data)
	if !strings.Contains(out, "kind=in-call") || !strings.Contains(out, "goroutine ") {
		t.Errorf("dump file missing expected content; got:\n%s", out)
	}
}

func TestStallWatch_Lifecycle(t *testing.T) {
	t.Setenv("OPENKANBAN_TUI_STALL_LOG", filepath.Join(t.TempDir(), "stall.log"))
	m := newStallMonitor(nil) // nil push reader must be safe
	m.start()
	m.stop()
	m.stop() // idempotent — must not panic or double-close
}

func TestStallWatch_OneDumpPerEpisode(t *testing.T) {
	var push, drop uint64 = 0, 0
	m := newTestMonitor(&push, &drop)
	m.phase.Store(phaseUpdate)
	start := time.Now().Add(-10 * time.Second).UnixNano()
	m.phaseStartNanos.Store(start)
	m.enterUpdate("ui.someMsg", "NORMAL", 0, 0)
	m.phaseStartNanos.Store(start) // enterUpdate reset it; force it old again

	var buf bytes.Buffer
	now := start + int64(10*time.Second)

	// First tick of the episode dumps.
	if !m.evaluateAndDump(&buf, now) {
		t.Fatal("first tick should dump")
	}
	dumps := strings.Count(buf.String(), "==== openkanban TUI stall dump")
	if dumps != 1 {
		t.Fatalf("after first tick want 1 dump, got %d", dumps)
	}

	// Four more ticks of the SAME episode must not add dumps.
	for i := 0; i < 4; i++ {
		m.evaluateAndDump(&buf, now+int64(i+1))
	}
	if got := strings.Count(buf.String(), "==== openkanban TUI stall dump"); got != 1 {
		t.Fatalf("sustained stall must yield exactly 1 dump, got %d", got)
	}

	// Progress (updateSeq advances) re-arms the episode; the next stall
	// dumps again.
	m.exitUpdate() // bumps updateSeq, phase->idle
	// Simulate the loop parking again, in-update and old.
	m.enterUpdate("ui.someMsg", "NORMAL", 0, 0)
	m.phaseStartNanos.Store(start)
	if !m.evaluateAndDump(&buf, now+int64(100)) {
		t.Fatal("after progress, a fresh stall should dump again")
	}
	if got := strings.Count(buf.String(), "==== openkanban TUI stall dump"); got != 2 {
		t.Fatalf("want 2 dumps total after re-arm, got %d", got)
	}
}
