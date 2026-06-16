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
	"os/signal"
	"runtime"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/techdufus/openkanban/internal/terminal"
	"github.com/techdufus/openkanban/internal/update"
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
// Default mode enforces last-client-shutdown semantics: when the final
// client disconnects (the connection count under clientsMu drops to
// zero), it tears down any remaining sessions defensively and exits.
// This is deliberate — by default the daemon must NOT outlive the
// last TUI.
//
// Persistent mode (Options.Persistent, set by `openkanban daemon
// --persistent`) inverts that: the daemon stays alive when the last
// client disconnects and only exits via explicit ShutdownReq, SIGTERM,
// or a stale-binary self-restart. This is the mode used by launchd /
// systemd to own the daemon's lifecycle independently of any TUI.
type Server struct {
	sock       string
	pidlock    *PidLock
	ln         net.Listener
	persistent bool

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

	// pendingRestart is flipped to true by watchBinaryStaleness when
	// it first observes update.BinaryStale() == true (i.e. the daemon
	// binary on disk has been replaced under us by go install /
	// openkanban update). Once set:
	//   - if zero sessions are attached at that moment, the watcher
	//     initiates immediate shutdown so the next TUI launch picks
	//     up the new binary;
	//   - otherwise the daemon keeps running but logs a loud warning,
	//     and the existing last-client-disconnect path handles the
	//     exit naturally once sessions wind down.
	// Read/written only from the watchBinaryStaleness goroutine and
	// handleLastClientDisconnect; both already hold the relevant
	// per-call mutexes for the data they care about, and the flag
	// itself is byte-sized — protected by stalenessMu to keep go's
	// race detector quiet across the two goroutines.
	stalenessMu    sync.Mutex
	pendingRestart bool

	// emitSessionExitFn is the seam watchSessionExit uses to publish
	// the "exited" SessionEvent. Production leaves this nil and the
	// goroutine falls back to s.emitEvent. Tests inject a panicking
	// override via setEmitSessionExitFnForTest to verify the
	// goroutine's panic-recovery + invariant-preserving cleanup.
	// Only read once at goroutine start; no mutex needed because
	// tests set it before triggering the event.
	emitSessionExitFn func(SessionEvent)
}

// clientConn tracks one open connection's per-client state.
type clientConn struct {
	id         uint16
	conn       net.Conn
	r          *bufio.Reader
	subscribed bool
	// name is the ClientName the client announced in HelloReq
	// ("openkanban-tui", "openkanban-cli", etc.). Used by
	// handlePrepareExit to answer "is another TUI watching?" Reads
	// and writes are serialized via s.clientsMu, matching the
	// subscribed-field precedent above.
	name string

	// exiting marks this client as having called PrepareExit. Read and
	// written only inside clientsMu. Used by handlePrepareExit's count
	// so the calling client and any peer that has also called
	// PrepareExit are excluded from OtherActiveClients — that is what
	// makes "am I the last one out?" race-free across simultaneous
	// closes. handleCancelExit clears it when the user backs out of
	// the exit modal. No reaper needed: unregisterClient removes the
	// whole row so a disconnected client's flag goes with it.
	exiting bool

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

// Options configures non-default Server behavior. Zero-value is the
// default (TUI-managed, last-client-shutdown semantics). Persistent
// flips the lifecycle to "stay alive across disconnects" — used by
// launchd / systemd integration.
type Options struct {
	Persistent bool
}

// NewServer acquires the pidlock, listens on the socket, and returns a
// ready-but-not-yet-running Server with default options. Call Serve to
// begin accepting connections.
//
// If another daemon already holds the pidlock the function returns the
// underlying *ErrAlreadyLocked so the caller can format a clean
// "already running with pid N" message.
func NewServer(sock, pidpath string) (*Server, error) {
	return NewServerWithOptions(sock, pidpath, Options{})
}

// NewServerWithOptions is NewServer with non-default Options. Kept as
// a sibling rather than expanding NewServer's signature so the (sock,
// pidpath) 2-arg shape stays stable for the many existing call sites.
func NewServerWithOptions(sock, pidpath string, opts Options) (*Server, error) {
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
		sock:       sock,
		pidlock:    lock,
		ln:         ln,
		persistent: opts.Persistent,
		sessions:   make(map[string]*Session),
		clients:    make(map[uint16]*clientConn),
		shutdown:   make(chan struct{}),
		events:     make(chan SessionEvent, 64),
	}, nil
}

