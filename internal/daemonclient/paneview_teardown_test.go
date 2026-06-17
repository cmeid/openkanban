package daemonclient

import (
	"testing"
	"time"
)

// TestPaneView_TeardownDoesNotDeadlock guards the flaky teardown deadlock
// (.stuck-session-capture/ANALYSIS.md follow-up).
//
// charm/x/vt is backed by a SYNCHRONOUS io.Pipe: InputPipe() returns the
// pipe's write end, and the parser writes query responses to that same pipe;
// the drain goroutine consumes them via Read. The old teardown woke the drain
// by writing a sentinel byte to that write end — but if the drain had already
// observed drainStop and stopped reading, the sentinel Write blocked forever
// with no reader, hanging teardown (and any caller holding p.mu). That is the
// daemon-wide wedge's little sibling at the single-pane layer.
//
// The race window opens whenever the emulator emits responses (so the drain
// cycles between Read and the stop check). Feeding a DA query (ESC[c) each
// iteration and tearing down in a tight loop reproduces it with high
// probability pre-fix; post-fix (Emulator.Close, which never blocks and makes
// Read return io.EOF) the loop always completes within the deadline.
func TestPaneView_TeardownDoesNotDeadlock(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 400; i++ {
			pv := &PaneView{width: 80, height: 24}
			pv.mu.Lock()
			pv.initEmulatorLocked()
			pv.mu.Unlock()

			// DA query elicits a response -> the parser writes to the pipe
			// -> the drain cycles, opening the stop-vs-read race window.
			pv.applyOutput([]byte("\x1b[c"))
			pv.applyOutput([]byte("hello world\r\n"))
			pv.applyOutput([]byte("\x1b[c"))

			pv.mu.Lock()
			pv.teardownEmulatorLocked()
			pv.mu.Unlock()
		}
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("init/output/teardown loop did not complete in 15s — teardown deadlocked " +
			"waking the emulator drain (synchronous-pipe sentinel-write with no reader)")
	}
}
