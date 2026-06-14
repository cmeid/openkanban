package terminal

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	xvt "github.com/charmbracelet/x/vt"
)

func TestDetectMouseModeChanges(t *testing.T) {
	tests := []struct {
		name           string
		data           []byte
		initialEnabled bool
		wantEnabled    bool
	}{
		{
			name:           "X10 mouse tracking enable",
			data:           []byte("\x1b[?1000h"),
			initialEnabled: false,
			wantEnabled:    true,
		},
		{
			name:           "Button-event tracking enable",
			data:           []byte("\x1b[?1002h"),
			initialEnabled: false,
			wantEnabled:    true,
		},
		{
			name:           "Any-event tracking enable",
			data:           []byte("\x1b[?1003h"),
			initialEnabled: false,
			wantEnabled:    true,
		},
		{
			name:           "SGR extended mode enable",
			data:           []byte("\x1b[?1006h"),
			initialEnabled: false,
			wantEnabled:    true,
		},
		{
			name:           "X10 mouse tracking disable",
			data:           []byte("\x1b[?1000l"),
			initialEnabled: true,
			wantEnabled:    false,
		},
		{
			name:           "Button-event tracking disable",
			data:           []byte("\x1b[?1002l"),
			initialEnabled: true,
			wantEnabled:    false,
		},
		{
			name:           "Any-event tracking disable",
			data:           []byte("\x1b[?1003l"),
			initialEnabled: true,
			wantEnabled:    false,
		},
		{
			name:           "SGR extended mode disable",
			data:           []byte("\x1b[?1006l"),
			initialEnabled: true,
			wantEnabled:    false,
		},
		{
			name:           "Sequence embedded in other data",
			data:           []byte("some text\x1b[?1000hmore text"),
			initialEnabled: false,
			wantEnabled:    true,
		},
		{
			name:           "No mouse sequence - state unchanged",
			data:           []byte("regular terminal output"),
			initialEnabled: false,
			wantEnabled:    false,
		},
		{
			name:           "No mouse sequence - enabled stays enabled",
			data:           []byte("regular terminal output"),
			initialEnabled: true,
			wantEnabled:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Pane{mouseEnabled: tt.initialEnabled}
			p.detectMouseModeChanges(tt.data)
			if p.mouseEnabled != tt.wantEnabled {
				t.Errorf("mouseEnabled = %v, want %v", p.mouseEnabled, tt.wantEnabled)
			}
		})
	}
}

func TestDetectAltScreenChanges(t *testing.T) {
	tests := []struct {
		name         string
		data         []byte
		initialState bool
		expectedState bool
	}{
		{
			name:         "Enable alt screen 1049h",
			data:         []byte("\x1b[?1049h"),
			initialState: false,
			expectedState: true,
		},
		{
			name:         "Enable alt screen 47h",
			data:         []byte("\x1b[?47h"),
			initialState: false,
			expectedState: true,
		},
		{
			name:         "Disable alt screen 1049l",
			data:         []byte("\x1b[?1049l"),
			initialState: true,
			expectedState: false,
		},
		{
			name:         "Disable alt screen 47l",
			data:         []byte("\x1b[?47l"),
			initialState: true,
			expectedState: false,
		},
		{
			name:         "Sequence embedded in other data",
			data:         []byte("Hello\x1b[?1049hWorld"),
			initialState: false,
			expectedState: true,
		},
		{
			name:         "No alt screen sequence - state unchanged",
			data:         []byte("Hello World"),
			initialState: false,
			expectedState: false,
		},
		{
			name:         "No alt screen sequence - enabled stays enabled",
			data:         []byte("Hello World"),
			initialState: true,
			expectedState: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pane := New("test", 80, 24, 1000)
			pane.altScreenActive = tc.initialState
			pane.detectAltScreenChanges(tc.data)
			if pane.altScreenActive != tc.expectedState {
				t.Errorf("expected altScreenActive=%v, got %v", tc.expectedState, pane.altScreenActive)
			}
		})
	}
}

