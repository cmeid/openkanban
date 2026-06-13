package daemonclient

import (
	"io"
	"os"
	"sync"
	"sync/atomic"
)

// Desktop-notification forwarding: re-emit OSC 9 sequences that the
// wrapped agent (typically Claude Code) writes when it needs the user's
// attention. The wrapped child's stdout is consumed by our terminal
// emulator and never reaches the host terminal, so iTerm2/Ghostty etc.
// would otherwise never see the notification escape. Re-emitting it on
// the openkanban client's own stderr lets the host terminal handle it
// natively — same behaviour the user would get from running the agent
// directly.

var (
	notifyForwardingEnabled atomic.Bool

	notifyWriterMu sync.Mutex
	// notifyWriter is the destination for forwarded OSC 9 sequences.
	// Defaults to os.Stderr so the host terminal sees them; tests swap
	// it for a buffer via setNotifyWriter.
	notifyWriter io.Writer = os.Stderr
)

// SetNotificationForwarding flips OSC 9 re-emission on or off. Wired
// from cmd/root.go once config is loaded; safe to call before any pane
// is constructed.
func SetNotificationForwarding(enabled bool) {
	notifyForwardingEnabled.Store(enabled)
}

// setNotifyWriter is the test seam; production code uses the default
// os.Stderr writer set at package init.
func setNotifyWriter(w io.Writer) {
	notifyWriterMu.Lock()
	defer notifyWriterMu.Unlock()
	notifyWriter = w
}

// forwardOSC9 receives the OSC payload as the vt emulator dispatched
// it — that is, with the leading "9;" parameter prefix still attached
// (see parseOscTitlePayload for the same convention on OSC 0/2).
// We strip the prefix, drop empty messages, and write the canonical
// "\x1b]9;<text>\x07" sequence to notifyWriter under a mutex so
// concurrent panes don't interleave.
//
// Returns true if a write was attempted (toggle on, payload non-empty).
// The vt library treats the bool as "handled"; we always return true
// when the toggle is on so an unhandled-OSC fallback doesn't surface
// the payload as a stray title.
func forwardOSC9(data []byte) bool {
	if !notifyForwardingEnabled.Load() {
		return false
	}
	text := stripOSCParam(data)
	if text == "" {
		return false
	}

	notifyWriterMu.Lock()
	defer notifyWriterMu.Unlock()
	if notifyWriter == nil {
		return false
	}
	out := make([]byte, 0, len(text)+5)
	out = append(out, 0x1b, ']', '9', ';')
	out = append(out, text...)
	out = append(out, 0x07)
	_, _ = notifyWriter.Write(out)
	return true
}

// stripOSCParam removes a leading "<digits>;" parameter from an OSC
// payload, matching what parseOscTitlePayload does for OSC 0/2.
func stripOSCParam(data []byte) string {
	i := 0
	for i < len(data) && data[i] >= '0' && data[i] <= '9' {
		i++
	}
	if i > 0 && i < len(data) && data[i] == ';' {
		return string(data[i+1:])
	}
	return string(data)
}
