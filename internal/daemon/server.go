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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/techdufus/openkanban/internal/agent"
	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/notify"
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

// handlerDeadline is the maximum time a short RPC handler may run before
// the dispatcher abandons it and returns a daemon_unresponsive error to
// the client. Only applies to non-blocking handlers — handleAttach and
// handleShutdown are explicitly excluded.
const handlerDeadline = 10 * time.Second

// handlerDeadlineOverride lets tests shorten the deadline. Zero means use
// handlerDeadline.
var handlerDeadlineOverride time.Duration

// reapTimeout is how long a Kill may run before we count it a reap failure
// (a child stuck in uninterruptible kernel exit — e.g. a PTY whose master
// the daemon already closed but the process won't die). The kill goroutine
// keeps running; the counter surfaces the leak via health.
const reapTimeout = 30 * time.Second

// runHandlerWithDeadline runs fn (a short RPC handler) and returns true if
// it finished within the deadline. On timeout it returns false and leaves
// the handler goroutine running (it will finish or leak — the wedge
// watchdog and the conn-sem bound the worst case). The caller writes an
// "unresponsive" error to the client so it doesn't hang.
// Not for use with handleAttach (blocks by design) or handleShutdown
// (legitimately slow across many sessions).
func (s *Server) runHandlerWithDeadline(name string, fn func()) bool {
	d := handlerDeadline
	if handlerDeadlineOverride > 0 {
		d = handlerDeadlineOverride
	}
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		log.Printf("openkanbankd: handler %q exceeded %s — abandoning (client will get unresponsive error)", name, d)
		return false
	}
}

// trackedKill starts a goroutine to kill the session and tracks it for
// timeout-based reap failure detection. The kill itself is non-blocking from
// the caller's perspective. If the kill exceeds reapTimeout, it moves from
// inflightKills to reapFailures to surface kernel-stuck children.
func (s *Server) trackedKill(sess *Session, grace int) {
	s.inflightKills.Add(1)
	done := make(chan error, 1)
	go func() { done <- sess.Kill(grace) }()
	go func() {
		defer s.inflightKills.Add(-1)
		select {
		case err := <-done:
			if err != nil {
				log.Printf("openkanbankd: kill session %s: %v", sess.ID(), err)
			}
		case <-time.After(reapTimeout):
			s.reapFailures.Add(1)
			log.Printf("WARN: openkanbankd: session %s did not reap within %s (possible kernel-stuck child); will keep trying", sess.ID(), reapTimeout)
			<-done // still account for eventual completion
			s.reapFailures.Add(-1)
		}
	}()
}

// killStats returns the current counts of in-flight kills and reap failures.
func (s *Server) killStats() (int64, int64) {
	return s.inflightKills.Load(), s.reapFailures.Load()
}

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

	reg *sessionRegistry

	clientsMu    sync.Mutex
	clients      map[uint16]*clientConn
	nextClientID uint16

	shutdown     chan struct{}
	shutdownOnce sync.Once

	wg sync.WaitGroup

	sem *connSem

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

	// drainMu guards drainPending, the single-in-flight guard for the
	// default-mode deferred-shutdown watcher (awaitSessionDrain). When
	// the last client disconnects with live sessions, default mode no
	// longer force-kills them; it spawns awaitSessionDrain to keep the
	// daemon alive until the registry drains, then shuts down. The flag
	// ensures only one such watcher runs at a time across repeated
	// connect/disconnect cycles.
	drainMu      sync.Mutex
	drainPending bool

	// statusDetector resolves a session's working/waiting AgentStatus
	// from the hook status file + the live PTY grid + the pane's last
	// output time. The daemon owns these inputs for EVERY session it
	// runs, attached or not, so it's the one place that can classify an
	// unattached session correctly. Safe for concurrent use (its cache is
	// mutex-guarded); the broadcaster goroutine is the only caller.
	statusDetector *agent.StatusDetector

	// emitSessionExitFn is the seam watchSessionExit uses to publish
	// the "exited" SessionEvent. Production leaves this nil and the
	// goroutine falls back to s.emitEvent. Tests inject a panicking
	// override via setEmitSessionExitFnForTest to verify the
	// goroutine's panic-recovery + invariant-preserving cleanup.
	// Only read once at goroutine start; no mutex needed because
	// tests set it before triggering the event.
	emitSessionExitFn func(SessionEvent)

	// dispatchSeq increments at the end of every dispatch() call; inflight
	// tracks handlers currently executing. The watchdog samples both: if
	// inflight>0 but dispatchSeq is frozen past the wedge threshold, the
	// daemon is stuck and must self-restart. Lock-free.
	dispatchSeq atomic.Uint64
	inflight    atomic.Int64

	// inflightKills tracks the number of in-flight session kill operations.
	// When a kill exceeds reapTimeout, it's moved to reapFailures to surface
	// kernel-stuck children as a health concern.
	inflightKills atomic.Int64

	// reapFailures counts sessions that failed to reap within reapTimeout.
	// These are kernel-stuck children (e.g. PTY master closed but process
	// won't die); the kill goroutine keeps trying, and this counter makes
	// the leak visible for health monitoring.
	reapFailures atomic.Int64
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
		config.GuardHomeWrite(sock)
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
		sock:           sock,
		pidlock:        lock,
		ln:             ln,
		persistent:     opts.Persistent,
		reg:            newSessionRegistry(),
		clients:        make(map[uint16]*clientConn),
		shutdown:       make(chan struct{}),
		events:         make(chan SessionEvent, 64),
		statusDetector: agent.NewStatusDetector(),
		sem:            newConnSem(maxConcurrentConns),
	}, nil
}

