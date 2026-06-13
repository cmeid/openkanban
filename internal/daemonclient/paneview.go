package daemonclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	xvt "github.com/charmbracelet/x/vt"

	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/terminal"
)

// PaneViewState models the three-state machine the UI needs to know
// about for a daemon-owned pane. The transitions match the plan's
// Task 7 state matrix exactly.
type PaneViewState int

const (
	// PaneViewDetached is the "no daemon, or no known session" state.
	// Most operations are no-ops; Running()=false; GetContent()="".
	PaneViewDetached PaneViewState = iota
	// PaneViewUnattached means the daemon owns a session for this
	// ticket but we have no binary connection open. HandleKey returns
	// AttachFirstMsg so the UI knows to prompt for attach.
	PaneViewUnattached
	// PaneViewAttached means we hold a live binary conn and the local
	// emulator is being kept in sync via TypePTYOutput frames.
	PaneViewAttached
)

// String makes the state self-describing in logs.
func (s PaneViewState) String() string {
	switch s {
	case PaneViewDetached:
		return "detached"
	case PaneViewUnattached:
		return "unattached"
	case PaneViewAttached:
		return "attached"
	}
	return "unknown"
}

// --- Tea messages emitted by PaneView and the Client. ---

// DaemonDisconnectedMsg is published when an attach loop (or, eventually,
// the client's control conn) observes the daemon going away. The model
// is expected to clean up local references and prompt the user. We do
// NOT silently reconnect from this layer.
type DaemonDisconnectedMsg struct {
	Err error
}

// PaneAttachedMsg fires when a PaneView completes Attach. Lets the
// model re-render the pane row's "running" indicator if it was previously
// unattached.
type PaneAttachedMsg struct {
	PaneID string
}

// PaneDetachedMsg fires when a PaneView detaches cleanly (user-initiated
// or daemon-initiated). The session may still be alive on the daemon
// side — the model decides whether to display it as unattached or
// detached based on its own List poll.
type PaneDetachedMsg struct {
	PaneID string
}

// AttachFirstMsg is returned from PaneView.HandleKey when the user
// presses a key while in PaneViewUnattached. The UI is expected to
// either auto-Attach or prompt.
type AttachFirstMsg struct {
	PaneID string
}

// PaneOutputMsg is the per-pane re-arming signal that the attach
// goroutine emits when it writes new bytes into the local emulator.
// Mirrors terminal.OutputMsg's role on the in-process side.
type PaneOutputMsg struct {
	PaneID string
}

// PaneExitMsg fires when the daemon notifies us the session has ended
// (TypeDetach frame with the session no longer running).
type PaneExitMsg struct {
	PaneID string
	Err    error
}

// PaneRenderTickMsg is the throttled-render trigger. Same role as
// terminal.RenderTickMsg, but namespaced so the model can route them.
type PaneRenderTickMsg struct {
	PaneID string
}

// --- PaneView ---

// PaneView is the client-side mirror of a daemon-owned session. It
// holds a local xvt.SafeEmulator that the attach goroutine drives via
// TypePTYOutput frames from the daemon. From the UI's perspective the
// surface matches terminal.Pane's: Title / SetSize / HandleKey /
// HandleMouse / View / GetContent / Running / etc.
//
// PaneView is NOT goroutine-safe by accident — every accessor takes
// p.mu before touching the emulator or any state that the attach
// goroutine also touches. Reads that don't need cross-field consistency
// (e.g. ID()) skip the lock.
type PaneView struct {
	client *Client
	id     string

	mu sync.Mutex

	// session bookkeeping
	state       PaneViewState
	sessionID   string
	sessionName string
	workdir     string

	// Local rendering state. Mirrors the corresponding fields on
	// terminal.Pane so View/GetContent can reuse the same render code.
	width, height   int
	vt              *xvt.SafeEmulator
	scrollback      *terminal.ScrollbackBuffer
	selection       *terminal.SelectionState
	viewportOffset  int
	cachedView      string
	dirty           bool
	cursorHidden    atomic.Bool
	mouseEnabled    bool
	altScreenActive bool
	cachedTitle     string

	// Last successful List snapshot — populated by NewPaneView (when
	// the caller passes one) and refreshed by Refresh(). Read in
	// Running / GetWorkdir when not attached.
	lastInfo *daemon.SessionInfo

	// Attach state. attachConn is the per-attach binary conn (NOT the
	// shared Client.conn). attachR is its bufio.Reader. detachCh is
	// closed when the attach loop exits to wake any external waiter.
	attachConn   net.Conn
	attachR      *bufio.Reader
	attachWriteM sync.Mutex // serializes binary writes on attachConn
	attachLoopWG sync.WaitGroup
	detachCh     chan struct{}

	// drainStop terminates the local emulator's response-drain
	// goroutine — exactly the same pattern terminal.Pane uses, because
	// charm/x/vt deadlocks on its first DA query without one. Unlike
	// the server's pane, we DROP the emulator's response bytes rather
	// than forward them — the daemon already has its own drain on the
	// real PTY side.
	drainStop chan struct{}
	drainWG   sync.WaitGroup

	// teaMsgs is the channel the UI's tea.Cmd reads from to learn
	// about output / exit / detach. Capacity is small — these are
	// signals, not data.
	teaMsgs   chan tea.Msg
	teaClosed atomic.Bool
}

