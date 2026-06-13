package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"

	"github.com/techdufus/openkanban/internal/terminal"
)

// BinaryVersion is the daemon's reported binary version in HelloResp.
// Overridden at build time (or from tests) so version skew can be
// surfaced without recompiling. Defaults to "dev" for unstamped builds.
var BinaryVersion = "dev"

// shutdownGraceSeconds is the SIGTERM-to-SIGKILL grace window used when
// the daemon's defensive kill path tears down sessions that survived
// past the last client disconnect.
const shutdownGraceSeconds = 3

// Server is the openkanbankd RPC server. It listens on a Unix socket,
// accepts client connections, multiplexes JSON-mode RPCs, and owns the
// set of live terminal.Pane-backed Sessions.
//
// The server enforces last-client-shutdown semantics: when the final
// client disconnects (the connection count under clientsMu drops to
// zero), it tears down any remaining sessions defensively and exits.
// This is deliberate — the daemon must NOT outlive the last TUI.
type Server struct {
	sock    string
	pidlock *PidLock
	ln      net.Listener

	sessionsMu sync.RWMutex
	sessions   map[string]*Session

	clientsMu    sync.Mutex
	clients      map[uint16]*clientConn
	nextClientID uint16

	shutdown     chan struct{}
	shutdownOnce sync.Once

	wg sync.WaitGroup

	// events fans daemon-internal SessionEvent updates out to the
	// goroutine that pushes them to subscribed clients (PR9's
	// broadcastEvents). Buffered so the emit-sites (handleSpawn,
	// handleKill, attach, the pane-exit watcher) don't block on
	// transient broadcaster slowness — emitEvent does a non-blocking
	// send and drops with a log if the buffer fills, since the next
	// client reconcile (List / status poll) will repair any missed
	// transition.
	events chan SessionEvent
}

// clientConn tracks one open connection's per-client state.
type clientConn struct {
	id         uint16
	conn       net.Conn
	r          *bufio.Reader
	subscribed bool

	// writeMu serializes WriteFrame calls so the JSON response,
	// push, and (PR5) PTY-output frames produced by separate
	// goroutines don't interleave on the wire.
	writeMu sync.Mutex
}

// SessionEvent is the daemon-internal event type fanned out via
// Server.events. The Type field mirrors the wire constant; consumers
// translate to a protocol.SessionEvent before pushing to clients.
//
// Exported so the (eventual) PR9 fan-out code can live in a separate
// file in this package without poking at private fields.

// NewServer acquires the pidlock, listens on the socket, and returns a
// ready-but-not-yet-running Server. Call Serve to begin accepting
// connections.
//
// If another daemon already holds the pidlock the function returns the
// underlying *ErrAlreadyLocked so the caller can format a clean
// "already running with pid N" message.
func NewServer(sock, pidpath string) (*Server, error) {
	lock, err := AcquirePidLock(pidpath)
	if err != nil {
		return nil, err
	}

	// Remove any stale socket file left over from a crash. The
	// pidlock above guarantees no other daemon is currently bound
	// to it, so this is safe.
	if _, statErr := os.Stat(sock); statErr == nil {
		if rmErr := os.Remove(sock); rmErr != nil {
			lock.Release()
			return nil, fmt.Errorf("daemon: remove stale socket %s: %w", sock, rmErr)
		}
	}

	ln, err := net.Listen("unix", sock)
	if err != nil {
		lock.Release()
		return nil, fmt.Errorf("daemon: listen %s: %w", sock, err)
	}
	if chmodErr := os.Chmod(sock, 0o600); chmodErr != nil {
		ln.Close()
		lock.Release()
		return nil, fmt.Errorf("daemon: chmod socket: %w", chmodErr)
	}

	return &Server{
		sock:     sock,
		pidlock:  lock,
		ln:       ln,
		sessions: make(map[string]*Session),
		clients:  make(map[uint16]*clientConn),
		shutdown: make(chan struct{}),
		events:   make(chan SessionEvent, 64),
	}, nil
}

// SocketPath returns the absolute path the server is listening on.
// Useful in tests for verifying the listener bound to the expected
// location.
func (s *Server) SocketPath() string { return s.sock }