// resolveSessionStatus computes the authoritative AgentStatus for a
// session the daemon owns, from inputs the daemon has for EVERY session
// (attached or not): the live PTY grid, the pane's last-output time, and
// the hook-written status file. The returned string is a board.AgentStatus
// value, or "" when the daemon has no verdict to push — for sessions with
// no recorded agent type (older client), for opencode (whose status the
// UI resolves via its own HTTP API), and when the detector can't
// determine a state. An empty return leaves the client's file-poll in
// charge; see Model.applyDaemonStatus.
func (s *Server) resolveSessionStatus(sess *Session) string {
	if s == nil || sess == nil {
		return ""
	}
	agentType := sess.AgentType()
	// Cheap early-out before the grid copy: opencode and type-less
	// (older-client) sessions get no daemon verdict.
	if agentType == "" || agentType == "opencode" {
		return ""
	}
	p := sess.Pane()
	if p == nil {
		return ""
	}
	// Non-blocking read. If the pane lock is held — notably by a Stop()
	// whose emulator-drain teardown can hang — skip this tick instead of
	// blocking the broadcaster goroutine. Blocking here would freeze the
	// status/activity heartbeat for EVERY session behind one stuck
	// teardown; the next tick retries once the lock frees.
	content, ok := p.GetContentTry()
	if !ok {
		return ""
	}
	// running=true is sound: the broadcaster only resolves status for
	// sessions in its alive set whose LastActivity advanced this tick, so
	// the pane is live and emitting. Passing the constant avoids the
	// blocking Running() lock (same Stop()-freeze hazard as GetContent).
	return resolveStatusVerdict(
		s.statusDetector, agentType, sess.SessionName(), sess.workdir,
		true, content, sess.LastActivity(),
	)
}