// NewPaneView constructs a fresh PaneView for the daemon-owned session
// identified by sessionID. info may be nil — in that case the view
// starts in PaneViewDetached and the caller is expected to set state
// via a subsequent List + Refresh, or to Attach directly (which will
// flip the state on success).
//
// The caller still owns the *Client; PaneView does not Close it.
func NewPaneView(client *Client, ticketID, sessionID string, info *daemon.SessionInfo) *PaneView {
	pv := &PaneView{
		client:    client,
		id:        ticketID,
		state:     PaneViewDetached,
		sessionID: sessionID,
		teaMsgs:   make(chan tea.Msg, 64),
		detachCh:  make(chan struct{}),
	}
	if info != nil {
		copy := *info
		pv.lastInfo = &copy
		pv.cachedTitle = info.Title
		pv.width = info.Cols
		pv.height = info.Rows
		pv.workdir = info.Workdir
		pv.sessionName = info.SessionName
		if info.Running {
			pv.state = PaneViewUnattached
		}
	}
	return pv
}

// ID returns the ticket identifier this view represents.
func (p *PaneView) ID() string { return p.id }

// SessionID returns the daemon-side session ID, or "" if the view does
// not yet point at a session.
func (p *PaneView) SessionID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessionID
}

// State returns the current state of the view. Exposed so the UI can
// branch on it without going through the side-effecting accessors.
func (p *PaneView) State() PaneViewState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// SetWorkdir caches a workdir locally. Informational — Spawn carries
// the real workdir to the daemon. The model still calls this after a
// successful spawn so GetWorkdir returns the expected value.
func (p *PaneView) SetWorkdir(dir string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.workdir = dir
}

// GetWorkdir returns the cached workdir. Falls back to lastInfo when
// the model hasn't called SetWorkdir yet.
func (p *PaneView) GetWorkdir() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.workdir != "" {
		return p.workdir
	}
	if p.lastInfo != nil {
		return p.lastInfo.Workdir
	}
	return ""
}

// SetSessionName caches the OPENKANBAN_SESSION value. Same shape as
// the corresponding terminal.Pane method.
func (p *PaneView) SetSessionName(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessionName = name
}

// Title returns the most recent OSC 0/2 window title.
//
// State matrix:
//   - PaneViewAttached: live emulator's last OSC 0/2 (via the title
//     callback we register on Attach).
//   - PaneViewUnattached: cached title from the last List response.
//   - PaneViewDetached: "".
func (p *PaneView) Title() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch p.state {
	case PaneViewAttached:
		return p.cachedTitle
	case PaneViewUnattached:
		if p.lastInfo != nil {
			return p.lastInfo.Title
		}
		return p.cachedTitle
	}
	return ""
}

// Running reports whether the underlying session is alive.
//
//   - PaneViewAttached: the binary conn is open → return true. (The
//     attach loop closes it when the daemon notifies of exit.)
//   - PaneViewUnattached: trust lastInfo.Running.
//   - PaneViewDetached: false.
func (p *PaneView) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch p.state {
	case PaneViewAttached:
		return p.attachConn != nil
	case PaneViewUnattached:
		return p.lastInfo != nil && p.lastInfo.Running
	}
	return false
}

// SetSize forwards a resize to the daemon when attached. When
// unattached we cache the requested dims so the next Attach uses them.
// In PaneViewDetached it's a no-op.
func (p *PaneView) SetSize(width, height int) {
	p.mu.Lock()
	if p.width == width && p.height == height {
		p.mu.Unlock()
		return
	}
	p.width = width
	p.height = height
	p.dirty = true
	p.cachedView = ""
	if p.selection != nil && p.selection.IsActive() {
		p.selection.Clear()
	}
	p.viewportOffset = 0
	if p.vt != nil {
		p.vt.Resize(width, height)
	}
	state := p.state
	conn := p.attachConn
	p.mu.Unlock()

	if state == PaneViewAttached && conn != nil {
		// 16-bit clamp — the wire protocol uses uint16 for cols/rows.
		cols, rows := uint16(width), uint16(height)
		if width < 0 || width > 0xFFFF {
			cols = 0
		}
		if height < 0 || height > 0xFFFF {
			rows = 0
		}
		payload := daemon.EncodeResize(cols, rows, p.client.ClientID())
		p.attachWriteM.Lock()
		_ = daemon.WriteFrame(conn, daemon.TypeResize, payload)
		p.attachWriteM.Unlock()
	}
}

// Size returns the cached pane dimensions. Provided for parity with
// terminal.Pane.Size, even though the UI does not currently call it.
func (p *PaneView) Size() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.width, p.height
}

// ScrollbackLen returns the number of lines in the local scrollback
// buffer. 0 when not attached.
func (p *PaneView) ScrollbackLen() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.scrollback == nil {
		return 0
	}
	return p.scrollback.Len()
}

// ViewportOffset returns how many lines the local viewport is scrolled
// back. 0 when not attached.
func (p *PaneView) ViewportOffset() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.viewportOffset
}