func TestParseOscTitlePayload(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "OSC 0 prefix", in: "0;hello-title", want: "hello-title"},
		{name: "OSC 2 prefix", in: "2;window-title", want: "window-title"},
		{name: "OSC 1 prefix", in: "1;icon name", want: "icon name"},
		{name: "Multi-digit prefix", in: "10;color-payload", want: "color-payload"},
		{name: "No prefix", in: "bare title", want: "bare title"},
		{name: "Empty payload after prefix", in: "2;", want: ""},
		{name: "Title contains semicolons", in: "2;a;b;c", want: "a;b;c"},
		{name: "Empty input", in: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseOscTitlePayload([]byte(tt.in))
			if got != tt.want {
				t.Errorf("parseOscTitlePayload(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestPaneOscTitleCapture wires the title handlers onto a fresh emulator
// and writes a raw OSC 2 escape sequence through it. Exercises the full
// path from emulator parse → registered handler → atomic Title()
// without spinning up a PTY (which would require a forked subprocess).
func TestPaneOscTitleCapture(t *testing.T) {
	p := New("test", 80, 24, 1000)
	p.vt = xvt.NewSafeEmulator(80, 24)
	p.registerTitleHandlersUnlocked()

	if got := p.Title(); got != "" {
		t.Fatalf("Title before any OSC = %q, want empty", got)
	}

	// OSC 2 (set window title), terminated with BEL.
	p.vt.Write([]byte("\x1b]2;hello-title\x07"))
	if got := p.Title(); got != "hello-title" {
		t.Errorf("after OSC 2, Title = %q, want %q", got, "hello-title")
	}

	// OSC 0 (set window title + icon name), terminated with ST.
	p.vt.Write([]byte("\x1b]0;next-title\x1b\\"))
	if got := p.Title(); got != "next-title" {
		t.Errorf("after OSC 0, Title = %q, want %q", got, "next-title")
	}
}

func TestViewportScrolling(t *testing.T) {
	pane := New("test", 80, 24, 100)
	pane.scrollback = NewScrollbackBuffer(100)

	// Add some lines to scrollback
	for i := 0; i < 50; i++ {
		pane.scrollback.Push(makeTestLine(fmt.Sprintf("line%d", i)))
	}

	// Test scrollUp
	pane.scrollUp(10)
	if pane.viewportOffset != 10 {
		t.Errorf("after scrollUp(10), expected offset=10, got %d", pane.viewportOffset)
	}

	// Test scrollUp beyond max
	pane.scrollUp(100)
	if pane.viewportOffset != 50 {
		t.Errorf("scrollUp beyond max should cap at scrollback length, got %d", pane.viewportOffset)
	}

	// Test scrollDown
	pane.scrollDown(20)
	if pane.viewportOffset != 30 {
		t.Errorf("after scrollDown(20), expected offset=30, got %d", pane.viewportOffset)
	}

	// Test scrollDown to 0
	pane.scrollDown(100)
	if pane.viewportOffset != 0 {
		t.Errorf("scrollDown beyond 0 should cap at 0, got %d", pane.viewportOffset)
	}
}

func TestTranslateKey(t *testing.T) {
	tests := []struct {
		name string
		msg  tea.KeyMsg
		want []byte
	}{
		{
			name: "Tab",
			msg:  tea.KeyMsg{Type: tea.KeyTab},
			want: []byte("\t"),
		},
		{
			name: "Shift+Tab emits CSI Z",
			msg:  tea.KeyMsg{Type: tea.KeyShiftTab},
			want: []byte("\x1b[Z"),
		},
		{
			name: "Enter",
			msg:  tea.KeyMsg{Type: tea.KeyEnter},
			want: []byte("\r"),
		},
		{
			name: "Up arrow",
			msg:  tea.KeyMsg{Type: tea.KeyUp},
			want: []byte("\x1b[A"),
		},
	}

	p := &Pane{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.translateKey(tt.msg)
			if !bytes.Equal(got, tt.want) {
				t.Errorf("translateKey(%v) = %q, want %q", tt.msg, got, tt.want)
			}
		})
	}
}

func TestBuildCleanEnv(t *testing.T) {
	tests := []struct {
		name         string
		sessionName  string
		ticketID     string
		wantSession  string // "" means must be absent
		wantTicketID string // "" means must be absent
	}{
		{
			name:         "both set",
			sessionName:  "task/foo",
			ticketID:     "abc-123",
			wantSession:  "OPENKANBAN_SESSION=task/foo",
			wantTicketID: "OPENKANBAN_TICKET_ID=abc-123",
		},
		{
			name:        "session only",
			sessionName: "task/foo",
			wantSession: "OPENKANBAN_SESSION=task/foo",
		},
		{
			name:         "ticket only",
			ticketID:     "abc-123",
			wantTicketID: "OPENKANBAN_TICKET_ID=abc-123",
		},
		{
			name: "neither",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := buildCleanEnv(tt.sessionName, tt.ticketID)

			contains := func(target string) bool {
				for _, e := range env {
					if e == target {
						return true
					}
				}
				return false
			}
			anyWithPrefix := func(prefix string) bool {
				for _, e := range env {
					if strings.HasPrefix(e, prefix) {
						return true
					}
				}
				return false
			}

			if tt.wantSession != "" && !contains(tt.wantSession) {
				t.Errorf("missing %q in env", tt.wantSession)
			}
			if tt.wantSession == "" && anyWithPrefix("OPENKANBAN_SESSION=") {
				t.Errorf("OPENKANBAN_SESSION must be absent when sessionName empty; got env=%v", env)
			}
			if tt.wantTicketID != "" && !contains(tt.wantTicketID) {
				t.Errorf("missing %q in env", tt.wantTicketID)
			}
			if tt.wantTicketID == "" && anyWithPrefix("OPENKANBAN_TICKET_ID=") {
				t.Errorf("OPENKANBAN_TICKET_ID must be absent when ticketID empty; got env=%v", env)
			}
			if !contains("TERM=xterm-256color") {
				t.Errorf("expected TERM=xterm-256color in env")
			}
		})
	}
}

