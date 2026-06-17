package daemonclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/techdufus/openkanban/internal/daemon"
)

// Client is the openkanbankd control-channel client. One per TUI
// process. Owns a single long-lived net.Conn that carries:
//
//   - JSON-mode RPCs (Hello / Spawn / List / Kill / Owns / Subscribe /
//     PrepareExit / Shutdown). Only one RPC may be in flight at a time;
//     the round-trip is serialized by rpcMu so push frames (TypeJSONPush)
//     can interleave between RPCs without confusing the response demux.
//   - Server-pushed SessionEvent frames, fanned out to in-process
//     subscribers registered via Subscribe.
//
// PaneView attach connections do NOT share this conn — each PaneView
// dials a fresh net.Conn so the binary upgrade only takes that one
// down.
//
// Lifecycle: New() does the Hello handshake and spawns a single
// read-loop goroutine. Close() tears the conn down and signals
// in-flight RPCs to fail with ErrDaemonUnavailable.
type Client struct {
	conn net.Conn
	r    *bufio.Reader

	writeMu sync.Mutex // serializes writes to conn
	rpcMu   sync.Mutex // serializes JSON RPC round-trips

	// pending receives the next TypeJSONResp frame the read loop sees.
	// Set up under rpcMu before each RPC's WriteFrame; cleared after
	// the response arrives or rpcMu is released. The read loop pushes
	// non-blocking — if pending is nil (no in-flight RPC) the response
	// is logged and dropped, since that indicates a server bug.
	pendingMu sync.Mutex
	pending   chan rawResp

	helloOnce      sync.Once
	clientID       uint16
	protocolVer    uint16
	binaryVer      string
	clientCountVal atomic.Int64 // last-known count (Hello / PrepareExit)

	subscribersMu sync.Mutex
	subscribers   map[chan<- daemon.SessionEvent]struct{}
	subscribeOnce sync.Once

	closed  atomic.Bool
	closeCh chan struct{}

	readLoopWG sync.WaitGroup

	// pushCount / dropCount are diagnostic counters: total SessionEvents
	// received from the daemon, and total dropped because a subscriber's
	// buffer was full. Read by the TUI stall watchdog to distinguish a
	// starved main loop (pushes arriving, Update not running) from
	// genuine idle. See internal/ui/stallwatch.go.
	pushCount atomic.Uint64
	dropCount atomic.Uint64
}

// DiagCounters returns the cumulative (received, dropped) SessionEvent
// counts. Used by the TUI stall watchdog; safe to call concurrently.
func (c *Client) DiagCounters() (push, drop uint64) {
	return c.pushCount.Load(), c.dropCount.Load()
}

// rawResp is what the read loop hands to a waiting RPC: the type name
// from the envelope plus the raw payload bytes. The RPC then unmarshals
// payload into its concrete response type.
type rawResp struct {
	typeName string
	payload  json.RawMessage
	err      error
}

// clientNameForHello is the identifier the daemon sees in HelloReq.
// Kept distinct from BinaryVersion so the daemon's logs can tell apart
// e.g. concurrent TUI clients on the same host. Sourced from
// internal/daemon's ClientNameTUI constant so server-side TUI counting
// (handlePrepareExit) and client-side naming agree.
const clientNameForHello = daemon.ClientNameTUI

// New dials the daemon (autostarting if needed), performs the Hello
// handshake, and spawns the read loop. Returns the connected Client on
// success or ErrDaemonUnavailable wrapping the cause if the daemon
// can't be reached.
func New(ctx context.Context) (*Client, error) {
	conn, err := DialOrStart(ctx)
	if err != nil {
		return nil, err
	}
	return newClient(ctx, conn)
}

// NewNoAutostart is New without the on-demand fork: it dials the
// existing daemon and never tries to start one. Used by the TUI when
// the user passes --no-launch-daemon or sets daemon.autostart=false
// in config, indicating openkanbankd is managed externally (e.g. by
// launchd). Returns ErrDaemonUnavailable wrapping the dial cause if
// the daemon isn't already listening.
func NewNoAutostart(ctx context.Context) (*Client, error) {
	conn, err := Dial(ctx)
	if err != nil {
		return nil, err
	}
	return newClient(ctx, conn)
}