// SocketPath returns the absolute path the server is listening on.
// Useful in tests for verifying the listener bound to the expected
// location.
func (s *Server) SocketPath() string { return s.sock }

// daemonSource reads OPENKANBAN_DAEMON_SOURCE to identify who spawned
// this daemon. Both fork sites in internal/daemon/autostart.go and
// internal/daemonclient/dial.go set it to "tui-fork"; the launchd
// plist sets it to "launchd" via EnvironmentVariables. Anything else
// (including unset / unknown values) reports as "manual" — direct
// invocation from a shell. This is the only signal in the startup
// log that lets a postmortem tell apart a TUI-forked daemon dying
// on TUI close from a launchd-managed one dying for another reason.
func daemonSource() string {
	switch v := os.Getenv("OPENKANBAN_DAEMON_SOURCE"); v {
	case "tui-fork", "launchd":
		return v
	default:
		return "manual"
	}
}

// Serve runs the accept loop until ctx is cancelled, the shutdown
// channel is closed (via initiateShutdown), or a fatal accept error
// occurs. On return the listener is closed, the pidfile is released,
// and any remaining sessions have been torn down. Safe to call once.
func (s *Server) Serve(ctx context.Context) error {
	log.Printf("openkanbankd: listening on %s (pid %d persistent=%v source=%s)", s.sock, os.Getpid(), s.persistent, daemonSource())

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

	// Emit "activity" SessionEvents whenever a session's pane produces
	// new PTY output. The UI uses this to override a stale "waiting"
	// status (Claude Code's Notification hook → file = waiting) back to
	// "working" when bytes are still flowing — closing the permission-
	// granted-but-tool-still-running gap. NOT tracked on s.wg (same
	// rationale as watchSessionExit): tied to daemon lifetime, not to
	// any client conn.
	go s.broadcastActivity()

	// Watch the daemon's own binary for replacement (go install /
	// openkanban update from another shell). When the on-disk binary
	// is newer than this process, flip pendingRestart and exit cleanly
	// if no sessions are attached — the next TUI launch will autostart
	// a fresh daemon from the new binary. Exits with the daemon.
	go s.watchBinaryStaleness()

	// Diagnostic: dump every goroutine's stack on SIGUSR1 so we can
	// inspect the daemon's runtime state without restarting it. The
	// handler never exits the process — only the existing shutdown
	// paths can do that. The handler goroutine itself exits when
	// shutdown begins, which un-registers the signal and closes the
	// channel; without this the goroutine would leak under repeated
	// Serve→shutdown cycles (visible only in tests, since prod runs
	// one Server for the daemon's lifetime).
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGUSR1)
	go func() {
		for range sigChan {
			buf := make([]byte, 1<<20) // 1 MB; plenty for dozens of goroutines
			n := runtime.Stack(buf, true)
			log.Printf("openkanbankd: SIGUSR1 received, goroutine dump:\n%s", buf[:n])
		}
	}()
	go func() {
		<-s.shutdown
		signal.Stop(sigChan)
		close(sigChan)
	}()
	log.Printf("openkanbankd: SIGUSR1 goroutine-dump handler ready")

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
			// Intentionally NO panic recovery here. A panic in
			// handleConn signals protocol / wire-state corruption,
			// not a transient telemetry hiccup like the background
			// goroutines. Recovering would let the daemon limp on
			// with inconsistent state visible to other clients;
			// crashing surfaces the bug and lets launchd respawn
			// cleanly. See docs/AGENT_INTEGRATION.md.
			s.handleConn(c)
		}()
	}
}

