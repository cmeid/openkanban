package ui

import (
	"context"
	"log"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
)

// daemonResyncInterval is the cadence at which the TUI re-asks the
// daemon for its current session set. 30s is a tradeoff: long enough
// that sibling TUI traffic and the activity-heartbeat firehose remain
// the primary signal channel, short enough that a missed lifecycle
// event (rare, but possible across daemon restart or under burst
// load) self-heals within half a minute.
const daemonResyncInterval = 30 * time.Second

// startupReconcileAttempts is the number of List attempts the startup
// PREFLIGHT (PreflightListSessions, called from internal/app before the
// TUI is built) makes before giving up. The preflight gates launch: on
// success its snapshot seeds NewModel; on exhaustion the caller prints a
// PID+kill hint and EXITS rather than launching a daemon-less board. That
// exit consequence is why the budget is kept short (fast-fail a wedged
// daemon) but not a single shot (tolerate a transiently-slow one) — the
// dial has already waited for the daemon to come up, so a healthy daemon
// answers the first List almost immediately.
const startupReconcileAttempts = 2

// startupReconcileTimeout is the per-attempt context timeout for the
// preflight List RPC. Shortened from the old 10s reconcile budget: the
// old code DEGRADED on timeout (sessions merely invisible), so a generous
// wait was cheap; the preflight EXITS, so a wedged daemon must surface
// fast. 3s is long enough that a healthy-but-loaded daemon answers, short
// enough that a wedge is reported in a handful of seconds, not minutes.
const startupReconcileTimeout = 3 * time.Second

// startupReconcileBackoff is the linear backoff between failed preflight
// List attempts. Linear (not exponential) because the failure mode we're
// absorbing is "daemon is briefly busy" which resolves on its own
// timescale; exponential would just delay the message-and-exit.
const startupReconcileBackoff = 500 * time.Millisecond

// startupSubscribeTimeout bounds the daemon Subscribe handshake, which is
// armed asynchronously from Init (subscribeDaemonEventsCmd). The handshake
// previously ran synchronously in NewModel under context.Background(): a
// wedged daemon — one that accepts the connection and completes hello but
// never answers SubscribeReq — blocked NewModel forever, before the
// bubbletea loop started, so the TUI never painted. A few seconds is ample
// for a healthy daemon's handshake; on a wedged one the deadline fires and
// the status-file poll takes over via daemonSubscribeFailedMsg.
const startupSubscribeTimeout = 5 * time.Second

// daemonResyncRPCTimeout is the per-attempt context timeout for the
// periodic 30s resync List RPC. Deliberately shorter than the
// startup timeout: at this point the TUI is already running with a
// reconciled session set, so a slow / stuck daemon should fail fast
// and the next tick (30s away) re-tries. Sharing the 10s startup
// budget here meant a wedged daemon could keep the periodic Update
// goroutine blocked for a third of the tick interval before re-arming.
const daemonResyncRPCTimeout = 3 * time.Second

// daemonResyncTickMsg fires when the 30s resync timer expires. The
// Update handler turns it into an actual List RPC via a tea.Cmd so
// the network call doesn't block the Update goroutine.
type daemonResyncTickMsg struct{}

// daemonResyncMsg carries the result of one List RPC back to the
// Update loop. The handler reconciles m.panes / m.daemonOwned against
// the snapshot, then re-arms the next tick.
type daemonResyncMsg struct {
	sessions map[board.TicketID]daemon.SessionInfo
	err      error
}

