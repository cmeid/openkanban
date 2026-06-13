package daemonclient

import (
	"bytes"
	"sync"
	"testing"

	xvt "github.com/charmbracelet/x/vt"
)

// withNotifyWriter swaps the package-level writer for the duration of
// the test and restores the previous one (typically os.Stderr) on
// cleanup. Mirrors the pattern used in t.Setenv etc.
func withNotifyWriter(t *testing.T, w *bytes.Buffer) {
	t.Helper()
	notifyWriterMu.Lock()
	prev := notifyWriter
	notifyWriterMu.Unlock()
	setNotifyWriter(w)
	t.Cleanup(func() { setNotifyWriter(prev) })
}

func TestForwardOSC9(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		input   []byte
		want    string
		handled bool
	}{
		{
			name:    "enabled, standard payload",
			enabled: true,
			input:   []byte("9;Claude is waiting"),
			want:    "\x1b]9;Claude is waiting\x07",
			handled: true,
		},
		{
			name:    "enabled, payload without parameter prefix",
			enabled: true,
			input:   []byte("Hello"),
			want:    "\x1b]9;Hello\x07",
			handled: true,
		},
		{
			name:    "disabled — no write, not handled",
			enabled: false,
			input:   []byte("9;Should not appear"),
			want:    "",
			handled: false,
		},
		{
			name:    "enabled, empty after strip — no write",
			enabled: true,
			input:   []byte("9;"),
			want:    "",
			handled: false,
		},
		{
			name:    "enabled, fully empty payload — no write",
			enabled: true,
			input:   []byte(""),
			want:    "",
			handled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			withNotifyWriter(t, buf)

			SetNotificationForwarding(tt.enabled)
			t.Cleanup(func() { SetNotificationForwarding(false) })

			got := forwardOSC9(tt.input)
			if got != tt.handled {
				t.Errorf("forwardOSC9() returned %v, want %v", got, tt.handled)
			}
			if buf.String() != tt.want {
				t.Errorf("forwardOSC9() wrote %q, want %q", buf.String(), tt.want)
			}
		})
	}
}

func TestStripOSCParam(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"standard", []byte("9;hello"), "hello"},
		{"multi-digit", []byte("777;notify;body"), "notify;body"},
		{"no prefix", []byte("just text"), "just text"},
		{"only digits", []byte("9"), "9"},
		{"empty", []byte(""), ""},
		{"only digits and semicolon", []byte("9;"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripOSCParam(tt.in); got != tt.want {
				t.Errorf("stripOSCParam(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestForwardOSC9Concurrent confirms the mutex serializes writes so
// concurrent panes don't interleave bytes mid-sequence.
func TestForwardOSC9Concurrent(t *testing.T) {
	buf := &bytes.Buffer{}
	withNotifyWriter(t, buf)

	SetNotificationForwarding(true)
	t.Cleanup(func() { SetNotificationForwarding(false) })

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			forwardOSC9([]byte("9;hi"))
		}()
	}
	wg.Wait()

	const seq = "\x1b]9;hi\x07"
	if buf.Len() != n*len(seq) {
		t.Fatalf("got %d bytes, want %d", buf.Len(), n*len(seq))
	}
	got := buf.String()
	for i := 0; i < n; i++ {
		start := i * len(seq)
		end := start + len(seq)
		if got[start:end] != seq {
			t.Fatalf("interleaved output at copy %d: %q", i, got[start:end])
		}
	}
}

// TestForwardOSC9_VTIntegration drives raw OSC 9 bytes through a real
// charm/x/vt emulator with our handler registered the same way PaneView
// registers it. This catches regressions where the library API changes
// or the handler isn't actually invoked on OSC 9 in practice.
func TestForwardOSC9_VTIntegration(t *testing.T) {
	buf := &bytes.Buffer{}
	withNotifyWriter(t, buf)

	SetNotificationForwarding(true)
	t.Cleanup(func() { SetNotificationForwarding(false) })

	vt := xvt.NewSafeEmulator(80, 24)
	vt.RegisterOscHandler(9, forwardOSC9)

	// OSC 9: "\x1b]9;<text>\x07" — what claude code emits for a
	// desktop notification per Anthropic's terminal-config docs.
	_, _ = vt.Write([]byte("\x1b]9;Claude needs your input\x07"))

	want := "\x1b]9;Claude needs your input\x07"
	if buf.String() != want {
		t.Errorf("after vt.Write, got %q, want %q", buf.String(), want)
	}
}

// TestForwardOSC9_VTIntegrationDisabled confirms the toggle short-
// circuits even when the vt emulator dispatches to our handler.
func TestForwardOSC9_VTIntegrationDisabled(t *testing.T) {
	buf := &bytes.Buffer{}
	withNotifyWriter(t, buf)

	SetNotificationForwarding(false)

	vt := xvt.NewSafeEmulator(80, 24)
	vt.RegisterOscHandler(9, forwardOSC9)

	_, _ = vt.Write([]byte("\x1b]9;should be suppressed\x07"))

	if buf.Len() != 0 {
		t.Errorf("expected no writes when forwarding disabled, got %q", buf.String())
	}
}
