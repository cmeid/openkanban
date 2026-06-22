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

// --- Tea messages for daemon-pushed session events ---

// daemonSessionEventMsg carries a single SessionEvent pushed by the
// daemon's subscribe channel into the model's Update loop. The model
// applies it to the matching ticket's AgentStatus and (when the event
// is "attached" or "detached") to its PaneView state.
type daemonSessionEventMsg struct {
	Event daemon.SessionEvent
}

// daemonSessionEventsMsg carries a BURST of SessionEvents drained from
// the subscribe channel in a single read. The daemon's broadcastActivity
// emits one "activity" heartbeat per actively-working session every 2s,
// so with N live sessions a tick delivers N events near-simultaneously.
// Coalescing them into one message means the Update loop applies the
// whole burst in a SINGLE Update/render cycle instead of N — bubbletea
// renders once per Update return, and a full board render is the
// expensive part (see [[reference_openkanban_tui_stall_watchdog]] — this
// O(N-agents) render churn is what the stall watchdog surfaced). Order
// is preserved (drained in receive order), so events with relative
// semantics (e.g. viewing→unviewing) still apply correctly.
type daemonSessionEventsMsg struct {
	Events []daemon.SessionEvent
}

// daemonSubscribeFailedMsg is returned by the subscribe tea.Cmd when
// the initial daemonclient.Subscribe call fails (e.g. the daemon has
// gone away between New() and Init()). The model logs and stops
// listening; the status-file poll continues to drive AgentStatus
// updates under the precedence rule.
type daemonSubscribeFailedMsg struct {
	Err error
}

// daemonSubscribeEndedMsg fires when the subscribe channel closes
// (typically because the client conn was torn down). The model treats
// this as a permanent loss of push events for the session and falls
// back to the status-file poll.
type daemonSubscribeEndedMsg struct{}

// daemonSubscribeReadyMsg carries a successful (bounded, async) Subscribe
// handshake back to Update, which installs the push channel + cancel func
// on the model and arms the first readNextDaemonEvent. The handshake runs
// in a tea.Cmd goroutine — NOT synchronously in NewModel — so a wedged
// daemon that never answers SubscribeReq can't block startup.
type daemonSubscribeReadyMsg struct {
	events <-chan daemon.SessionEvent
	unsub  func()
}

// subscribeDaemonEventsCmd performs the daemon Subscribe handshake from a
// tea.Cmd goroutine under a bounded context, then reports the outcome to
// Update. This replaces the old synchronous subscribe in NewModel, which
// called client.Subscribe(context.Background()) — a wedged daemon (accepts
// the connection and completes hello, then never answers SubscribeReq)
// blocked NewModel forever, before the bubbletea loop started, so the TUI
// never painted and the stall watchdog never armed. The bounded context
// makes the handshake fail with a deadline instead of hanging.
//
// Resolves to daemonSubscribeReadyMsg on success (the handler installs the
// channel + cancel func and arms readNextDaemonEvent) or
// daemonSubscribeFailedMsg on a nil client / deadline / handshake error
// (the status-file poll continues to drive AgentStatus). The context only
// bounds the handshake; the subscription, once established, outlives it.
func subscribeDaemonEventsCmd(client *daemonclient.Client) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return daemonSubscribeFailedMsg{Err: errDaemonClientNil}
		}
		ctx, cancel := context.WithTimeout(context.Background(), startupSubscribeTimeout)
		defer cancel()
		events, unsub, err := client.Subscribe(ctx)
		if err != nil {
			return daemonSubscribeFailedMsg{Err: err}
		}
		return daemonSubscribeReadyMsg{events: events, unsub: unsub}
	}
}