// resolveStatusVerdict is the pure core of resolveSessionStatus, split
// out so it can be unit-tested without forking a real PTY session. It
// returns a board.AgentStatus value, or "" when the daemon has no verdict
// to push: a nil detector, a missing/opencode agent type, an empty
// session name, or a detector result of AgentNone ("can't tell"). An
// empty return leaves the client's file-poll in charge.
func resolveStatusVerdict(d *agent.StatusDetector, agentType, sessionName, workdir string, running bool, content string, lastActivity time.Time) string {
	if d == nil || agentType == "" || agentType == "opencode" || sessionName == "" {
		return ""
	}
	status := d.DetectStatusWithActivity(agentType, sessionName, sessionName, workdir, 0, running, content, lastActivity)
	if status == board.AgentNone {
		return ""
	}
	return string(status)
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

	// Force-restart the daemon if dispatch is wedged (work queued but
	// nothing completing) past the threshold so launchd/systemd can
	// respawn it, picking up a fresh binary if one is present.
	go s.runWedgeWatchdog()

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
		if !s.sem.tryAcquire() {
			log.Printf("openkanbankd: connection cap (%d) reached — rejecting client %d", maxConcurrentConns, c.id)
			s.writeError(c, "server_busy", "daemon at connection capacity")
			conn.Close()
			s.unregisterClient(c)
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.sem.release()
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
//   - Default mode: handleLastClientDisconnect defers shutdown when
//     the last client drops with live sessions (it no longer kills
//     them), so the daemon stays on the stale binary until sessions
//     drain naturally and then exits cleanly — the next launch picks
//     up the new binary.
//   - Persistent mode: handleLastClientDisconnect no longer exits,
//     so the daemon stays on the stale binary until sessions drain
//     naturally and the user explicitly runs `openkanban daemon
//     stop` (after which launchd respawns it on the new binary,
//     given KeepAlive={SuccessfulExit:false}).
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

			liveSessions := s.reg.len()

			if liveSessions == 0 {
				log.Printf("openkanbankd: binary on disk is newer than running process and no sessions are attached; shutting down so the next launch picks up the update")
				s.initiateShutdown("binary updated on disk")
				return
			}

			if s.persistent {
				log.Printf("WARN: openkanbankd binary on disk is newer than running process (%d live session(s) still attached); persistent mode will NOT auto-restart — run `openkanban daemon restart` or rely on the wedge watchdog", liveSessions)
			} else {
				log.Printf("WARN: openkanbankd binary on disk is newer than running process (%d live session(s) still attached); will exit when the last client disconnects so the next launch picks up the update", liveSessions)
			}
		}
	}
}

// drainPollInterval is how often awaitSessionDrain wakes to re-check the
// live session count. Per-tick cost is two cheap locked map-length
// reads, so a 1s tick is fine; it bounds the post-drain shutdown latency
// to ≤1s, which is harmless.
const drainPollInterval = 1 * time.Second

// awaitSessionDrain runs (default mode only) after the last client
// disconnects while sessions are still live. Rather than force-kill that
// in-progress agent work, the daemon stays alive until the registry
// drains naturally, then initiates shutdown so it does not linger as an
// orphan process.
//
// While live > 0 the watcher keeps waiting regardless of client count:
// if a client re-attaches it owns the lifecycle again, and its eventual
// disconnect re-enters handleLastClientDisconnect (which no-ops here
// because drainPending is still set — this watcher keeps covering until
// live == 0). Once the registry is empty, if no client is connected we
// shut down; if a client IS connected, its disconnect drives the normal
// (now-immediate, live==0) shutdown path, so we just exit the watcher.
// Single-in-flight via drainPending, cleared on exit.
//
// Panic recovery mirrors the other background goroutines: log + exit the
// goroutine rather than crash the daemon and every PTY with it.
func (s *Server) awaitSessionDrain() {
	defer func() {
		s.drainMu.Lock()
		s.drainPending = false
		s.drainMu.Unlock()
		if r := recover(); r != nil {
			log.Printf("openkanbankd: panic in awaitSessionDrain: %v\n%s", r, debug.Stack())
		}
	}()
	ticker := time.NewTicker(drainPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.shutdown:
			return
		case <-ticker.C:
			if live := s.reg.len(); live > 0 {
				continue
			}

			s.clientsMu.Lock()
			clients := len(s.clients)
			s.clientsMu.Unlock()
			if clients == 0 {
				log.Printf("openkanbankd: deferred shutdown — live sessions drained after last client left")
				s.initiateShutdown("live sessions drained")
			}
			return
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

	// Per-session memo of which sessions we've already flagged "stuck"
	// this episode, so the WARN log + "stuck" SessionEvent fire exactly
	// once per wedge (not every tick). Cleared when the session
	// un-wedges or leaves the alive set.
	stuckSeen := make(map[string]struct{})

	for {
		select {
		case <-s.shutdown:
			return
		case <-ticker.C:
		}

		// Snapshot the session set (lock-free), then operate on the
		// immutable copy. Sessions can be added/removed concurrently and
		// we don't want to block while emitting (which takes
		// s.events / s.clientsMu under broadcaster goroutines).
		alive := s.reg.snapshot()

		// Drop memo entries for sessions that vanished.
		for id := range lastSeen {
			if _, ok := alive[id]; !ok {
				delete(lastSeen, id)
			}
		}
		for id := range stuckSeen {
			if _, ok := alive[id]; !ok {
				delete(stuckSeen, id)
			}
		}

		for id, sess := range alive {
			// Wedge detection (DETECT-ONLY — never auto-kill). Reads
			// ONLY atomics via WedgedSince(), so it can never block on
			// the wedged pane — preserving the broadcaster's
			// never-block-on-a-stuck-session guarantee. A pane counts as
			// stuck when WriteInput has been backpressured (the child
			// stopped draining stdin) for at least stuckThreshold.
			s.checkSessionWedge(id, sess, stuckSeen)

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
				// Resolve status from the live grid here, where the daemon
				// always has it — so the heartbeat that reports "bytes
				// flowed" also says WHAT they were (a work spinner vs a
				// re-rendered prompt). This is what keeps an UNATTACHED
				// session's card accurate; the client has no grid for it.
				Status: s.resolveSessionStatus(sess),
			})
		}
	}
}

