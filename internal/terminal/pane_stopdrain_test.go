package terminal

import (
	"testing"
	"time"

	xvt "github.com/charmbracelet/x/vt"
)

// TestPane_StopDrainDoesNotDeadlock guards the daemon-side analog of the
// client teardown deadlock (see daemonclient TestPaneView_TeardownDoesNotDeadlock).
//
// stopDrainUnlocked wakes the response-drain goroutine by writing a sentinel
// byte to the emulator's InputPipe — but charm/x/vt is backed by a SYNCHRONOUS
// io.Pipe, so that write blocks forever whenever the drain has already observed
// drainStop and stopped reading (no reader). Worse than the client side:
// stopDrainUnlocked then calls drainWG.Wait() SYNCHRONOUSLY while the caller
// holds p.mu, so the block wedges the pane (and any p.mu-dependent path).
//
// The race window opens when the emulator emits responses (the drain cycles
// between Read and the stop check). Feeding DA queries (ESC[c) each iteration
// and stopping the drain in a tight loop reproduces it pre-fix; post-fix
// (CloseWithError(io.EOF) on the pipe writer) the loop always completes.
func TestPane_StopDrainDoesNotDeadlock(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 400; i++ {
			p := New("test", 80, 24, 1000)
			p.vt = xvt.NewSafeEmulator(80, 24)
			p.mu.Lock()
			p.startDrainUnlocked()
			p.mu.Unlock()

			// DA query elicits a response -> the parser writes it to the
			// pipe -> the drain cycles, opening the stop-vs-read race window.
			_, _ = p.vt.Write([]byte("\x1b[c"))
			_, _ = p.vt.Write([]byte("hello world\r\n"))
			_, _ = p.vt.Write([]byte("\x1b[c"))

			p.mu.Lock()
			p.stopDrainUnlocked()
			p.mu.Unlock()
		}
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("startDrain/stopDrain loop did not complete in 15s — stopDrainUnlocked " +
			"deadlocked on the sentinel write to the synchronous io.Pipe with no reader")
	}
}
