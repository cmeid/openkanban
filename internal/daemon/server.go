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
	// goroutine that pushes them to subscribed clients. PR4 only
	// emits started/exited; PR9 expands to status. Unbuffered so the
	// emitter blocks if no consumer is draining — which is fine in
	// PR4 because there is no consumer yet and only the test path
	// would observe blocking. We make a real channel so PR9 has a
	// stable target to wire into.
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
		events:   make(chan SessionEvent),
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

	// Drain the events channel so SessionEvent emitters don't block.
	// PR9 will replace this with the real fan-out goroutine.
	go s.drainEvents()

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

// drainEvents consumes the events channel until shutdown. PR4 has no
// real consumer; PR9 will replace this with a real fan-out goroutine.
func (s *Server) drainEvents() {
	for {
		select {
		case <-s.shutdown:
			return
		case ev := <-s.events:
			_ = ev
		}
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
		// Attach lands in PR5.
		s.writeError(c, "not_implemented", "attach lands in PR5")

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

	// Non-blocking emit: PR4's drainEvents is the only listener and
	// it never falls behind, but a non-blocking send keeps the spawn
	// path snappy regardless.
	select {
	case s.events <- SessionEvent{Event: "started", SessionID: sess.ID(), TicketID: sess.TicketID()}:
	default:
	}

	return SpawnResp{SessionID: sess.ID(), PID: sess.pane.PID()}, nil
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
		return KillResp{}, fmt.Errorf("session %q not found", req.SessionID)
	}

	if err := sess.Kill(req.GraceSeconds); err != nil {
		return KillResp{}, err
	}

	log.Printf("openkanbankd: client %d killed session %s", c.id, req.SessionID)

	select {
	case s.events <- SessionEvent{Event: "exited", SessionID: req.SessionID, TicketID: sess.TicketID()}:
	default:
	}

	return KillResp{}, nil
}

// handleOwns answers whether the daemon currently owns the agent
// session whose Claude / opencode UUID matches req.SessionUUID.
//
// TODO(PR10): Sessions don't currently track the inner agent's UUID;
// only the openkanban-side SessionName the pane was spawned with. PR10
// will plumb agent.UUID extraction through and complete this lookup.
// For now we conservatively report Owned=false for every request so
// callers don't latch onto a wrong session.
func (s *Server) handleOwns(c *clientConn, req OwnsReq) OwnsResp {
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