// TeaMessages returns a channel the model can range over to receive
// output / exit / detach signals from the attach goroutine. The
// channel is created at construction; the attach goroutine writes to
// it; the model's tea.Cmd reads one event per Cmd invocation.
//
// Returns nil after Close().
func (p *PaneView) TeaMessages() <-chan tea.Msg {
	return p.teaMsgs
}

// --- Local emulator setup ---

// initEmulatorLocked allocates the local emulator and scrollback for
// PaneViewAttached. Must be called with p.mu held.
func (p *PaneView) initEmulatorLocked() {
	if p.vt != nil {
		return
	}
	w, h := p.width, p.height
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	p.width, p.height = w, h
	p.vt = xvt.NewSafeEmulator(w, h)
	p.vt.SetCallbacks(xvt.Callbacks{
		CursorVisibility: func(visible bool) {
			p.cursorHidden.Store(!visible)
		},
	})
	p.cursorHidden.Store(false)
	p.scrollback = terminal.NewScrollbackBuffer(10000)
	p.selection = terminal.NewSelectionState()
	titleHandler := func(data []byte) bool {
		title := parseOscTitlePayload(data)
		p.mu.Lock()
		p.cachedTitle = title
		p.mu.Unlock()
		return true
	}
	p.vt.RegisterOscHandler(0, titleHandler)
	p.vt.RegisterOscHandler(2, titleHandler)

	// charm/x/vt deadlocks on its first device-attributes query
	// without a Read drain. We DON'T forward the response anywhere —
	// the daemon's own pane already did that round-trip with the real
	// child. We just keep the pipe drained.
	p.drainStop = make(chan struct{})
	stop := p.drainStop
	vt := p.vt
	p.drainWG.Add(1)
	go func() {
		defer p.drainWG.Done()
		buf := make([]byte, 4096)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, err := vt.Read(buf)
			if err != nil {
				if errors.Is(err, io.EOF) {
					return
				}
				return
			}
		}
	}()
}

// teardownEmulatorLocked stops the drain goroutine and releases the
// emulator. Must be called with p.mu held.
func (p *PaneView) teardownEmulatorLocked() {
	if p.drainStop != nil {
		close(p.drainStop)
		// Wake the drain goroutine via a sentinel byte, same as
		// terminal.Pane.stopDrainUnlocked. Without this, vt.Read
		// blocks forever and drainWG.Wait deadlocks.
		if p.vt != nil {
			if w := p.vt.Emulator.InputPipe(); w != nil {
				_, _ = w.Write([]byte{0})
			}
		}
		p.drainStop = nil
	}
	// Wait without holding the lock to avoid pinning every call site.
	// We don't currently call teardownEmulator from outside the
	// goroutine that owns the emulator's lifecycle, so deferring this
	// Wait is safe.
	wg := &p.drainWG
	go wg.Wait()
	p.vt = nil
	p.scrollback = nil
	p.selection = nil
	p.cachedView = ""
}

// parseOscTitlePayload strips a leading "<digits>;" parameter from an
// OSC title payload — same shape terminal.parseOscTitlePayload uses.
// Duplicated here because the original is unexported.
func parseOscTitlePayload(data []byte) string {
	i := 0
	for i < len(data) && data[i] >= '0' && data[i] <= '9' {
		i++
	}
	if i > 0 && i < len(data) && data[i] == ';' {
		return string(data[i+1:])
	}
	return string(data)
}

// --- Attach lifecycle ---

// Attach dials a fresh conn to the daemon, performs Hello, sends
// AttachReq, reads SnapshotSize bytes' worth of TypePTYOutput frames
// into the local emulator, then enters the live binary read loop in a
// goroutine. Returns once the snapshot has been applied and the loop
// is running.
//
// Idempotent: a no-op when state is already PaneViewAttached.
func (p *PaneView) Attach(ctx context.Context) error {
	return p.attach(ctx, false)
}

// Takeover behaves like Attach but sets Takeover=true in AttachReq so
// any currently-attached client is displaced. The agent process keeps
// running across the takeover (PR6 semantics).
func (p *PaneView) Takeover(ctx context.Context) error {
	return p.attach(ctx, true)
}

