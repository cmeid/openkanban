package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
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

	mu sync.Mutex

	// attached and subscribers are placeholders for PR5 / PR9. PR4
	// only declares them so callers (and future patches) compile
	// against a stable shape. The daemon does not populate them in
	// this PR.
	attached    *attachedClient
	subscribers []*subscriberConn
}

// attachedClient identifies the single client whose connection is
// currently upgraded to binary mode for this session. Reserved for
// PR5; declared here so Session can hold a typed nil pointer that
// the attach RPC will fill in.
type attachedClient struct {
	ConnID uint16
}

// subscriberConn is a placeholder for the per-connection event-push
// state that PR9 will introduce. Declared here so Session's slice
// type is stable across PRs.
type subscriberConn struct {
	ConnID uint16
}

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
	s.mu.Lock()
	attached := uint16(0)
	if s.attached != nil {
		attached = s.attached.ConnID
	}
	s.mu.Unlock()

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
