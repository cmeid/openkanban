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
	"time"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"
	xvt "github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
)

const (
	renderInterval = 50 * time.Millisecond
	readBufferSize = 65536
)

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
		defer p.mu.Unlock()

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
				p.cursorHidden.Store(!visible)
			},
		})
		p.cursorHidden.Store(false)
		p.registerTitleHandlersUnlocked()
		p.startDrainUnlocked()

		p.scrollback = NewScrollbackBuffer(p.scrollbackSize)
		p.selection = NewSelectionState()

		// Start read loop
		return p.readOutputUnlocked()()
	}
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
// to capture the child process's window title. OSC 0 sets both window
// title and icon name; OSC 2 sets only the window title. Both feed the
// same Pane field — we only care about the window title.
//
// Must be called with p.mu held and after p.vt has been constructed.
func (p *Pane) registerTitleHandlersUnlocked() {
	if p.vt == nil {
		return
	}
	handler := func(data []byte) bool {
		p.paneTitle.Store(parseOscTitlePayload(data))
		return true
	}
	p.vt.RegisterOscHandler(0, handler)
	p.vt.RegisterOscHandler(2, handler)
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

// stopDrainUnlocked terminates the response-drain goroutine. Must be
// called with p.mu held. Closing the emulator unblocks any in-flight
// Read so the goroutine actually exits.
func (p *Pane) stopDrainUnlocked() {
	if p.drainStop == nil {
		return
	}
	close(p.drainStop)
	if p.vt != nil {
		// Close the emulator to unblock Read().
		_ = p.vt.Emulator.Close()
	}
	p.drainStop = nil
	// Don't Wait here while holding p.mu — the drain goroutine
	// doesn't grab p.mu, but a future caller might want Stop() to
	// return promptly. The wait is cheap regardless.
	p.drainWG.Wait()
}

func (p *Pane) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cmd != nil && p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
	if p.pty != nil {
		p.pty.Close()
	}
	p.stopDrainUnlocked()
	p.running = false
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

	if err := proc.Signal(os.Interrupt); err != nil {
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

// readOutputUnlocked must be called with mu held
func (p *Pane) readOutputUnlocked() tea.Cmd {
	if p.pty == nil {
		return nil
	}

	ptyFile := p.pty
	paneID := p.id

	return func() tea.Msg {
		buf := make([]byte, readBufferSize)
		n, err := ptyFile.Read(buf)
		if err != nil {
			return ExitMsg{PaneID: paneID, Err: err}
		}
		return OutputMsg{PaneID: paneID, Data: buf[:n]}
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
		p.handleOutput(msg.Data)
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

	// Capture scrollback: snapshot before, compare after
	p.captureScrollbackBeforeWrite()
	p.vt.Write(data)
	p.captureScrollbackAfterWrite()

	p.dirty = true
}

// detectMouseModeChanges scans output for mouse tracking mode escape sequences.
// Called with mutex held.
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
			p.mouseEnabled = true
			return
		}
	}

	// Check for disable sequences
	for _, seq := range disableSeqs {
		if bytes.Contains(data, seq) {
			p.mouseEnabled = false
			return
		}
	}
}

// detectAltScreenChanges scans output for alternate screen mode escape sequences.
// Called with mutex held.
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
			p.altScreenActive = true
			p.viewportOffset = 0 // Reset viewport when entering alt screen
			return
		}
	}

	// Check for disable sequences
	for _, seq := range disableSeqs {
		if bytes.Contains(data, seq) {
			p.altScreenActive = false
			return
		}
	}
}

// captureScrollbackBeforeWrite takes a snapshot of row 0 before vt.Write.
// Called with mutex held. SafeEmulator's own internal mutex serializes
// emulator access; we additionally rely on p.mu to keep this pair
// atomic with respect to other pane operations.
func (p *Pane) captureScrollbackBeforeWrite() {
	if p.vt == nil || p.altScreenActive {
		p.lastTopRow = nil
		return
	}

	cols := p.vt.Width()
	if cols <= 0 {
		p.lastTopRow = nil
		return
	}

	// Snapshot row 0
	p.lastTopRow = make([]Glyph, cols)
	for col := 0; col < cols; col++ {
		p.lastTopRow[col] = cellToGlyph(p.vt.CellAt(col, 0))
	}
}