// Serve runs the accept loop until ctx is cancelled, the shutdown
// channel is closed (via initiateShutdown), or a fatal accept error
// occurs. On return the listener is closed, the pidfile is released,
// and any remaining sessions have been torn down. Safe to call once.
func (s *Server) Serve(ctx context.Context) error {
	log.Printf("openkanbankd: listening on %s (pid %d)", s.sock, os.Getpid())

	// Watch ctx in a goroutine that triggers the same shutdown path
	// the last-client-disconnect handler uses, so both initiations
	// converge on identical cleanup.
	go func() {
		select {
		case <-ctx.Done():
			s.initiateShutdown("context cancelled")
		case <-s.shutdown:
		}
	}()

	// Fan SessionEvent emissions out to subscribed clients. The
	// goroutine outlives every individual client conn and exits when
	// the shutdown channel closes.
	go s.broadcastEvents()

	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.shutdown:
				// Listener closed by our own shutdown path —
				// not an error. Wait for in-flight connections
				// and exit cleanly.
				s.wg.Wait()
				s.cleanup()
				return nil
			default:
			}

			if errors.Is(err, net.ErrClosed) {
				s.wg.Wait()
				s.cleanup()
				return nil
			}
			log.Printf("openkanbankd: accept error: %v", err)
			s.wg.Wait()
			s.cleanup()
			return fmt.Errorf("daemon: accept: %w", err)
		}

		c := s.registerClient(conn)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(c)
		}()
	}
}

// broadcastEvents consumes s.events and pushes each SessionEvent to
// every currently-subscribed client as a TypeJSONPush frame. Exits
// when s.shutdown closes.
//
// Per-client writes are serialized under c.writeMu so push frames don't
// interleave with concurrent JSON responses or PTY-output binary frames
// produced by attach.go's fanOut. If a write fails (broken conn, etc.)
// the client is logged and skipped — broadcastEvents must never block
// on a single bad client, otherwise an emit-site (handleSpawn / Kill /
// attach) would back up behind it.
func (s *Server) broadcastEvents() {
	for {
		select {
		case <-s.shutdown:
			return
		case ev := <-s.events:
			s.dispatchSessionEvent(ev)
		}
	}
}

// dispatchSessionEvent encodes ev as MsgSessionEvent inside a
// TypeJSONPush envelope and writes it to every currently-subscribed
// client. Snapshots the subscriber set under clientsMu so per-client
// writes happen without holding the global lock.
func (s *Server) dispatchSessionEvent(ev SessionEvent) {
	payload, err := EncodeMsg(MsgSessionEvent, ev)
	if err != nil {
		log.Printf("openkanbankd: encode session event %q: %v", ev.Event, err)
		return
	}

	s.clientsMu.Lock()
	subs := make([]*clientConn, 0, len(s.clients))
	for _, c := range s.clients {
		if c.subscribed {
			subs = append(subs, c)
		}
	}
	s.clientsMu.Unlock()

	for _, c := range subs {
		c.writeMu.Lock()
		werr := WriteFrame(c.conn, TypeJSONPush, payload)
		c.writeMu.Unlock()
		if werr != nil {
			log.Printf("openkanbankd: client %d push session event %q: %v", c.id, ev.Event, werr)
		}
	}
}

// emitEvent does a non-blocking send to s.events. The broadcaster
// goroutine should always be draining the channel, but a non-blocking
// send guards against the broadcaster being momentarily slow (e.g. one
// subscriber's writeMu is held for an unusual length of time). Dropping
// an event is preferable to deadlocking the emit-site, since the next
// List / status poll will reconcile the missed transition.
func (s *Server) emitEvent(ev SessionEvent) {
	select {
	case s.events <- ev:
	default:
		log.Printf("openkanbankd: dropped session event %q for session %s (broadcaster busy)", ev.Event, ev.SessionID)
	}
}

// initiateShutdown closes the shutdown channel exactly once and the
// listener so the accept loop returns. Safe to call from any goroutine.
func (s *Server) initiateShutdown(reason string) {
	s.shutdownOnce.Do(func() {
		log.Printf("openkanbankd: shutdown initiated (%s)", reason)
		close(s.shutdown)
		if s.ln != nil {
			s.ln.Close()
		}
	})
}

