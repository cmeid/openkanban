package daemonclient

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/daemon"
)

// spawnSessionForTest spins up a /bin/cat session that we can attach to
// and feed input to. Returns the SessionID and a function that kills it.
func spawnSessionForTest(t *testing.T, c *Client) (string, *daemon.SessionInfo) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := c.Spawn(ctx, daemon.SpawnReq{
		TicketID:    "T-PV-1",
		SessionName: "pv-test",
		Command:     "/bin/cat",
		Cols:        80,
		Rows:        24,
		Scrollback:  1000,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	list, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var info daemon.SessionInfo
	for _, s := range list.Sessions {
		if s.SessionID == resp.SessionID {
			info = s
		}
	}
	return resp.SessionID, &info
}

func TestPaneView_Unattached_HandleKeyReturnsAttachFirstMsg(t *testing.T) {
	_ = startTestDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	sid, info := spawnSessionForTest(t, c)
	defer c.Kill(context.Background(), sid, 0)

	pv := NewPaneView(c, "T-PV-1", sid, info)
	defer pv.Close()

	if state := pv.State(); state != PaneViewUnattached {
		t.Fatalf("State: got %s want unattached", state)
	}

	got := pv.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	afm, ok := got.(AttachFirstMsg)
	if !ok {
		t.Fatalf("HandleKey: got %T (%v), want AttachFirstMsg", got, got)
	}
	if afm.PaneID != "T-PV-1" {
		t.Errorf("PaneID: got %q want T-PV-1", afm.PaneID)
	}
}

func TestPaneView_Detached_HandleKeyReturnsNil(t *testing.T) {
	_ = startTestDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	pv := NewPaneView(c, "T-PV-D", "", nil)
	defer pv.Close()
	if state := pv.State(); state != PaneViewDetached {
		t.Fatalf("State: got %s want detached", state)
	}
	got := pv.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if got != nil {
		t.Errorf("HandleKey detached: got %T (%v), want nil", got, got)
	}
	if pv.Running() {
		t.Errorf("Running detached: got true, want false")
	}
	if title := pv.Title(); title != "" {
		t.Errorf("Title detached: got %q, want \"\"", title)
	}
	if content := pv.GetContent(); content != "" {
		t.Errorf("GetContent detached: got %q, want \"\"", content)
	}
}

func TestPaneView_Attach_RenderEcho(t *testing.T) {
	_ = startTestDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	sid, info := spawnSessionForTest(t, c)
	defer c.Kill(context.Background(), sid, 0)

	pv := NewPaneView(c, "T-PV-2", sid, info)
	pv.SetSize(80, 24)
	defer pv.Close()

	if err := pv.Attach(ctx); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if state := pv.State(); state != PaneViewAttached {
		t.Fatalf("post-Attach State: got %s want attached", state)
	}
	if !pv.Running() {
		t.Errorf("Running attached: got false, want true")
	}

	// Feed 'h' through HandleKey. cat echoes it back; the local
	// emulator should reflect it within a brief poll.
	pv.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		view := pv.View()
		content := pv.GetContent()
		if strings.Contains(view, "h") || strings.Contains(content, "h") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("Attach echo: 'h' did not appear in View/GetContent within 2s\nView=%q\nContent=%q",
		pv.View(), pv.GetContent())
}

func TestPaneView_Detach_ClearsState(t *testing.T) {
	_ = startTestDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	sid, info := spawnSessionForTest(t, c)
	defer c.Kill(context.Background(), sid, 0)

	pv := NewPaneView(c, "T-PV-3", sid, info)
	pv.SetSize(80, 24)
	defer pv.Close()

	if err := pv.Attach(ctx); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := pv.Detach(); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if state := pv.State(); state != PaneViewUnattached {
		t.Errorf("post-Detach State: got %s want unattached", state)
	}
	pv.mu.Lock()
	conn := pv.attachConn
	vt := pv.vt
	pv.mu.Unlock()
	if conn != nil {
		t.Errorf("post-Detach attachConn: got non-nil, want nil")
	}
	if vt != nil {
		t.Errorf("post-Detach vt: got non-nil, want nil")
	}
}

func TestPaneView_DaemonDisconnect_EmitsMsg(t *testing.T) {
	_ = startTestDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	sid, info := spawnSessionForTest(t, c)
	defer func() { _ = c.Kill(context.Background(), sid, 0) }()

	pv := NewPaneView(c, "T-PV-4", sid, info)
	pv.SetSize(80, 24)
	defer pv.Close()

	if err := pv.Attach(ctx); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Drain PaneAttachedMsg.
	select {
	case <-pv.TeaMessages():
	case <-time.After(2 * time.Second):
		t.Fatalf("PaneAttachedMsg not received")
	}

	// Kill the daemon's session — the daemon's binaryLoop emits a
	// TypeDetach frame on PaneExitEvent, then the attach conn closes.
	// PaneView's attachLoop sees the TypeDetach (clean exit) and
	// emits PaneDetachedMsg. To exercise the DaemonDisconnectedMsg
	// path instead, we close the underlying attach conn directly.
	pv.mu.Lock()
	conn := pv.attachConn
	pv.mu.Unlock()
	if conn == nil {
		t.Fatalf("attachConn is nil")
	}
	_ = conn.Close()

	// Wait for the attachLoop to surface a disconnect-class message.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case msg := <-pv.TeaMessages():
			switch msg.(type) {
			case DaemonDisconnectedMsg, PaneDetachedMsg:
				return
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Errorf("no DaemonDisconnectedMsg / PaneDetachedMsg within 3s")
}

func TestPaneView_SetSize_Cached_WhenUnattached(t *testing.T) {
	_ = startTestDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	sid, info := spawnSessionForTest(t, c)
	defer c.Kill(context.Background(), sid, 0)

	pv := NewPaneView(c, "T-PV-5", sid, info)
	defer pv.Close()
	pv.SetSize(100, 30)

	w, h := pv.Size()
	if w != 100 || h != 30 {
		t.Errorf("Size cached: got %dx%d want 100x30", w, h)
	}
	if pv.State() != PaneViewUnattached {
		t.Errorf("State after SetSize unattached: got %s want unattached", pv.State())
	}
}

// TestPaneView_View_UnattachedReturnsBlankGrid asserts the vt-nil
// View() returns a full-size grid of spaces (not "") so bubbletea's
// diff renderer overwrites the entire pane region during the brief
// window between mode=ModeAgentView and Attach completion. Returning
// "" leaves the previous mode's content visible underneath the chrome
// bar — the symptom of "garbled initial render on session attach".
func TestPaneView_View_UnattachedReturnsBlankGrid(t *testing.T) {
	_ = startTestDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	pv := NewPaneView(c, "T-PV-VIEW-BLANK", "", nil)
	defer pv.Close()
	pv.SetSize(80, 24)

	got := pv.View()
	if got == "" {
		t.Fatalf("View() with vt=nil returned empty string; want full-size blank grid")
	}
	lines := strings.Split(got, "\n")
	if len(lines) != 24 {
		t.Errorf("View() row count: got %d want 24", len(lines))
	}
	for i, line := range lines {
		if len(line) != 80 {
			t.Errorf("row %d: visible width got %d want 80 (line=%q)", i, len(line), line)
		}
		if strings.TrimSpace(line) != "" {
			t.Errorf("row %d: expected all spaces, got %q", i, line)
		}
	}
}

// TestBlankPaneView_Dimensions covers the degenerate inputs.
func TestBlankPaneView_Dimensions(t *testing.T) {
	tests := []struct {
		name        string
		cols, rows  int
		wantNewline int
		wantLineLen int
		wantEmpty   bool
	}{
		{name: "80x24", cols: 80, rows: 24, wantNewline: 23, wantLineLen: 80},
		{name: "1x1", cols: 1, rows: 1, wantNewline: 0, wantLineLen: 1},
		{name: "zero_cols", cols: 0, rows: 24, wantEmpty: true},
		{name: "zero_rows", cols: 80, rows: 0, wantEmpty: true},
		{name: "negative", cols: -1, rows: -1, wantEmpty: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := blankPaneView(tt.cols, tt.rows)
			if tt.wantEmpty {
				if got != "" {
					t.Errorf("blankPaneView(%d, %d): got %q want \"\"", tt.cols, tt.rows, got)
				}
				return
			}
			gotNewlines := strings.Count(got, "\n")
			if gotNewlines != tt.wantNewline {
				t.Errorf("newline count: got %d want %d", gotNewlines, tt.wantNewline)
			}
			for i, line := range strings.Split(got, "\n") {
				if len(line) != tt.wantLineLen {
					t.Errorf("row %d: got width %d want %d", i, len(line), tt.wantLineLen)
				}
			}
		})
	}
}

func TestPaneView_GetSetWorkdirAndSession(t *testing.T) {
	_ = startTestDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	pv := NewPaneView(c, "T-PV-6", "", nil)
	defer pv.Close()
	pv.SetWorkdir("/tmp/x")
	pv.SetSessionName("named")
	if got := pv.GetWorkdir(); got != "/tmp/x" {
		t.Errorf("GetWorkdir: got %q want /tmp/x", got)
	}
}

// fakeUnknownCSI mimics bubbletea v1's internal unknownCSISequenceMsg:
// a named []byte whose String() begins with "?CSI". ExtractRawCSIBytes
// must recover the underlying bytes via reflection because the real
// type is unexported.
type fakeUnknownCSI []byte

func (u fakeUnknownCSI) String() string {
	return fmt.Sprintf("?CSI%+v?", []byte(u)[2:])
}

func TestExtractRawCSIBytes(t *testing.T) {
	shiftEnter := fakeUnknownCSI{0x1b, '[', '2', '7', ';', '2', ';', '1', '3', '~'}

	tests := []struct {
		name string
		msg  tea.Msg
		want []byte
	}{
		{
			name: "Ghostty shift+enter (CSI 27;2;13~) returns full sequence",
			msg:  shiftEnter,
			want: []byte{0x1b, '[', '2', '7', ';', '2', ';', '1', '3', '~'},
		},
		{
			name: "plain KeyMsg returns nil",
			msg:  tea.KeyMsg{Type: tea.KeyEnter},
			want: nil,
		},
		{
			name: "tea.MouseMsg returns nil",
			msg:  tea.MouseMsg{},
			want: nil,
		},
		{
			name: "arbitrary stringer without ?CSI prefix returns nil",
			msg:  notACSIStringer{},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractRawCSIBytes(tt.msg)
			if !bytes.Equal(got, tt.want) {
				t.Errorf("ExtractRawCSIBytes(%v) = %v, want %v", tt.msg, got, tt.want)
			}
		})
	}
}

type notACSIStringer struct{}

func (notACSIStringer) String() string { return "some other tea msg" }

func TestTranslateKey_EnterAndShiftEnter(t *testing.T) {
	tests := []struct {
		name string
		msg  tea.KeyMsg
		want []byte
	}{
		{
			name: "plain Enter emits CR",
			msg:  tea.KeyMsg{Type: tea.KeyEnter},
			want: []byte("\r"),
		},
		{
			// Bubbletea v1 has no Shift field; terminals configured
			// for shift+enter emit ESC+CR, which lands here as
			// Alt+Enter. The PTY must see ESC+CR so the inner agent
			// (Claude Code, etc.) inserts a newline instead of
			// submitting.
			name: "Alt+Enter (shift+enter) emits ESC+CR",
			msg:  tea.KeyMsg{Type: tea.KeyEnter, Alt: true},
			want: []byte{27, '\r'},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := translateKey(tt.msg)
			if !bytes.Equal(got, tt.want) {
				t.Errorf("translateKey(%v) = %q, want %q", tt.msg, got, tt.want)
			}
		})
	}
}