// stuckThreshold is how long a pane must be continuously wedged on input
// backpressure before the watchdog surfaces it as "stuck". Two activity
// ticks: long enough that a brief paste burst that drains within a tick
// doesn't trip it, short enough that a genuinely-wedged session surfaces
// within a few seconds.
const stuckThreshold = 2 * activityTickInterval

// checkSessionWedge surfaces a wedged session to subscribers + the log
// exactly once per wedge episode, and clears the per-episode memo when
// the session un-wedges. DETECT-ONLY: it NEVER kills the session
// (honoring the no-force-kill invariant — input backpressure can't
// distinguish "busy" from "wedged"; the user decides via the TUI).
//
// Reads only the pane's lock-free WedgedSince() atomic, so it can never
// block on the wedged pane. Must not take p.mu or call any blocking pane
// accessor.
func (s *Server) checkSessionWedge(id string, sess *Session, stuckSeen map[string]struct{}) {
	if sess == nil || sess.pane == nil {
		return
	}
	since := sess.pane.WedgedSince()
	if since.IsZero() || time.Since(since) < stuckThreshold {
		// Not (yet) stuck — if it had been flagged, the wedge cleared,
		// so reset the memo for the next episode.
		delete(stuckSeen, id)
		return
	}
	if _, already := stuckSeen[id]; already {
		return // already surfaced this episode
	}
	stuckSeen[id] = struct{}{}

	log.Printf("WARN: openkanbankd: session %s (ticket=%s) appears stuck — input backpressured for %s (child not draining stdin); user can recover or destroy it from the TUI",
		id, sess.TicketID(), time.Since(since).Round(time.Second))

	s.emitEvent(SessionEvent{
		Event:          "status",
		SessionID:      id,
		TicketID:       sess.TicketID(),
		Status:         "stuck",
		LastActivityAt: sess.LastActivity(),
	})

	// Best-effort desktop notification, fired in its own goroutine so it
	// can never block the tick. Errors are logged, never fatal — the
	// macOS bundle may be absent (notify is a no-op off-bundle). The
	// primary surfaces are the WARN log + the "stuck" SessionEvent → TUI
	// red card; the notification is a bonus.
	ticketID := sess.TicketID()
	go func() {
		if err := notify.Send(fmt.Sprintf("Session stuck (ticket %s) — recover or destroy it in openkanban", ticketID)); err != nil {
			log.Printf("openkanbankd: stuck-session notify failed: %v", err)
		}
	}()
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
	live := s.reg.drain()

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
	config.GuardHomeWrite(s.sock)
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
	s.inflight.Add(1)
	defer func() {
		s.inflight.Add(-1)
		s.dispatchSeq.Add(1)
	}()
	switch typeName {
	case MsgHelloReq:
		var req HelloReq
		if err := json.Unmarshal(raw, &req); err != nil {
			s.writeError(c, "bad_request", err.Error())
			return
		}
		var resp HelloResp
		if s.runHandlerWithDeadline("hello", func() { resp = s.handleHello(c, req) }) {
			s.writeResp(c, MsgHelloResp, resp)
		} else {
			s.writeError(c, "daemon_unresponsive", "hello handler timed out")
		}

	case MsgSpawnReq:
		var req SpawnReq
		if err := json.Unmarshal(raw, &req); err != nil {
			s.writeError(c, "bad_request", err.Error())
			return
		}
		var resp SpawnResp
		var spawnErr error
		if s.runHandlerWithDeadline("spawn", func() { resp, spawnErr = s.handleSpawn(c, req) }) {
			if spawnErr != nil {
				s.writeError(c, "spawn_failed", spawnErr.Error())
			} else {
				s.writeResp(c, MsgSpawnResp, resp)
			}
		} else {
			s.writeError(c, "daemon_unresponsive", "spawn handler timed out")
		}

	case MsgListReq:
		var req ListReq
		_ = json.Unmarshal(raw, &req)
		var resp ListResp
		if s.runHandlerWithDeadline("list", func() { resp = s.handleList(c, req) }) {
			s.writeResp(c, MsgListResp, resp)
		} else {
			s.writeError(c, "daemon_unresponsive", "list handler timed out")
		}

	case MsgKillReq:
		var req KillReq
		if err := json.Unmarshal(raw, &req); err != nil {
			s.writeError(c, "bad_request", err.Error())
			return
		}
		var resp KillResp
		var killErr error
		if s.runHandlerWithDeadline("kill", func() { resp, killErr = s.handleKill(c, req) }) {
			if killErr != nil {
				s.writeError(c, "kill_failed", killErr.Error())
			} else {
				s.writeResp(c, MsgKillResp, resp)
			}
		} else {
			s.writeError(c, "daemon_unresponsive", "kill handler timed out")
		}

	case MsgTicketDoneReq:
		var req TicketDoneReq
		if err := json.Unmarshal(raw, &req); err != nil {
			s.writeError(c, "bad_request", err.Error())
			return
		}
		var resp TicketDoneResp
		var tdErr error
		if s.runHandlerWithDeadline("ticket_done", func() { resp, tdErr = s.handleTicketDone(c, req) }) {
			if tdErr != nil {
				s.writeError(c, "ticket_done_failed", tdErr.Error())
			} else {
				s.writeResp(c, MsgTicketDoneResp, resp)
			}
		} else {
			s.writeError(c, "daemon_unresponsive", "ticket_done handler timed out")
		}

	case MsgOwnsReq:
		var req OwnsReq
		if err := json.Unmarshal(raw, &req); err != nil {
			s.writeError(c, "bad_request", err.Error())
			return
		}
		var resp OwnsResp
		if s.runHandlerWithDeadline("owns", func() { resp = s.handleOwns(c, req) }) {
			s.writeResp(c, MsgOwnsResp, resp)
		} else {
			s.writeError(c, "daemon_unresponsive", "owns handler timed out")
		}

	case MsgSubscribeReq:
		var req SubscribeReq
		_ = json.Unmarshal(raw, &req)
		var resp SubscribeResp
		if s.runHandlerWithDeadline("subscribe", func() { resp = s.handleSubscribe(c, req) }) {
			s.writeResp(c, MsgSubscribeResp, resp)
		} else {
			s.writeError(c, "daemon_unresponsive", "subscribe handler timed out")
		}

	case MsgPrepareExitReq:
		var req PrepareExitReq
		_ = json.Unmarshal(raw, &req)
		var resp PrepareExitResp
		if s.runHandlerWithDeadline("prepare_exit", func() { resp = s.handlePrepareExit(c, req) }) {
			s.writeResp(c, MsgPrepareExitResp, resp)
		} else {
			s.writeError(c, "daemon_unresponsive", "prepare_exit handler timed out")
		}

	case MsgCancelExitReq:
		var req CancelExitReq
		_ = json.Unmarshal(raw, &req)
		var resp CancelExitResp
		if s.runHandlerWithDeadline("cancel_exit", func() { resp = s.handleCancelExit(c, req) }) {
			s.writeResp(c, MsgCancelExitResp, resp)
		} else {
			s.writeError(c, "daemon_unresponsive", "cancel_exit handler timed out")
		}

	case MsgShutdownReq:
		// NOT wrapped — legitimately slow: kills every live session (up to
		// grace seconds each) before replying; healthy multi-session shutdown
		// can exceed handlerDeadline.
		var req ShutdownReq
		_ = json.Unmarshal(raw, &req)
		s.writeResp(c, MsgShutdownResp, s.handleShutdown(c, req))

	case MsgAttachReq:
		var req AttachReq
		if err := json.Unmarshal(raw, &req); err != nil {
			s.writeError(c, "bad_request", err.Error())
			return
		}
		// NOT wrapped — handleAttach BLOCKS for the lifetime of the binary
		// stream. When it returns the conn is fully drained / closed; the
		// outer handleConn loop will hit EOF on its next ReadFrame
		// and exit through the usual disconnect path.
		s.handleAttach(c, req)

	case MsgPeekReq:
		var req PeekReq
		if err := json.Unmarshal(raw, &req); err != nil {
			s.writeError(c, "bad_request", err.Error())
			return
		}
		// handlePeek does NOT block — it ships a snapshot then returns,
		// leaving the conn in JSON mode. The client closes its dedicated
		// peek conn after reading the snapshot, so handleConn hits EOF
		// next and disconnects cleanly.
		if !s.runHandlerWithDeadline("peek", func() { s.handlePeek(c, req) }) {
			s.writeError(c, "daemon_unresponsive", "peek handler timed out")
		}

	case MsgSetViewingReq:
		var req SetViewingReq
		if err := json.Unmarshal(raw, &req); err != nil {
			s.writeError(c, "bad_request", err.Error())
			return
		}
		var resp SetViewingResp
		if s.runHandlerWithDeadline("set_viewing", func() { resp = s.handleSetViewing(c, req) }) {
			s.writeResp(c, MsgSetViewingResp, resp)
		} else {
			s.writeError(c, "daemon_unresponsive", "set_viewing handler timed out")
		}

	case MsgHealthReq:
		var req HealthReq
		_ = json.Unmarshal(raw, &req)
		var resp HealthResp
		if s.runHandlerWithDeadline("health", func() { resp = s.handleHealth(c, req) }) {
			s.writeResp(c, MsgHealthResp, resp)
		} else {
			s.writeError(c, "daemon_unresponsive", "health handler timed out")
		}

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
		existing := s.reg.findByTicket(req.TicketID)
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

	// Re-check under writeMu (inside storeIfNoTicket) to close the
	// construct-outside-lock race window: two concurrent spawns may both
	// have seen no existing session and both called NewSession. Exactly
	// one wins the writeMu; the other discards its just-built session.
	winner, stored := s.reg.storeIfNoTicket(req.TicketID, sess.ID(), sess)
	if !stored {
		log.Printf("openkanbankd: client %d spawn lost race ticket=%s discarding new session in favor of %s",
			c.id, req.TicketID, winner.ID())
		if killErr := sess.Kill(0); killErr != nil {
			log.Printf("openkanbankd: cleanup of race-loser session %s: %v", sess.ID(), killErr)
		}
		return SpawnResp{SessionID: winner.ID(), PID: winner.pane.PID()}, nil
	}

	log.Printf("openkanbankd: client %d spawned session %s (ticket=%s pid=%d)", c.id, sess.ID(), sess.TicketID(), sess.pane.PID())

	// Wire pane-exit observation BEFORE announcing the spawn so we
	// can't miss an ExitEvent fired by a child that races out the
	// gate. The watcher emits an "exited" SessionEvent when the pane
	// publishes its final ExitEvent.
	s.watchSessionExit(sess)

	s.emitEvent(SessionEvent{Event: "started", SessionID: sess.ID(), TicketID: sess.TicketID(), Status: "working", LastActivityAt: sess.LastActivity()})

	return SpawnResp{SessionID: sess.ID(), PID: sess.pane.PID()}, nil
}

// watchSessionExit subscribes to sess.pane's event stream and emits an
// "exited" SessionEvent when the pane publishes its final ExitEvent
// (i.e. the underlying child process closed its PTY). Idempotent for
// the daemon broadcast: if handleKill already emitted "exited",
// subscribers will see both — fine. If sess is removed from
// the registry before the exit fires, we still emit so cross-instance
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
			if s.reg.deleteIf(sessID, sess) {
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
				if other := s.reg.findByTicket(ticketID); other != nil {
					log.Printf("WARN: openkanbankd: after removing session %s, another session %s still references ticket %s — invariant violation",
						sessID, other.ID(), ticketID)
				}
			}
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
	infos := make([]SessionInfo, 0)
	for _, sess := range s.reg.snapshot() {
		infos = append(infos, sess.Info())
	}
	return ListResp{Sessions: infos}
}