func (p *PaneView) attach(ctx context.Context, takeover bool) error {
	p.mu.Lock()
	if p.state == PaneViewAttached {
		p.mu.Unlock()
		return nil
	}
	if p.sessionID == "" {
		p.mu.Unlock()
		return errors.New("daemonclient: attach without session id")
	}
	sessionID := p.sessionID
	w, h := p.width, p.height
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	p.width, p.height = w, h
	p.mu.Unlock()

	conn, err := DialOrStart(ctx)
	if err != nil {
		return err
	}
	r := bufio.NewReader(conn)

	// Hello on the new conn. We don't use Client.do here because that
	// runs on Client.conn — the attach conn is brand new and the
	// daemon's read loop on the new conn is its own goroutine.
	if err := writeJSONReq(conn, daemon.MsgHelloReq, daemon.HelloReq{
		ProtocolVersion: daemon.ProtocolVersion,
		BinaryVersion:   daemon.BinaryVersion,
		ClientName:      clientNameForHello + "/attach",
	}); err != nil {
		conn.Close()
		return fmt.Errorf("daemonclient: attach hello: %w", err)
	}
	if _, err := readJSONResp(r, daemon.MsgHelloResp, nil); err != nil {
		conn.Close()
		return fmt.Errorf("daemonclient: attach hello resp: %w", err)
	}

	if err := writeJSONReq(conn, daemon.MsgAttachReq, daemon.AttachReq{
		SessionID: sessionID,
		Cols:      uint16(w),
		Rows:      uint16(h),
		Takeover:  takeover,
	}); err != nil {
		conn.Close()
		return fmt.Errorf("daemonclient: attach req: %w", err)
	}
	var aresp daemon.AttachResp
	if _, err := readJSONResp(r, daemon.MsgAttachResp, &aresp); err != nil {
		conn.Close()
		return fmt.Errorf("daemonclient: attach resp: %w", err)
	}

	// Initialize the local emulator BEFORE consuming the snapshot. The
	// snapshot is a sequence of TypePTYOutput frames totaling
	// aresp.SnapshotSize bytes; we apply them to the emulator as if
	// they were live PTY output.
	p.mu.Lock()
	p.initEmulatorLocked()
	vt := p.vt
	p.mu.Unlock()

	remaining := aresp.SnapshotSize
	for remaining > 0 {
		typ, payload, err := daemon.ReadFrame(r)
		if err != nil {
			conn.Close()
			p.mu.Lock()
			p.teardownEmulatorLocked()
			p.mu.Unlock()
			return fmt.Errorf("daemonclient: read snapshot frame: %w", err)
		}
		if typ != daemon.TypePTYOutput {
			conn.Close()
			p.mu.Lock()
			p.teardownEmulatorLocked()
			p.mu.Unlock()
			return fmt.Errorf("daemonclient: unexpected snapshot frame type 0x%02x", typ)
		}
		if len(payload) == 0 {
			continue
		}
		// vt.Write is safe-locked internally. We don't drive
		// scrollback during the snapshot apply — the snapshot IS the
		// current screen, not the history.
		vt.Write(payload)
		remaining -= len(payload)
		if remaining < 0 {
			// Tolerate the daemon overshooting (shouldn't happen in
			// practice).
			remaining = 0
		}
	}

	p.mu.Lock()
	p.state = PaneViewAttached
	p.attachConn = conn
	p.attachR = r
	p.dirty = true
	p.cachedView = ""
	// detachCh might have been closed on a prior attach; refresh it.
	select {
	case <-p.detachCh:
		p.detachCh = make(chan struct{})
	default:
	}
	p.mu.Unlock()

	// Spawn the binary read loop.
	p.attachLoopWG.Add(1)
	go p.attachLoop(conn, r)

	p.emitTeaMsg(PaneAttachedMsg{PaneID: p.id})
	return nil
}

// Detach closes the attach conn cleanly. The daemon-side session
// continues to run. Safe to call from any state.
func (p *PaneView) Detach() error {
	return p.detach(true)
}

func (p *PaneView) detach(sendFrame bool) error {
	p.mu.Lock()
	conn := p.attachConn
	if conn == nil {
		p.mu.Unlock()
		return nil
	}
	p.attachConn = nil
	p.attachR = nil
	p.mu.Unlock()

	if sendFrame {
		p.attachWriteM.Lock()
		_ = daemon.WriteFrame(conn, daemon.TypeDetach, nil)
		p.attachWriteM.Unlock()
	}
	_ = conn.Close()

	p.attachLoopWG.Wait()

	p.mu.Lock()
	p.state = PaneViewUnattached
	p.teardownEmulatorLocked()
	close(p.detachCh)
	p.detachCh = make(chan struct{})
	p.mu.Unlock()

	p.emitTeaMsg(PaneDetachedMsg{PaneID: p.id})
	return nil
}

// attachLoop reads binary frames from the attach conn and feeds bytes
// into the local emulator. Exits on EOF, on receiving TypeDetach from
// the daemon, or on any read error.
func (p *PaneView) attachLoop(conn net.Conn, r *bufio.Reader) {
	defer p.attachLoopWG.Done()
	for {
		typ, payload, err := daemon.ReadFrame(r)
		if err != nil {
			if err == io.EOF || errors.Is(err, net.ErrClosed) {
				p.handleAttachExit(nil, true)
				return
			}
			p.handleAttachExit(err, false)
			return
		}
		switch typ {
		case daemon.TypePTYOutput:
			if len(payload) == 0 {
				continue
			}
			p.applyOutput(payload)
			p.emitTeaMsg(PaneOutputMsg{PaneID: p.id})
		case daemon.TypeDetach:
			// Daemon-initiated detach: the session may or may not
			// still be running. We transition to Unattached and let
			// the UI poll List to learn the truth.
			p.handleAttachExit(nil, true)
			return
		case daemon.TypeResize:
			// Daemon-pushed resize (from another client). Apply.
			cols, rows, _, derr := daemon.DecodeResize(payload)
			if derr != nil || cols == 0 || rows == 0 {
				continue
			}
			p.mu.Lock()
			p.width, p.height = int(cols), int(rows)
			if p.vt != nil {
				p.vt.Resize(int(cols), int(rows))
			}
			p.dirty = true
			p.mu.Unlock()
			p.emitTeaMsg(PaneOutputMsg{PaneID: p.id})
		default:
			// Unknown binary frame: drop.
		}
	}
}