// watchBinaryStaleness periodically checks whether the daemon's own
// binary on disk has been replaced under it (e.g. by `go install` or
// `openkanban update` from another shell). The check runs every
// update.BinaryStaleCheckInterval and exits when s.shutdown closes.
//
// When the binary first goes stale, we set pendingRestart and decide
// what to do based on the live session count:
//   - zero sessions: initiate immediate shutdown so the next TUI
//     launch (default mode) — or launchd / systemd respawn (persistent
//     mode) — picks up the new binary.
//   - >0 sessions: log a loud warning and keep running.
//     - Default mode: handleLastClientDisconnect will exit cleanly
//       when the last client drops, and the next launch picks up the
//       new binary.
//     - Persistent mode: handleLastClientDisconnect no longer exits,
//       so the daemon stays on the stale binary until sessions drain
//       naturally and the user explicitly runs `openkanban daemon
//       stop` (after which launchd respawns it on the new binary,
//       given KeepAlive={SuccessfulExit:false}).
//
// We deliberately don't kill live sessions to "force" a restart —
// that would surprise the user and orphan in-progress agent work.
//
// Panic recovery: same rationale as broadcastEvents — a panic here used
// to crash the whole daemon and every PTY with it. We log + exit the
// goroutine; binary-staleness checks resume on next daemon start.
func (s *Server) watchBinaryStaleness() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("openkanbankd: panic in watchBinaryStaleness: %v\n%s", r, debug.Stack())
		}
	}()
	ticker := time.NewTicker(update.BinaryStaleCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.shutdown:
			return
		case <-ticker.C:
			if !update.BinaryStale() {
				continue
			}

			s.stalenessMu.Lock()
			alreadyNotified := s.pendingRestart
			s.pendingRestart = true
			s.stalenessMu.Unlock()
			if alreadyNotified {
				// Already logged on first detection; don't spam.
				continue
			}

			s.sessionsMu.RLock()
			liveSessions := len(s.sessions)
			s.sessionsMu.RUnlock()

			if liveSessions == 0 {
				log.Printf("openkanbankd: binary on disk is newer than running process and no sessions are attached; shutting down so the next launch picks up the update")
				s.initiateShutdown("binary updated on disk")
				return
			}

			log.Printf("WARN: openkanbankd binary on disk is newer than running process (%d live session(s) still attached); will exit when the last client disconnects so the next launch picks up the update", liveSessions)
		}
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
//
// Panics in dispatchSessionEvent are recovered to a log line: a
// background-loop crash brought down the whole daemon (and every live
// PTY with it) before this defer was added. The recover is at the loop
// boundary, not inside; if dispatch panics on the same event repeatedly
// the loop would re-panic immediately, so we exit the goroutine on first
// panic and rely on the next reconcile (List poll) to repair state.
func (s *Server) broadcastEvents() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("openkanbankd: panic in broadcastEvents: %v\n%s", r, debug.Stack())
		}
	}()
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

	log.Printf("openkanbankd: dispatchSessionEvent event=%q session=%s subs=%d", ev.Event, ev.SessionID, len(subs))
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

// activityTickInterval is how often broadcastActivity wakes up to scan
// session LastActivity timestamps. 2s is a balance: short enough that
// the UI override fires within one poll cycle of a Notification hook,
// long enough that idle sessions don't flood the events channel.
const activityTickInterval = 2 * time.Second

