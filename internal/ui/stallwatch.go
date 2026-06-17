package ui

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/techdufus/openkanban/internal/config"
)

// stallMonitor is always-on diagnostic instrumentation for the bubbletea
// main goroutine. The TUI has intermittently frozen for tens of seconds
// immediately after a graceful session close (a daemon-pushed
// SessionEvent Event="exited" Expected=true). The block is on the
// Update/View goroutine; the live logs show it processing zero messages
// for 84s while the daemon client's read goroutine kept draining the
// socket and overflowing the subscriber channel.
//
// Because the freeze is intermittent and not reproducible on demand, we
// capture WHAT the main goroutine is parked on the next time it happens:
// a watchdog goroutine samples published atomics every tick and, on a
// stall, writes every goroutine's stack plus discriminating counters to
// a durable file (stderr+SIGQUIT is useless under bubbletea's alt-screen).
//
// Two stall shapes are detected:
//   - "in-call": Update or View has been executing for longer than the
//     threshold (the main goroutine is parked INSIDE a handler/render).
//   - "starved": Update has not run for longer than the threshold WHILE
//     the daemon client kept receiving pushes (the main goroutine is
//     parked OUTSIDE Update/View — e.g. a tea.Batch/tea.Sequence g.Wait
//     in the event loop). This is the shape the incident logs hint at.
//
// Concurrency: the published atomics are written ONLY by the main
// goroutine (enterUpdate/exitUpdate/enterView/exitView run there, and
// bubbletea calls Update then View sequentially on one goroutine) and
// read ONLY by the watchdog — single-writer/single-reader. The
// non-atomic episode-tracking fields (prevSeq, lastSeqChangeNanos, …)
// are touched ONLY by the watchdog goroutine (or, in tests, the test
// goroutine), never concurrently.
type stallMonitor struct {
	// --- published by the main goroutine, read by the watchdog ---
	updateSeq       atomic.Uint64         // bumped on every Update exit (the heartbeat)
	phase           atomic.Int32          // phaseIdle / phaseUpdate / phaseView
	phaseStartNanos atomic.Int64          // when the current non-idle phase began
	inflightMsg     atomic.Pointer[string] // type name of the msg being handled
	mode            atomic.Pointer[string] // Model.mode snapshot at Update entry
	numPanes        atomic.Int64           // len(m.panes) snapshot
	numActive       atomic.Int64           // len(m.daemonOwned) snapshot

	// pushDrop returns the daemon client's cumulative (push, drop)
	// counters. Lets the watchdog tell "starved" (Update idle but events
	// arriving) from genuine idle (nothing arriving). May be nil.
	pushDrop func() (push, drop uint64)

	thresholdNanos int64

	// --- watchdog-goroutine-local episode tracking (no concurrency) ---
	initialized        bool
	prevSeq            uint64
	lastSeqChangeNanos int64
	pushAtLastSeq      uint64
	dumped             bool

	// dumpPath is where stall dumps are appended.
	dumpPath string

	// recover, if set, is invoked once per stall episode for a "starved"
	// stall — the watchdog's corrective action (detach the focused agent
	// view to the board via program.Send). Set by Model.SetStallRecoverySink
	// after the program exists; nil in tests / before wiring. Stored behind
	// a pointer so the watchdog goroutine reads it lock-free. Tea-agnostic
	// on purpose: the closure owns the msg + Send.
	recover atomic.Pointer[func()]

	stopCh   chan struct{}
	stopOnce sync.Once
	sigCh    chan os.Signal
}

// setRecover wires the per-episode recovery action (see the recover field).
func (m *stallMonitor) setRecover(fn func()) {
	if m == nil {
		return
	}
	m.recover.Store(&fn)
}

const (
	phaseIdle   int32 = 0
	phaseUpdate int32 = 1
	phaseView   int32 = 2
)

type stallKind string

const (
	stallInCall  stallKind = "in-call"
	stallStarved stallKind = "starved"
	stallManual  stallKind = "manual"
)

const (
	stallThreshold   = 3 * time.Second
	stallTickInterval = 1 * time.Second
)

// newStallMonitor builds a monitor. pushDrop may be nil (no daemon).
func newStallMonitor(pushDrop func() (push, drop uint64)) *stallMonitor {
	return &stallMonitor{
		pushDrop:       pushDrop,
		thresholdNanos: int64(stallThreshold),
		dumpPath:       stallDumpPath(),
		stopCh:         make(chan struct{}),
	}
}