// readNextDaemonEvent returns a tea.Cmd that blocks on the next read
// from ch, then non-blockingly DRAINS any already-queued events so a
// burst is delivered as one daemonSessionEventsMsg. The model's Update
// re-arms the listener after every batch so events keep flowing.
//
// Coalescing matters because the daemon emits one "activity" heartbeat
// per live session every 2s; without draining, N sessions cost N
// Update/render cycles per tick. The drain is bounded by the subscribe
// channel buffer (cap 64).
func readNextDaemonEvent(ch <-chan daemon.SessionEvent) tea.Cmd {
	if ch == nil {
		log.Printf("openkanban model: readNextDaemonEvent invoked with nil channel")
		return func() tea.Msg { return daemonSubscribeEndedMsg{} }
	}
	return func() tea.Msg {
		log.Printf("openkanban model: readNextDaemonEvent waiting on channel")
		ev, ok := <-ch
		if !ok {
			log.Printf("openkanban model: readNextDaemonEvent channel closed")
			return daemonSubscribeEndedMsg{}
		}
		evs := []daemon.SessionEvent{ev}
	drain:
		for {
			select {
			case e, ok := <-ch:
				if !ok {
					// Channel closed mid-drain: deliver what we have; the
					// re-arm read will observe the close and end the loop.
					break drain
				}
				evs = append(evs, e)
			default:
				break drain
			}
		}
		log.Printf("openkanban model: readNextDaemonEvent got %d event(s); first=%q session=%s",
			len(evs), evs[0].Event, evs[0].SessionID)
		return daemonSessionEventsMsg{Events: evs}
	}
}

// errDaemonClientNil is the sentinel returned when subscribeDaemonEvents
// is called without a client. Defined here so test assertions can
// errors.Is against it.
var errDaemonClientNil = daemonSubscribeErr("daemon client is nil")

type daemonSubscribeErr string

func (e daemonSubscribeErr) Error() string { return string(e) }

// handleDaemonSessionEvents applies a coalesced burst of events in a
// single Update cycle, then re-arms the listener once. This is the live
// dispatch path (see daemonSessionEventsMsg). bubbletea renders once per
// Update return, so a burst of N "activity" heartbeats costs one render,
// not N.
func (m *Model) handleDaemonSessionEvents(msg daemonSessionEventsMsg) (tea.Model, tea.Cmd) {
	for i := range msg.Events {
		m.applyDaemonSessionEvent(msg.Events[i])
	}
	return m, readNextDaemonEvent(m.daemonEvents)
}

// handleDaemonSessionEvent applies a single event and re-arms. Retained
// for the single-event call path exercised by tests.
func (m *Model) handleDaemonSessionEvent(msg daemonSessionEventMsg) (tea.Model, tea.Cmd) {
	m.applyDaemonSessionEvent(msg.Event)
	return m, readNextDaemonEvent(m.daemonEvents)
}

