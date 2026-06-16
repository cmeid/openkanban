package terminal

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"
	xvt "github.com/charmbracelet/x/vt"
	"github.com/creack/pty"

	"github.com/techdufus/openkanban/internal/notify"
)

const (
	renderInterval = 50 * time.Millisecond
	readBufferSize = 65536

	// subscriberChanCapacity is the buffer depth of channels returned by
	// Pane.Subscribe. Sized for short bursts of OutputEvents at typical
	// agent throughput; if a subscriber falls further behind, the read
	// loop drops events for that subscriber rather than block the PTY
	// pipeline. See Subscribe / startReadLoop.
	subscriberChanCapacity = 256
)

// --- Event subscription API ---
//
// Subscribe returns a channel that receives every Event observed by the
// pane: OutputEvent for each PTY read, ExitEvent on PTY close, and
// TitleEvent / ModeEvent when the child mutates window title or
// mouse / alt-screen / cursor-visibility state.
//
// Lives ALONGSIDE the existing tea.Cmd path (OutputMsg / ExitMsg) so
// non-BubbleTea consumers — namely the daemon process in PR4 — can
// observe pane traffic without dragging tea.Program along.

// Event is the sum type emitted to Subscribers. The unexported
// paneEvent() marker forces variants to live in this package.
type Event interface {
	paneEvent()
}

// OutputEvent carries a chunk of bytes read from the PTY. The byte
// slice is unique to this event; subscribers may retain it.
type OutputEvent struct {
	Data []byte
}

// ExitEvent is published once when the PTY read returns an error
// (typically io.EOF after the child process exits).
type ExitEvent struct {
	Err error
}

// TitleEvent fires when the child sets the OSC 0/2 window title.
type TitleEvent struct {
	Title string
}

// ModeEvent reports the current value of the three booleans whenever
// any of them transitions. Carries the full snapshot so a fresh
// subscriber doesn't need to query the pane to learn the state.
type ModeEvent struct {
	Mouse        bool
	AltScreen    bool
	CursorHidden bool
}

func (OutputEvent) paneEvent() {}
func (ExitEvent) paneEvent()   {}
func (TitleEvent) paneEvent()  {}
func (ModeEvent) paneEvent()   {}

type Pane struct {
	id          string
	vt          *xvt.SafeEmulator
	pty         *os.File
	cmd         *exec.Cmd
	mu          sync.Mutex
	running     bool
	exitErr     error
	workdir     string
	sessionName string
	ticketID    string
	width       int
	height      int

	// expectedCompletedExit is true when the TUI initiated a
	// StopGraceful as a reaction to AgentCompleted on a Done ticket
	// (the `openkanban ticket done` flow). The ExitMsg handler reads
	// this to know it should preserve AgentCompleted on the ticket
	// instead of resetting AgentStatus to AgentNone like a normal
	// pane exit.
	expectedCompletedExit bool

	cachedView      string
	lastRender      time.Time
	dirty           bool
	renderScheduled bool

	mouseEnabled bool // tracks if child process has enabled mouse tracking

	// Scrollback and viewport state (Issue #95)
	scrollback      *ScrollbackBuffer
	altScreenActive bool            // tracks if child process is in alternate screen mode
	viewportOffset  int             // lines scrolled back (0 = live view)
	lastTopRow      []Glyph         // snapshot of row 0 before write for scroll detection
	scrollbackSize  int             // configured scrollback buffer size
	selection       *SelectionState // mouse text selection state

	// cursorHidden tracks DECTCEM state for the live emulator. charm/x/vt
	// does not expose a getter on Emulator, so we maintain our own flag
	// via the Callbacks.CursorVisibility hook. Atomic so the goroutine
	// that drives the callback can safely write while renderers read.
	cursorHidden atomic.Bool

	// forwardNotifications gates the OSC 9 handler. When true, an OSC 9
	// sequence emitted by the agent is forwarded to notify.Send (which
	// fires a desktop notification on darwin and is a no-op elsewhere).
	// Atomic because the handler runs synchronously inside vt.Write
	// under p.mu — taking p.mu in the handler would deadlock. The
	// daemon flips this via SetForwardNotifications based on the spawn
	// request's ForwardNotifications field.
	forwardNotifications atomic.Bool

	// lastActivityNs is the unix-nanosecond timestamp of the last
	// observed PTY output for this pane — stamped from handleOutput on
	// every non-empty vt.Write. Used by the daemon's activity broadcaster
	// to push "activity" SessionEvents to subscribers, which the UI uses
	// to distinguish "stuck at waiting" (no bytes flowing) from "actively
	// working" (Claude's spinner / tool output streaming). atomic.Int64
	// so daemon goroutines can read without taking p.mu.
	//
	// Why bytes-flowed rather than grid-changed: cursor blinks are
	// terminal-side (not PTY output), and Claude's "Cogitating…" /
	// "Combobulating…" spinner emits bytes throughout tool execution.
	// Hashing the grid added cost on every handleOutput without
	// catching anything bytes-flowed misses in practice.
	lastActivityNs atomic.Int64

	// drainStop stops the goroutine that pipes emulator-emitted responses
	// (DA queries, etc.) back to the PTY. Without that drain charm/x/vt
	// deadlocks on its first device-attributes write.
	drainStop chan struct{}
	drainWG   sync.WaitGroup

	// paneTitle holds the most recent title the child set via OSC 0/2.
	// Updated from the emulator callback (sync, during Write); read by
	// the model when computing the host-window title. atomic.Value lets
	// the read be lock-free.
	paneTitle atomic.Value // string

	// Event subscription state (see Subscribe / startReadLoop). The
	// subscribers slice is mutated only under subscribersMu; the read
	// loop holds subscribersMu briefly during fan-out, so subscribersMu
	// MUST NOT be acquired while p.mu is held (the read loop drops p.mu
	// before fan-out to keep the lock order one-way).
	subscribers     []chan Event
	subscribersMu   sync.Mutex
	readLoopOnce    sync.Once
	readLoopStop    chan struct{}
	readLoopWG      sync.WaitGroup
	stopReadLoopMu  sync.Mutex // serializes one-shot close of readLoopStop
	readLoopStopped bool

	// teaBridgeCh is the dedicated subscriber feeding readOutputUnlocked's
	// tea.Cmd. We allocate it once per Pane lifetime (during the first
	// readOutputUnlocked call) so the tea path doesn't race with consumer
	// Subscribe()s and so re-arming the Cmd doesn't churn subscriptions.
	teaBridgeCh   <-chan Event
	teaBridgeOnce sync.Once
}

