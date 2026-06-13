package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/techdufus/openkanban/internal/terminal"
)

// Session is a daemon-owned PTY-backed agent process. It wraps a
// terminal.Pane (which owns the PTY + emulator + scrollback) and
// records the bookkeeping the daemon needs to answer List / Owns /
// Attach queries without rummaging through the Pane internals.
//
// All exported methods are safe to call concurrently.
type Session struct {
	id          string
	ticketID    string
	sessionName string
	workdir     string
	pane        *terminal.Pane
	startedAt   time.Time

	// attachMu protects attached and serializes Attach/Detach.
	attachMu sync.Mutex
	attached *attachedClient // nil = no current attacher

	// subscribers reserved for PR9 fan-out — declared here so the
	// slice type is stable across PRs. Not populated in PR5.
	subscribers []*subscriberConn
}

// attachedClient identifies the single client whose connection is
// currently upgraded to binary mode for this session.
//
// Conn / WriteMu are the wire handles fanOut and binaryLoop use to
// emit PTY-output frames and read PTY-input frames respectively. The
// Subscription is the pane event channel feeding fanOut; cancelling
// it both removes the subscriber from the pane and closes the channel,
// which lets fanOut return cleanly.
//
// DetachCh is closed exactly once (DetachOnce) when this attach is
// torn down — by the client clean-detach path, by the binary loop
// observing the conn close, or (in PR6) by a takeover request.
type attachedClient struct {
	ClientID     uint16
	Conn         net.Conn
	WriteMu      *sync.Mutex
	Subscription *subscriptionHandle
	DetachOnce   sync.Once
	DetachCh     chan struct{}
}

// subscriptionHandle bundles a pane.Subscribe() result so attachedClient
// can carry both the channel and the unsubscribe func without leaking
// the closure shape across the package.
type subscriptionHandle struct {
	Ch          <-chan terminal.Event
	Unsubscribe func()
}

// subscriberConn is a placeholder for the per-connection event-push
// state PR9 will introduce. Declared here so Session's slice type is
// stable across PRs.
type subscriberConn struct {
	ConnID uint16
}

// ErrAlreadyAttached is returned by AttemptAttach when the session
// already has an attached client and Takeover=false. PR5 always returns
// this even when Takeover=true; PR6 will land the real takeover path.
var ErrAlreadyAttached = errors.New("daemon: session already has an attached client")

// NewSession allocates a fresh Session ID, constructs the underlying
// terminal.Pane, and forks the requested command via the Pane's
// headless start path. It returns a fully running session — on error,
// the partially-initialized pane is torn down before the error is
// returned so no PTY fd leaks.
func NewSession(req SpawnReq) (*Session, error) {
	if req.Cols <= 0 {
		req.Cols = 80
	}
	if req.Rows <= 0 {
		req.Rows = 24
	}

	id, err := newSessionID()
	if err != nil {
		return nil, fmt.Errorf("daemon: generate session id: %w", err)
	}

	pane := terminal.New(req.TicketID, req.Cols, req.Rows, req.Scrollback)
	if req.Workdir != "" {
		pane.SetWorkdir(req.Workdir)
	}
	if req.SessionName != "" {
		pane.SetSessionName(req.SessionName)
	}

	if err := pane.StartHeadless(req.Command, req.Args, req.Env); err != nil {
		return nil, fmt.Errorf("daemon: start pane: %w", err)
	}

	return &Session{
		id:          id,
		ticketID:    req.TicketID,
		sessionName: req.SessionName,
		workdir:     req.Workdir,
		pane:        pane,
		startedAt:   time.Now().UTC(),
	}, nil
}

// newSessionID returns a fresh 16-character hex string drawn from
// crypto/rand. The collision probability for ~2^64 ids is negligible
// for the daemon's lifetime.
func newSessionID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ID returns the daemon-internal session identifier.
func (s *Session) ID() string { return s.id }

// TicketID returns the ticket the session was spawned for.
func (s *Session) TicketID() string { return s.ticketID }

// SessionName returns the OPENKANBAN_SESSION value the session was
// spawned with.
func (s *Session) SessionName() string { return s.sessionName }

// Pane returns the underlying terminal.Pane. Reserved for PR5/PR7,
// where the daemon's attach + snapshot paths need to talk to the
// pane directly. Not part of the wire protocol.
func (s *Session) Pane() *terminal.Pane { return s.pane }

// Running reports whether the underlying PTY child is still alive.
func (s *Session) Running() bool {
	if s == nil || s.pane == nil {
		return false
	}
	return s.pane.Running()
}

// Info returns a wire-shaped snapshot of the session's state for
// inclusion in ListResp / PrepareExitResp.
func (s *Session) Info() SessionInfo {
	s.attachMu.Lock()
	attached := uint16(0)
	if s.attached != nil {
		attached = s.attached.ClientID
	}
	s.attachMu.Unlock()

	cols, rows := s.pane.Size()
	return SessionInfo{
		SessionID:      s.id,
		TicketID:       s.ticketID,
		SessionName:    s.sessionName,
		Workdir:        s.workdir,
		Title:          s.pane.Title(),
		PID:            s.pane.PID(),
		Cols:           cols,
		Rows:           rows,
		Running:        s.pane.Running(),
		AttachedClient: attached,
		StartedAt:      s.startedAt,
	}
}

