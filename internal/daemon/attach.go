package daemon

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"github.com/techdufus/openkanban/internal/terminal"
)

// snapshotChunkSize is the maximum payload size for a single
// TypePTYOutput frame carrying snapshot bytes. The protocol-level
// MaxFrameSize is 1 MiB (including the type byte); we cap chunks well
// below that so the client's frame-read loop never blocks on a single
// outsized payload, and so the snapshot can interleave with live
// output frames as soon as they start arriving from the read pump.
//
// 64 KiB matches the PTY read buffer (terminal.readBufferSize) — once
// the snapshot is over, fanOut's per-event frames will be at most this
// large too, keeping output cadence smooth.
const snapshotChunkSize = 64 * 1024

// handleAttach drives the Attach RPC. It runs synchronously on the
// dispatcher goroutine and BLOCKS until the binary stream ends; that
// way handleConn's outer JSON read loop sees a clean conn shutdown
// (EOF or client-side close) once binary mode wraps up, and exits via
// its existing disconnect path.
//
// The function never decrements clientCount on its own — the deferred
// unregisterClient in handleConn handles that. If we tried to manage
// it here we'd double-decrement on every attach.
func (s *Server) handleAttach(c *clientConn, req AttachReq) {
	// Validate dimensions before grabbing the session — same bounds
	// as the rest of the daemon defaults.
	if req.Cols == 0 {
		req.Cols = 80
	}
	if req.Rows == 0 {
		req.Rows = 24
	}
	if req.Cols > 1024 || req.Rows > 1024 {
		s.writeError(c, "bad_request", fmt.Sprintf("dims out of range: %dx%d", req.Cols, req.Rows))
		return
	}

	log.Printf("openkanbankd: client %d handleAttach session=%s cols=%d rows=%d takeover=%v", c.id, req.SessionID, req.Cols, req.Rows, req.Takeover)

	sess, _ := s.reg.get(req.SessionID)

	if sess == nil {
		s.writeError(c, "session_not_found", fmt.Sprintf("session %q not found", req.SessionID))
		return
	}

	snapshot, ac, err := sess.AttemptAttach(c, req.Cols, req.Rows, req.Takeover)
	if err != nil {
		if errors.Is(err, ErrAlreadyAttached) {
			s.writeError(c, "already_attached", err.Error())
			return
		}
		s.writeError(c, "attach_failed", err.Error())
		return
	}

	log.Printf("openkanbankd: client %d attach approved session=%s snapshot_bytes=%d", c.id, req.SessionID, len(snapshot))

	// AttachResp BEFORE the binary frames so the client can switch
	// its read loop based on the JSON envelope.
	s.writeResp(c, MsgAttachResp, AttachResp{
		ClientID:     c.id,
		SnapshotSize: len(snapshot),
	})

	// Announce the attach to subscribed clients so sibling TUIs learn
	// the binary stream is now owned. Emitted after AttachResp so the
	// attaching client sees its response first (the response demuxer
	// is in JSON-mode; the push interleaves before the connection
	// upgrades to binary).
	s.emitEvent(SessionEvent{Event: "attached", SessionID: req.SessionID, TicketID: sess.TicketID(), LastActivityAt: sess.LastActivity()})

	// Ship the snapshot as one or more TypePTYOutput frames. We do
	// this under writeMu just like fanOut would — keeps any future
	// JSON pushes from interleaving mid-snapshot.
	if err := writeSnapshotChunks(c.conn, &c.writeMu, snapshot, c.id); err != nil {
		log.Printf("openkanbankd: client %d write snapshot: %v", c.id, err)
		sess.Detach(ac)
		return
	}

	// Spawn the fan-out goroutine. It exits when the subscription
	// channel closes (Detach) or when its conn writes fail. ac.Ch
	// is set once at construction and never mutated, so it's safe
	// to read here without holding attachMu.
	fanOutDone := make(chan struct{})
	go func() {
		defer close(fanOutDone)
		s.fanOut(ac, ac.Ch)
	}()

	// Drive the inbound binary loop on this goroutine. Blocks until
	// the client cleanly detaches, closes the conn, or sends an
	// unrecoverable frame.
	s.binaryLoop(c, sess, ac)

	// Tear down the attach. Detach is idempotent — fanOut may have
	// already pushed a TypeDetach on ExitEvent and noted the
	// subscription close, and a takeover may have already done the
	// per-session cleanup. Detach(ac) is safe in any of these cases.
	sess.Detach(ac)

	// Announce the detach to subscribed clients. Emitted unconditionally
	// — whether the detach came from a clean client TypeDetach, a
	// natural pane exit, or a takeover, the prior owner's binary stream
	// is now done. Subscribers can treat "detached" as informational
	// (the session may still be alive with a new attacher) and reconcile
	// against List/owns-by snapshots if they care.
	s.emitEvent(SessionEvent{Event: "detached", SessionID: req.SessionID, TicketID: sess.TicketID(), LastActivityAt: sess.LastActivity()})

	// Wait for the fan-out to fully exit before returning. Without
	// this, a late publish could race the conn close and log a
	// confusing write error after handleConn already shut down.
	<-fanOutDone
}