func New(id string, width, height int, scrollbackSize int) *Pane {
	if scrollbackSize <= 0 {
		scrollbackSize = 10000
	}
	return &Pane{
		id:             id,
		width:          width,
		height:         height,
		scrollbackSize: scrollbackSize,
	}
}

// ID returns the pane's identifier
func (p *Pane) ID() string {
	return p.id
}

// SetWorkdir sets the working directory for commands
func (p *Pane) SetWorkdir(dir string) {
	p.workdir = dir
}

func (p *Pane) GetWorkdir() string {
	return p.workdir
}

// SetSessionName sets the session name for OPENKANBAN_SESSION env var
func (p *Pane) SetSessionName(name string) {
	p.sessionName = name
}

// SetTicketID sets the openkanban ticket id for OPENKANBAN_TICKET_ID env var.
// The child reads this to resolve back to the ticket .md when running
// `openkanban ticket done` from inside the session.
func (p *Pane) SetTicketID(id string) {
	p.ticketID = id
}

// MarkExpectedCompletedExit flags this pane as being stopped because
// its ticket was marked done by the agent. The ExitMsg handler reads
// this to preserve AgentCompleted instead of resetting to AgentNone.
func (p *Pane) MarkExpectedCompletedExit() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expectedCompletedExit = true
}

// ExpectedCompletedExit reports whether StopGraceful was initiated via
// the ticket-done path.
func (p *Pane) ExpectedCompletedExit() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.expectedCompletedExit
}

// Running returns whether the pane has a running process
func (p *Pane) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// ExitErr returns any error from the process exit
func (p *Pane) ExitErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitErr
}

func (p *Pane) SetSize(width, height int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// No-op when dimensions haven't changed. Otherwise every entry
	// into agent view fires an unnecessary SIGWINCH at the child
	// process; over a few cycles of leave/re-enter or AskUserQuestion
	// open/close, ink's layout cache gets re-invalidated repeatedly
	// and can land in a state where bottom-anchored UI renders at the
	// top. Skip when there's nothing actually to resize.
	if p.width == width && p.height == height {
		return
	}

	p.width = width
	p.height = height
	p.dirty = true
	p.cachedView = ""

	// Clear selection on resize (coordinates become invalid)
	if p.selection != nil && p.selection.IsActive() {
		p.selection.Clear()
	}

	// Reset viewport to live view on resize
	p.viewportOffset = 0

	if p.vt != nil {
		p.vt.Resize(width, height)
	}

	if p.pty != nil && p.running {
		pty.Setsize(p.pty, &pty.Winsize{
			Rows: uint16(height),
			Cols: uint16(width),
		})
	}
}

// Size returns the current dimensions
func (p *Pane) Size() (width, height int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.width, p.height
}

// ScrollbackLen returns the number of lines in the scrollback buffer.
func (p *Pane) ScrollbackLen() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.scrollback == nil {
		return 0
	}
	return p.scrollback.Len()
}

// SnapshotScrollback returns a copy of the scrollback ring's contents,
// oldest line first. Returns nil if scrollback is nil or empty. Used by
// the daemon to ship scrollback history to attaching clients.
func (p *Pane) SnapshotScrollback() [][]Glyph {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.scrollback == nil {
		return nil
	}
	n := p.scrollback.Len()
	if n == 0 {
		return nil
	}
	return p.scrollback.GetRange(0, n)
}

// ViewportOffset returns how many lines the viewport is scrolled back.
func (p *Pane) ViewportOffset() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.viewportOffset
}

// IsAltScreenActive returns whether the terminal is in alternate screen mode.
func (p *Pane) IsAltScreenActive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.altScreenActive
}

// SnapshotState returns the emulator and the three modal booleans the
// daemon's redraw serializer needs to reproduce the pane's current
// view. The returned emulator pointer is the live one — callers MUST
// only read from it via SafeEmulator's locked methods (CellAt,
// CursorPosition, Width, Height); they must not mutate it.
//
// The cursor-visibility / mouse / alt-screen booleans are tracked on
// Pane (not on the emulator), so the daemon couldn't reconstruct them
// from vt alone. This getter is the single seam through which the
// daemon's snapshot path reads them; PR7 will fold it into a richer
// Pane.View interface.
func (p *Pane) SnapshotState() (vt *xvt.SafeEmulator, cursorVisible, mouseEnabled, altScreen bool, title string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	vt = p.vt
	mouseEnabled = p.mouseEnabled
	altScreen = p.altScreenActive
	cursorVisible = !p.cursorHidden.Load()
	title = ""
	if v, ok := p.paneTitle.Load().(string); ok {
		title = v
	}
	return vt, cursorVisible, mouseEnabled, altScreen, title
}

// --- Bubbletea Messages ---

// OutputMsg carries data read from the PTY
type OutputMsg struct {
	PaneID string
	Data   []byte
}

// ExitMsg indicates the process has exited
type ExitMsg struct {
	PaneID string
	Err    error
}

// RenderTickMsg triggers a throttled render
type RenderTickMsg struct {
	PaneID string
}

// ExitFocusMsg signals to return to board view
type ExitFocusMsg struct{}

// --- PTY Lifecycle (Issue #13) ---

