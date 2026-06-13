package terminal

import (
	"fmt"
	"testing"

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