// listSessionsWithRetry calls api.List up to attempts times, with a
// linear backoff between failures. Returns the per-TicketID map on
// success or the last error on exhaustion. The per-attempt context
// timeout is bounded by timeout; the call is synchronous — invoked from
// the startup preflight (PreflightListSessions) where a brief bounded
// wait is the gate that decides launch-vs-exit.
//
// Returned even on partial success (one attempt succeeded after
// retries) — the caller can't tell from the result map whether retries
// fired, only from the absence of error.
func listSessionsWithRetry(api daemonAPI, attempts int, timeout, backoff time.Duration) (map[board.TicketID]daemon.SessionInfo, error) {
	if api == nil {
		return map[board.TicketID]daemon.SessionInfo{}, nil
	}
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i == 0 {
			// Log breadcrumb so a slow preflight isn't a silent freeze.
			// The worst case is attempts*timeout (a few seconds); without
			// this line the user staring at the still-launching terminal
			// has no signal that anything is happening before the board
			// paints (or the message-and-exit fires).
			log.Printf("openkanban: contacting daemon...")
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		resp, err := api.List(ctx)
		cancel()
		if err == nil {
			out := make(map[board.TicketID]daemon.SessionInfo, len(resp.Sessions))
			for _, s := range resp.Sessions {
				out[board.TicketID(s.TicketID)] = s
			}
			return out, nil
		}
		lastErr = err
		log.Printf("openkanban: daemon List attempt %d/%d failed: %v", i+1, attempts, err)
		if i < attempts-1 {
			time.Sleep(backoff * time.Duration(i+1))
		}
	}
	return nil, lastErr
}

// PreflightListSessions is the bounded startup probe internal/app runs
// against a freshly-dialed daemon BEFORE constructing the TUI. It serves
// two purposes at once:
//
//  1. Liveness gate: a wedged daemon (alive, accepting connections,
//     completing hello, but not answering RPCs) passes the dial but fails
//     this List. The caller turns a non-nil error into a PID+kill message
//     and a clean exit — never the old blank, unbounded hang.
//  2. Reconcile source: on success the returned snapshot is passed to
//     NewModel, which consumes it instead of issuing its own (formerly
//     blocking) startup List. One RPC, gated and bounded.
//
// nil client returns an empty snapshot and no error (degenerate/test
// path); the caller decides whether a nil client is itself an exit
// condition.
func PreflightListSessions(client *daemonclient.Client) (map[board.TicketID]daemon.SessionInfo, error) {
	if client == nil {
		return map[board.TicketID]daemon.SessionInfo{}, nil
	}
	return listSessionsWithRetry(client, startupReconcileAttempts, startupReconcileTimeout, startupReconcileBackoff)
}

// scheduleDaemonResync returns a tea.Cmd that fires a
// daemonResyncTickMsg after daemonResyncInterval. Used both by Init
// (to arm the first tick) and by the resync handler (to re-arm after
// each completed reconcile).
//
// Returns nil when m.daemon is missing — without a daemon there's
// nothing to resync against. Callers should batch this in
// unconditionally; the nil-cmd is dropped by tea.Batch.
func (m *Model) scheduleDaemonResync() tea.Cmd {
	if m.daemon == nil {
		return nil
	}
	return tea.Tick(daemonResyncInterval, func(time.Time) tea.Msg {
		return daemonResyncTickMsg{}
	})
}

// handleDaemonResyncTick fires when the periodic timer expires. It
// returns a tea.Cmd that calls List on the daemon and emits a
// daemonResyncMsg with the snapshot. The handler re-arms the next
// tick after applying the result, NOT here — re-arming here would
// create a runaway loop if the RPC takes longer than the interval.
func (m *Model) handleDaemonResyncTick() (tea.Model, tea.Cmd) {
	if m.daemon == nil {
		// Daemon went away mid-run; stop the timer chain. The Init
		// wiring will re-arm on the next NewModel call.
		return m, nil
	}
	api := m.daemon
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), daemonResyncRPCTimeout)
		defer cancel()
		resp, err := api.List(ctx)
		if err != nil {
			return daemonResyncMsg{err: err}
		}
		out := make(map[board.TicketID]daemon.SessionInfo, len(resp.Sessions))
		for _, s := range resp.Sessions {
			out[board.TicketID(s.TicketID)] = s
		}
		return daemonResyncMsg{sessions: out}
	}
}