// cleanup tears down any remaining sessions, releases the pidlock,
// removes the socket file, and is safe to call multiple times. Called
// once from Serve as the final step before returning.
func (s *Server) cleanup() {
	s.sessionsMu.Lock()
	live := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		live = append(live, sess)
	}
	s.sessions = map[string]*Session{}
	s.sessionsMu.Unlock()

	for _, sess := range live {
		log.Printf("openkanbankd: shutdown-cleanup killing session %s (ticket=%s)", sess.ID(), sess.TicketID())
		if err := sess.Kill(shutdownGraceSeconds); err != nil {
			log.Printf("openkanbankd: kill session %s: %v", sess.ID(), err)
		}
	}

	if s.pidlock != nil {
		s.pidlock.Release()
		s.pidlock = nil
	}
	// Best-effort socket removal: net.Listen("unix") doesn't unlink
	// the file when the listener is closed.
	_ = os.Remove(s.sock)
}

// registerClient assigns a fresh ClientID and inserts the conn into
// the clients map under clientsMu.
func (s *Server) registerClient(conn net.Conn) *clientConn {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()

	s.nextClientID++
	id := s.nextClientID
	c := &clientConn{
		id:   id,
		conn: conn,
		r:    bufio.NewReader(conn),
	}
	s.clients[id] = c
	log.Printf("openkanbankd: client %d connected (total=%d)", id, len(s.clients))
	return c
}

// unregisterClient removes c from the clients map. Returns the
// post-removal client count so the caller can decide whether to
// trigger last-client shutdown.
func (s *Server) unregisterClient(c *clientConn) int {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	delete(s.clients, c.id)
	return len(s.clients)
}

// handleConn drives the read loop for one client connection. It
// dispatches each JSON-mode frame to its per-message handler and exits
// when the client disconnects or sends a fatal protocol error.
//
// On exit it unregisters the client and — if it was the last — kicks
// off the daemon shutdown path.
func (s *Server) handleConn(c *clientConn) {
	defer func() {
		c.conn.Close()
		remaining := s.unregisterClient(c)
		log.Printf("openkanbankd: client %d disconnected (remaining=%d)", c.id, remaining)
		if remaining == 0 {
			s.handleLastClientDisconnect()
		}
	}()

	for {
		typ, payload, err := ReadFrame(c.r)
		if err != nil {
			if err == io.EOF || errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("openkanbankd: client %d read frame: %v", c.id, err)
			return
		}

		if typ != TypeJSONReq {
			// PR4 only accepts JSON requests. Binary mode is the
			// PR5 attach upgrade; receiving e.g. TypePTYInput
			// before Attach is a protocol error.
			s.writeError(c, "protocol_error", fmt.Sprintf("unexpected frame type 0x%02x", typ))
			return
		}

		typeName, raw, err := DecodeEnvelope(payload)
		if err != nil {
			log.Printf("openkanbankd: client %d decode envelope: %v", c.id, err)
			s.writeError(c, "bad_envelope", err.Error())
			continue
		}

		s.dispatch(c, typeName, raw)
	}
}