// Start launches a command in a PTY and returns a Cmd to begin reading
func (p *Pane) Start(command string, args ...string) tea.Cmd {
	return func() tea.Msg {
		p.mu.Lock()
		// No `defer Unlock` here — the final `readOutputUnlocked()()`
		// invocation blocks waiting for the first event from the
		// subscription channel, which is filled by a goroutine that
		// must acquire p.mu (handleOutput → p.mu). We MUST release
		// p.mu before invoking the bridge Cmd.

		// Build command
		p.cmd = exec.Command(command, args...)
		p.cmd.Env = buildCleanEnv(p.sessionName, p.ticketID)

		// Set working directory if specified
		if p.workdir != "" {
			p.cmd.Dir = p.workdir
		}

		// Fork the child with the correct PTY size from the start.
		// pty.Start + pty.Setsize would race: the child renders its
		// first frame at the OS-default 80x24 before SIGWINCH arrives,
		// causing bottom-anchored UI (input bars, status lines) to
		// pin to row 24 of the child's coordinate space — which lands
		// at the wrong row in our actual-sized emulator buffer.
		// StartWithSize sets TIOCSWINSZ atomically with the fork.
		ptmx, err := pty.StartWithSize(p.cmd, &pty.Winsize{
			Rows: uint16(p.height),
			Cols: uint16(p.width),
		})
		if err != nil {
			p.exitErr = err
			p.mu.Unlock()
			return ExitMsg{PaneID: p.id, Err: err}
		}
		p.pty = ptmx
		p.running = true
		p.exitErr = nil

		// Create the virtual terminal. charm/x/vt emits responses
		// (device attributes, cursor reports, etc.) via Read(); we
		// MUST drain that pipe and forward to the PTY or the
		// emulator deadlocks on the first DA query.
		p.vt = xvt.NewSafeEmulator(p.width, p.height)
		p.vt.SetCallbacks(xvt.Callbacks{
			CursorVisibility: func(visible bool) {
				newHidden := !visible
				prev := p.cursorHidden.Swap(newHidden)
				if prev != newHidden {
					// We're inside p.vt.Write, which runs under
					// p.mu inside handleOutput. The mouse/alt-screen
					// flags are stable while p.mu is held, so it's
					// safe to read them directly here. publish takes
					// only subscribersMu (lock order: p.mu →
					// subscribersMu).
					p.publish(ModeEvent{
						Mouse:        p.mouseEnabled,
						AltScreen:    p.altScreenActive,
						CursorHidden: newHidden,
					})
				}
			},
		})
		p.cursorHidden.Store(false)
		p.registerTitleHandlersUnlocked()
		p.startDrainUnlocked()

		p.scrollback = NewScrollbackBuffer(p.scrollbackSize)
		p.selection = NewSelectionState()

		// Build the read Cmd while we still hold p.mu (so the nil-
		// check on p.pty sees a stable value), then release the lock
		// BEFORE invoking it. The Cmd lazily Subscribes — which
		// kicks off the read goroutine — and then blocks on the
		// channel. The read goroutine grabs p.mu for handleOutput,
		// so we cannot still be holding it here.
		readCmd := p.readOutputUnlocked()
		p.mu.Unlock()
		return readCmd()
	}
}

// StartHeadless launches a command in a PTY exactly like Start, but
// returns synchronously and does not use BubbleTea's tea.Cmd. The read
// loop is spawned via the same subscription machinery as Start; the
// caller is expected to Subscribe() if they want to observe output.
//
// The optional env slice fully replaces the per-process env (after the
// pane's buildCleanEnv pass). Pass nil to use the inherited environment
// with the OPENKANBAN_SESSION addition.
//
// Used by the openkanbankd daemon, which owns Panes without a tea
// runtime to drive the Cmd indirection. Mirrors Start's body byte-for-
// byte aside from the tea.Cmd plumbing at the end.
func (p *Pane) StartHeadless(command string, args []string, extraEnv []string) error {
	p.mu.Lock()

	p.cmd = exec.Command(command, args...)
	p.cmd.Env = buildCleanEnv(p.sessionName, p.ticketID)
	if len(extraEnv) > 0 {
		p.cmd.Env = append(p.cmd.Env, extraEnv...)
	}

	if p.workdir != "" {
		p.cmd.Dir = p.workdir
	}

	ptmx, err := pty.StartWithSize(p.cmd, &pty.Winsize{
		Rows: uint16(p.height),
		Cols: uint16(p.width),
	})
	if err != nil {
		p.exitErr = err
		p.mu.Unlock()
		return err
	}
	p.pty = ptmx
	p.running = true
	p.exitErr = nil

	p.vt = xvt.NewSafeEmulator(p.width, p.height)
	p.vt.SetCallbacks(xvt.Callbacks{
		CursorVisibility: func(visible bool) {
			newHidden := !visible
			prev := p.cursorHidden.Swap(newHidden)
			if prev != newHidden {
				p.publish(ModeEvent{
					Mouse:        p.mouseEnabled,
					AltScreen:    p.altScreenActive,
					CursorHidden: newHidden,
				})
			}
		},
	})
	p.cursorHidden.Store(false)
	p.registerTitleHandlersUnlocked()
	p.startDrainUnlocked()

	p.scrollback = NewScrollbackBuffer(p.scrollbackSize)
	p.selection = NewSelectionState()

	p.mu.Unlock()

	// Kick the read loop without going through the tea bridge. Any
	// Subscribe call (including the very first one a daemon-side
	// caller makes) will hit readLoopOnce, but we start the loop
	// here eagerly so the PTY is being drained immediately even if
	// no one has Subscribed yet. This matches Start's behavior
	// (where the returned tea.Cmd Subscribes lazily on first call).
	p.readLoopOnce.Do(p.startReadLoop)
	return nil
}

// PID returns the OS pid of the child process, or 0 if the pane has
// not started or the process has exited. Safe to call from any
// goroutine.
func (p *Pane) PID() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Title returns the most recent title the child process set via OSC 0/2
// escape sequences. Empty string if the child has not set a title (yet).
func (p *Pane) Title() string {
	if v, ok := p.paneTitle.Load().(string); ok {
		return v
	}
	return ""
}

// parseOscTitlePayload extracts the title from an OSC 0/1/2 payload as
// delivered by charm/x/vt. The handler sees the raw payload bytes
// including the leading "<cmd>;" parameter (e.g. "2;hello-title"),
// because charm/x/vt's handleTitle does its own bytes.Split on ';'.
// We strip a leading run of digits followed by ';' if present.
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

