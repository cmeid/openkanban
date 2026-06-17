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

// subscribeDaemonEvents kicks off the subscribe and returns a tea.Cmd
// that reads the first SessionEvent from the registered channel. The
// returned channel is wired into the model's daemonSubscriptionCh so
// subsequent calls to readNextDaemonEvent re-arm the listener.
//
// On Subscribe failure the cmd resolves to daemonSubscribeFailedMsg.
// On channel close it resolves to daemonSubscribeEndedMsg. Otherwise
// it returns daemonSessionEventMsg and the caller must re-arm with
// readNextDaemonEvent(ch).
func subscribeDaemonEvents(client *daemonclient.Client) (<-chan daemon.SessionEvent, func(), tea.Cmd) {
	if client == nil {
		return nil, nil, func() tea.Msg {
			return daemonSubscribeFailedMsg{Err: errDaemonClientNil}
		}
	}
	events, unsub, err := client.Subscribe(context.Background())
	if err != nil {
		return nil, nil, func() tea.Msg {
			return daemonSubscribeFailedMsg{Err: err}
		}
	}
	return events, unsub, readNextDaemonEvent(events)
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
//                   done`), preserve AgentCompleted; else reset to
//                   AgentNone. PaneView (if held) is closed; the
//                   focused-pane mode unwinds to ModeNormal when the
//                   event lands on the focused ticket.
//   - "attached"  → informational; no model change. PTY-stream
//                   ownership is tracked elsewhere (PaneView state).
//   - "detached"  → informational; the local PaneView may already
//                   have transitioned to PaneViewDetached via its
//                   own DetachMsg path.
//   - "viewing"   → no AgentStatus change; increments
//                   daemonViewing[ticketID] so the board card
//                   renders the "TUI viewing this ticket" indicator.
//   - "unviewing" → no AgentStatus change; decrements
//                   daemonViewing[ticketID] (guarded against
//                   underflow).
//
// Precedence rule: this handler is unconditional — push events
// authoritatively trump the status-file poll while the daemon
// subscription is live. The poll handler (agentStatusResultMsg) honors
// daemonConnected.Load() so it doesn't clobber what we just wrote.
func (m *Model) applyDaemonSessionEvent(ev daemon.SessionEvent) {
	ticketID := board.TicketID(ev.TicketID)

	log.Printf("openkanban model: handleDaemonSessionEvent event=%q session=%s ticket=%s expected=%v",
		ev.Event, ev.SessionID, ev.TicketID, ev.Expected)

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
				if ticket.SetAgentStatus(board.AgentWorking) {
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
					m.mode = ModeNormal
					m.focusedPane = ""
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
				// Pure-heartbeat event — the lastPTYActivity stamp above
				// already absorbed the timestamp. No further state change.
			}
		}
	}
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