// dispatch unmarshals raw into the concrete message type and invokes
// the corresponding handler. Unknown message types produce an
// ErrorResp but do not close the connection.
func (s *Server) dispatch(c *clientConn, typeName string, raw json.RawMessage) {
	switch typeName {
	case MsgHelloReq:
		var req HelloReq
		if err := json.Unmarshal(raw, &req); err != nil {
			s.writeError(c, "bad_request", err.Error())
			return
		}
		s.writeResp(c, MsgHelloResp, s.handleHello(c, req))

	case MsgSpawnReq:
		var req SpawnReq
		if err := json.Unmarshal(raw, &req); err != nil {
			s.writeError(c, "bad_request", err.Error())
			return
		}
		resp, err := s.handleSpawn(c, req)
		if err != nil {
			s.writeError(c, "spawn_failed", err.Error())
			return
		}
		s.writeResp(c, MsgSpawnResp, resp)

	case MsgListReq:
		var req ListReq
		_ = json.Unmarshal(raw, &req)
		s.writeResp(c, MsgListResp, s.handleList(c, req))

	case MsgKillReq:
		var req KillReq
		if err := json.Unmarshal(raw, &req); err != nil {
			s.writeError(c, "bad_request", err.Error())
			return
		}
		resp, err := s.handleKill(c, req)
		if err != nil {
			s.writeError(c, "kill_failed", err.Error())
			return
		}
		s.writeResp(c, MsgKillResp, resp)

	case MsgTicketDoneReq:
		var req TicketDoneReq
		if err := json.Unmarshal(raw, &req); err != nil {
			s.writeError(c, "bad_request", err.Error())
			return
		}
		resp, err := s.handleTicketDone(c, req)
		if err != nil {
			s.writeError(c, "ticket_done_failed", err.Error())
			return
		}
		s.writeResp(c, MsgTicketDoneResp, resp)

	case MsgOwnsReq:
		var req OwnsReq
		if err := json.Unmarshal(raw, &req); err != nil {
			s.writeError(c, "bad_request", err.Error())
			return
		}
		s.writeResp(c, MsgOwnsResp, s.handleOwns(c, req))

	case MsgSubscribeReq:
		var req SubscribeReq
		_ = json.Unmarshal(raw, &req)
		s.writeResp(c, MsgSubscribeResp, s.handleSubscribe(c, req))

	case MsgPrepareExitReq:
		var req PrepareExitReq
		_ = json.Unmarshal(raw, &req)
		s.writeResp(c, MsgPrepareExitResp, s.handlePrepareExit(c, req))

	case MsgShutdownReq:
		var req ShutdownReq
		_ = json.Unmarshal(raw, &req)
		s.writeResp(c, MsgShutdownResp, s.handleShutdown(c, req))

	case MsgAttachReq:
		var req AttachReq
		if err := json.Unmarshal(raw, &req); err != nil {
			s.writeError(c, "bad_request", err.Error())
			return
		}
		// handleAttach BLOCKS for the lifetime of the binary stream.
		// When it returns the conn is fully drained / closed; the
		// outer handleConn loop will hit EOF on its next ReadFrame
		// and exit through the usual disconnect path.
		s.handleAttach(c, req)

	default:
		s.writeError(c, "unknown_message", fmt.Sprintf("unknown message type %q", typeName))
	}
}

// --- Per-message handlers ---

func (s *Server) handleHello(c *clientConn, req HelloReq) HelloResp {
	s.clientsMu.Lock()
	count := len(s.clients)
	s.clientsMu.Unlock()

	return HelloResp{
		ProtocolVersion: ProtocolVersion,
		BinaryVersion:   BinaryVersion,
		ClientCount:     count,
		ClientID:        c.id,
	}
}

func (s *Server) handleSpawn(c *clientConn, req SpawnReq) (SpawnResp, error) {
	sess, err := NewSession(req)
	if err != nil {
		return SpawnResp{}, err
	}

	s.sessionsMu.Lock()
	s.sessions[sess.ID()] = sess
	s.sessionsMu.Unlock()

	log.Printf("openkanbankd: client %d spawned session %s (ticket=%s pid=%d)", c.id, sess.ID(), sess.TicketID(), sess.pane.PID())

	// Wire pane-exit observation BEFORE announcing the spawn so we
	// can't miss an ExitEvent fired by a child that races out the
	// gate. The watcher emits an "exited" SessionEvent when the pane
	// publishes its final ExitEvent.
	s.watchSessionExit(sess)

	s.emitEvent(SessionEvent{Event: "started", SessionID: sess.ID(), TicketID: sess.TicketID(), Status: "working"})

	return SpawnResp{SessionID: sess.ID(), PID: sess.pane.PID()}, nil
}

// watchSessionExit subscribes to sess.pane's event stream and emits an
// "exited" SessionEvent when the pane publishes its final ExitEvent
// (i.e. the underlying child process closed its PTY). Idempotent for
// the daemon broadcast: if handleKill already emitted "exited",
// subscribers will see both — fine. If sess is removed from
// s.sessions before the exit fires, we still emit so cross-instance
// observers learn the child is gone.
//
// The Subscribe registers a fresh subscriber dedicated to lifecycle
// observation; its OutputEvent/Title/Mode events are discarded.
//
// NOTE: the watcher goroutine is intentionally NOT tracked on s.wg.
// s.wg is the per-client-conn lifecycle group that Serve waits on
// before tearing down sessions in cleanup(). If we added the watcher
// to s.wg we'd deadlock at shutdown: Serve.Wait would block on the
// watcher → the watcher would block on its pane subscription channel
// → the channel only closes once cleanup() kills the session, which
// comes AFTER Wait. The watcher is fundamentally tied to the pane
// lifetime, not the connection lifetime; letting it run to completion
// in the background after Serve returns is correct.
func (s *Server) watchSessionExit(sess *Session) {
	if sess == nil || sess.pane == nil {
		return
	}
	ch, unsub := sess.pane.Subscribe()
	go func() {
		defer unsub()
		sessID := sess.ID()
		ticketID := sess.TicketID()
		emit := func() {
			expected := sess.ExpectedCompletion()
			reason := "natural_exit"
			if expected {
				reason = "ticket_done"
			}
			s.emitEvent(SessionEvent{
				Event:     "exited",
				SessionID: sessID,
				TicketID:  ticketID,
				Expected:  expected,
				Reason:    reason,
			})
		}
		for ev := range ch {
			if _, ok := ev.(terminal.ExitEvent); ok {
				emit()
				return
			}
		}
		// Channel closed without ExitEvent (e.g. Stop tore the loop
		// down before the read returned). Emit anyway so subscribers
		// learn the session is gone.
		emit()
	}()
}