// handleDaemonResyncMsg applies a periodic resync result. Diff
// semantics:
//
//   - For each daemon-owned TicketID NOT in m.panes: build a new
//     PaneView in PaneViewUnattached state (sibling-TUI spawn case).
//   - For each m.panes entry whose TicketID is NOT in the daemon's
//     list AND whose pane state is NOT Attached: remove it (external
//     kill — the daemon already broadcast "exited" but our subscribe
//     channel may have missed it across a reconnect).
//   - Attached panes that vanished from the daemon: leave them; the
//     binary-stream "exited" event flow tears them down naturally.
//     Forcing a cleanup here would race the natural path and double-
//     fire the close logic.
//
// AgentStatus is intentionally not touched in this path — the periodic
// path is a structural reconcile, not a state reset. The push-event
// channel + the file poll are the authoritative AgentStatus sources.
//
// Re-arms the next tick unconditionally so the chain continues even
// after a failed RPC. A failed RPC is logged but does not surface a
// notification — periodic-failure noise would drown out the signal,
// and the startup-time failure mode already shows the toast once.
func (m *Model) handleDaemonResyncMsg(msg daemonResyncMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		log.Printf("openkanban: periodic daemon resync failed: %v", msg.err)
		// The resync tick is the always-on observer of a dead control
		// conn (it fires even with no pane attached — the exact case
		// DaemonDisconnectedMsg misses). When the client is terminally
		// Closed (e.g. the daemon restarted on a stale-binary upgrade),
		// drive a reconnect alongside re-arming the tick.
		cmds := []tea.Cmd{m.scheduleDaemonResync()}
		if m.daemonClient != nil && m.daemonClient.Closed() {
			if rc := m.maybeReconnectDaemon(); rc != nil {
				cmds = append(cmds, rc)
			}
		}
		return m, tea.Batch(cmds...)
	}

	owned := msg.sessions
	if owned == nil {
		owned = map[board.TicketID]daemon.SessionInfo{}
	}

	// Pass 1: add panes for new daemon-owned tickets.
	for ticketID, info := range owned {
		if _, present := m.panes[ticketID]; present {
			// Already known; just refresh the daemonOwned bookkeeping
			// in case startup missed it.
			m.daemonOwned[ticketID] = struct{}{}
			continue
		}
		// Only build a PaneView for tickets the local store actually
		// knows about. If the daemon owns a session for a ticket this
		// store doesn't have (e.g. cross-project session in a sibling
		// TUI), we still want to track daemonOwned so the indicator
		// renders correctly, but a PaneView without a backing ticket
		// would dangle.
		ticket, _ := m.globalStore.Get(ticketID)
		if ticket == nil {
			m.daemonOwned[ticketID] = struct{}{}
			continue
		}
		// Defensive: m.daemonClient can be nil in degenerate states
		// (daemon disconnected mid-run, m.daemon satisfied by a fake
		// in tests) even though m.daemon is non-nil. NewPaneView with
		// a nil client builds a pane whose Attach path will dereference
		// nil — leave the daemonOwned bookkeeping so the indicator
		// still renders, but skip constructing a dangling pane.
		if m.daemonClient == nil {
			m.daemonOwned[ticketID] = struct{}{}
			continue
		}
		pv := daemonclient.NewPaneView(m.daemonClient, string(ticketID), info.SessionID, &info)
		if info.Workdir != "" {
			pv.SetWorkdir(info.Workdir)
		}
		if info.SessionName != "" {
			pv.SetSessionName(info.SessionName)
		}
		pv.SetTicketTitle(ticket.Title)
		m.panes[ticketID] = pv
		m.daemonOwned[ticketID] = struct{}{}
	}

	// Pass 2: remove m.panes entries the daemon no longer owns,
	// excluding Attached panes (the binary stream tears them down
	// via the normal exit event flow). Also clean m.daemonOwned for
	// tickets we'll never hear another event for.
	for ticketID, pv := range m.panes {
		if _, stillOwned := owned[ticketID]; stillOwned {
			continue
		}
		if pv != nil && pv.State() == daemonclient.PaneViewAttached {
			// Leave attached panes alone — the binary stream's
			// natural exit handling will reach them.
			continue
		}
		if pv != nil {
			_ = pv.Close()
		}
		delete(m.panes, ticketID)
		delete(m.daemonOwned, ticketID)
	}

	// Reconcile m.daemonOwned for tickets that no longer have a
	// pane but were marked owned (e.g. sibling-TUI sessions on
	// other projects that exited).
	for ticketID := range m.daemonOwned {
		if _, stillOwned := owned[ticketID]; stillOwned {
			continue
		}
		if _, hasPane := m.panes[ticketID]; hasPane {
			// Attached pane path handled above; skip.
			continue
		}
		delete(m.daemonOwned, ticketID)
	}

	return m, m.scheduleDaemonResync()
}