// applyDaemonSessionEvent applies one daemon-pushed SessionEvent to the
// model. It does NOT re-arm the listener — the caller re-arms once per
// batch.
//
// The mapping is intentionally minimal:
//
//   - "started"   → AgentStatus = AgentWorking (unless already higher).
//   - "exited"    → If ev.Expected (daemon-initiated `openkanban ticket
//     done`), preserve AgentCompleted; else reset to
//     AgentNone. PaneView (if held) is closed; the
//     focused-pane mode unwinds to ModeNormal when the
//     event lands on the focused ticket.
//   - "attached"  → informational; no model change. PTY-stream
//     ownership is tracked elsewhere (PaneView state).
//   - "detached"  → informational; the local PaneView may already
//     have transitioned to PaneViewDetached via its
//     own DetachMsg path.
//   - "viewing"   → no AgentStatus change; increments
//     daemonViewing[ticketID] so the board card
//     renders the "TUI viewing this ticket" indicator.
//   - "unviewing" → no AgentStatus change; decrements
//     daemonViewing[ticketID] (guarded against
//     underflow).
//
// Precedence rule: this handler is unconditional — push events
// authoritatively trump the status-file poll while the daemon
// subscription is live. The poll handler (agentStatusResultMsg) honors
// daemonConnected.Load() so it doesn't clobber what we just wrote.
func (m *Model) applyDaemonSessionEvent(ev daemon.SessionEvent) {
	ticketID := board.TicketID(ev.TicketID)

	log.Printf("openkanban model: handleDaemonSessionEvent event=%q session=%s ticket=%s expected=%v",
		ev.Event, ev.SessionID, ev.TicketID, ev.Expected)

	// Daemon-global events (no ticket): the wedge watchdog's suspicion
	// signal. The daemon reports a suspected wedge rather than self-restarting
	// (that would kill live sessions), so the TUI surfaces it and lets the
	// operator decide. Cleared when the daemon reports dispatch resumed.
	switch ev.Event {
	case "daemon_wedged":
		if !m.daemonWedged {
			log.Printf("openkanban model: daemon reports suspected wedge: %s", ev.Reason)
		}
		m.daemonWedged = true
		return
	case "daemon_unwedged":
		if m.daemonWedged {
			log.Printf("openkanban model: daemon wedge cleared; dropping banner")
		}
		m.daemonWedged = false
		return
	}

	if ticketID != "" {
		// Stamp the per-ticket activity timestamp from any event that
		// carries one. The daemon seeds it on lifecycle events (started,
		// attached, detached, exited) and emits it on every "activity"
		// heartbeat, so the UI map stays warm both at session boundaries
		// and during steady-state work. Used by the status detector to
		// override "waiting" → "working" when bytes are flowing despite
		// the Notification hook's stale file.
		if !ev.LastActivityAt.IsZero() {
			if cur, ok := m.lastPTYActivity[ticketID]; !ok || ev.LastActivityAt.After(cur) {
				m.lastPTYActivity[ticketID] = ev.LastActivityAt
			}
		}

		ticket, _ := m.globalStore.Get(ticketID)
		if ticket != nil {
			switch ev.Event {
			case "started":
				m.daemonOwned[ticketID] = struct{}{}
				// Prefer the daemon's resolved verdict when present; fall
				// back to AgentWorking for an older daemon (or any event
				// without a Status) so a freshly-spawned session still
				// shows activity immediately.
				if ev.Status != "" {
					if m.applyDaemonStatus(ticket, ev.Status) {
						m.saveTicket(ticket)
					}
				} else if ticket.SetAgentStatus(board.AgentWorking) {
					m.saveTicket(ticket)
				}
			case "exited":
				// Instrument the "exited" handler body so we can spot
				// regressions if the saveTicket disk write, the pv.Close
				// (now non-blocking after the Task 1 refactor in
				// internal/daemonclient/paneview.go), or anything else in
				// this branch starts taking real wall-clock. Anything
				// over ~100ms is a follow-up.
				t0 := time.Now()
				defer func() {
					log.Printf("openkanban: handleDaemonSessionEvent.exited(%s expected=%v) took %s",
						ticketID, ev.Expected, time.Since(t0))
				}()
				delete(m.daemonOwned, ticketID)
				delete(m.daemonViewing, ticketID)
				delete(m.lastPTYActivity, ticketID)
				// Expected=true means the daemon initiated the kill via
				// handleTicketDone (i.e. the agent invoked `openkanban
				// ticket done`, or the TUI's board-promotion wrap-up
				// sent a TicketDone RPC). Preserve AgentCompleted so the
				// card renders as done rather than getting reset to
				// AgentNone. Expected=false is a natural exit / plain
				// Kill — reset to AgentNone as before.
				stateChanged := false
				if ev.Expected {
					stateChanged = ticket.SetAgentStatus(board.AgentCompleted)
				} else {
					stateChanged = ticket.SetAgentStatus(board.AgentNone)
				}
				// On an EXPECTED exit, clear the on-disk session
				// linkage so a later resume doesn't pick up the dead
				// UUID. The clean wrap-up signal (handleTicketDone) is
				// our certainty that the session is truly gone — the
				// JSONL has been finalised by the agent's /quit motion.
				//
				// On UNEXPECTED exits we deliberately preserve the
				// linkage. A daemon crash or transient PTY tear-down
				// can fire an "exited" event while the JSONL is still
				// on disk and resumable; clearing here would lose the
				// user's link to a session that's coming right back.
				// See commit c718699 — the UUID persistence is what
				// makes --resume work.
				if ev.Expected {
					if ticket.AgentSessionID != "" || ticket.AgentSpawnedAt != nil {
						ticket.AgentSessionID = ""
						ticket.AgentSpawnedAt = nil
						stateChanged = true
					}
				}
				if stateChanged {
					m.saveTicket(ticket)
				}
				if pv, ok := m.panes[ticketID]; ok {
					_ = pv.Close()
					delete(m.panes, ticketID)
				}
				if m.focusedPane == ticketID {
					m.exitToBoard()
				}
			case "attached", "detached":
				// Informational only — PTY-stream ownership; doesn't
				// drive the board indicator.
			case "viewing":
				m.daemonViewing[ticketID]++
			case "unviewing":
				if m.daemonViewing[ticketID] > 0 {
					m.daemonViewing[ticketID]--
					if m.daemonViewing[ticketID] == 0 {
						delete(m.daemonViewing, ticketID)
					}
				}
			case "activity":
				// Heartbeat: the lastPTYActivity stamp above already
				// absorbed the timestamp. When the daemon also resolved a
				// status from its live PTY grid, apply it — this is the
				// daemon-authoritative path that keeps an UNATTACHED
				// session's card accurate. The daemon classifies the grid
				// whether or not any TUI is attached, so a bg-spawned
				// session that's genuinely working reads "working" here
				// even though the client has no local grid for it.
				if m.applyDaemonStatus(ticket, ev.Status) {
					m.saveTicket(ticket)
				}
			case "status":
				// Daemon watchdog verdict (currently only Status:"stuck"):
				// the pane wedged on input backpressure. applyDaemonStatus
				// maps it to AgentStuck and guards AgentCompleted (a
				// wrapped-up ticket is never knocked back to stuck), so the
				// card renders red and the user can recover or destroy the
				// session from the stuck-action modal.
				if m.applyDaemonStatus(ticket, ev.Status) {
					m.saveTicket(ticket)
				}
			}
		}
	}
}

