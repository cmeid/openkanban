package ui

import (
	"context"
	"log"

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
// from ch. The model's Update re-arms the listener after every
// daemonSessionEventMsg so events keep flowing.
func readNextDaemonEvent(ch <-chan daemon.SessionEvent) tea.Cmd {
	if ch == nil {
		return func() tea.Msg { return daemonSubscribeEndedMsg{} }
	}
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return daemonSubscribeEndedMsg{}
		}
		return daemonSessionEventMsg{Event: ev}
	}
}

// errDaemonClientNil is the sentinel returned when subscribeDaemonEvents
// is called without a client. Defined here so test assertions can
// errors.Is against it.
var errDaemonClientNil = daemonSubscribeErr("daemon client is nil")

type daemonSubscribeErr string

func (e daemonSubscribeErr) Error() string { return string(e) }

// handleDaemonSessionEvent applies one daemon-pushed SessionEvent to
// the model. Returns a tea.Cmd that re-arms the listener so further
// events flow.
//
// The mapping is intentionally minimal:
//
//   - "started"   → AgentStatus = AgentWorking (unless already higher).
//   - "exited"    → If ev.Expected (daemon-initiated `openkanban ticket
//                   done`), preserve AgentCompleted; else reset to
//                   AgentNone. PaneView (if held) is closed; the
//                   focused-pane mode unwinds to ModeNormal when the
//                   event lands on the focused ticket.
//   - "attached"  → informational; no AgentStatus change.
//   - "detached"  → informational; no AgentStatus change. The local
//                   PaneView may already have transitioned to
//                   PaneViewDetached via its own DetachMsg path.
//
// Precedence rule: this handler is unconditional — push events
// authoritatively trump the status-file poll while the daemon
// subscription is live. The poll handler (agentStatusResultMsg) honors
// daemonConnected.Load() so it doesn't clobber what we just wrote.
func (m *Model) handleDaemonSessionEvent(msg daemonSessionEventMsg) (tea.Model, tea.Cmd) {
	ev := msg.Event
	ticketID := board.TicketID(ev.TicketID)

	log.Printf("openkanban model: handleDaemonSessionEvent event=%q session=%s ticket=%s expected=%v",
		ev.Event, ev.SessionID, ev.TicketID, ev.Expected)

	if ticketID != "" {
		ticket, _ := m.globalStore.Get(ticketID)
		if ticket != nil {
			switch ev.Event {
			case "started":
				if ticket.AgentStatus != board.AgentWorking {
					ticket.AgentStatus = board.AgentWorking
					m.saveTicket(ticket)
				}
			case "exited":
				// Expected=true means the daemon initiated the kill via
				// handleTicketDone (i.e. the agent invoked `openkanban
				// ticket done`). Preserve AgentCompleted so the card
				// renders as done rather than getting reset to AgentNone.
				// Expected=false is a natural exit / plain Kill — reset
				// to AgentNone as before.
				if ev.Expected {
					if ticket.AgentStatus != board.AgentCompleted {
						ticket.AgentStatus = board.AgentCompleted
						m.saveTicket(ticket)
					}
				} else {
					if ticket.AgentStatus != board.AgentNone {
						ticket.AgentStatus = board.AgentNone
						m.saveTicket(ticket)
					}
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
				// Informational only — no AgentStatus change. The
				// per-pane attached/detached UI is driven by the local
				// PaneView's own PaneAttachedMsg / PaneDetachedMsg.
			}
		}
	}

	// Re-arm the listener so we keep draining the channel.
	return m, readNextDaemonEvent(m.daemonEvents)
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