// stallDumpPath returns ~/.cache/openkanban/tui-stall.log, honoring the
// OPENKANBAN_TUI_STALL_LOG override. Kept separate from tui.log (which is
// high-volume) so the multi-goroutine dumps stay isolated and greppable.
func stallDumpPath() string {
	if p := os.Getenv("OPENKANBAN_TUI_STALL_LOG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "openkanban-tui-stall.log")
	}
	return filepath.Join(home, ".cache", "openkanban", "tui-stall.log")
}

// --- main-goroutine stamping (hot path: atomics only) ---

func (m *stallMonitor) enterUpdate(msgName, mode string, panes, active int) {
	if m == nil {
		return
	}
	m.inflightMsg.Store(&msgName)
	m.mode.Store(&mode)
	m.numPanes.Store(int64(panes))
	m.numActive.Store(int64(active))
	m.phaseStartNanos.Store(time.Now().UnixNano())
	m.phase.Store(phaseUpdate)
}

func (m *stallMonitor) exitUpdate() {
	if m == nil {
		return
	}
	m.phase.Store(phaseIdle)
	m.updateSeq.Add(1)
}

func (m *stallMonitor) enterView() {
	if m == nil {
		return
	}
	m.phaseStartNanos.Store(time.Now().UnixNano())
	m.phase.Store(phaseView)
}

func (m *stallMonitor) exitView() {
	if m == nil {
		return
	}
	m.phase.Store(phaseIdle)
}

// --- watchdog goroutine ---

func (m *stallMonitor) start() {
	if m == nil {
		return
	}
	m.sigCh = make(chan os.Signal, 1)
	signal.Notify(m.sigCh, syscall.SIGUSR2)
	go m.run()
}

func (m *stallMonitor) stop() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() {
		if m.sigCh != nil {
			signal.Stop(m.sigCh)
		}
		close(m.stopCh)
	})
}

func (m *stallMonitor) run() {
	ticker := time.NewTicker(stallTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-m.sigCh:
			m.manualDump()
		case <-ticker.C:
			m.onTick(time.Now().UnixNano())
		}
	}
}