func (s *Server) handleKill(c *clientConn, req KillReq) (KillResp, error) {
	sess, ok := s.reg.get(req.SessionID)
	if ok {
		s.reg.delete(req.SessionID)
	}

	if !ok {
		// Idempotent: a concurrent Kill / TicketDone / delete path may
		// have already removed the session. Return success rather than
		// an error so callers don't toast "session not found" for what
		// is functionally a no-op.
		log.Printf("openkanbankd: client %d kill on unknown session %s (no-op)", c.id, req.SessionID)
		return KillResp{}, nil
	}

	log.Printf("openkanbankd: client %d killed session %s", c.id, req.SessionID)

	// watchSessionExit emits the "exited" SessionEvent once the pane
	// publishes its final ExitEvent; emitting again here would result
	// in subscribers seeing two "exited" frames for one death. The Kill
	// path inherits whatever Expected/Reason the watcher decides (false
	// / "natural_exit" by default, true / "ticket_done" if the
	// TicketDone path got there first).
	//
	// Kill is async via trackedKill so we return success immediately and
	// account for in-flight kills; the kill itself runs in a goroutine.
	s.trackedKill(sess, req.GraceSeconds)

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

	snap := s.reg.snapshot()
	matches := make([]*Session, 0, 1)
	for _, sess := range snap {
		if sess.TicketID() == req.TicketID {
			matches = append(matches, sess)
		}
	}
	for _, m := range matches {
		s.reg.deleteIf(m.ID(), m)
	}

	if len(matches) == 0 {
		return TicketDoneResp{Killed: false}, nil
	}

	if len(matches) > 1 {
		log.Printf("WARN: openkanbankd: client %d ticket-done found %d sessions for ticket=%s (pre-dedup duplicates); terminating all", c.id, len(matches), req.TicketID)
	}

	for _, m := range matches {
		m.MarkExpectedCompletion()
		log.Printf("openkanbankd: client %d ticket-done session %s (ticket=%s)", c.id, m.ID(), req.TicketID)
		// Kill via trackedKill so the RPC returns synchronously and we account
		// for in-flight kills. The grace window matches shutdownGraceSeconds —
		// agents may have a few seconds of cleanup. The watcher emits the
		// "exited" event when the pane's ExitEvent lands.
		s.trackedKill(m, shutdownGraceSeconds)
	}

	return TicketDoneResp{SessionID: matches[0].ID(), Killed: true}, nil
}