// broadcastActivity wakes periodically and emits an "activity"
// SessionEvent for every running session whose pane LastActivity()
// advanced since the last tick. Idle sessions are skipped — the event
// stream stays quiet when nothing's happening. Stamped LastActivityAt
// is what subscribers actually consume; the Event field exists so the
// dispatch can route it specifically.
//
// Lifetime: tied to the daemon, not to any client. Exits on shutdown.
// Skipping s.wg by design — see Serve's go-broadcastActivity comment.
func (s *Server) broadcastActivity() {
	ticker := time.NewTicker(activityTickInterval)
	defer ticker.Stop()

	// Per-session memo of the last LastActivity we emitted, so we don't
	// re-broadcast the same timestamp every tick. Keyed by session ID.
	// Cleared lazily as sessions disappear — bounded by max concurrent
	// sessions, no leak hazard.
	lastSeen := make(map[string]time.Time)

	for {
		select {
		case <-s.shutdown:
			return
		case <-ticker.C:
		}

		// Snapshot the session set under lock, then operate on the
		// copy. Sessions can be added/removed concurrently and we don't
		// want to hold the lock while emitting (which takes
		// s.events / s.clientsMu under broadcaster goroutines).
		s.sessionsMu.RLock()
		alive := make(map[string]*Session, len(s.sessions))
		for id, sess := range s.sessions {
			alive[id] = sess
		}
		s.sessionsMu.RUnlock()

		// Drop memo entries for sessions that vanished.
		for id := range lastSeen {
			if _, ok := alive[id]; !ok {
				delete(lastSeen, id)
			}
		}

		for id, sess := range alive {
			la := sess.LastActivity()
			if la.IsZero() {
				continue
			}
			if prev, ok := lastSeen[id]; ok && !la.After(prev) {
				continue
			}
			lastSeen[id] = la
			s.emitEvent(SessionEvent{
				Event:          "activity",
				SessionID:      id,
				TicketID:       sess.TicketID(),
				LastActivityAt: la,
			})
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
		// Scrub this client out of every session's viewers set BEFORE
		// the last-client check — a TUI that vanishes without sending
		// SetViewing(false) would otherwise leave sibling boards with
		// a stuck "viewing" indicator until the session itself exits.
		s.cleanupViewersForClient(c.id)
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

	case MsgCancelExitReq:
		var req CancelExitReq
		_ = json.Unmarshal(raw, &req)
		s.writeResp(c, MsgCancelExitResp, s.handleCancelExit(c, req))

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

	case MsgSetViewingReq:
		var req SetViewingReq
		if err := json.Unmarshal(raw, &req); err != nil {
			s.writeError(c, "bad_request", err.Error())
			return
		}
		s.writeResp(c, MsgSetViewingResp, s.handleSetViewing(c, req))

	default:
		s.writeError(c, "unknown_message", fmt.Sprintf("unknown message type %q", typeName))
	}
}

// --- Per-message handlers ---

func (s *Server) handleHello(c *clientConn, req HelloReq) HelloResp {
	s.clientsMu.Lock()
	c.name = req.ClientName
	count := len(s.clients)
	s.clientsMu.Unlock()

	return HelloResp{
		ProtocolVersion: ProtocolVersion,
		BinaryVersion:   BinaryVersion,
		ClientCount:     count,
		ClientID:        c.id,
	}
}

// handleSpawn is idempotent per TicketID. The invariant the daemon
// enforces is 1:1 ticket↔session: a second Spawn for a ticket whose
// session is already live returns the existing SessionID instead of
// constructing a new one. This is the only place the dedup is enforced
// — every client-side spawn discipline gap (panicked TUI, racing CLI
// `ticket continue`, etc.) collapses here.
//
// Empty TicketID is rejected outright with an error. Anonymous sessions
// are disallowed structurally: with no TicketID, the daemon cannot
// dedup, cannot route TicketDone, and cannot be reaped on ticket
// deletion — i.e. it would be an orphan. The wire shape still permits
// the field to be empty, but handleSpawn refuses it at the entry.
//
// The check happens in two phases to close the construct-outside-lock
// race window: an RLock fast path that avoids NewSession when a match
// already exists, and a WLock re-check that catches the case where two
// concurrent spawns both saw an empty slot under RLock and raced into
// NewSession. The loser of the WLock re-check kills its just-spawned
// session and returns the winner's SessionID — the agent process the
// loser forked is the only collateral, and it's terminated before
// handleSpawn returns.
func (s *Server) handleSpawn(c *clientConn, req SpawnReq) (SpawnResp, error) {
	// Reject anonymous spawns at the door: no TicketID means no dedup,
	// no TicketDone routing, no cleanup path — i.e. an orphan by
	// construction. The dispatcher surfaces this as a spawn_failed
	// error to the client.
	if req.TicketID == "" {
		return SpawnResp{}, fmt.Errorf("spawn: empty TicketID rejected (anonymous sessions disallowed)")
	}

	// Fast path: if a session for this TicketID already exists, return
	// it without constructing a new one. The empty-TicketID guard above
	// makes the `req.TicketID != ""` test below unreachable in practice,
	// but it's kept as belt-and-braces in case the entry check is ever
	// removed or refactored.
	if req.TicketID != "" {
		s.sessionsMu.RLock()
		existing := s.findSessionForTicketLocked(req.TicketID)
		s.sessionsMu.RUnlock()
		if existing != nil {
			log.Printf("openkanbankd: client %d spawn idempotent hit ticket=%s reused session=%s pid=%d",
				c.id, req.TicketID, existing.ID(), existing.pane.PID())
			return SpawnResp{SessionID: existing.ID(), PID: existing.pane.PID()}, nil
		}
	}

	sess, err := NewSession(req)
	if err != nil {
		return SpawnResp{}, err
	}

	// Re-check under WLock to close the construct-outside-lock race
	// window: two concurrent spawns may both have seen no existing
	// session under RLock and both called NewSession. Exactly one wins
	// the WLock; the other discards its just-built session. As above,
	// the empty-TicketID guard at function entry makes this nil-check
	// belt-and-braces — left in to defend against future refactors.
	s.sessionsMu.Lock()
	if req.TicketID != "" {
		if winner := s.findSessionForTicketLocked(req.TicketID); winner != nil {
			s.sessionsMu.Unlock()
			log.Printf("openkanbankd: client %d spawn lost race ticket=%s discarding new session in favor of %s",
				c.id, req.TicketID, winner.ID())
			if killErr := sess.Kill(0); killErr != nil {
				log.Printf("openkanbankd: cleanup of race-loser session %s: %v", sess.ID(), killErr)
			}
			return SpawnResp{SessionID: winner.ID(), PID: winner.pane.PID()}, nil
		}
	}
	s.sessions[sess.ID()] = sess
	s.sessionsMu.Unlock()

	log.Printf("openkanbankd: client %d spawned session %s (ticket=%s pid=%d)", c.id, sess.ID(), sess.TicketID(), sess.pane.PID())

	// Wire pane-exit observation BEFORE announcing the spawn so we
	// can't miss an ExitEvent fired by a child that races out the
	// gate. The watcher emits an "exited" SessionEvent when the pane
	// publishes its final ExitEvent.
	s.watchSessionExit(sess)

	s.emitEvent(SessionEvent{Event: "started", SessionID: sess.ID(), TicketID: sess.TicketID(), Status: "working", LastActivityAt: sess.LastActivity()})

	return SpawnResp{SessionID: sess.ID(), PID: sess.pane.PID()}, nil
}

// findSessionForTicketLocked returns the (sole) session whose TicketID
// matches, or nil if none. Caller must hold sessionsMu (R or W). After
// handleSpawn enforces uniqueness on insert, at most one match exists
// — but handleTicketDone defensively iterates the full map for any
// duplicates inherited from older daemons that lacked this check.
func (s *Server) findSessionForTicketLocked(ticketID string) *Session {
	// Belt-and-braces: no caller should ever pass empty TicketID;
	// handleSpawn rejects it at the door. Kept defensively so a future
	// caller wired up with sloppy validation can't trigger a full-map
	// scan that returns the first arbitrary session.
	if ticketID == "" {
		return nil
	}
	for _, sess := range s.sessions {
		if sess.TicketID() == ticketID {
			return sess
		}
	}
	return nil
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
//
// Panic-safety invariant: removeSession() + the "exited" emit MUST
// always run, even if a panic occurs in the subscription loop or in
// emit itself. We register a single outer-recover defer that handles
// any panic from the loop body, then runs removeSession + emit with
// an inner recover so a panic IN emit can't skip the registry
// cleanup or leave the goroutine in an undefined state.
func (s *Server) watchSessionExit(sess *Session) {
	if sess == nil || sess.pane == nil {
		return
	}
	ch, unsub := sess.pane.Subscribe()
	go func() {
		defer unsub()
		sessID := sess.ID()
		ticketID := sess.TicketID()
		// removeSession deletes sess from the registry if (and only if)
		// it's still the entry under sessID. handleKill / handleTicketDone
		// may have already removed it via the explicit path; both paths
		// must be safe to run concurrently.
		removeSession := func() {
			s.sessionsMu.Lock()
			if cur, ok := s.sessions[sessID]; ok && cur == sess {
				delete(s.sessions, sessID)
				log.Printf("openkanbankd: session %s (ticket=%s) exited; removed from registry", sessID, ticketID)
			}
			// Defense-in-depth: with the per-TicketID dedup in
			// handleSpawn (PR #34) no other session should ever share
			// this TicketID. If one does, it's an invariant violation
			// — log loudly so the regression is visible, but don't
			// auto-kill (that would silently change cleanup
			// semantics). This is purely observability for a path
			// that should never fire post-dedup; if it ever does we
			// want a breadcrumb in the daemon log pointing at it.
			if ticketID != "" {
				if other := s.findSessionForTicketLocked(ticketID); other != nil {
					log.Printf("WARN: openkanbankd: after removing session %s, another session %s still references ticket %s — invariant violation",
						sessID, other.ID(), ticketID)
				}
			}
			s.sessionsMu.Unlock()
		}
		emit := func() {
			expected := sess.ExpectedCompletion()
			reason := "natural_exit"
			if expected {
				reason = "ticket_done"
			}
			ev := SessionEvent{
				Event:          "exited",
				SessionID:      sessID,
				TicketID:       ticketID,
				Expected:       expected,
				Reason:         reason,
				LastActivityAt: sess.LastActivity(),
			}
			if fn := s.emitSessionExitFn; fn != nil {
				fn(ev)
				return
			}
			s.emitEvent(ev)
		}
		defer func() {
			// Catches any panic from the loop body. Then runs cleanup
			// with an inner recover so a panic in emit doesn't skip
			// removeSession from another invocation path.
			if r := recover(); r != nil {
				log.Printf("openkanbankd: panic in watchSessionExit(%s): %v\n%s", sessID, r, debug.Stack())
			}
			defer func() {
				if r := recover(); r != nil {
					log.Printf("openkanbankd: panic in watchSessionExit cleanup emit(%s): %v\n%s", sessID, r, debug.Stack())
				}
			}()
			removeSession()
			emit()
		}()
		for ev := range ch {
			if _, ok := ev.(terminal.ExitEvent); ok {
				return // defer runs removeSession + emit
			}
		}
		// Channel closed without ExitEvent (e.g. Stop tore the loop
		// down before the read returned). The defer still runs
		// removeSession + emit so subscribers learn the session is gone.
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
// done` and `openkanban ticket in-review` — both CLIs send the same
// TicketDoneReq because the daemon-side motion is identical (terminate
// the live PTY as an expected wrap-up; the CLI is responsible for the
// status the ticket lands in). It scans the live sessions for any
// bound to req.TicketID; for each, it flips that session's
// expected-completion flag, removes it from the registry, and kicks
// off the kill in a goroutine. The resulting "exited" SessionEvent
// (emitted by watchSessionExit when the pane publishes ExitEvent)
// carries Expected=true / Reason="ticket_done" so subscribers preserve
// AgentCompleted instead of resetting to AgentNone.
//
// Iterates all matches (not just the first) as defense-in-depth: a
// daemon that ran on a pre-dedup binary may have ended up with two
// sessions sharing a TicketID. handleSpawn now refuses to create such
// a duplicate, but any inherited one is cleaned up here on the next
// ticket-done flow. The response carries the first match's SessionID
// for backward compatibility with clients that index off it; the
// per-session SessionEvent broadcasts surface the rest.
//
// Returns synchronously: Killed:true plus the first matched session's
// SessionID on hit; Killed:false (no error) on miss. The CLI treats
// the miss as informational — the .md and status-file writes are
// authoritative.
func (s *Server) handleTicketDone(c *clientConn, req TicketDoneReq) (TicketDoneResp, error) {
	if req.TicketID == "" {
		return TicketDoneResp{}, nil
	}

	s.sessionsMu.Lock()
	matches := make([]*Session, 0, 1)
	for _, sess := range s.sessions {
		if sess.TicketID() == req.TicketID {
			matches = append(matches, sess)
		}
	}
	for _, m := range matches {
		delete(s.sessions, m.ID())
	}
	s.sessionsMu.Unlock()

	if len(matches) == 0 {
		return TicketDoneResp{Killed: false}, nil
	}

	if len(matches) > 1 {
		log.Printf("WARN: openkanbankd: client %d ticket-done found %d sessions for ticket=%s (pre-dedup duplicates); terminating all", c.id, len(matches), req.TicketID)
	}

	for _, m := range matches {
		m.MarkExpectedCompletion()
		log.Printf("openkanbankd: client %d ticket-done session %s (ticket=%s)", c.id, m.ID(), req.TicketID)
		// Kill in a goroutine so the RPC returns synchronously. The
		// grace window matches shutdownGraceSeconds — agents may have
		// a few seconds of cleanup. The watcher emits the "exited"
		// event when the pane's ExitEvent lands.
		go func(sess *Session) {
			if err := sess.Kill(shutdownGraceSeconds); err != nil {
				log.Printf("openkanbankd: ticket-done kill session %s: %v", sess.ID(), err)
			}
		}(m)
	}

	return TicketDoneResp{SessionID: matches[0].ID(), Killed: true}, nil
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
			return OwnsResp{
				Owned:       true,
				SessionID:   sess.ID(),
				SessionName: sess.SessionName(),
			}
		}
	}
	return OwnsResp{Owned: false}
}

func (s *Server) handleSubscribe(c *clientConn, req SubscribeReq) SubscribeResp {
	s.clientsMu.Lock()
	c.subscribed = true
	s.clientsMu.Unlock()
	log.Printf("openkanbankd: client %d subscribed", c.id)
	return SubscribeResp{}
}

// handleSetViewing toggles this client's membership in the session's
// viewers set. Broadcasts a "viewing" or "unviewing" SessionEvent
// only when the set actually changed (idempotent on duplicate calls).
// Unknown session_id returns ViewerCount=0 with no event — the TUI
// may race the daemon's "exited" event for a session it was viewing
// and the right behavior is silent no-op, not error.
func (s *Server) handleSetViewing(c *clientConn, req SetViewingReq) SetViewingResp {
	s.sessionsMu.RLock()
	sess, ok := s.sessions[req.SessionID]
	s.sessionsMu.RUnlock()
	if !ok {
		return SetViewingResp{ViewerCount: 0}
	}
	count, changed := sess.SetViewing(c.id, req.Viewing)
	if changed {
		ev := "viewing"
		if !req.Viewing {
			ev = "unviewing"
		}
		s.emitEvent(SessionEvent{Event: ev, SessionID: sess.ID(), TicketID: sess.TicketID()})
	}
	return SetViewingResp{ViewerCount: count}
}

// cleanupViewersForClient removes clientID from every session's
// viewers set and emits an "unviewing" SessionEvent for each session
// where the client was actually a viewer. Called from the disconnect
// path so a crashed or unceremoniously-closed TUI doesn't leave
// zombie viewer counts on sibling boards.
func (s *Server) cleanupViewersForClient(clientID uint16) {
	s.sessionsMu.RLock()
	sessions := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.sessionsMu.RUnlock()
	for _, sess := range sessions {
		if sess.RemoveViewer(clientID) {
			s.emitEvent(SessionEvent{Event: "unviewing", SessionID: sess.ID(), TicketID: sess.TicketID()})
		}
	}
}

// handlePrepareExit atomically marks the calling client as exiting and
// reports back how many peer clients are still *active* (i.e. have NOT
// also called PrepareExit). The TUI uses OtherActiveClients to decide
// whether to silent-quit (peers remain to keep the daemon alive) or
// open the exit-confirm modal (we're the last one out and sessions are
// at stake).
//
// The flag-then-count is done under a single clientsMu acquisition so
// concurrent PrepareExit calls from multiple TUIs are serialized:
// exactly one caller observes OtherActiveClients == 0, even when they
// all fire at the same instant. unregisterClient also takes clientsMu,
// so a peer disconnecting mid-call can't slip a stale entry into the
// count.
//
// ClientCount is preserved for one release as deprecated wire surface.
func (s *Server) handlePrepareExit(c *clientConn, req PrepareExitReq) PrepareExitResp {
	s.clientsMu.Lock()
	c.exiting = true
	total := len(s.clients)
	// One pass over s.clients produces both peer counts: OtherTUIClients
	// (filter by ClientNameTUI, used by CLI daemon-stop) and
	// OtherActiveClients (filter by !exiting, used by TUI exit-guard).
	// They answer different questions and are not interchangeable —
	// see PrepareExitResp doc.
	otherTUIs := 0
	otherActive := 0
	for id, oc := range s.clients {
		if id == c.id {
			continue
		}
		if oc.name == ClientNameTUI {
			otherTUIs++
		}
		if !oc.exiting {
			otherActive++
		}
	}
	s.clientsMu.Unlock()

	s.sessionsMu.RLock()
	infos := make([]SessionInfo, 0, len(s.sessions))
	for _, sess := range s.sessions {
		infos = append(infos, sess.Info())
	}
	s.sessionsMu.RUnlock()

	return PrepareExitResp{
		ClientCount:        total,
		OtherTUIClients:    otherTUIs,
		OtherActiveClients: otherActive,
		Sessions:           infos,
	}
}

// handleCancelExit clears the exit-intent flag set by a prior
// PrepareExit on this connection. The TUI calls this when the user
// dismisses the exit-confirm modal (Esc/q), so subsequent PrepareExit
// RPCs from peer TUIs see this client as active again. Idempotent:
// calling on a connection that never set exiting is a no-op.
func (s *Server) handleCancelExit(c *clientConn, req CancelExitReq) CancelExitResp {
	s.clientsMu.Lock()
	c.exiting = false
	s.clientsMu.Unlock()
	return CancelExitResp{}
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
// zero. In default mode, this triggers a shutdown (the daemon is not
// supposed to outlive the last TUI). In persistent mode (launchd /
// systemd integration), the daemon stays up and only logs; explicit
// ShutdownReq or signals are the exit paths.
//
// If sessions are still alive at that moment the exit-guard in the
// TUI failed; we log loudly. In default mode we then kill them
// defensively via initiateShutdown's cleanup; in persistent mode the
// sessions stay attached to the daemon and a future TUI can re-attach.
func (s *Server) handleLastClientDisconnect() {
	s.sessionsMu.RLock()
	live := len(s.sessions)
	s.sessionsMu.RUnlock()

	if s.persistent {
		log.Printf("openkanbankd: last client disconnected; staying up (persistent mode); %d live session(s)", live)
		return
	}

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