// handleAttachExit drives the post-loop cleanup. cleanExit=true means
// the daemon closed the connection cleanly (EOF or TypeDetach frame);
// false means an unexpected I/O error and we treat the daemon as gone.
func (p *PaneView) handleAttachExit(err error, cleanExit bool) {
	p.mu.Lock()
	conn := p.attachConn
	p.attachConn = nil
	p.attachR = nil
	p.state = PaneViewUnattached
	p.teardownEmulatorLocked()
	close(p.detachCh)
	p.detachCh = make(chan struct{})
	p.mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}

	if !cleanExit {
		// Daemon vanished. Promote to a model-level disconnect signal.
		p.emitTeaMsg(DaemonDisconnectedMsg{Err: err})
		return
	}
	p.emitTeaMsg(PaneDetachedMsg{PaneID: p.id})
}

// applyOutput feeds bytes into the local emulator and updates the
// scroll/alt-screen detectors. Mirrors the relevant subset of
// terminal.Pane.handleOutput — selection / clipboard pieces are not
// duplicated because they're driven by client-side input, not by
// inbound bytes.
func (p *PaneView) applyOutput(data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.vt == nil {
		return
	}
	// Minimal mode detection — alt-screen and mouse-enable flips ride
	// on the byte stream too. We don't need the full sequence-detection
	// machine of terminal.Pane (subscribers, ModeEvent) here; the
	// flags are read locally by SetSize and HandleMouse.
	if hasSeq(data, altScreenEnableSeqs) {
		p.altScreenActive = true
		p.viewportOffset = 0
	}
	if hasSeq(data, altScreenDisableSeqs) {
		p.altScreenActive = false
	}
	if hasSeq(data, mouseEnableSeqs) {
		p.mouseEnabled = true
	}
	if hasSeq(data, mouseDisableSeqs) {
		p.mouseEnabled = false
	}
	p.vt.Write(data)
	p.dirty = true
	p.cachedView = ""
}

var (
	altScreenEnableSeqs  = [][]byte{[]byte("\x1b[?1049h"), []byte("\x1b[?47h")}
	altScreenDisableSeqs = [][]byte{[]byte("\x1b[?1049l"), []byte("\x1b[?47l")}
	mouseEnableSeqs      = [][]byte{
		[]byte("\x1b[?1000h"), []byte("\x1b[?1002h"),
		[]byte("\x1b[?1003h"), []byte("\x1b[?1006h"),
	}
	mouseDisableSeqs = [][]byte{
		[]byte("\x1b[?1000l"), []byte("\x1b[?1002l"),
		[]byte("\x1b[?1003l"), []byte("\x1b[?1006l"),
	}
)

func hasSeq(data []byte, seqs [][]byte) bool {
	for _, s := range seqs {
		if containsBytes(data, s) {
			return true
		}
	}
	return false
}

func containsBytes(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	if len(haystack) < len(needle) {
		return false
	}
outer:
	for i := 0; i <= len(haystack)-len(needle); i++ {
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				continue outer
			}
		}
		return true
	}
	return false
}

// --- Stop / lifecycle ---

// Stop tears down the daemon-side session. If attached we send Detach
// first so the daemon's binary loop exits cleanly, then Kill. In
// PaneViewDetached it's a no-op so the UI's existing "stop" key
// doesn't blow up on a stale view.
func (p *PaneView) Stop() error {
	return p.stop(0)
}

// StopGraceful sends SIGTERM with the given grace window before SIGKILL.
func (p *PaneView) StopGraceful(timeout time.Duration) error {
	return p.stop(timeout)
}