// NewWithConn wraps an already-dialed net.Conn in a Client without
// dialing or autostarting. Used by short-lived probe paths (e.g.
// cmd/ticket.go's Owns / Kill flows) that have already done their own
// Dial — typically because they want a quick negative answer when the
// daemon isn't running, rather than the multi-second autostart wait
// New() does. On Hello failure the conn is closed.
func NewWithConn(ctx context.Context, conn net.Conn) (*Client, error) {
	return newClient(ctx, conn)
}

// newClient is the shared body of New / NewWithConn: install the conn,
// start the read loop, do the Hello handshake.
func newClient(ctx context.Context, conn net.Conn) (*Client, error) {
	c := &Client{
		conn:        conn,
		r:           bufio.NewReader(conn),
		subscribers: make(map[chan<- daemon.SessionEvent]struct{}),
		closeCh:     make(chan struct{}),
	}

	// Start the read loop BEFORE Hello so the response frame is read
	// off the wire via the same demux path as everything else.
	c.readLoopWG.Add(1)
	go c.readLoop()

	resp, err := c.hello(ctx)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("daemonclient: hello: %w", err)
	}
	// Version-skew guard: if the daemon was started from an older (or
	// newer) openkanban binary, its wire protocol won't match the
	// client's compiled-in ProtocolVersion. Fail fast with a clear,
	// actionable error so the TUI degrades to no-daemon mode instead of
	// soldiering on with a mismatched codec.
	if resp.ProtocolVersion != daemon.ProtocolVersion {
		clientVer := daemon.ProtocolVersion
		daemonVer := resp.ProtocolVersion
		c.Close()
		return nil, fmt.Errorf("%w: client=%d, daemon=%d — run `openkanban daemon restart`",
			ErrProtocolVersionSkew, clientVer, daemonVer)
	}
	c.clientID = resp.ClientID
	c.protocolVer = resp.ProtocolVersion
	c.binaryVer = resp.BinaryVersion
	c.clientCountVal.Store(int64(resp.ClientCount))

	return c, nil
}

// Close terminates the conn and unblocks any in-flight RPC. Safe to
// call more than once. Subscribers receive a final read on closeCh —
// they MUST NOT receive a synthetic event here (the daemon disconnect
// is a separate concern handled by PaneView's attach loop and the
// upcoming PR9 status routing).
func (c *Client) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	close(c.closeCh)
	err := c.conn.Close()
	c.readLoopWG.Wait()

	// Wake any RPC waiter that may have raced the close.
	c.pendingMu.Lock()
	if c.pending != nil {
		select {
		case c.pending <- rawResp{err: ErrDaemonUnavailable}:
		default:
		}
		c.pending = nil
	}
	c.pendingMu.Unlock()

	// Drain subscriber registry — we do NOT close subscriber channels
	// because the channel owners (PR9 / the model) may still be ranging
	// over them. They'll observe the disconnect via Closed() or a
	// separate signal.
	return err
}

// Closed reports whether Close has been called on this client.
func (c *Client) Closed() bool {
	return c.closed.Load()
}

// ClientID returns the daemon-assigned ID for this client. 0 until
// Hello has completed.
func (c *Client) ClientID() uint16 { return c.clientID }

// ProtocolVersion returns the daemon-reported protocol version.
func (c *Client) ProtocolVersion() uint16 { return c.protocolVer }

// BinaryVersion returns the daemon-reported binary version string.
func (c *Client) BinaryVersion() string { return c.binaryVer }

// ClientCount returns the daemon's last-known live client count (set
// at Hello and refreshed by PrepareExit).
//
// Deprecated: this value includes self and any peer that has also
// called PrepareExit, so it's unsafe for "am I the last one out?"
// decisions — multiple TUIs closing simultaneously all see ClientCount
// > 1 and race. Use the PrepareExit response's OtherActiveClients
// field instead, which the daemon computes atomically under clientsMu.
func (c *Client) ClientCount() int {
	return int(c.clientCountVal.Load())
}

// --- Read loop ---

