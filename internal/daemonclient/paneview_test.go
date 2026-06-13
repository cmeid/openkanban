package daemonclient

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"
	xvt "github.com/charmbracelet/x/vt"

	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/terminal"
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

// TestPaneView_HandleMouse_BareDragAutoCopies drives a bare-modifier
// left-button Press/Motion/Release through HandleMouse and verifies
// the selected text lands in the system clipboard. This is the core
// invariant of the "cmd-c without shift" fix — on macOS the terminal
// emulator (Ghostty / iTerm2) intercepts ⌘C before the app sees it,
// so the user needs the auto-copy on selection finish to actually get
// any text into the clipboard.
//
// Skipped when clipboard syscalls aren't available (CI without
// pbcopy / xclip / wl-copy).
func TestPaneView_HandleMouse_BareDragAutoCopies(t *testing.T) {
	if err := clipboard.WriteAll(""); err != nil {
		t.Skipf("clipboard unavailable: %v", err)
	}

	sentinel := "openkanban-paneview-test-sentinel"
	if err := clipboard.WriteAll(sentinel); err != nil {
		t.Fatalf("seed clipboard: %v", err)
	}

	pv := &PaneView{
		state:     PaneViewAttached,
		vt:        xvt.NewSafeEmulator(80, 24),
		selection: terminal.NewSelectionState(),
	}
	pv.vt.Write([]byte("hello world"))

	pv.HandleMouse(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 0, Y: 0})
	pv.HandleMouse(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion, X: 4, Y: 0})
	pv.HandleMouse(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease, X: 4, Y: 0})

	got, err := clipboard.ReadAll()
	if err != nil {
		t.Fatalf("clipboard.ReadAll: %v", err)
	}
	if got == sentinel {
		t.Fatalf("clipboard unchanged from sentinel %q — auto-copy did not fire", sentinel)
	}
	if want := "hello"; !strings.Contains(got, want) {
		t.Errorf("clipboard = %q, want it to contain %q", got, want)
	}
}

// TestPaneView_HandleMouse_BareClickDoesNotCopy confirms that a
// Press-Release without any motion (anchor == cursor) does NOT write
// to the clipboard. selection.Finish() returns the state to Idle in
// that case, so the auto-copy guard (Mode == SelectionSelected) must
// not fire.
func TestPaneView_HandleMouse_BareClickDoesNotCopy(t *testing.T) {
	if err := clipboard.WriteAll(""); err != nil {
		t.Skipf("clipboard unavailable: %v", err)
	}

	sentinel := "openkanban-paneview-click-sentinel"
	if err := clipboard.WriteAll(sentinel); err != nil {
		t.Fatalf("seed clipboard: %v", err)
	}

	pv := &PaneView{
		state:     PaneViewAttached,
		vt:        xvt.NewSafeEmulator(80, 24),
		selection: terminal.NewSelectionState(),
	}
	pv.vt.Write([]byte("hello world"))

	pv.HandleMouse(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 3, Y: 0})
	pv.HandleMouse(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease, X: 3, Y: 0})

	got, err := clipboard.ReadAll()
	if err != nil {
		t.Fatalf("clipboard.ReadAll: %v", err)
	}
	if got != sentinel {
		t.Errorf("clipboard changed after bare click: got %q, want sentinel %q", got, sentinel)
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