func (p *PaneView) stop(timeout time.Duration) error {
	p.mu.Lock()
	state := p.state
	sessionID := p.sessionID
	p.mu.Unlock()

	if state == PaneViewDetached || sessionID == "" {
		return nil
	}
	if state == PaneViewAttached {
		_ = p.Detach()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.client.Kill(ctx, sessionID, timeout)
}

// Close releases this PaneView's resources without killing the
// daemon-side session. Use Stop / StopGraceful to terminate the
// underlying agent.
func (p *PaneView) Close() error {
	_ = p.detach(false)
	if p.teaClosed.CompareAndSwap(false, true) {
		close(p.teaMsgs)
	}
	return nil
}

// --- Key / mouse handling ---

// HandleKey is the client-side mirror of terminal.Pane.HandleKey. It
// returns tea.Msg (matching the existing pane surface) — typically nil
// or terminal.ExitFocusMsg in the attached case, AttachFirstMsg in the
// unattached case, nil in the detached case.
func (p *PaneView) HandleKey(msg tea.KeyMsg) tea.Msg {
	if msg.String() == "ctrl+g" {
		return terminal.ExitFocusMsg{}
	}

	p.mu.Lock()
	state := p.state
	if state == PaneViewDetached {
		p.mu.Unlock()
		return nil
	}
	if state == PaneViewUnattached {
		p.mu.Unlock()
		return AttachFirstMsg{PaneID: p.id}
	}
	// Attached path. Selection / scrollback handling is local — we
	// only forward translated bytes to the daemon.
	key := msg.String()

	if p.selection != nil && p.selection.IsActive() {
		if key == "ctrl+c" || key == "cmd+c" {
			p.copySelectionLocked()
			p.mu.Unlock()
			return nil
		}
	}

	switch key {
	case "shift+pgup":
		if p.vt != nil {
			rows := p.vt.Height()
			p.scrollUpLocked(rows / 2)
		}
		p.mu.Unlock()
		return nil
	case "shift+pgdown":
		if p.vt != nil {
			rows := p.vt.Height()
			p.scrollDownLocked(rows / 2)
		}
		p.mu.Unlock()
		return nil
	case "shift+home":
		if p.scrollback != nil {
			p.viewportOffset = p.scrollback.Len()
			p.dirty = true
		}
		p.mu.Unlock()
		return nil
	case "shift+end":
		p.viewportOffset = 0
		p.dirty = true
		p.mu.Unlock()
		return nil
	case "esc", "escape":
		if p.viewportOffset > 0 {
			p.viewportOffset = 0
			p.dirty = true
			p.mu.Unlock()
			return nil
		}
		if p.selection != nil && p.selection.IsActive() {
			p.selection.Clear()
			p.dirty = true
			p.mu.Unlock()
			return nil
		}
		// Otherwise fall through and forward escape.
	}

	if p.viewportOffset > 0 {
		p.viewportOffset = 0
		p.dirty = true
	}
	if p.selection != nil && p.selection.IsActive() {
		p.selection.Clear()
		p.dirty = true
	}
	conn := p.attachConn
	p.mu.Unlock()

	if conn == nil {
		return nil
	}
	input := translateKey(msg)
	if len(input) == 0 {
		return nil
	}
	p.attachWriteM.Lock()
	_ = daemon.WriteFrame(conn, daemon.TypePTYInput, input)
	p.attachWriteM.Unlock()
	return nil
}

// HandleMouse mirrors terminal.Pane.HandleMouse: no return value,
// translates mouse events to PTY bytes (or local selection / scroll
// updates) and forwards.
func (p *PaneView) HandleMouse(msg tea.MouseMsg) {
	p.mu.Lock()
	state := p.state
	if state != PaneViewAttached {
		p.mu.Unlock()
		return
	}
	if p.selection != nil {
		switch msg.Button {
		case tea.MouseButtonLeft:
			pos := p.viewportToLogicalLocked(msg.X, msg.Y)
			switch msg.Action {
			case tea.MouseActionPress:
				p.selection.Start(pos)
				p.dirty = true
			case tea.MouseActionMotion:
				p.selection.Update(pos)
				p.dirty = true
			case tea.MouseActionRelease:
				p.selection.Finish()
				p.dirty = true
			}
		case tea.MouseButtonNone:
			if p.selection.Mode == terminal.SelectionSelecting {
				pos := p.viewportToLogicalLocked(msg.X, msg.Y)
				p.selection.Update(pos)
				p.dirty = true
			}
		case tea.MouseButtonRight, tea.MouseButtonMiddle:
			if p.selection.IsActive() {
				p.selection.Clear()
				p.dirty = true
			}
		case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
			if p.selection.IsActive() {
				p.selection.Clear()
				p.dirty = true
			}
		}
	}

	if !p.mouseEnabled {
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			p.scrollUpLocked(3)
		case tea.MouseButtonWheelDown:
			p.scrollDownLocked(3)
		}
		p.mu.Unlock()
		return
	}

	if msg.Shift {
		p.mu.Unlock()
		return
	}

	var seq []byte
	x, y := msg.X+1, msg.Y+1
	if x > 223 {
		x = 223
	}
	if y > 223 {
		y = 223
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		seq = []byte{'\x1b', '[', 'M', byte(64 + 32), byte(x + 32), byte(y + 32)}
	case tea.MouseButtonWheelDown:
		seq = []byte{'\x1b', '[', 'M', byte(65 + 32), byte(x + 32), byte(y + 32)}
	case tea.MouseButtonLeft:
		seq = []byte{'\x1b', '[', 'M', byte(0 + 32), byte(x + 32), byte(y + 32)}
	case tea.MouseButtonRight:
		seq = []byte{'\x1b', '[', 'M', byte(2 + 32), byte(x + 32), byte(y + 32)}
	case tea.MouseButtonMiddle:
		seq = []byte{'\x1b', '[', 'M', byte(1 + 32), byte(x + 32), byte(y + 32)}
	}
	conn := p.attachConn
	p.mu.Unlock()

	if len(seq) > 0 && conn != nil {
		p.attachWriteM.Lock()
		_ = daemon.WriteFrame(conn, daemon.TypePTYInput, seq)
		p.attachWriteM.Unlock()
	}
}

