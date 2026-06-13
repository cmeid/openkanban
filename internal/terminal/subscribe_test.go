package terminal

import (
	"bytes"
	"runtime"
	"testing"
	"time"
)

// drainEvents reads events from ch until it sees an ExitEvent or the
// deadline elapses. Returns the concatenated OutputEvent payloads and
// whether ExitEvent was observed.
func drainEvents(t *testing.T, ch <-chan Event, deadline time.Duration) (output []byte, sawExit bool) {
	t.Helper()
	timeout := time.After(deadline)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return output, sawExit
			}
			switch e := ev.(type) {
			case OutputEvent:
				output = append(output, e.Data...)
			case ExitEvent:
				sawExit = true
				// keep draining until close so caller sees the
				// complete picture
			}
		case <-timeout:
			return output, sawExit
		}
	}
}

// drainEventsSlow is like drainEvents but inserts a sleep between
// reads so a slow subscriber falls behind a chatty producer.
func drainEventsSlow(t *testing.T, ch <-chan Event, deadline, sleep time.Duration) (output []byte, sawExit bool, drops int) {
	t.Helper()
	timeout := time.After(deadline)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return output, sawExit, drops
			}
			switch e := ev.(type) {
			case OutputEvent:
				output = append(output, e.Data...)
			case ExitEvent:
				sawExit = true
			}
			time.Sleep(sleep)
		case <-timeout:
			return output, sawExit, drops
		}
	}
}

// startTestPane forks a /bin/sh process with the given command and
// returns the running Pane.
func startTestPane(t *testing.T, shCmd string) *Pane {
	t.Helper()
	p := New("test-"+t.Name(), 80, 24, 1000)
	cmd := p.Start("/bin/sh", "-c", shCmd)
	// Start returns a tea.Cmd. Invoke it in a goroutine so it can
	// drive the read loop without blocking the test goroutine; we
	// observe state via subscribers instead.
	go func() { _ = cmd() }()
	// Give Start's goroutine time to acquire/release p.mu and set up
	// the PTY. The exact duration is small (a few ms) — using a
	// short Eventually-style poll on Running keeps tests fast.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.Running() {
			return p
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("pane did not enter Running state within 2s")
	return p
}

func TestSubscribe_TwoSubscribersReceiveSameBytes(t *testing.T) {
	// We intentionally do NOT call p.Stop() in this test: the shell
	// command exits on its own, the PTY closes, and the read loop
	// publishes ExitEvent + closes subscriber channels. Calling
	// Stop() would tear down the response-drain goroutine via
	// Emulator.Close(), which trips a pre-existing race in
	// charm/x/vt's unsynchronized `closed` field. Letting the
	// process exit naturally avoids that path. (See PR2 notes.)
	p := startTestPane(t, "echo hello; sleep 0.05; echo world")

	sub1, unsub1 := p.Subscribe()
	defer unsub1()
	sub2, unsub2 := p.Subscribe()
	defer unsub2()

	out1, exit1 := drainEvents(t, sub1, 3*time.Second)
	out2, exit2 := drainEvents(t, sub2, 3*time.Second)

	if !bytes.Contains(out1, []byte("hello")) || !bytes.Contains(out1, []byte("world")) {
		t.Errorf("sub1 output missing expected tokens: %q", out1)
	}
	if !bytes.Contains(out2, []byte("hello")) || !bytes.Contains(out2, []byte("world")) {
		t.Errorf("sub2 output missing expected tokens: %q", out2)
	}
	if !exit1 {
		t.Errorf("sub1 never saw ExitEvent")
	}
	if !exit2 {
		t.Errorf("sub2 never saw ExitEvent")
	}
}