// applyDaemonStatus applies a daemon-resolved AgentStatus (carried on
// SessionEvent.Status) to the ticket, mirroring the file-poll's guards in
// the agentStatusResultMsg handler:
//
//   - An empty or "none" value is "the daemon has no verdict" (an older
//     daemon, or a session it doesn't classify such as opencode) — a
//     no-op, leaving the file-poll as the source.
//   - An unknown string is ignored defensively.
//   - AgentCompleted is terminal: only another terminal value
//     (Completed / Error) may displace it, so a late activity event can't
//     knock a wrapped-up ticket back to working/waiting.
//
// Returns true if the ticket's AgentStatus actually changed (caller
// persists). SetAgentStatus stamps StatusChangedAt on a real change.
func (m *Model) applyDaemonStatus(ticket *board.Ticket, raw string) bool {
	if ticket == nil || raw == "" {
		return false
	}
	status := board.AgentStatus(raw)
	switch status {
	case board.AgentIdle, board.AgentWorking, board.AgentWaiting,
		board.AgentCompleted, board.AgentError, board.AgentStuck:
		// known, applicable value
	default:
		// AgentNone ("no verdict") and any unknown string: leave the
		// current status alone.
		return false
	}
	if ticket.AgentStatus == board.AgentCompleted &&
		status != board.AgentCompleted && status != board.AgentError {
		return false
	}
	return ticket.SetAgentStatus(status)
}

// handleDaemonSubscribeReady installs the push channel from a successful
// async Subscribe (subscribeDaemonEventsCmd) and arms the first event
// read. This is the deferred counterpart to what NewModel used to do
// synchronously; running it from Update keeps startup non-blocking.
func (m *Model) handleDaemonSubscribeReady(msg daemonSubscribeReadyMsg) (tea.Model, tea.Cmd) {
	m.daemonEvents = msg.events
	m.daemonUnsub = msg.unsub
	m.daemonConnected.Store(true)
	log.Printf("openkanban model: daemon Subscribe ok; push channel armed")
	return m, readNextDaemonEvent(msg.events)
}

// handleDaemonSubscribeFailed records a subscribe failure. We log and
// flip daemonConnected to false so the status-file poll can fill in
// for the lost push channel. No retry — the daemon disconnect path
// (DaemonDisconnectedMsg) is the right reconnect hook.
func (m *Model) handleDaemonSubscribeFailed(msg daemonSubscribeFailedMsg) (tea.Model, tea.Cmd) {
	if msg.Err != nil {
		log.Printf("openkanban: daemon Subscribe failed: %v", msg.Err)
	}
	m.daemonConnected.Store(false)
	return m, nil
}

// handleDaemonSubscribeEnded reacts to the subscribe channel closing.
// In practice this means the daemon conn was torn down (DaemonDisconnectedMsg
// will arrive soon if it hasn't already); treat it as "push events are
// gone, fall back to the file poll" and stop listening.
func (m *Model) handleDaemonSubscribeEnded(_ daemonSubscribeEndedMsg) (tea.Model, tea.Cmd) {
	m.daemonConnected.Store(false)
	if m.daemonUnsub != nil {
		m.daemonUnsub()
		m.daemonUnsub = nil
	}
	m.daemonEvents = nil
	return m, nil
}