// TestBuildCleanEnv_StripsInheritedOpenkanban guards the env-leak fix
// in T2 of the integration plan: any OPENKANBAN_* in the inherited
// process env MUST be stripped before the freshly-spawned values are
// appended, so a nested openkanban pane (or a daemon-spawned session
// whose daemon process itself has those vars set) doesn't accidentally
// inherit the outer pane's identity.
func TestBuildCleanEnv_StripsInheritedOpenkanban(t *testing.T) {
	t.Setenv("OPENKANBAN_SESSION", "leaky")
	t.Setenv("OPENKANBAN_TICKET_ID", "stale-id")
	t.Setenv("OPENKANBAN_PTY_DEBUG_LOG", "/tmp/whatever")

	env := buildCleanEnv("fresh-session", "T-1")

	contains := func(target string) bool {
		for _, e := range env {
			if e == target {
				return true
			}
		}
		return false
	}
	anyWithPrefix := func(prefix string) bool {
		for _, e := range env {
			if strings.HasPrefix(e, prefix) {
				return true
			}
		}
		return false
	}

	if !contains("OPENKANBAN_SESSION=fresh-session") {
		t.Errorf("expected OPENKANBAN_SESSION=fresh-session; env=%v", env)
	}
	if !contains("OPENKANBAN_TICKET_ID=T-1") {
		t.Errorf("expected OPENKANBAN_TICKET_ID=T-1; env=%v", env)
	}
	// Specifically: the inherited leaky values must NOT have made it
	// through. Each var must appear exactly once, with the fresh value.
	for _, e := range env {
		if e == "OPENKANBAN_SESSION=leaky" {
			t.Errorf("inherited OPENKANBAN_SESSION=leaky leaked through; env=%v", env)
		}
		if e == "OPENKANBAN_TICKET_ID=stale-id" {
			t.Errorf("inherited OPENKANBAN_TICKET_ID=stale-id leaked through; env=%v", env)
		}
	}
	// And no other OPENKANBAN_* (e.g. OPENKANBAN_PTY_DEBUG_LOG) should
	// have survived the strip.
	for _, e := range env {
		if strings.HasPrefix(e, "OPENKANBAN_") &&
			!strings.HasPrefix(e, "OPENKANBAN_SESSION=") &&
			!strings.HasPrefix(e, "OPENKANBAN_TICKET_ID=") {
			t.Errorf("unexpected inherited OPENKANBAN_* survived strip: %q", e)
		}
	}
	if anyWithPrefix("OPENKANBAN_PTY_DEBUG_LOG") {
		t.Errorf("OPENKANBAN_PTY_DEBUG_LOG should have been stripped; env=%v", env)
	}
}