func TestSubscribe_UnsubscribeStopsReceiving(t *testing.T) {
	// Use a longer-running command so we have time to unsubscribe
	// between writes. We rely on natural process exit (not Stop()) —
	// see the note in TestSubscribe_TwoSubscribersReceiveSameBytes.
	p := startTestPane(t, "echo first; sleep 0.2; echo second; sleep 0.1")

	sub1, unsub1 := p.Subscribe()
	defer unsub1()
	sub2, unsub2 := p.Subscribe()

	// Read the first burst on both subscribers.
	deadline := time.After(1 * time.Second)
	gotFirstOn1 := false
	gotFirstOn2 := false
	for !(gotFirstOn1 && gotFirstOn2) {
		select {
		case ev := <-sub1:
			if oe, ok := ev.(OutputEvent); ok && bytes.Contains(oe.Data, []byte("first")) {
				gotFirstOn1 = true
			}
		case ev := <-sub2:
			if oe, ok := ev.(OutputEvent); ok && bytes.Contains(oe.Data, []byte("first")) {
				gotFirstOn2 = true
			}
		case <-deadline:
			t.Fatalf("timeout waiting for 'first' on both subs (gotFirstOn1=%v, gotFirstOn2=%v)", gotFirstOn1, gotFirstOn2)
		}
	}

	// Unsubscribe sub2; assert the channel closes.
	unsub2()
	closedDeadline := time.After(1 * time.Second)
selectLoop:
	for {
		select {
		case _, ok := <-sub2:
			if !ok {
				break selectLoop
			}
			// Drain any leftover events delivered just before
			// the close.
		case <-closedDeadline:
			t.Fatalf("sub2 channel never closed after unsubscribe")
		}
	}

	// sub1 should still receive subsequent output.
	out1, _ := drainEvents(t, sub1, 3*time.Second)
	if !bytes.Contains(out1, []byte("second")) {
		t.Errorf("sub1 missed 'second' after sub2 unsubscribed: %q", out1)
	}
}

func TestSubscribe_NoGoroutineLeak(t *testing.T) {
	// Capture a baseline. Running tests with -race may slightly
	// fluctuate, so the test allows a small constant slop.
	baseline := runtime.NumGoroutine()

	for i := 0; i < 3; i++ {
		p := startTestPane(t, "echo hi")
		sub, unsub := p.Subscribe()
		// Drain until exit so all the goroutines have a chance to
		// finish their final work before Stop runs.
		_, _ = drainEvents(t, sub, 2*time.Second)
		unsub()
		if err := p.Stop(); err != nil {
			t.Fatalf("Stop returned %v", err)
		}
	}

	// Let scheduled-but-not-yet-finished goroutines (e.g. test
	// runtime housekeeping) settle. We don't use time.Sleep loops
	// to wait for a condition; this is just to let the runtime
	// finalize.
	for attempts := 0; attempts < 50; attempts++ {
		if runtime.NumGoroutine() <= baseline+5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	delta := runtime.NumGoroutine() - baseline
	if delta > 5 {
		t.Errorf("goroutine leak: started with %d, now %d (delta %d)", baseline, runtime.NumGoroutine(), delta)
	}
}

func TestSubscribe_SlowSubscriberDropsNotBlocks(t *testing.T) {
	// Emit many short messages quickly so we exceed the slow
	// subscriber's buffer. Natural process exit handles teardown —
	// see the note in TestSubscribe_TwoSubscribersReceiveSameBytes.
	p := startTestPane(t, "for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do echo line$i; done")

	slow, unsubSlow := p.Subscribe()
	defer unsubSlow()
	fast, unsubFast := p.Subscribe()
	defer unsubFast()

	// fast subscriber drains immediately
	fastDone := make(chan []byte, 1)
	go func() {
		out, _ := drainEvents(t, fast, 3*time.Second)
		fastDone <- out
	}()

	// slow subscriber drains with a delay between reads — this is
	// long enough that the producer outpaces it for the smaller
	// 20-line output but still gets some data through. The key
	// assertion is that the pane DOES NOT DEADLOCK; either
	// subscriber gets dropped events or not, we don't care which.
	slowDone := make(chan struct{})
	go func() {
		_, _, _ = drainEventsSlow(t, slow, 3*time.Second, 100*time.Millisecond)
		close(slowDone)
	}()

	select {
	case fastOut := <-fastDone:
		// Fast subscriber gets the full output because it's reading
		// faster than the producer (and the buffer is well bigger
		// than the burst).
		if !bytes.Contains(fastOut, []byte("line1")) || !bytes.Contains(fastOut, []byte("line20")) {
			t.Errorf("fast subscriber missing expected tokens: %q", fastOut)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("fast subscriber deadlocked — pane is blocking on slow subscriber")
	}

	// slow drain should also eventually wrap up (when the pane
	// exits, all subscriber channels close).
	select {
	case <-slowDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("slow subscriber goroutine deadlocked")
	}
}