// Kill terminates the session's child process. If graceSeconds is
// positive, SIGTERM is sent first and the daemon waits up to that many
// seconds for the child to exit before sending SIGKILL. graceSeconds
// of 0 (or negative) is an immediate kill.
func (s *Session) Kill(graceSeconds int) error {
	if s == nil || s.pane == nil {
		return nil
	}
	if graceSeconds <= 0 {
		return s.pane.Stop()
	}
	return s.pane.StopGraceful(time.Duration(graceSeconds) * time.Second)
}

// AttemptAttach registers c as the session's sole attacher. Returns
// ErrAlreadyAttached if the session already has one — Takeover=true is
// not yet honored in PR5 and still returns ErrAlreadyAttached.
//
// On success it:
//   - subscribes to the pane's event stream
//   - constructs the attachedClient bookkeeping
//   - sets s.attached
//   - resizes the pane to the client's requested cols/rows
//   - returns the snapshot bytes the caller must ship before the
//     connection enters binary mode
//
// The caller (server.handleAttach) is responsible for:
//   - sending the AttachResp JSON frame
//   - writing snapshot bytes as TypePTYOutput frame(s)
//   - then reading binary frames in a loop until the conn closes or
//     a TypeDetach frame arrives
//   - calling Detach() when the binary loop returns
func (s *Session) AttemptAttach(c *clientConn, cols, rows uint16, takeover bool) (snapshot []byte, ac *attachedClient, err error) {
	if s == nil || s.pane == nil {
		return nil, nil, fmt.Errorf("daemon: AttemptAttach on nil session")
	}

	s.attachMu.Lock()
	defer s.attachMu.Unlock()

	if s.attached != nil {
		// TODO(PR6): implement Takeover. For PR5 we always refuse a
		// second attach. The takeover path will (a) signal the
		// existing client's binary loop to detach cleanly, (b) wait
		// for it to release s.attached, then (c) install the new
		// client below. Returning ErrAlreadyAttached here is the
		// conservative pre-PR6 shape — tests assert it.
		return nil, nil, ErrAlreadyAttached
	}

	// Snapshot first so the bytes describe the pre-resize grid. If we
	// resized first the cursor + cell positions would already reflect
	// the new geometry, but the cells themselves wouldn't be re-laid
	// out — better to send the existing grid and let the client see
	// the resize-driven redraw the child emits afterward.
	snap := buildSnapshotForPane(s.pane)

	// Subscribe before flipping s.attached so the fanOut goroutine
	// can't start reading from a nil channel.
	ch, unsub := s.pane.Subscribe()

	ac = &attachedClient{
		ClientID:     c.id,
		Conn:         c.conn,
		WriteMu:      &c.writeMu,
		Subscription: &subscriptionHandle{Ch: ch, Unsubscribe: unsub},
		DetachCh:     make(chan struct{}),
	}
	s.attached = ac

	// Resize after the snapshot so the child sees the new dims and
	// emits its own redraw as needed.
	if cols > 0 && rows > 0 {
		s.pane.SetSize(int(cols), int(rows))
	}

	return snap, ac, nil
}

// Detach releases the current attacher and tears down its
// subscription. Idempotent: safe to call when no client is attached.
//
// Called by the server after the binary loop returns. PR6's takeover
// path will also call Detach() on the displaced client before
// installing the new one.
func (s *Session) Detach() {
	s.attachMu.Lock()
	ac := s.attached
	s.attached = nil
	s.attachMu.Unlock()

	if ac == nil {
		return
	}

	// Close the detach channel exactly once so any goroutine still
	// blocked on it unblocks.
	ac.DetachOnce.Do(func() { close(ac.DetachCh) })

	// Cancel pane subscription. This closes the channel — fanOut's
	// range loop will exit.
	if ac.Subscription != nil && ac.Subscription.Unsubscribe != nil {
		ac.Subscription.Unsubscribe()
	}
}

// buildSnapshotForPane reaches into the pane's emulator + modal state
// to produce a SerializeRedraw-shaped byte stream. It is the only
// place the daemon couples to the pane's internals; PR7 will replace
// this with a cleaner Pane.Snapshot() accessor.
//
// Returns nil when the pane isn't ready (no emulator). Safe to call
// concurrently — SerializeRedraw only reads via SafeEmulator's locked
// methods, and the modal getters on Pane take their own locks.
func buildSnapshotForPane(p *terminal.Pane) []byte {
	// SafeEmulatorForSnapshot + the three modal getters are exported
	// on Pane in this PR. The cursor-visibility / mouse / altScreen
	// booleans are tracked exclusively on Pane (the emulator's own
	// state machine doesn't expose them), so we have to query the
	// pane rather than the emulator directly.
	vt, cursorVisible, mouseEnabled, altScreen, title := p.SnapshotState()
	if vt == nil {
		return nil
	}
	return SerializeRedraw(vt, cursorVisible, mouseEnabled, altScreen, title)
}

// logWriteFailure logs and returns errors uniformly so the binary I/O
// paths don't sprinkle ad-hoc formatting. Currently only used by the
// fan-out goroutine; binary loop logs inline.
func logWriteFailure(prefix string, clientID uint16, err error) {
	log.Printf("openkanbankd: client %d %s: %v", clientID, prefix, err)
}