// registerTitleHandlersUnlocked wires OSC 0/2 handlers on the emulator
// to capture the child process's window title, plus an OSC 9 handler
// that forwards desktop notifications via the notify package when
// forwardNotifications is set. OSC 0 sets both window title and icon
// name; OSC 2 sets only the window title. Both feed the same Pane
// field — we only care about the window title.
//
// Must be called with p.mu held and after p.vt has been constructed.
func (p *Pane) registerTitleHandlersUnlocked() {
	if p.vt == nil {
		return
	}
	handler := func(data []byte) bool {
		title := parseOscTitlePayload(data)
		p.paneTitle.Store(title)
		// Title handlers fire from within p.vt.Write, which is
		// invoked under p.mu by handleOutput. publish takes only
		// subscribersMu — see lock-order note on publish().
		p.publish(TitleEvent{Title: title})
		return true
	}
	p.vt.RegisterOscHandler(0, handler)
	p.vt.RegisterOscHandler(2, handler)
	p.vt.RegisterOscHandler(9, p.forwardNotificationHandler)
}

// forwardNotificationHandler is the OSC 9 callback registered on the
// emulator. The agent (typically claude code) emits OSC 9 with a
// payload of "9;<body>" to request the host terminal raise a desktop
// notification. We strip the "9;" prefix using parseOscTitlePayload
// (which handles any "<digits>;" prefix) and forward the body to
// notify.Send.
//
// Lock discipline: this runs SYNCHRONOUSLY inside p.vt.Write which is
// invoked under p.mu by handleOutput. Taking p.mu here would deadlock.
// We only touch the forwardNotifications atomic.Bool, so the handler
// is lock-free.
//
// Returns false when forwarding is disabled or the stripped payload is
// empty (so an unhandled-OSC fallback in the emulator doesn't surface
// the payload as a stray title); returns true when notify.Send was
// invoked, regardless of any error notify.Send returned.
func (p *Pane) forwardNotificationHandler(data []byte) bool {
	if !p.forwardNotifications.Load() {
		return false
	}
	body := parseOscTitlePayload(data)
	if body == "" {
		return false
	}
	// iTerm2's OSC 9 progress-bar protocol shares the OSC 9 cmd
	// namespace with simple-text notifications. The progress form is
	// "\x1b]9;<state>;<value>\x07" where state ∈ 0..4 (0 clear, 1 set
	// percent, 2 indeterminate, 3 error, 4 warning); Claude Code emits
	// these to drive the terminal's progress indicator, NOT to raise a
	// desktop notification. After parseOscTitlePayload strips the
	// leading "9;", a progress payload looks like "4;3;" / "1;50" /
	// "2" — digits + semicolons only, no letters. Discriminate by
	// checking for any alphabetic rune: real notification text always
	// contains letters; progress control payloads don't.
	if !payloadContainsLetter(body) {
		return false
	}
	// Errors from notify.Send are swallowed: there's no actionable
	// recovery from the emulator callback, and a logging side-effect
	// would surface inside vt.Write under p.mu. The notify package
	// itself is responsible for any necessary observability.
	_ = notify.Send(body)
	return true
}

// payloadContainsLetter reports whether s has any Unicode letter rune.
// Used by forwardNotificationHandler to discriminate notification text
// from iTerm2 OSC 9 progress-bar control sequences (which are digits +
// semicolons only — no alphabetic characters).
func payloadContainsLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// SetForwardNotifications toggles OSC 9 → desktop notification
// forwarding for this pane. Safe to call from any goroutine; the
// underlying field is atomic.Bool. The daemon calls this once during
// session construction based on SpawnReq.ForwardNotifications.
func (p *Pane) SetForwardNotifications(enabled bool) {
	if p == nil {
		return
	}
	p.forwardNotifications.Store(enabled)
}

// startDrainUnlocked spawns the goroutine that forwards emulator
// responses back to the PTY. Must be called with p.mu held.
func (p *Pane) startDrainUnlocked() {
	p.drainStop = make(chan struct{})
	stop := p.drainStop
	vt := p.vt
	ptyFile := p.pty

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
			n, err := vt.Read(buf)
			if n > 0 && ptyFile != nil {
				_, _ = ptyFile.Write(buf[:n])
			}
			if err != nil {
				if err == io.EOF {
					return
				}
				// Other errors: stop draining; the pane is being torn down.
				return
			}
		}
	}()
}

// Subscribe registers a new event subscriber and returns the receive
// end of a buffered channel plus an unsubscribe func. Calling the
// unsubscribe func removes the channel from the registry and closes
// it; calling it more than once is a no-op.
//
// Safe to call before or after Start. The first call (regardless of
// who makes it — public consumer or the internal tea bridge) starts
// the PTY-read goroutine via sync.Once. Subsequent calls share the
// same loop.
func (p *Pane) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, subscriberChanCapacity)

	p.subscribersMu.Lock()
	p.subscribers = append(p.subscribers, ch)
	p.subscribersMu.Unlock()

	p.readLoopOnce.Do(p.startReadLoop)

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			p.subscribersMu.Lock()
			defer p.subscribersMu.Unlock()
			for i, c := range p.subscribers {
				if c == ch {
					p.subscribers = append(p.subscribers[:i], p.subscribers[i+1:]...)
					close(ch)
					return
				}
			}
			// Not found means the read loop already closed it on
			// shutdown; nothing to do.
		})
	}
	return ch, cancel
}