// handlePeek ships a one-shot snapshot of the session's terminal state
// WITHOUT attaching. It does not call AttemptAttach, does not resize the
// pane, does not subscribe to PTY output, and emits no attached/detached
// events — the current attacher (if any) is left completely undisturbed.
// Unlike handleAttach it does NOT block: after the snapshot frames the
// conn stays in JSON mode and dispatch returns; the client closes its
// dedicated peek conn once it has read the snapshot. Cols/Rows in the
// request are advisory only — the snapshot reflects the pane's current
// geometry (no resize, since the peeker isn't the owner).
func (s *Server) handlePeek(c *clientConn, req PeekReq) {
	sess, _ := s.reg.get(req.SessionID)

	if sess == nil {
		s.writeError(c, "session_not_found", fmt.Sprintf("session %q not found", req.SessionID))
		return
	}

	snapshot := sess.Snapshot()
	log.Printf("openkanbankd: client %d handlePeek session=%s snapshot_bytes=%d", c.id, req.SessionID, len(snapshot))

	s.writeResp(c, MsgPeekResp, PeekResp{SnapshotSize: len(snapshot)})

	if err := writeSnapshotChunks(c.conn, &c.writeMu, snapshot, c.id); err != nil {
		log.Printf("openkanbankd: client %d write peek snapshot: %v", c.id, err)
	}
}

// writeSnapshotChunks ships data over conn as one or more
// TypePTYOutput frames, each ≤ snapshotChunkSize bytes. writeMu is
// taken once per chunk so other potential conn writers (none today,
// but the JSON push path may grow into one) can interleave between
// chunks without corrupting frames.
//
// Returns the first write error encountered. A short snapshot fits
// in one frame; a multi-megabyte one chunks into ~16 frames.
func writeSnapshotChunks(conn net.Conn, writeMu lockable, data []byte, clientID uint16) error {
	if len(data) == 0 {
		return nil
	}
	for off := 0; off < len(data); off += snapshotChunkSize {
		end := off + snapshotChunkSize
		if end > len(data) {
			end = len(data)
		}
		writeMu.Lock()
		err := WriteFrame(conn, TypePTYOutput, data[off:end])
		writeMu.Unlock()
		if err != nil {
			return fmt.Errorf("snapshot chunk %d-%d (client %d): %w", off, end, clientID, err)
		}
	}
	log.Printf("openkanbankd: client %d snapshot sent session=%s bytes=%d", clientID, "n/a", len(data))
	return nil
}

// lockable narrows the *sync.Mutex shape we expect for serialized
// conn writes. Defined as a tiny interface so writeSnapshotChunks can
// accept either *sync.Mutex (the real callers) or a stub in tests.
type lockable interface {
	Lock()
	Unlock()
}

// fanOut pumps pane events to the client conn as binary frames. It
// runs until the subscription channel closes (i.e. the pane was
// unsubscribed via Detach, or the pane published its final ExitEvent
// and closed all subscribers).
//
// On ExitEvent it emits a TypeDetach so the client learns the child
// is gone and can release its end of the binary stream. fanOut does
// not call Detach itself — that's the caller (handleAttach) — but it
// will close ac.DetachCh on ExitEvent so any goroutine waiting on the
// channel (e.g. PR6's takeover path) wakes up.
func (s *Server) fanOut(ac *attachedClient, ch <-chan terminal.Event) {
	if ac == nil || ch == nil {
		return
	}

	for ev := range ch {
		switch e := ev.(type) {
		case terminal.OutputEvent:
			if len(e.Data) == 0 {
				continue
			}
			ac.WriteMu.Lock()
			err := WriteFrame(ac.Conn, TypePTYOutput, e.Data)
			ac.WriteMu.Unlock()
			if err != nil {
				logWriteFailure("fan-out write", ac.ClientID, err)
				return
			}
		case terminal.ExitEvent:
			// Notify the client. Best effort — the conn may already
			// be torn down on this side. After sending we still wait
			// for the channel close (range loop exit) below.
			ac.WriteMu.Lock()
			_ = WriteFrame(ac.Conn, TypeDetach, nil)
			ac.WriteMu.Unlock()
			ac.DetachOnce.Do(func() { close(ac.DetachCh) })
		case terminal.TitleEvent, terminal.ModeEvent:
			// PR5 does not propagate Title/Mode through the binary
			// stream — those flips are recoverable by the child's
			// own emitted escape sequences, which fanOut already
			// relays as OutputEvent bytes. PR9 will revisit if we
			// want server-pushed mode notifications for non-attached
			// subscribers.
			continue
		}
	}
}