func (s *Server) handleList(c *clientConn, req ListReq) ListResp {
	s.sessionsMu.RLock()
	defer s.sessionsMu.RUnlock()

	infos := make([]SessionInfo, 0, len(s.sessions))
	for _, sess := range s.sessions {
		infos = append(infos, sess.Info())
	}
	return ListResp{Sessions: infos}
}

func (s *Server) handleKill(c *clientConn, req KillReq) (KillResp, error) {
	s.sessionsMu.Lock()
	sess, ok := s.sessions[req.SessionID]
	if ok {
		delete(s.sessions, req.SessionID)
	}
	s.sessionsMu.Unlock()

	if !ok {
		// Idempotent: a concurrent Kill / TicketDone / delete path may
		// have already removed the session. Return success rather than
		// an error so callers don't toast "session not found" for what
		// is functionally a no-op.
		log.Printf("openkanbankd: client %d kill on unknown session %s (no-op)", c.id, req.SessionID)
		return KillResp{}, nil
	}

	if err := sess.Kill(req.GraceSeconds); err != nil {
		return KillResp{}, err
	}

	log.Printf("openkanbankd: client %d killed session %s", c.id, req.SessionID)

	// watchSessionExit emits the "exited" SessionEvent once the pane
	// publishes its final ExitEvent; emitting again here would result
	// in subscribers seeing two "exited" frames for one death. The Kill
	// path inherits whatever Expected/Reason the watcher decides (false
	// / "natural_exit" by default, true / "ticket_done" if the
	// TicketDone path got there first).

	return KillResp{}, nil
}

// handleTicketDone is the load-bearing handler for `openkanban ticket
// done`. It scans the live sessions for one bound to req.TicketID; if
// found, it flips that session's expected-completion flag, removes it
// from the registry, and kicks off the kill in a goroutine. The
// resulting "exited" SessionEvent (emitted by watchSessionExit when the
// pane publishes ExitEvent) carries Expected=true / Reason="ticket_done"
// so subscribers preserve AgentCompleted instead of resetting to
// AgentNone.
//
// Returns synchronously: Killed:true plus the daemon-internal SessionID
// on hit; Killed:false (no error) on miss. The CLI treats the miss as
// informational — the .md and status-file writes are authoritative.
func (s *Server) handleTicketDone(c *clientConn, req TicketDoneReq) (TicketDoneResp, error) {
	if req.TicketID == "" {
		return TicketDoneResp{}, nil
	}

	s.sessionsMu.Lock()
	var match *Session
	for _, sess := range s.sessions {
		if sess.TicketID() == req.TicketID {
			match = sess
			break
		}
	}
	if match != nil {
		delete(s.sessions, match.ID())
	}
	s.sessionsMu.Unlock()

	if match == nil {
		return TicketDoneResp{Killed: false}, nil
	}

	match.MarkExpectedCompletion()

	log.Printf("openkanbankd: client %d ticket-done session %s (ticket=%s)", c.id, match.ID(), req.TicketID)

	// Kill in a goroutine so the RPC returns synchronously. The grace
	// window matches shutdownGraceSeconds — agents may have a few
	// seconds of cleanup. The watcher emits the "exited" event when
	// the pane's ExitEvent lands.
	go func(sess *Session) {
		if err := sess.Kill(shutdownGraceSeconds); err != nil {
			log.Printf("openkanbankd: ticket-done kill session %s: %v", sess.ID(), err)
		}
	}(match)

	return TicketDoneResp{SessionID: match.ID(), Killed: true}, nil
}