// handleOwns answers whether the daemon currently owns one or more
// agent sessions whose Claude / opencode UUID matches req.SessionUUID.
//
// Sessions record their agent UUID at spawn time
// (SpawnReq.AgentSessionUUID → Session.agentSessionUUID). We walk all
// live sessions via a registry snapshot and collect every match. The
// caller distinguishes three states:
//
//   - len(matches) == 0: Owned=false (no caller action; either fresh
//     spawn or a pre-back-fill state).
//   - len(matches) == 1: Owned=true with SessionID + OwnedByTicketID
//     populated. The caller compares OwnedByTicketID against its
//     requesting ticket to decide idempotent re-attach vs foreign
//     ownership refuse.
//   - len(matches) > 1: Owned=true, Conflict=true,
//     ConflictSessionIDs lists every session ID. The 1:1 invariant
//     has been violated by something upstream — the caller refuses
//     and surfaces the multi-match to the user.
//
// Empty UUIDs never match: a Spawn made without --session carries
// AgentSessionUUID="" and an Owns query with SessionUUID="" is
// ill-formed and reported as Owned=false.
func (s *Server) handleOwns(c *clientConn, req OwnsReq) OwnsResp {
	if req.SessionUUID == "" {
		return OwnsResp{Owned: false}
	}
	var matches []*Session
	for _, sess := range s.reg.snapshot() {
		if sess.AgentSessionUUID() == req.SessionUUID {
			matches = append(matches, sess)
		}
	}
	switch len(matches) {
	case 0:
		return OwnsResp{Owned: false}
	case 1:
		return OwnsResp{
			Owned:           true,
			SessionID:       matches[0].ID(),
			SessionName:     matches[0].SessionName(),
			OwnedByTicketID: matches[0].TicketID(),
		}
	default:
		// > 1: conflict. Return Owned=true so old clients reading only
		// the legacy fields see "daemon claims this UUID" (their
		// behavior won't be MORE wrong than today's first-match). New
		// clients see Conflict=true and refuse.
		conflictIDs := make([]string, 0, len(matches))
		for _, sess := range matches {
			conflictIDs = append(conflictIDs, sess.ID())
		}
		return OwnsResp{
			Owned:              true,
			SessionID:          matches[0].ID(),
			SessionName:        matches[0].SessionName(),
			OwnedByTicketID:    matches[0].TicketID(),
			Conflict:           true,
			ConflictSessionIDs: conflictIDs,
		}
	}
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
	sess, ok := s.reg.get(req.SessionID)
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

// handleHealth returns the daemon's runtime counters — used by
// `openkanban daemon health` and the client's wedge diagnostics.
func (s *Server) handleHealth(c *clientConn, req HealthReq) HealthResp {
	seq, inflight := s.dispatchStats()
	kills, reapFail := s.killStats()
	return HealthResp{
		Goroutines:       runtime.NumGoroutine(),
		Sessions:         s.reg.len(),
		InflightHandlers: inflight,
		InflightKills:    kills,
		ReapFailures:     reapFail,
		DispatchSeq:      seq,
		PID:              os.Getpid(),
	}
}

// cleanupViewersForClient removes clientID from every session's
// viewers set and emits an "unviewing" SessionEvent for each session
// where the client was actually a viewer. Called from the disconnect
// path so a crashed or unceremoniously-closed TUI doesn't leave
// zombie viewer counts on sibling boards.
func (s *Server) cleanupViewersForClient(clientID uint16) {
	for _, sess := range s.reg.snapshot() {
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

	infos := make([]SessionInfo, 0)
	for _, sess := range s.reg.snapshot() {
		infos = append(infos, sess.Info())
	}

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
	live := s.reg.drain()

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
// zero. In persistent mode (launchd / systemd integration), the daemon
// stays up and only logs; explicit ShutdownReq or signals are the exit
// paths.
//
// In default mode the daemon is not supposed to outlive its TUI, so a
// last-client-disconnect with no live sessions shuts down immediately.
// But if sessions ARE still alive at that moment, the TUI's exit-guard
// failed to capture user intent — and force-killing live agent work to
// preserve the "daemon doesn't outlive the TUI" invariant compounds the
// bug. Instead we DEFER: spawn awaitSessionDrain to keep the daemon
// alive until the registry drains naturally, then shut down (so we don't
// linger as an orphan). A future TUI may also re-attach in the meantime.
func (s *Server) handleLastClientDisconnect() {
	live := s.reg.len()

	if s.persistent {
		log.Printf("openkanbankd: last client disconnected; staying up (persistent mode); %d live session(s)", live)
		return
	}

	if live > 0 {
		s.drainMu.Lock()
		start := !s.drainPending
		if start {
			s.drainPending = true
		}
		s.drainMu.Unlock()
		if start {
			log.Printf("openkanbankd: last client disconnected with %d live session(s); deferring shutdown until they exit (default mode no longer force-kills live work)", live)
			go s.awaitSessionDrain()
		}
		// Do NOT initiateShutdown — sessions are work, not transient UI
		// state. awaitSessionDrain owns the eventual shutdown.
		return
	}

	log.Printf("openkanbankd: last client disconnected; shutting down")
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

func (s *Server) dispatchStats() (uint64, int64) {
	return s.dispatchSeq.Load(), s.inflight.Load()
}