// startReadLoop spawns the single PTY-read goroutine. Called via
// p.readLoopOnce.Do, so exactly one runs across a Pane's lifetime.
// Callers MUST ensure Pane.Start has completed before the first
// Subscribe (which triggers this) — otherwise p.pty is nil and the
// loop publishes an immediate ExitEvent.
//
// The loop reads bytes from p.pty, hands them to handleOutput (which
// takes p.mu) to update emulator/scrollback/mode flags, then fans the
// resulting OutputEvent out to all subscribers. ModeEvent / TitleEvent
// are emitted from handleOutput's helpers and the emulator callbacks,
// not from this function directly.
//
// On p.pty.Read error (typically io.EOF after the child exits) the
// loop publishes a final ExitEvent and returns.
//
// IMPORTANT: This function does NOT take p.mu. It would be a deadlock
// when invoked from Subscribe under sync.Once during Start's critical
// section (Start holds p.mu while calling the returned tea.Cmd which
// in turn Subscribes). The read of p.pty here is safe because
// sync.Once / Subscribe is only called via the tea.Cmd returned from
// Start, which the tea runtime invokes after the Cmd closure returns
// — by which point Start's p.mu critical section has finished
// publishing p.pty.
//
// We DO acquire p.mu briefly to allocate readLoopStop, but only via
// stopReadLoopMu (a finer-grained lock dedicated to lifecycle). This
// avoids the Start re-entry deadlock.
func (p *Pane) startReadLoop() {
	p.stopReadLoopMu.Lock()
	p.readLoopStop = make(chan struct{})
	stop := p.readLoopStop
	p.stopReadLoopMu.Unlock()

	ptyFile := p.pty
	if ptyFile == nil {
		// Defensive: Subscribe was called before Start finished.
		// Publish exit so subscribers don't hang.
		p.publishExit(fmt.Errorf("pane %s: read loop started before PTY ready", p.id))
		return
	}

	p.readLoopWG.Add(1)
	go func() {
		defer p.readLoopWG.Done()
		buf := make([]byte, readBufferSize)
		for {
			select {
			case <-stop:
				return
			default:
			}

			n, err := ptyFile.Read(buf)
			if n > 0 {
				// Copy bytes — buf is reused on the next iteration
				// and we want each subscriber to own the slice it
				// receives.
				data := make([]byte, n)
				copy(data, buf[:n])

				p.handleOutput(data)
				p.publish(OutputEvent{Data: data})
			}
			if err != nil {
				p.publishExit(err)
				return
			}
		}
	}()
}

// publish fans an event out to all current subscribers. Subscribers
// whose buffer is full receive a dropped event (logged once per drop
// to stderr) — the read loop never blocks on a slow consumer.
//
// Safe to call with p.mu held: publish takes only subscribersMu and
// performs only non-blocking channel sends. Lock order is
// p.mu → subscribersMu (never the reverse).
func (p *Pane) publish(ev Event) {
	p.subscribersMu.Lock()
	defer p.subscribersMu.Unlock()
	for _, ch := range p.subscribers {
		select {
		case ch <- ev:
		default:
			fmt.Fprintf(os.Stderr, "pane %s: dropped event for slow subscriber\n", p.id)
		}
	}
}

// publishExit emits a final ExitEvent and closes every subscriber
// channel. After this returns no further publishes will succeed
// (subscribers slice is nil'd).
func (p *Pane) publishExit(err error) {
	p.subscribersMu.Lock()
	defer p.subscribersMu.Unlock()
	for _, ch := range p.subscribers {
		// Best-effort delivery of the terminal event. If the buffer
		// is full, drop it: subscribers learn about exit via the
		// channel close that follows.
		select {
		case ch <- ExitEvent{Err: err}:
		default:
			fmt.Fprintf(os.Stderr, "pane %s: dropped ExitEvent for slow subscriber\n", p.id)
		}
		close(ch)
	}
	p.subscribers = nil
}

// stopReadLoop closes the stop channel exactly once and waits for the
// read goroutine to exit. Safe to call from Stop / StopGraceful (which
// also close p.pty, unblocking any in-flight Read). Must be called
// with p.mu released to avoid deadlocking with publish().
func (p *Pane) stopReadLoop() {
	p.stopReadLoopMu.Lock()
	if !p.readLoopStopped && p.readLoopStop != nil {
		close(p.readLoopStop)
		p.readLoopStopped = true
	}
	p.stopReadLoopMu.Unlock()
	p.readLoopWG.Wait()
}

// stopDrainUnlocked terminates the response-drain goroutine. Must be
// called with p.mu held.
//
// To unblock the drain goroutine's vt.Read we write a sentinel byte
// into the emulator's response pipe (InputPipe is the writer end of
// pr/pw; pr is what the drain reads). vt.Read returns with the
// byte, the drain loop iterates, sees drainStop closed, and exits.
//
// This avoids calling Emulator.Close(), which mutates an internal
// `closed` field without a lock — a benign race in practice but one
// the -race detector trips on against the concurrent Read.
func (p *Pane) stopDrainUnlocked() {
	if p.drainStop == nil {
		return
	}
	close(p.drainStop)
	if p.vt != nil {
		// One-byte wakeup. The byte itself is irrelevant — drain
		// will write it to ptyFile (which is likely already closed,
		// so the write errors and is ignored) and then re-enter the
		// for-loop which sees the closed stop channel and returns.
		if w := p.vt.Emulator.InputPipe(); w != nil {
			_, _ = w.Write([]byte{0})
		}
	}
	p.drainStop = nil
	// Wait without holding p.mu — currently callers already hold
	// p.mu, but the drain goroutine doesn't touch p.mu so the Wait
	// is safe. (We did NOT take p.mu in the goroutine itself.)
	p.drainWG.Wait()
}

func (p *Pane) Stop() error {
	p.mu.Lock()

	if p.cmd != nil && p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
	if p.pty != nil {
		p.pty.Close()
	}
	p.stopDrainUnlocked()
	p.running = false
	p.mu.Unlock()

	// Tear down the read loop without holding p.mu: stopReadLoop
	// waits for the goroutine to exit, and that goroutine calls
	// handleOutput (which takes p.mu) — Wait()ing under p.mu would
	// self-deadlock.
	p.stopReadLoop()
	return nil
}

// StopGraceful sends SIGTERM, waits for timeout, then SIGKILL if needed.
func (p *Pane) StopGraceful(timeout time.Duration) error {
	p.mu.Lock()
	if !p.running || p.cmd == nil || p.cmd.Process == nil {
		p.mu.Unlock()
		return nil
	}

	proc := p.cmd.Process
	p.mu.Unlock()

	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return p.Stop()
	}

	done := make(chan error, 1)
	go func() {
		_, err := proc.Wait()
		done <- err
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		proc.Kill()
	}

	p.mu.Lock()
	if p.pty != nil {
		p.pty.Close()
	}
	p.stopDrainUnlocked()
	p.running = false
	p.mu.Unlock()

	// See Stop(): tear down the read loop outside p.mu.
	p.stopReadLoop()
	return nil
}