// readLoop is the single reader of c.conn. It demuxes:
//
//   - TypeJSONResp frames → c.pending (one in-flight RPC at a time;
//     pending is set up under rpcMu before each WriteFrame).
//   - TypeJSONPush frames → fan out to registered subscribers.
//
// On any read error it closes pending (if any) with the error so the
// blocked RPC unblocks, marks the client closed, and returns. Pushes
// to a closed channel are dropped silently — subscribers that wanted
// reliable delivery should observe disconnect through Closed().
func (c *Client) readLoop() {
	defer c.readLoopWG.Done()

	for {
		if c.closed.Load() {
			return
		}
		typ, payload, err := daemon.ReadFrame(c.r)
		if err != nil {
			if err == io.EOF || errors.Is(err, net.ErrClosed) {
				c.signalDisconnect(nil)
				return
			}
			c.signalDisconnect(err)
			return
		}

		switch typ {
		case daemon.TypeJSONResp:
			c.deliverResp(payload, nil)
		case daemon.TypeJSONPush:
			c.handlePush(payload)
		default:
			// Unknown / unexpected frame type on the control conn.
			// Log via stderr is too noisy for a library; just drop.
			runtime.Gosched()
		}
	}
}

// signalDisconnect runs the once-only post-read-error cleanup: mark
// closed and wake any in-flight RPC.
func (c *Client) signalDisconnect(err error) {
	if c.closed.Swap(true) {
		return
	}
	close(c.closeCh)
	_ = c.conn.Close()

	c.pendingMu.Lock()
	if c.pending != nil {
		e := err
		if e == nil {
			e = ErrDaemonUnavailable
		}
		select {
		case c.pending <- rawResp{err: e}:
		default:
		}
		c.pending = nil
	}
	c.pendingMu.Unlock()
}

func (c *Client) deliverResp(payload []byte, env error) {
	c.pendingMu.Lock()
	pending := c.pending
	c.pendingMu.Unlock()
	if pending == nil {
		// No in-flight RPC. Indicates a server bug — drop.
		return
	}

	typeName, raw, err := daemon.DecodeEnvelope(payload)
	if err != nil {
		select {
		case pending <- rawResp{err: fmt.Errorf("decode envelope: %w", err)}:
		default:
		}
		return
	}
	select {
	case pending <- rawResp{typeName: typeName, payload: raw, err: env}:
	default:
		// Buffer was 1, so this only happens if the RPC already
		// timed out. Drop.
	}
}

func (c *Client) handlePush(payload []byte) {
	typeName, raw, err := daemon.DecodeEnvelope(payload)
	if err != nil {
		log.Printf("openkanban client: handlePush decode err: %v", err)
		return
	}
	if typeName != daemon.MsgSessionEvent {
		log.Printf("openkanban client: handlePush unknown type %q", typeName)
		return
	}
	var ev daemon.SessionEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		log.Printf("openkanban client: handlePush unmarshal err: %v", err)
		return
	}
	c.pushCount.Add(1)

	c.subscribersMu.Lock()
	subs := make([]chan<- daemon.SessionEvent, 0, len(c.subscribers))
	for ch := range c.subscribers {
		subs = append(subs, ch)
	}
	c.subscribersMu.Unlock()

	log.Printf("openkanban client: handlePush event=%q session=%s subs=%d", ev.Event, ev.SessionID, len(subs))
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
			c.dropCount.Add(1)
			log.Printf("openkanban client: handlePush dropped event %q (subscriber buffer full)", ev.Event)
		}
	}
}

// --- RPC primitives ---