// TestSchedulePostSpawnInput_FiresAndEchoes asserts the timer-driven
// PTY write actually lands on the child. We spawn `cat`, which echoes
// stdin back to stdout. After SchedulePostSpawnInput with a short
// delay, the bytes written into the PTY master come back through the
// pane's subscriber as an OutputEvent. The cat process is killed via
// p.Stop() before the test returns; that also cancels the timer (no-
// op here since it already fired).
func TestSchedulePostSpawnInput_FiresAndEchoes(t *testing.T) {
	p := startTestPane(t, "cat")
	t.Cleanup(func() { _ = p.Stop() })

	sub, unsub := p.Subscribe()
	defer unsub()

	const payload = "hello-post-spawn\n"
	p.SchedulePostSpawnInput([]byte(payload), 50*time.Millisecond)

	// Read events until we see the payload echoed back, or 2s passes.
	deadline := time.After(2 * time.Second)
	var collected []byte
	for {
		select {
		case ev, ok := <-sub:
			if !ok {
				t.Fatalf("subscriber channel closed before seeing %q (got %q)", payload, collected)
			}
			if oe, ok := ev.(OutputEvent); ok {
				collected = append(collected, oe.Data...)
				if strings.Contains(string(collected), "hello-post-spawn") {
					return
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for cat to echo %q; got %q", payload, collected)
		}
	}
}

// TestSchedulePostSpawnInput_EmptyIsNoOp asserts the zero-length
// guard: no timer is created (so nothing can later fire) when the
// caller passes an empty slice. We assert p.postSpawnTimer remains
// nil after the call.
func TestSchedulePostSpawnInput_EmptyIsNoOp(t *testing.T) {
	p := New("test", 80, 24, 100)
	p.SchedulePostSpawnInput(nil, 10*time.Millisecond)
	p.SchedulePostSpawnInput([]byte{}, 10*time.Millisecond)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.postSpawnTimer != nil {
		t.Errorf("postSpawnTimer should be nil for empty input; got %v", p.postSpawnTimer)
	}
}

// TestSchedulePostSpawnInput_StopCancels asserts Stop() cancels the
// pending timer so the callback can't fire after teardown. We use a
// long delay (2s) plus a short test window (200ms); without the
// cancel in Stop, the callback would race the test exit and try to
// write to a closed PTY.
func TestSchedulePostSpawnInput_StopCancels(t *testing.T) {
	p := startTestPane(t, "cat")
	p.SchedulePostSpawnInput([]byte("late\n"), 2*time.Second)

	// Snapshot timer pointer under the lock.
	p.mu.Lock()
	had := p.postSpawnTimer != nil
	p.mu.Unlock()
	if !had {
		t.Fatalf("expected postSpawnTimer to be set before Stop")
	}

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop returned %v", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.postSpawnTimer != nil {
		t.Errorf("postSpawnTimer should be cleared after Stop; got %v", p.postSpawnTimer)
	}
}