var ErrPaneNotRunning = fmt.Errorf("pane is not running")

func (p *Pane) WriteInput(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.running || p.pty == nil {
		return 0, ErrPaneNotRunning
	}
	return p.pty.Write(data)
}

// readOutput returns a Cmd that reads from the PTY
func (p *Pane) readOutput() tea.Cmd {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.readOutputUnlocked()
}

// readOutputUnlocked must be called with mu held.
//
// Bridges the new channel-based subscription API back into BubbleTea's
// Cmd model. Returns a Cmd that, on first invocation, lazily
// Subscribes the Pane (which kicks off startReadLoop the first time
// any subscriber appears) and caches the channel. Subsequent
// invocations read one event from that cached channel and translate
// it to an OutputMsg or ExitMsg. Title/ModeEvents are silently
// dropped by this bridge — the UI does not yet wire them through
// tea.Msg.
//
// The lazy Subscribe is deliberately deferred into the Cmd closure
// (not done eagerly here). The closure runs from the tea runtime
// without p.mu held; Subscribe → startReadLoop wants p.mu briefly,
// which would self-deadlock if we Subscribed inline while Start still
// holds the lock.
func (p *Pane) readOutputUnlocked() tea.Cmd {
	if p.pty == nil {
		return nil
	}

	paneID := p.id

	return func() tea.Msg {
		p.teaBridgeOnce.Do(func() {
			ch, _ := p.Subscribe()
			p.teaBridgeCh = ch
		})
		ch := p.teaBridgeCh

		for ev := range ch {
			switch e := ev.(type) {
			case OutputEvent:
				return OutputMsg{PaneID: paneID, Data: e.Data}
			case ExitEvent:
				return ExitMsg{PaneID: paneID, Err: e.Err}
			default:
				// TitleEvent / ModeEvent: drop. The UI learns
				// about these through other paths (Pane.Title()
				// is polled by the model; mouse/alt-screen are
				// checked at use-time).
				continue
			}
		}
		// Channel closed without ExitEvent (e.g. Stop torn the
		// pane down). Synthesize an exit so the UI cleans up.
		return ExitMsg{PaneID: paneID, Err: io.EOF}
	}
}

// --- Update Handler ---

// Update handles messages for this pane, returns commands to execute
func (p *Pane) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case OutputMsg:
		if msg.PaneID != p.id {
			return nil
		}
		// Bytes were already processed by the read goroutine
		// (handleOutput is invoked there before fan-out). The
		// OutputMsg arriving here is purely a signal to schedule a
		// render tick and re-arm the Cmd that reads from the tea
		// bridge channel.
		return tea.Batch(p.readOutput(), p.scheduleRenderTick())

	case RenderTickMsg:
		if msg.PaneID != p.id {
			return nil
		}
		p.mu.Lock()
		p.renderScheduled = false
		p.mu.Unlock()
		return nil

	case ExitMsg:
		if msg.PaneID != p.id {
			return nil
		}
		p.mu.Lock()
		p.running = false
		p.exitErr = msg.Err
		if p.pty != nil {
			p.pty.Close()
		}
		p.stopDrainUnlocked()
		p.mu.Unlock()
		// The read loop has already exited (it's what produced
		// this ExitMsg). stopReadLoop is a cheap no-op in that
		// case but ensures readLoopStop is closed exactly once.
		p.stopReadLoop()
		return nil
	}

	return nil
}

func (p *Pane) handleOutput(data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.vt == nil {
		return
	}

	// Diagnostic: capture the raw byte stream when OPENKANBAN_PTY_DEBUG_LOG
	// is set. No-op overhead when disabled (a single nil-check).
	ptyDebugLog(p.id, data)

	p.detectMouseModeChanges(data)
	p.detectAltScreenChanges(data)

	// Capture scrollback: snapshot row 0 before vt.Write, push it to
	// the ring after if it scrolled off. Shared with PaneView via
	// scrollback_capture.go so the daemon-side pane and the client-side
	// mirror produce identical scrollback content.
	p.lastTopRow = CaptureTopRow(p.vt, p.altScreenActive)
	p.vt.Write(data)
	PushScrolledLine(p.vt, p.altScreenActive, p.lastTopRow, p.scrollback)
	p.lastTopRow = nil

	// Stamp activity for the daemon's status broadcaster. Lock-free
	// write — readers (the broadcaster goroutine) use atomic.Load.
	if len(data) > 0 {
		p.lastActivityNs.Store(time.Now().UnixNano())
	}

	p.dirty = true
}