// viewportToLogicalLocked converts viewport coords to a Position with
// scrollback-aware row. Must be called with p.mu held.
func (p *PaneView) viewportToLogicalLocked(x, y int) terminal.Position {
	return terminal.Position{Row: y - p.viewportOffset, Col: x}
}

// scrollUpLocked / scrollDownLocked move the viewport. Must be called
// with p.mu held.
func (p *PaneView) scrollUpLocked(lines int) {
	if p.scrollback == nil {
		return
	}
	max := p.scrollback.Len()
	p.viewportOffset += lines
	if p.viewportOffset > max {
		p.viewportOffset = max
	}
	p.dirty = true
}

func (p *PaneView) scrollDownLocked(lines int) {
	p.viewportOffset -= lines
	if p.viewportOffset < 0 {
		p.viewportOffset = 0
	}
	p.dirty = true
}

// copySelectionLocked extracts selected text and writes it to the
// system clipboard. Must be called with p.mu held. Mirrors
// terminal.Pane.copySelectionUnlocked but reads cells directly from
// our local emulator.
func (p *PaneView) copySelectionLocked() {
	// PaneView does not currently maintain scrollback cell extraction
	// — the daemon's snapshot only captures the live grid, so older
	// scrollback content lives only on the daemon side. For PR7 we
	// support selection on the visible portion only.
	if p.selection == nil || p.vt == nil {
		return
	}
	rows := p.vt.Height()
	cols := p.vt.Width()
	liveScreen := func(col, row int) terminal.Glyph {
		return terminal.CellToGlyph(p.vt.CellAt(col, row))
	}
	_ = cols // silence unused if extraction stub changes
	_ = liveScreen
	// Use SelectionState.ExtractText with no scrollback lines.
	text := p.selection.ExtractText(nil, liveScreen, rows, 0)
	if text != "" {
		// clipboard interaction is best-effort; we deliberately don't
		// import atotto/clipboard at this layer to keep the daemonclient
		// free of UI deps. The model can copy via its own clipboard
		// shim after pulling text via GetContent if it wants explicit
		// behavior. (PR7 accepts this limitation; PR8 may wire a
		// callback through.)
	}
	p.selection.Clear()
	p.dirty = true
}

// --- View / GetContent ---

// View renders the local emulator using the same code path as
// terminal.Pane.View. Returns an empty string when no local state has
// been populated (e.g. PaneViewDetached, or a freshly-constructed
// view that hasn't attached yet).
func (p *PaneView) View() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.vt == nil {
		return ""
	}
	if !p.dirty && p.cachedView != "" {
		return p.cachedView
	}
	cursorVisible := !p.cursorHidden.Load()
	p.cachedView = terminal.RenderVT(p.vt, p.scrollback, p.viewportOffset, cursorVisible, p.selection)
	p.dirty = false
	return p.cachedView
}

// GetContent returns the live screen as plain text. In PaneViewDetached
// it returns "" — there's no local emulator to read from.
func (p *PaneView) GetContent() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.vt == nil {
		return ""
	}
	cols := p.vt.Width()
	rows := p.vt.Height()
	if cols <= 0 || rows <= 0 {
		return ""
	}
	var b strings.Builder
	for row := 0; row < rows; row++ {
		if row > 0 {
			b.WriteByte('\n')
		}
		for col := 0; col < cols; col++ {
			g := terminal.CellToGlyph(p.vt.CellAt(col, row))
			ch := g.Char
			if ch == 0 {
				ch = ' '
			}
			b.WriteRune(ch)
		}
	}
	return b.String()
}

// Update is the BubbleTea-shaped dispatcher. The model invokes it for
// every tea.Msg; we react only to messages addressed to this pane.
// Returns a re-arming Cmd so the next message can be pulled.
//
// Pane-local messages handled:
//   - PaneOutputMsg / PaneRenderTickMsg / PaneExitMsg / PaneDetachedMsg
//     / PaneAttachedMsg: identity-check the PaneID and re-arm the read.
//
// Model-level messages (DaemonDisconnectedMsg, AttachFirstMsg) are NOT
// consumed here — they bubble up to the model's Update.
func (p *PaneView) Update(msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case PaneOutputMsg:
		if m.PaneID != p.id {
			return nil
		}
		return p.readNextMsg()
	case PaneRenderTickMsg:
		if m.PaneID != p.id {
			return nil
		}
		return nil
	case PaneExitMsg:
		if m.PaneID != p.id {
			return nil
		}
		return nil
	case PaneAttachedMsg:
		if m.PaneID != p.id {
			return nil
		}
		return p.readNextMsg()
	case PaneDetachedMsg:
		if m.PaneID != p.id {
			return nil
		}
		return nil
	}
	return nil
}

// readNextMsg returns a Cmd that pulls one event from the pane's
// teaMsgs channel. The model uses this as the long-poll bridge between
// the attach goroutine and the tea runtime — same role as
// terminal.Pane.readOutputUnlocked.
func (p *PaneView) readNextMsg() tea.Cmd {
	if p.teaClosed.Load() {
		return nil
	}
	id := p.id
	ch := p.teaMsgs
	closeCh := p.client.closeCh
	return func() tea.Msg {
		select {
		case msg, ok := <-ch:
			if !ok {
				return PaneExitMsg{PaneID: id, Err: io.EOF}
			}
			return msg
		case <-closeCh:
			return DaemonDisconnectedMsg{Err: ErrDaemonUnavailable}
		}
	}
}