// do performs a single JSON request and waits for the matching response
// frame on c.conn. Serialized by rpcMu so only one in-flight at a time.
func (c *Client) do(ctx context.Context, reqType string, req any, expectResp string, out any) error {
	if c.closed.Load() {
		return ErrDaemonUnavailable
	}

	payload, err := daemon.EncodeMsg(reqType, req)
	if err != nil {
		return fmt.Errorf("daemonclient: encode %s: %w", reqType, err)
	}

	c.rpcMu.Lock()
	defer c.rpcMu.Unlock()

	pending := make(chan rawResp, 1)
	c.pendingMu.Lock()
	c.pending = pending
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		c.pending = nil
		c.pendingMu.Unlock()
	}()

	c.writeMu.Lock()
	werr := daemon.WriteFrameCtx(ctx, c.conn, daemon.TypeJSONReq, payload)
	c.writeMu.Unlock()
	if werr != nil {
		// A write failure (deadline against a wedged daemon, EPIPE, etc.)
		// leaves the conn poisoned — a half-written frame would desync
		// the next RPC. Tear the client down so subsequent calls fail
		// fast with ErrDaemonUnavailable instead of reusing it; mirrors
		// readLoop's disconnect-on-error behavior. The wrapped error still
		// carries daemon.ErrDaemonUnresponsive for errors.Is at the caller.
		c.signalDisconnect(werr)
		return fmt.Errorf("daemonclient: write %s: %w", reqType, werr)
	}

	handle := func(r rawResp) error {
		if r.err != nil {
			return r.err
		}
		if r.typeName == daemon.MsgErrorResp {
			var er daemon.ErrorResp
			if err := json.Unmarshal(r.payload, &er); err != nil {
				return fmt.Errorf("daemonclient: decode error response: %w", err)
			}
			return fmt.Errorf("daemonclient: %s: %s", er.Code, er.Message)
		}
		if r.typeName != expectResp {
			return fmt.Errorf("daemonclient: %s: unexpected response %q", reqType, r.typeName)
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(r.payload, out); err != nil {
			return fmt.Errorf("daemonclient: decode %s: %w", expectResp, err)
		}
		return nil
	}

	select {
	case r := <-pending:
		return handle(r)
	case <-ctx.Done():
		// A response delivered concurrently with cancellation still wins:
		// the round-trip completed. select fires randomly when both are
		// ready, so re-check pending non-blockingly before reporting the
		// context error.
		select {
		case r := <-pending:
			return handle(r)
		default:
		}
		return ctx.Err()
	case <-c.closeCh:
		// Same for disconnect: the read loop delivers our response to the
		// (buffered) pending channel before it can hit EOF and close
		// closeCh, so a daemon that closes immediately after replying
		// (e.g. the version-skew path) must not be reported as
		// "unavailable" for an RPC that actually got its answer.
		select {
		case r := <-pending:
			return handle(r)
		default:
		}
		return ErrDaemonUnavailable
	}
}

// --- Per-message RPC methods ---

// hello is the first RPC sent on a fresh conn. Internal — called from
// New as part of construction.
func (c *Client) hello(ctx context.Context) (daemon.HelloResp, error) {
	var resp daemon.HelloResp
	err := c.do(ctx, daemon.MsgHelloReq, daemon.HelloReq{
		ProtocolVersion: daemon.ProtocolVersion,
		BinaryVersion:   daemon.BinaryVersion,
		ClientName:      clientNameForHello,
	}, daemon.MsgHelloResp, &resp)
	return resp, err
}

// Spawn asks the daemon to start a new PTY-backed session.
func (c *Client) Spawn(ctx context.Context, req daemon.SpawnReq) (daemon.SpawnResp, error) {
	var resp daemon.SpawnResp
	err := c.do(ctx, daemon.MsgSpawnReq, req, daemon.MsgSpawnResp, &resp)
	return resp, err
}

// List returns the daemon's current set of sessions.
func (c *Client) List(ctx context.Context) (daemon.ListResp, error) {
	var resp daemon.ListResp
	err := c.do(ctx, daemon.MsgListReq, daemon.ListReq{}, daemon.MsgListResp, &resp)
	return resp, err
}

// Kill asks the daemon to terminate sessionID. graceSeconds is the
// SIGTERM-to-SIGKILL window; 0 = immediate kill.
func (c *Client) Kill(ctx context.Context, sessionID string, grace time.Duration) error {
	graceSeconds := int(grace / time.Second)
	if grace > 0 && graceSeconds <= 0 {
		graceSeconds = 1
	}
	return c.do(ctx, daemon.MsgKillReq, daemon.KillReq{
		SessionID:    sessionID,
		GraceSeconds: graceSeconds,
	}, daemon.MsgKillResp, nil)
}

// Owns asks whether the daemon currently owns the agent session with
// the given Claude / opencode UUID.
func (c *Client) Owns(ctx context.Context, sessionUUID string) (daemon.OwnsResp, error) {
	var resp daemon.OwnsResp
	err := c.do(ctx, daemon.MsgOwnsReq, daemon.OwnsReq{SessionUUID: sessionUUID},
		daemon.MsgOwnsResp, &resp)
	return resp, err
}

// SetViewing tells the daemon that this client is now (or no longer)
// in agent-view mode on sessionID. The daemon broadcasts a "viewing"
// or "unviewing" SessionEvent to subscribers when the call changes
// state — the board's TUI-viewing indicator on sibling instances is
// driven off that. Idempotent: SetViewing(true) twice in a row is a
// silent no-op the second time. Disconnect implicitly clears all of
// this client's SetViewing(true) calls.
//
// Returns the post-call viewer count for sessionID. Unknown
// sessionID returns 0 with no error — TUIs may race the daemon's
// "exited" event for a session they were viewing.
func (c *Client) SetViewing(ctx context.Context, sessionID string, viewing bool) (int, error) {
	var resp daemon.SetViewingResp
	err := c.do(ctx, daemon.MsgSetViewingReq, daemon.SetViewingReq{
		SessionID: sessionID,
		Viewing:   viewing,
	}, daemon.MsgSetViewingResp, &resp)
	return resp.ViewerCount, err
}