// LastActivity returns the timestamp of the most recent PTY output
// observed by this pane, or the zero time if the pane has produced no
// output yet. Safe to call from any goroutine; reads atomically without
// taking p.mu so the daemon's activity broadcaster doesn't contend with
// the read loop.
func (p *Pane) LastActivity() time.Time {
	if p == nil {
		return time.Time{}
	}
	ns := p.lastActivityNs.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// detectMouseModeChanges scans output for mouse tracking mode escape sequences.
// Called with mutex held. Emits a ModeEvent on transition so subscribers
// learn about mouse-tracking flips without parsing bytes themselves.
func (p *Pane) detectMouseModeChanges(data []byte) {
	// Mouse tracking enable sequences (any of these enables mouse mode)
	enableSeqs := [][]byte{
		[]byte("\x1b[?1000h"), // X10 mouse tracking
		[]byte("\x1b[?1002h"), // Button-event tracking
		[]byte("\x1b[?1003h"), // Any-event tracking
		[]byte("\x1b[?1006h"), // SGR extended mode
	}

	// Mouse tracking disable sequences
	disableSeqs := [][]byte{
		[]byte("\x1b[?1000l"),
		[]byte("\x1b[?1002l"),
		[]byte("\x1b[?1003l"),
		[]byte("\x1b[?1006l"),
	}

	// Check for enable sequences
	for _, seq := range enableSeqs {
		if bytes.Contains(data, seq) {
			if !p.mouseEnabled {
				p.mouseEnabled = true
				p.publishModeEventLocked()
			}
			return
		}
	}

	// Check for disable sequences
	for _, seq := range disableSeqs {
		if bytes.Contains(data, seq) {
			if p.mouseEnabled {
				p.mouseEnabled = false
				p.publishModeEventLocked()
			}
			return
		}
	}
}

// publishModeEventLocked snapshots the three flags and publishes a
// ModeEvent. Must be called with p.mu held. publish only takes
// subscribersMu, so this is safe under p.mu (see publish docstring).
func (p *Pane) publishModeEventLocked() {
	p.publish(ModeEvent{
		Mouse:        p.mouseEnabled,
		AltScreen:    p.altScreenActive,
		CursorHidden: p.cursorHidden.Load(),
	})
}

// detectAltScreenChanges scans output for alternate screen mode escape sequences.
// Called with mutex held. Emits a ModeEvent on transition so subscribers
// can react without re-parsing bytes.
func (p *Pane) detectAltScreenChanges(data []byte) {
	// Alternate screen enable sequences (smcup)
	enableSeqs := [][]byte{
		[]byte("\x1b[?1049h"), // Save cursor + switch to alt screen
		[]byte("\x1b[?47h"),   // Switch to alt screen (legacy)
	}

	// Alternate screen disable sequences (rmcup)
	disableSeqs := [][]byte{
		[]byte("\x1b[?1049l"), // Restore cursor + switch from alt screen
		[]byte("\x1b[?47l"),   // Switch from alt screen (legacy)
	}

	// Check for enable sequences
	for _, seq := range enableSeqs {
		if bytes.Contains(data, seq) {
			if !p.altScreenActive {
				p.altScreenActive = true
				p.viewportOffset = 0 // Reset viewport when entering alt screen
				p.publishModeEventLocked()
			}
			return
		}
	}

	// Check for disable sequences
	for _, seq := range disableSeqs {
		if bytes.Contains(data, seq) {
			if p.altScreenActive {
				p.altScreenActive = false
				p.publishModeEventLocked()
			}
			return
		}
	}
}


// scheduleRenderTick returns a Cmd to trigger render after throttle interval
func (p *Pane) scheduleRenderTick() tea.Cmd {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.renderScheduled {
		return nil
	}
	p.renderScheduled = true

	timeSinceLastRender := time.Since(p.lastRender)
	delay := renderInterval - timeSinceLastRender
	if delay < 0 {
		delay = 0
	}

	paneID := p.id
	return tea.Tick(delay, func(time.Time) tea.Msg {
		return RenderTickMsg{PaneID: paneID}
	})
}

// --- Key Handling (Issue #15) ---

func (p *Pane) HandleMouse(msg tea.MouseMsg) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.running || p.pty == nil {
		return
	}

	// Selection processing runs regardless of whether the child has
	// mouse tracking enabled, so the user can always drag-to-select
	// and Cmd+C-to-copy text from any pane. When the child also has
	// mouse tracking on, the event is ALSO forwarded to it below
	// (unless Shift is held — see the Shift bypass at the end).
	//
	// A bare click without drag does not produce a persistent
	// selection (SelectionState.Finish transitions to Idle when
	// anchor==cursor), so menu clicks still work normally.
	if p.selection != nil {
		switch msg.Button {
		case tea.MouseButtonLeft:
			pos := p.viewportToLogical(msg.X, msg.Y)
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
			if p.selection.Mode == SelectionSelecting {
				pos := p.viewportToLogical(msg.X, msg.Y)
				p.selection.Update(pos)
				p.dirty = true
			}
		case tea.MouseButtonRight, tea.MouseButtonMiddle:
			if p.selection.IsActive() {
				p.selection.Clear()
				p.dirty = true
			}
		case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
			// Scrolling invalidates the pinned selection coordinates.
			if p.selection.IsActive() {
				p.selection.Clear()
				p.dirty = true
			}
		}
	}

	// When the child has not enabled mouse tracking, also handle wheel
	// scrolling locally against our own scrollback buffer.
	if !p.mouseEnabled {
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			p.scrollUp(3)
		case tea.MouseButtonWheelDown:
			p.scrollDown(3)
		}
		return
	}

	// Mouse tracking is enabled. Shift held = the user is claiming the
	// event for openkanban (selection already handled above); don't
	// also pass it to the child.
	if msg.Shift {
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

	if len(seq) > 0 {
		p.pty.Write(seq)
	}
}

// viewportToLogical converts viewport coordinates to logical position
// Logical position: negative row = scrollback, 0+ = live screen
// Called with mutex held.
func (p *Pane) viewportToLogical(x, y int) Position {
	// When scrolled back, top of viewport shows scrollback
	// viewportOffset = how many scrollback lines are visible at top
	// Calculate logical row
	// If viewportOffset > 0, the top rows are from scrollback
	// Row 0 in viewport corresponds to scrollback line (scrollbackLen - viewportOffset)

	logicalRow := y - p.viewportOffset

	return Position{Row: logicalRow, Col: x}
}