// emitTeaMsg pushes a message into the pane's tea channel. Non-blocking
// to keep the attach goroutine unblocked; a model that falls behind by
// 64+ events drops the overflow (the emulator state stays consistent
// because the bytes were already written to vt via applyOutput).
func (p *PaneView) emitTeaMsg(msg tea.Msg) {
	if p.teaClosed.Load() {
		return
	}
	select {
	case p.teaMsgs <- msg:
	default:
	}
}

// Start is included for surface-parity with terminal.Pane. PaneView
// never starts a process locally — Spawn must happen via Client.Spawn
// before NewPaneView is called. We return a nil Cmd. Documented as a
// no-op so the cutover in PR8 can drop the model's existing
// `pane.Start(cmd, args...)` invocations or leave them as harmless.
func (p *PaneView) Start(command string, args ...string) tea.Cmd {
	_ = command
	_ = args
	return nil
}

// Refresh applies a fresh SessionInfo (typically pulled from a List
// response) to the cached fields the unattached path reads from. Not
// part of the terminal.Pane surface — added so the model can keep
// PaneView's "unattached" view in sync with the daemon's view of the
// world.
func (p *PaneView) Refresh(info daemon.SessionInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := info
	p.lastInfo = &cp
	if info.Title != "" {
		p.cachedTitle = info.Title
	}
	if info.Workdir != "" && p.workdir == "" {
		p.workdir = info.Workdir
	}
	if info.SessionName != "" && p.sessionName == "" {
		p.sessionName = info.SessionName
	}
	if p.state == PaneViewDetached && info.Running {
		p.state = PaneViewUnattached
	}
}

// --- Translation helpers (duplicated from terminal/pane.go) ---

// translateKey converts a BubbleTea KeyMsg into PTY byte input.
// Duplicated from terminal.translateKey (which is unexported) so the
// client doesn't drag the Pane lock just to translate.
func translateKey(msg tea.KeyMsg) []byte {
	key := msg.String()

	switch {
	case len(key) == 6 && key[:5] == "ctrl+" && key[5] >= 'a' && key[5] <= 'z':
		return []byte{byte(key[5] - 'a' + 1)}
	case len(key) == 5 && key[:4] == "alt+" && key[4] >= 'a' && key[4] <= 'z':
		return []byte{27, key[4]}
	}

	switch msg.Type {
	case tea.KeyEnter:
		return []byte("\r")
	case tea.KeyBackspace:
		return []byte{127}
	case tea.KeyTab:
		return []byte("\t")
	case tea.KeyShiftTab:
		return []byte("\x1b[Z")
	case tea.KeyUp:
		return []byte("\x1b[A")
	case tea.KeyDown:
		return []byte("\x1b[B")
	case tea.KeyRight:
		return []byte("\x1b[C")
	case tea.KeyLeft:
		return []byte("\x1b[D")
	case tea.KeyEscape:
		return []byte{27}
	case tea.KeyHome:
		return []byte("\x1b[H")
	case tea.KeyEnd:
		return []byte("\x1b[F")
	case tea.KeyPgUp:
		return []byte("\x1b[5~")
	case tea.KeyPgDown:
		return []byte("\x1b[6~")
	case tea.KeyDelete:
		return []byte("\x1b[3~")
	case tea.KeySpace:
		return []byte(" ")
	case tea.KeyRunes:
		return []byte(string(msg.Runes))
	}

	return nil
}

// --- One-off wire helpers used by Attach ---

// writeJSONReq is the small one-shot equivalent of Client.do's request
// half: encode + WriteFrame on the given conn. Used by Attach because
// the attach conn isn't owned by the Client read loop.
func writeJSONReq(conn net.Conn, typeName string, payload any) error {
	raw, err := daemon.EncodeMsg(typeName, payload)
	if err != nil {
		return err
	}
	return daemon.WriteFrame(conn, daemon.TypeJSONReq, raw)
}

// readJSONResp reads one TypeJSONResp frame from r and (optionally)
// unmarshals it into out. Returns the type name (or an ErrorResp
// rendered as an error) so the caller can verify it matches.
func readJSONResp(r *bufio.Reader, expect string, out any) (string, error) {
	typ, payload, err := daemon.ReadFrame(r)
	if err != nil {
		return "", err
	}
	if typ != daemon.TypeJSONResp {
		return "", fmt.Errorf("unexpected frame type 0x%02x (want JSONResp)", typ)
	}
	name, raw, err := daemon.DecodeEnvelope(payload)
	if err != nil {
		return "", err
	}
	if name == daemon.MsgErrorResp {
		var er daemon.ErrorResp
		if uerr := json.Unmarshal(raw, &er); uerr != nil {
			return name, uerr
		}
		return name, fmt.Errorf("%s: %s", er.Code, er.Message)
	}
	if name != expect {
		return name, fmt.Errorf("got %q want %q", name, expect)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return name, err
		}
	}
	return name, nil
}