// TicketDone informs the daemon that the agent invoked `openkanban
// ticket done` or `openkanban ticket in-review` for ticketID — both
// wrap-up motions take the same daemon path. The daemon scans its live
// sessions for one bound to that ticket, flips its expected-completion
// flag, and kills the pane — the resulting SessionEvent broadcast
// carries Expected=true / Reason="ticket_done" so subscribed TUIs
// preserve AgentCompleted instead of resetting to AgentNone.
//
// On miss (no live session for ticketID), the response carries
// Killed=false with no error. The caller is expected to treat the .md
// and status-file writes as authoritative either way.
func (c *Client) TicketDone(ctx context.Context, ticketID string) (daemon.TicketDoneResp, error) {
	var resp daemon.TicketDoneResp
	err := c.do(ctx, daemon.MsgTicketDoneReq, daemon.TicketDoneReq{TicketID: ticketID},
		daemon.MsgTicketDoneResp, &resp)
	return resp, err
}

// Subscribe registers a fresh in-process channel for daemon push
// events. The first call sends MsgSubscribeReq to the daemon so it
// knows to start pushing; subsequent calls just register a local
// channel. The returned cancel func removes the channel from the
// registry but does NOT close it — the caller owns the channel and is
// expected to drain or discard it.
//
// The channel is buffered (cap 64); pushes that would block are dropped
// silently in handlePush. PR9 will revisit the back-pressure policy.
func (c *Client) Subscribe(ctx context.Context) (<-chan daemon.SessionEvent, func(), error) {
	ch := make(chan daemon.SessionEvent, 64)

	c.subscribersMu.Lock()
	c.subscribers[ch] = struct{}{}
	c.subscribersMu.Unlock()

	var sendErr error
	c.subscribeOnce.Do(func() {
		sendErr = c.do(ctx, daemon.MsgSubscribeReq, daemon.SubscribeReq{},
			daemon.MsgSubscribeResp, nil)
	})
	if sendErr != nil {
		c.subscribersMu.Lock()
		delete(c.subscribers, ch)
		c.subscribersMu.Unlock()
		return nil, nil, sendErr
	}

	cancel := func() {
		c.subscribersMu.Lock()
		delete(c.subscribers, ch)
		c.subscribersMu.Unlock()
	}
	return ch, cancel, nil
}

// PrepareExit asks the daemon for the live-client / live-session
// summary used by the TUI's exit-guard. Also refreshes ClientCount.
// The daemon atomically marks this connection as exiting; pair with
// CancelExit when the user dismisses the exit-confirm modal.
func (c *Client) PrepareExit(ctx context.Context) (daemon.PrepareExitResp, error) {
	var resp daemon.PrepareExitResp
	err := c.do(ctx, daemon.MsgPrepareExitReq, daemon.PrepareExitReq{},
		daemon.MsgPrepareExitResp, &resp)
	if err == nil {
		c.clientCountVal.Store(int64(resp.ClientCount))
	}
	return resp, err
}

// CancelExit clears the exit-intent flag set by a prior PrepareExit on
// this connection. The TUI calls this when the user dismisses the
// exit-confirm modal so subsequent PrepareExit RPCs from peer TUIs see
// this client as active again. Idempotent.
func (c *Client) CancelExit(ctx context.Context) error {
	var resp daemon.CancelExitResp
	return c.do(ctx, daemon.MsgCancelExitReq, daemon.CancelExitReq{},
		daemon.MsgCancelExitResp, &resp)
}

// Shutdown asks the daemon to exit. force=false refuses if any sessions
// are still alive.
func (c *Client) Shutdown(ctx context.Context, force bool) (daemon.ShutdownResp, error) {
	var resp daemon.ShutdownResp
	err := c.do(ctx, daemon.MsgShutdownReq, daemon.ShutdownReq{Force: force},
		daemon.MsgShutdownResp, &resp)
	return resp, err
}