// HandleKey processes a key event and sends to PTY
func (p *Pane) HandleKey(msg tea.KeyMsg) tea.Msg {
	if msg.String() == "ctrl+g" {
		return ExitFocusMsg{}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.running || p.pty == nil {
		return nil
	}

	key := msg.String()

	// Check for selection copy FIRST (before forwarding Ctrl+C to PTY)
	if p.selection != nil && p.selection.IsActive() {
		if key == "ctrl+c" || key == "cmd+c" {
			p.copySelectionUnlocked()
			return nil
		}
	}

	// Handle scroll navigation keys (work regardless of mouse mode)
	switch key {
	case "shift+pgup":
		rows := p.vt.Height()
		p.scrollUp(rows / 2)
		return nil
	case "shift+pgdown":
		rows := p.vt.Height()
		p.scrollDown(rows / 2)
		return nil
	case "shift+home":
		// Scroll to top of scrollback
		if p.scrollback != nil {
			p.viewportOffset = p.scrollback.Len()
			p.dirty = true
		}
		return nil
	case "shift+end":
		// Scroll to bottom (live view)
		p.viewportOffset = 0
		p.dirty = true
		return nil
	case "esc", "escape":
		// Esc returns to live view if scrolled
		if p.viewportOffset > 0 {
			p.viewportOffset = 0
			p.dirty = true
			return nil
		}
		// Also clear selection on Esc
		if p.selection != nil && p.selection.IsActive() {
			p.selection.Clear()
			p.dirty = true
			return nil
		}
		// Otherwise forward escape to PTY
	}

	// Snap to live view on any other keyboard input
	if p.viewportOffset > 0 {
		p.viewportOffset = 0
		p.dirty = true
	}

	// Clear selection on any keyboard input (except copy)
	if p.selection != nil && p.selection.IsActive() {
		p.selection.Clear()
		p.dirty = true
	}

	input := p.translateKey(msg)
	if len(input) > 0 {
		p.pty.Write(input)
	}

	return nil
}

// copySelectionUnlocked copies selected text to clipboard
// Called with mutex held.
func (p *Pane) copySelectionUnlocked() {
	if p.selection == nil || !p.selection.IsActive() {
		return
	}

	// Get scrollback lines for text extraction
	var scrollbackLines [][]Glyph
	scrollbackLen := 0
	if p.scrollback != nil {
		scrollbackLen = p.scrollback.Len()
		scrollbackLines = p.scrollback.GetRange(0, scrollbackLen)
	}

	// Get live screen accessor. SafeEmulator handles its own locking
	// for CellAt, so the closure is safe to use directly.
	liveRows := p.vt.Height()
	liveScreen := func(col, row int) Glyph {
		return cellToGlyph(p.vt.CellAt(col, row))
	}

	text := p.selection.ExtractText(scrollbackLines, liveScreen, liveRows, scrollbackLen)

	if text != "" {
		clipboard.WriteAll(text)
	}

	// Clear selection after copy
	p.selection.Clear()
	p.dirty = true
}

// scrollUp scrolls the viewport up (into scrollback history)
// Called with mutex held.
func (p *Pane) scrollUp(lines int) {
	if p.scrollback == nil {
		return
	}
	maxOffset := p.scrollback.Len()
	p.viewportOffset += lines
	if p.viewportOffset > maxOffset {
		p.viewportOffset = maxOffset
	}
	p.dirty = true
}

// scrollDown scrolls the viewport down (toward live view)
// Called with mutex held.
func (p *Pane) scrollDown(lines int) {
	p.viewportOffset -= lines
	if p.viewportOffset < 0 {
		p.viewportOffset = 0
	}
	p.dirty = true
}

// translateKey converts Bubbletea KeyMsg to PTY byte sequences
func (p *Pane) translateKey(msg tea.KeyMsg) []byte {
	key := msg.String()

	// Handle modifier combinations
	switch {
	// Ctrl+A through Ctrl+Z → 0x01-0x1A
	case len(key) == 6 && key[:5] == "ctrl+" && key[5] >= 'a' && key[5] <= 'z':
		return []byte{byte(key[5] - 'a' + 1)}

	// Alt+letter → ESC + letter
	case len(key) == 5 && key[:4] == "alt+" && key[4] >= 'a' && key[4] <= 'z':
		return []byte{27, key[4]}
	}

	// Handle special keys
	switch msg.Type {
	case tea.KeyEnter:
		// Bubbletea v1 has no Shift field; terminals configured to
		// send shift+enter emit ESC+CR, which bubbletea reports as
		// Alt+Enter. Pass that through verbatim so the inner agent
		// (Claude Code, etc.) sees the meta-Enter and inserts a
		// newline instead of submitting.
		if msg.Alt {
			return []byte{27, '\r'}
		}
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

// GetContent returns the current terminal content as plain text for analysis.
func (p *Pane) GetContent() string {
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

	var result strings.Builder
	for row := 0; row < rows; row++ {
		if row > 0 {
			result.WriteByte('\n')
		}
		for col := 0; col < cols; col++ {
			ch := cellToGlyph(p.vt.CellAt(col, row)).Char
			if ch == 0 {
				ch = ' '
			}
			result.WriteRune(ch)
		}
	}

	return result.String()
}

// --- Rendering (Issue #14) ---

// View returns the rendered terminal content
// View returns the cached rendered view of the pane, regenerating
// only when dirty. The actual rendering lives in render.go — kept as
// loose functions taking emulator + scrollback + selection params so
// PR7's client/server split can lift them into the daemon-client
// without dragging Pane along.
func (p *Pane) View() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.dirty && p.cachedView != "" {
		return p.cachedView
	}

	p.cachedView = renderVT(p.vt, p.scrollback, p.viewportOffset, !p.cursorHidden.Load(), p.selection)
	p.lastRender = time.Now()
	p.dirty = false
	return p.cachedView
}

func buildCleanEnv(sessionName, ticketID string) []string {
	var env []string
	for _, e := range os.Environ() {
		key := strings.Split(e, "=")[0]
		if key == "OPENCODE" || strings.HasPrefix(key, "OPENCODE_") {
			continue
		}
		if key == "CLAUDE" || strings.HasPrefix(key, "CLAUDE_") {
			continue
		}
		if key == "GEMINI" || strings.HasPrefix(key, "GEMINI_") {
			continue
		}
		if key == "CODEX" || strings.HasPrefix(key, "CODEX_") {
			continue
		}
		// Strip any inherited OPENKANBAN_* so each spawn gets exactly the
		// session + ticket id configured for THIS pane and nothing else.
		// Without this, nesting openkanban inside an openkanban-spawned
		// shell would leak the outer pane's identity into the inner child.
		if strings.HasPrefix(key, "OPENKANBAN_") {
			continue
		}
		env = append(env, e)
	}
	env = append(env, "TERM=xterm-256color")
	if sessionName != "" {
		env = append(env, "OPENKANBAN_SESSION="+sessionName)
	}
	if ticketID != "" {
		env = append(env, "OPENKANBAN_TICKET_ID="+ticketID)
	}
	return env
}
