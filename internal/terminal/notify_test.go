package terminal

import (
	"sync"
	"testing"

	xvt "github.com/charmbracelet/x/vt"

	"github.com/techdufus/openkanban/internal/notify"
)

// withRecordedSend swaps notify.Send for the duration of the test with
// a goroutine-safe recorder that captures every body it was called
// with. The recorder is returned so the test can assert on the
// collected bodies; the previous Send is restored on cleanup.
//
// We use a mutex (not just a slice append) because the OSC handler is
// invoked synchronously from vt.Write, which is itself called from the
// drain goroutine in production. Tests don't exercise that goroutine
// here (we drive vt.Write directly), but the helper stays
// goroutine-safe so a future test can fan out without surprise.
type sendRecorder struct {
	mu     sync.Mutex
	bodies []string
}

func (r *sendRecorder) record(body string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies = append(r.bodies, body)
	return nil
}

func (r *sendRecorder) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.bodies))
	copy(out, r.bodies)
	return out
}

func withRecordedSend(t *testing.T) *sendRecorder {
	t.Helper()
	rec := &sendRecorder{}
	prev := notify.Send
	notify.Send = rec.record
	t.Cleanup(func() { notify.Send = prev })
	return rec
}

// newOscPane constructs a Pane wired up to a fresh emulator and
// returns both so a test can register handlers and drive bytes
// through. Doesn't fork a PTY — the handler path is exercised
// entirely in-process.
func newOscPane(t *testing.T) *Pane {
	t.Helper()
	p := New("test", 80, 24, 1000)
	p.vt = xvt.NewSafeEmulator(80, 24)
	p.registerTitleHandlersUnlocked()
	return p
}

// TestForwardNotificationHandler_StripPrefix asserts the OSC 9 handler
// strips the leading "9;" parameter prefix and forwards only the body
// to notify.Send.
func TestForwardNotificationHandler_StripPrefix(t *testing.T) {
	rec := withRecordedSend(t)
	p := newOscPane(t)
	p.SetForwardNotifications(true)

	if !p.forwardNotificationHandler([]byte("9;hello")) {
		t.Fatalf("handler returned false for non-empty enabled payload")
	}

	got := rec.calls()
	if len(got) != 1 {
		t.Fatalf("notify.Send call count = %d, want 1 (recorded=%v)", len(got), got)
	}
	if got[0] != "hello" {
		t.Errorf("notify.Send body = %q, want %q", got[0], "hello")
	}
}

// TestForwardNotificationHandler_Disabled asserts the toggle short-
// circuits the handler entirely: notify.Send must not fire when
// forwardNotifications is false, and the handler must return false so
// the emulator does not treat the OSC as handled.
func TestForwardNotificationHandler_Disabled(t *testing.T) {
	rec := withRecordedSend(t)
	p := newOscPane(t)
	p.SetForwardNotifications(false)

	if p.forwardNotificationHandler([]byte("9;ignored")) {
		t.Errorf("handler returned true with forwarding disabled")
	}
	if got := rec.calls(); len(got) != 0 {
		t.Errorf("notify.Send invoked despite disabled forwarding: %v", got)
	}
}

// TestForwardNotificationHandler_EmptyAfterStrip asserts the handler
// drops payloads that strip down to an empty body — there is nothing
// useful to notify on, and an empty notification would still raise a
// stray system banner on macOS.
func TestForwardNotificationHandler_EmptyAfterStrip(t *testing.T) {
	rec := withRecordedSend(t)
	p := newOscPane(t)
	p.SetForwardNotifications(true)

	if p.forwardNotificationHandler([]byte("9;")) {
		t.Errorf("handler returned true for empty-after-strip payload")
	}
	if got := rec.calls(); len(got) != 0 {
		t.Errorf("notify.Send invoked for empty payload: %v", got)
	}
}