// handleOwns answers whether the daemon currently owns the agent
// session whose Claude / opencode UUID matches req.SessionUUID.
//
// Sessions record their agent UUID at spawn time
// (SpawnReq.AgentSessionUUID → Session.agentSessionUUID). We walk
// the live sessions under sessionsMu.RLock and return the first match.
// Empty UUIDs never match: a Spawn made without --session carries
// AgentSessionUUID="" and an Owns query with SessionUUID="" is
// ill-formed and reported as Owned=false.
func (s *Server) handleOwns(c *clientConn, req OwnsReq) OwnsResp {
	if req.SessionUUID == "" {
		return OwnsResp{Owned: false}
	}
	s.sessionsMu.RLock()
	defer s.sessionsMu.RUnlock()
	for _, sess := range s.sessions {
		if sess.AgentSessionUUID() == req.SessionUUID {
			return OwnsResp{Owned: true, SessionID: sess.ID()}
		}
	}
	return OwnsResp{Owned: false}
}

func (s *Server) handleSubscribe(c *clientConn, req SubscribeReq) SubscribeResp {
	s.clientsMu.Lock()
	c.subscribed = true
	s.clientsMu.Unlock()
	return SubscribeResp{}
}

func (s *Server) handlePrepareExit(c *clientConn, req PrepareExitReq) PrepareExitResp {
	s.clientsMu.Lock()
	count := len(s.clients)
	s.clientsMu.Unlock()

	s.sessionsMu.RLock()
	infos := make([]SessionInfo, 0, len(s.sessions))
	for _, sess := range s.sessions {
		infos = append(infos, sess.Info())
	}
	s.sessionsMu.RUnlock()

	return PrepareExitResp{ClientCount: count, Sessions: infos}
}

func (s *Server) handleShutdown(c *clientConn, req ShutdownReq) ShutdownResp {
	s.sessionsMu.Lock()
	live := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		live = append(live, sess)
	}
	s.sessions = map[string]*Session{}
	s.sessionsMu.Unlock()

	killed := 0
	for _, sess := range live {
		if err := sess.Kill(shutdownGraceSeconds); err != nil {
			log.Printf("openkanbankd: shutdown kill session %s: %v", sess.ID(), err)
			continue
		}
		killed++
	}

	log.Printf("openkanbankd: client %d requested shutdown (killed=%d, force=%v)", c.id, killed, req.Force)

	// Initiate shutdown after we've responded to the client. We do
	// this from a goroutine so the writeResp below completes first.
	go s.initiateShutdown(fmt.Sprintf("client %d requested", c.id))

	return ShutdownResp{KilledSessions: killed}
}

// handleLastClientDisconnect is invoked when the clients map drops to
// zero. If sessions are still alive at that moment the exit-guard in
// the TUI failed; we log loudly and kill them defensively before
// shutting the daemon down.
func (s *Server) handleLastClientDisconnect() {
	s.sessionsMu.RLock()
	live := len(s.sessions)
	s.sessionsMu.RUnlock()

	if live > 0 {
		log.Printf("WARN: last client disconnected with %d live sessions; exit-guard was bypassed; terminating sessions", live)
	} else {
		log.Printf("openkanbankd: last client disconnected; shutting down")
	}

	s.initiateShutdown("last client disconnected")
}

// --- Wire helpers ---

// writeResp encodes resp as a TypeJSONResp envelope and writes it to
// the connection under c.writeMu. Errors are logged but not returned;
// a failed write closes the connection at the next ReadFrame attempt.
func (s *Server) writeResp(c *clientConn, typeName string, resp any) {
	raw, err := EncodeMsg(typeName, resp)
	if err != nil {
		log.Printf("openkanbankd: client %d encode %s: %v", c.id, typeName, err)
		return
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := WriteFrame(c.conn, TypeJSONResp, raw); err != nil {
		log.Printf("openkanbankd: client %d write %s: %v", c.id, typeName, err)
	}
}

// writeError sends an ErrorResp envelope to the client.
func (s *Server) writeError(c *clientConn, code, message string) {
	s.writeResp(c, MsgErrorResp, ErrorResp{Code: code, Message: message})
}
