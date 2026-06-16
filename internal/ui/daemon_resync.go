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
// reconcile makes before giving up. Three attempts at a 10s timeout
// with linear backoff give roughly 30s of headroom for a
// slow-autostarting daemon (cold-start TUI on a host where
// openkanbankd is launched by the same TUI invocation).
const startupReconcileAttempts = 3

// startupReconcileTimeout is the per-attempt context timeout for the
// List RPC. Raised from the original 2s after evidence that 2s was
// tight enough to miss a daemon under load — the consequence (every
// session invisible to this TUI) was disproportionate to the cost of
// waiting longer.
const startupReconcileTimeout = 10 * time.Second

// startupReconcileBackoff is the linear backoff between failed List
// attempts during startup. Linear (not exponential) because the
// failure mode we're absorbing is "daemon is busy / slow" which
// resolves on its own timescale; exponential would just push the
// failure-surface to the user with more delay.
const startupReconcileBackoff = 500 * time.Millisecond

// daemonResyncRPCTimeout is the per-attempt context timeout for the
// periodic 30s resync List RPC. Deliberately shorter than the
// startup timeout: at this point the TUI is already running with a
// reconciled session set, so a slow / stuck daemon should fail fast
// and the next tick (30s away) re-tries. Sharing the 10s startup
// budget here meant a wedged daemon could keep the periodic Update
// goroutine blocked for a third of the tick interval before re-arming.
const daemonResyncRPCTimeout = 3 * time.Second

// startupReconcileFailureMsg is the toast the user sees when every
// retry failed. It points at "restart openkanban" because the rest of
// the TUI is going to behave as if no sessions are owned by the
// daemon (no Unattached panes will appear in m.panes), which is
// silently wrong — better to surface the inconsistency.
const startupReconcileFailureMsg = "Daemon reconcile failed; restart openkanban to re-sync"

// daemonListAPI is the subset of daemonGuardAPI the resync paths
// touch. Held separately from the full guard interface so the
// reconcile helper can take a narrower seam, but in practice every
// call site uses m.guardAPI (which already satisfies it via the
// extension in exit_guard.go).
type daemonListAPI interface {
	List(ctx context.Context) (daemon.ListResp, error)
}

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
// timeout is bounded by timeout; the outer call is synchronous —
// designed to be invoked from NewModel where blocking briefly at
// startup is preferable to an empty-pane window.
//
// Returned even on partial success (one attempt succeeded after
// retries) — the caller can't tell from the result map whether retries
// fired, only from the absence of error.
func listSessionsWithRetry(api daemonListAPI, attempts int, timeout, backoff time.Duration) (map[board.TicketID]daemon.SessionInfo, error) {
	if api == nil {
		return map[board.TicketID]daemon.SessionInfo{}, nil
	}
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i == 0 {
			// Stderr breadcrumb so a slow startup reconcile isn't a
			// silent freeze. The worst case here is ~30s
			// (attempts * timeout); without this line the user staring
			// at the launching TUI has no signal that anything is
			// happening. A real spinner UI is the right long-term fix
			// (tracked separately as ui-spinner-for-long-running-
			// daemon-ops) — this is the minimum the code-review pass
			// asked for.
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

// scheduleDaemonResync returns a tea.Cmd that fires a
// daemonResyncTickMsg after daemonResyncInterval. Used both by Init
// (to arm the first tick) and by the resync handler (to re-arm after
// each completed reconcile).
//
// Returns nil when there's no guardAPI — without a daemon there's
// nothing to resync against. Callers should batch this in
// unconditionally; the nil-cmd is dropped by tea.Batch.
func (m *Model) scheduleDaemonResync() tea.Cmd {
	if m.guardAPI == nil {
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
	if m.guardAPI == nil {
		// Daemon went away mid-run; stop the timer chain. The Init
		// wiring will re-arm on the next NewModel call.
		return m, nil
	}
	api := m.guardAPI
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
		return m, m.scheduleDaemonResync()
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
		// (daemon disconnected mid-run, guardAPI satisfied by a fake
		// in tests) even though m.guardAPI is non-nil. NewPaneView with
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