// captureScrollbackAfterWrite checks if row 0 changed and captures scrolled line.
// Called with mutex held.
func (p *Pane) captureScrollbackAfterWrite() {
	if p.vt == nil || p.altScreenActive || p.lastTopRow == nil {
		return
	}

	cols := p.vt.Width()
	if cols <= 0 || cols != len(p.lastTopRow) {
		return
	}

	// Compare current row 0 with snapshot
	changed := false
	for col := 0; col < cols; col++ {
		if cellToGlyph(p.vt.CellAt(col, 0)) != p.lastTopRow[col] {
			changed = true
			break
		}
	}

	// If row 0 changed and the old content isn't visible anywhere,
	// the old top row has scrolled off - add to scrollback
	if changed && !p.isLineVisible(p.lastTopRow) {
		p.scrollback.Push(p.lastTopRow)
	}

	p.lastTopRow = nil
}

// isLineVisible checks if a line is still visible on screen.
// Called with mutex held.
func (p *Pane) isLineVisible(line []Glyph) bool {
	cols := p.vt.Width()
	rows := p.vt.Height()
	if len(line) != cols {
		return false
	}

	for row := 0; row < rows; row++ {
		match := true
		for col := 0; col < cols; col++ {
			if cellToGlyph(p.vt.CellAt(col, row)) != line[col] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
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
func (p *Pane) View() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Return cached view if not dirty
	if !p.dirty && p.cachedView != "" {
		return p.cachedView
	}

	p.cachedView = p.renderVTUnlocked()
	p.lastRender = time.Now()
	p.dirty = false
	return p.cachedView
}

func (p *Pane) renderVTUnlocked() string {
	if p.vt == nil {
		return "Terminal not initialized"
	}

	cols := p.vt.Width()
	rows := p.vt.Height()
	if cols <= 0 || rows <= 0 {
		return ""
	}

	// If scrolled back, render mixed scrollback + live content
	if p.viewportOffset > 0 && p.scrollback != nil {
		return p.renderScrolledViewUnlocked(cols, rows)
	}

	return p.renderLiveScreenUnlocked(cols, rows)
}

// renderScrolledViewUnlocked renders a viewport that includes scrollback history.
// Must hold p.mu.
func (p *Pane) renderScrolledViewUnlocked(cols, rows int) string {
	scrollbackLen := p.scrollback.Len()
	offset := p.viewportOffset
	if offset > scrollbackLen {
		offset = scrollbackLen
	}

	var result strings.Builder
	result.Grow(rows * cols * 2)

	// Calculate which lines to show
	// viewportOffset is how many lines we've scrolled back from live view
	// So if offset=5, we show 5 less live lines and 5 scrollback lines at top

	// Number of scrollback lines visible at top of viewport
	scrollbackRowsVisible := offset
	if scrollbackRowsVisible > rows {
		scrollbackRowsVisible = rows
	}

	// Starting scrollback index (from the end of scrollback)
	scrollbackStart := scrollbackLen - offset

	for viewRow := 0; viewRow < rows; viewRow++ {
		if viewRow > 0 {
			result.WriteByte('\n')
		}

		if viewRow < scrollbackRowsVisible {
			// Render from scrollback
			scrollbackIdx := scrollbackStart + viewRow
			line := p.scrollback.Get(scrollbackIdx)
			// Logical row: negative for scrollback (counting from 0)
			// scrollbackIdx 0 = oldest line = logicalRow -(scrollbackLen)
			// scrollbackIdx scrollbackLen-1 = newest scrollback = logicalRow -1
			logicalRow := scrollbackIdx - scrollbackLen
			result.WriteString(p.renderGlyphLine(line, cols, logicalRow))
		} else {
			// Render from live screen
			liveRow := viewRow - scrollbackRowsVisible
			logicalRow := liveRow // Live rows are 0+
			result.WriteString(p.renderLiveRow(cols, liveRow, logicalRow))
		}
	}

	return result.String()
}

// renderGlyphLine renders a line of glyphs with ANSI styling.
// logicalRow is used for selection highlighting.
func (p *Pane) renderGlyphLine(line []Glyph, cols int, logicalRow int) string {
	var result strings.Builder
	var currentStyle Glyph
	var batch strings.Builder
	firstCell := true
	inSelection := false

	flushBatch := func() {
		if batch.Len() == 0 {
			return
		}
		if inSelection {
			result.WriteString("\x1b[7m") // Reverse video for selection
		} else {
			result.WriteString(glyphANSI(currentStyle))
		}
		result.WriteString(batch.String())
		result.WriteString("\x1b[0m")
		batch.Reset()
	}

	for col := 0; col < cols; col++ {
		var glyph Glyph
		if col < len(line) {
			glyph = line[col]
		}
		ch := glyph.Char
		if ch == 0 {
			ch = ' '
		}

		// Check if this cell is selected
		cellSelected := p.selection != nil && p.selection.Contains(Position{Row: logicalRow, Col: col})

		// Style changed or selection changed? Flush batch
		if !firstCell && (!glyph.styleEqual(currentStyle) || cellSelected != inSelection) {
			flushBatch()
		}

		currentStyle = glyph
		inSelection = cellSelected
		firstCell = false

		batch.WriteRune(ch)
	}
	flushBatch()

	return result.String()
}

// renderLiveRow renders a single row from the live terminal screen.
// logicalRow is used for selection highlighting.
// Must hold p.mu.
func (p *Pane) renderLiveRow(cols, row int, logicalRow int) string {
	var result strings.Builder
	var currentStyle Glyph
	var batch strings.Builder
	firstCell := true
	inSelection := false

	flushBatch := func() {
		if batch.Len() == 0 {
			return
		}
		if inSelection {
			result.WriteString("\x1b[7m") // Reverse video for selection
		} else {
			result.WriteString(glyphANSI(currentStyle))
		}
		result.WriteString(batch.String())
		result.WriteString("\x1b[0m")
		batch.Reset()
	}

	cursor := p.vt.CursorPosition()
	cursorVisible := !p.cursorHidden.Load()

	for col := 0; col < cols; col++ {
		glyph := cellToGlyph(p.vt.CellAt(col, row))
		ch := glyph.Char
		if ch == 0 {
			ch = ' '
		}

		isCursor := cursorVisible && col == cursor.X && row == cursor.Y
		cellSelected := p.selection != nil && p.selection.Contains(Position{Row: logicalRow, Col: col})

		// Style changed or selection changed? Flush batch
		if !firstCell && (!glyph.styleEqual(currentStyle) || isCursor || cellSelected != inSelection) {
			flushBatch()
		}

		// Handle cursor with reverse video (cursor takes priority over selection)
		if isCursor {
			result.WriteString("\x1b[7m") // Reverse
			result.WriteRune(ch)
			result.WriteString("\x1b[27m") // Un-reverse
			firstCell = true
			inSelection = false
			continue
		}

		currentStyle = glyph
		inSelection = cellSelected
		firstCell = false

		batch.WriteRune(ch)
	}
	flushBatch()

	return result.String()
}

// renderLiveScreenUnlocked renders the live terminal screen. Must hold p.mu.
func (p *Pane) renderLiveScreenUnlocked(cols, rows int) string {
	cursor := p.vt.CursorPosition()
	cursorVisible := !p.cursorHidden.Load()

	var result strings.Builder
	result.Grow(rows * cols * 2)

	for row := 0; row < rows; row++ {
		if row > 0 {
			result.WriteByte('\n')
		}

		// Track current style for batching
		var currentStyle Glyph
		var batch strings.Builder
		firstCell := true
		inSelection := false

		flushBatch := func() {
			if batch.Len() == 0 {
				return
			}
			if inSelection {
				result.WriteString("\x1b[7m") // Reverse video for selection
			} else {
				result.WriteString(glyphANSI(currentStyle))
			}
			result.WriteString(batch.String())
			result.WriteString("\x1b[0m")
			batch.Reset()
		}

		for col := 0; col < cols; col++ {
			glyph := cellToGlyph(p.vt.CellAt(col, row))
			ch := glyph.Char
			if ch == 0 {
				ch = ' '
			}

			isCursor := cursorVisible && col == cursor.X && row == cursor.Y
			logicalRow := row // When not scrolled, logical row = screen row
			cellSelected := p.selection != nil && p.selection.Contains(Position{Row: logicalRow, Col: col})

			// Style changed or selection changed? Flush batch
			if !firstCell && (!glyph.styleEqual(currentStyle) || isCursor || cellSelected != inSelection) {
				flushBatch()
			}

			// Handle cursor with reverse video
			if isCursor {
				result.WriteString("\x1b[7m") // Reverse
				result.WriteRune(ch)
				result.WriteString("\x1b[27m") // Un-reverse
				firstCell = true
				inSelection = false
				continue
			}

			currentStyle = glyph
			inSelection = cellSelected
			firstCell = false

			batch.WriteRune(ch)
		}
		flushBatch()
	}

	return result.String()
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