// TestForwardNotificationHandler_VTIntegration drives raw OSC 9 bytes
// through a real xvt.SafeEmulator with the handler registered via
// registerTitleHandlersUnlocked. This is the regression guard that
// fires if the vt library ever stops dispatching to our registered
// handler for OSC 9, or if registerTitleHandlersUnlocked drops the
// OSC 9 registration.
func TestForwardNotificationHandler_VTIntegration(t *testing.T) {
	rec := withRecordedSend(t)
	p := newOscPane(t)
	p.SetForwardNotifications(true)

	// "\x1b]9;<body>\x07" is the canonical OSC 9 shape claude code emits
	// for a desktop notification.
	if _, err := p.vt.Write([]byte("\x1b]9;Claude needs your input\x07")); err != nil {
		t.Fatalf("vt.Write returned err: %v", err)
	}

	got := rec.calls()
	if len(got) != 1 {
		t.Fatalf("notify.Send call count = %d, want 1 (recorded=%v)", len(got), got)
	}
	if got[0] != "Claude needs your input" {
		t.Errorf("notify.Send body = %q, want %q", got[0], "Claude needs your input")
	}
}

// TestForwardNotificationHandler_VTIntegrationDisabled is the negative
// pair of the integration test: with forwarding off, raw OSC 9 bytes
// must not produce a notify.Send call.
func TestForwardNotificationHandler_VTIntegrationDisabled(t *testing.T) {
	rec := withRecordedSend(t)
	p := newOscPane(t)
	p.SetForwardNotifications(false)

	if _, err := p.vt.Write([]byte("\x1b]9;should be suppressed\x07")); err != nil {
		t.Fatalf("vt.Write returned err: %v", err)
	}

	if got := rec.calls(); len(got) != 0 {
		t.Errorf("notify.Send invoked despite disabled forwarding: %v", got)
	}
}

// TestForwardNotificationHandler_SuppressesProgressBar covers iTerm2's
// OSC 9 progress-bar protocol: \x1b]9;<state>;<value>\x07 where state
// ∈ 0..4 (0 clear, 1 set percent, 2 indeterminate, 3 error, 4 warning).
// Claude Code emits these as terminal-progress directives; they MUST
// NOT surface as desktop notifications. After parseOscTitlePayload
// strips the leading "9;" the body has no letters, which is the
// discriminator the handler uses to skip notify.Send.
func TestForwardNotificationHandler_SuppressesProgressBar(t *testing.T) {
	tests := []struct {
		name    string
		payload string // value passed to forwardNotificationHandler (already-cmd-stripped on the vt side)
	}{
		{"warning subtype 4 with value", "9;4;3;"},
		{"set-percent subtype 1", "9;1;50"},
		{"indeterminate subtype 2", "9;2"},
		{"error subtype 3", "9;3"},
		{"clear subtype 0", "9;0"},
		{"trailing semicolons only", "9;4;3;;;"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := withRecordedSend(t)
			p := newOscPane(t)
			p.SetForwardNotifications(true)

			if p.forwardNotificationHandler([]byte(tt.payload)) {
				t.Errorf("handler returned true for progress-bar payload %q; want false (no notify)", tt.payload)
			}
			if got := rec.calls(); len(got) != 0 {
				t.Errorf("notify.Send invoked for progress-bar payload %q: %v", tt.payload, got)
			}
		})
	}
}

// TestForwardNotificationHandler_AllowsLetterPayloads is the positive
// pair: payloads that contain ANY letter pass the discriminator and
// fire notify.Send. Covers the common shapes claude code uses.
func TestForwardNotificationHandler_AllowsLetterPayloads(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{"simple text", "9;Hello", "Hello"},
		{"with punctuation", "9;Claude needs your input", "Claude needs your input"},
		{"single letter", "9;X", "X"},
		{"digits + letters mixed", "9;Step 1 of 3", "Step 1 of 3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := withRecordedSend(t)
			p := newOscPane(t)
			p.SetForwardNotifications(true)

			if !p.forwardNotificationHandler([]byte(tt.payload)) {
				t.Fatalf("handler returned false for text payload %q; want true", tt.payload)
			}
			got := rec.calls()
			if len(got) != 1 {
				t.Fatalf("notify.Send call count = %d, want 1 (recorded=%v)", len(got), got)
			}
			if got[0] != tt.want {
				t.Errorf("notify.Send body = %q, want %q", got[0], tt.want)
			}
		})
	}
}