// binaryLoop reads inbound frames from the attached client and
// dispatches them to the pane. Exits on:
//   - clean detach (TypeDetach frame)
//   - conn EOF / closed
//   - read error of any other kind
//   - DetachCh closed externally (takeover or pane Exit)
//
// On an externally-driven detach (DetachCh close), the watcher
// goroutine writes a TypeDetach frame to the client (so the client
// knows binary mode is over) and interrupts the blocked ReadFrame via
// SetReadDeadline. We do NOT close the conn — the displaced client's
// conn remains open so handleConn's outer JSON-read loop can resume.
// After binaryLoop returns we clear the read deadline so that resumed
// JSON read isn't immediately killed by a stale past-deadline.
//
// Note we do NOT take any per-session lock here; the pane's own
// WriteInput / SetSize are concurrent-safe.
func (s *Server) binaryLoop(c *clientConn, sess *Session, ac *attachedClient) {
	// Watcher goroutine: when DetachCh closes (PR6 takeover or pane
	// Exit), we (a) push a TypeDetach frame to the client so it
	// learns binary mode is over, then (b) interrupt the blocked
	// ReadFrame by setting an immediate read deadline. The conn
	// stays alive — only the binary read loop ends.
	stopWatcher := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ac.DetachCh:
			// fanOut may have already emitted a TypeDetach on
			// ExitEvent — duplicate frames are fine for the client
			// (it stops reading binary anyway) and we explicitly
			// ignore the write error here since the conn may have
			// already gone away on the other side.
			ac.WriteMu.Lock()
			_ = WriteFrame(ac.Conn, TypeDetach, nil)
			ac.WriteMu.Unlock()
			// Trip the read deadline so the next ReadFrame returns.
			// Use a sentinel past time so the ongoing read fails
			// immediately regardless of clock skew.
			_ = c.conn.SetReadDeadline(time.Unix(1, 0))
		case <-stopWatcher:
		}
	}()
	defer func() {
		close(stopWatcher)
		// Wait for the watcher to finish so its SetReadDeadline call
		// can't race the deadline-clear below. Otherwise a delayed
		// watcher firing after binaryLoop returns would set a stale
		// deadline that handleConn's JSON read loop would inherit.
		<-watcherDone
		// Clear any deadline the watcher set so handleConn's resumed
		// JSON read loop on the same conn works normally.
		_ = c.conn.SetReadDeadline(time.Time{})
	}()

	for {
		typ, payload, err := ReadFrame(c.r)
		if err != nil {
			if err == io.EOF || errors.Is(err, net.ErrClosed) {
				return
			}
			// Deadline errors during an external-detach interruption
			// are expected: the watcher trips the deadline on
			// purpose to break the blocked read. Don't log as a
			// failure in that case.
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				select {
				case <-ac.DetachCh:
					return
				default:
				}
			}
			log.Printf("openkanbankd: client %d binary read: %v", c.id, err)
			return
		}

		switch typ {
		case TypePTYInput:
			if len(payload) == 0 {
				continue
			}
			if _, werr := sess.pane.WriteInput(payload); werr != nil {
				if errors.Is(werr, terminal.ErrInputBackpressure) {
					// The child has stopped draining stdin and the
					// pane's bounded input buffer is full. Drop this
					// chunk but keep the client attached — backpressure
					// (transient or sustained) must not detach the user.
					// The watchdog (broadcastActivity) surfaces a
					// persistently-wedged session as "stuck" so the user
					// can recover or destroy it from the TUI.
					continue
				}
				if errors.Is(werr, terminal.ErrPaneNotRunning) {
					// Child gone — emit detach and bail out so the
					// client knows the session is no longer
					// accepting input.
					ac.WriteMu.Lock()
					_ = WriteFrame(c.conn, TypeDetach, nil)
					ac.WriteMu.Unlock()
					return
				}
				log.Printf("openkanbankd: client %d WriteInput: %v", c.id, werr)
				return
			}
		case TypeResize:
			cols, rows, _, derr := DecodeResize(payload)
			if derr != nil {
				log.Printf("openkanbankd: client %d bad resize payload: %v", c.id, derr)
				continue
			}
			if cols == 0 || rows == 0 {
				continue
			}
			if cols > 1024 || rows > 1024 {
				log.Printf("openkanbankd: client %d resize out of range %dx%d", c.id, cols, rows)
				continue
			}
			sess.pane.SetSize(int(cols), int(rows))
		case TypeDetach:
			// Client-initiated clean detach. Return so handleAttach
			// proceeds to its post-loop cleanup.
			return
		default:
			// Unknown frame type during binary mode: log and ignore
			// rather than tear down — the client may be a slightly
			// newer version that knows a new frame type.
			log.Printf("openkanbankd: client %d unexpected binary frame 0x%02x", c.id, typ)
		}
	}
}