// onTick evaluates the current sample and, if it's a stall, writes one
// dump for this episode (dump FIRST, then the tui.log marker — see the
// ordering note in the body).
func (m *stallMonitor) onTick(nowNanos int64) {
	kind, ok := m.evaluate(nowNanos)
	if !ok {
		return
	}
	config.GuardHomeWrite(m.dumpPath)
	f, err := os.OpenFile(m.dumpPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err == nil {
		m.writeDump(f, kind, nowNanos)
		_ = f.Close()
	}
	m.dumped = true
	// Marker LAST: if the blocked main goroutine is parked inside a
	// log.Printf call holding the log package mutex, this would block —
	// the dump above (its own os.File, no shared mutex) must never be
	// starved by it.
	log.Printf("openkanban: STALL kind=%s see %s", kind, m.dumpPath)

	// Corrective action — once per episode, ONLY for "starved" stalls. In
	// that shape the main loop is parked outside Update/View, so a
	// program.Send is actually processed (and it's a genuine wedge, not a
	// transient slow render — which an "in-call" stall can be, and where
	// Send would queue unprocessed anyway). The closure detaches the
	// focused agent view to the board; the session lives on in the daemon.
	if kind == stallStarved {
		if fp := m.recover.Load(); fp != nil {
			(*fp)()
		}
	}
}

func (m *stallMonitor) manualDump() {
	config.GuardHomeWrite(m.dumpPath)
	f, err := os.OpenFile(m.dumpPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	m.writeDump(f, stallManual, time.Now().UnixNano())
}

// evaluate samples the published atomics and the push counter and
// returns whether the current sample is a stall (and which shape).
//
// It maintains episode state across ticks: a progress observation
// (updateSeq advancing) resets the episode so one sustained stall yields
// exactly one dump, not one per tick.
//
// nowNanos is injected so tests can drive deterministic time windows.
func (m *stallMonitor) evaluate(nowNanos int64) (stallKind, bool) {
	seq := m.updateSeq.Load()
	var pushNow uint64
	if m.pushDrop != nil {
		pushNow, _ = m.pushDrop()
	}

	// First observation seeds the baseline so a slow startup before the
	// first Update can't false-fire "starved" (the starved check below is
	// self-guarding on this tick: now-lastSeqChangeNanos == 0). We fall
	// through rather than return so an already-long-running in-call stall
	// is caught on the first tick too.
	if !m.initialized {
		m.initialized = true
		m.prevSeq = seq
		m.lastSeqChangeNanos = nowNanos
		m.pushAtLastSeq = pushNow
	} else if seq != m.prevSeq {
		// Progress: the main loop ran since the last tick. Re-arm.
		m.prevSeq = seq
		m.lastSeqChangeNanos = nowNanos
		m.pushAtLastSeq = pushNow
		m.dumped = false
	}
	if m.dumped {
		return "", false
	}

	// in-call: a single Update or View has been running past the threshold.
	if ph := m.phase.Load(); ph != phaseIdle {
		if nowNanos-m.phaseStartNanos.Load() > m.thresholdNanos {
			return stallInCall, true
		}
		return "", false
	}

	// starved: Update hasn't run past the threshold WHILE pushes kept
	// arriving — the main goroutine is parked outside Update/View. The
	// push-advance requirement is what distinguishes this from genuine
	// idle (nothing arriving), and is load-bearing: a detector that
	// ignored it would false-positive on a quiet board.
	if nowNanos-m.lastSeqChangeNanos > m.thresholdNanos && pushNow > m.pushAtLastSeq {
		return stallStarved, true
	}
	return "", false
}

// evaluateAndDump is the test seam: evaluate, and on a stall write the
// dump to w and mark the episode dumped. Production uses onTick (which
// opens the dump file); both share evaluate + writeDump.
func (m *stallMonitor) evaluateAndDump(w io.Writer, nowNanos int64) bool {
	kind, ok := m.evaluate(nowNanos)
	if !ok {
		return false
	}
	m.writeDump(w, kind, nowNanos)
	m.dumped = true
	return true
}

// writeDump emits the header (the disambiguating counters) followed by a
// full all-goroutines stack. Mirrors the daemon's SIGUSR1 dump style.
func (m *stallMonitor) writeDump(w io.Writer, kind stallKind, nowNanos int64) {
	var push, drop uint64
	if m.pushDrop != nil {
		push, drop = m.pushDrop()
	}
	ph := m.phase.Load()
	var blocked int64
	if ph != phaseIdle {
		blocked = nowNanos - m.phaseStartNanos.Load()
	} else {
		blocked = nowNanos - m.lastSeqChangeNanos
	}
	msg := ""
	if p := m.inflightMsg.Load(); p != nil {
		msg = *p
	}
	mode := ""
	if p := m.mode.Load(); p != nil {
		mode = *p
	}
	fmt.Fprintf(w, "==== openkanban TUI stall dump @ %s ====\n", time.Now().Format(time.RFC3339Nano))
	fmt.Fprintf(w, "kind=%s blocked=%s phase=%d msg=%q mode=%q panes=%d activeSessions=%d updateSeq=%d pushTotal=%d pushDelta=%d dropTotal=%d\n",
		kind, time.Duration(blocked), ph, msg, mode,
		m.numPanes.Load(), m.numActive.Load(), m.updateSeq.Load(),
		push, push-m.pushAtLastSeq, drop)
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	_, _ = w.Write(buf[:n])
	if n == len(buf) {
		fmt.Fprintf(w, "\n[stack truncated at %d bytes]\n", len(buf))
	}
	fmt.Fprintf(w, "\n")
}

// --- synthetic stall (test-only, env-gated) ---

var debugStallOnce sync.Once

// maybeDebugStall sleeps once if OPENKANBAN_DEBUG_STALL_MS is set, to
// exercise the in-call detector end-to-end against the real bubbletea
// loop. Test/diagnostic only — never a shipped behavior.
func maybeDebugStall() {
	debugStallOnce.Do(func() {
		ms := os.Getenv("OPENKANBAN_DEBUG_STALL_MS")
		if ms == "" {
			return
		}
		d, err := time.ParseDuration(ms + "ms")
		if err != nil || d <= 0 {
			return
		}
		time.Sleep(d)
	})
}
