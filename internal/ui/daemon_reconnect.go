package ui

import (
	"context"
	"errors"
	"log"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/daemonclient"
)

// reconnectDialTimeout bounds a single re-dial attempt (dial + Hello).
// Matches the 5s startup daemon-dial budget in app.go so a wedged daemon
// fails the reconnect fast instead of stalling the cmd goroutine.
const reconnectDialTimeout = 5 * time.Second

// daemonReconnectedMsg carries the result of one async re-dial attempt
// back to the Update loop. Exactly one of client / err is set. The
// handler (handleDaemonReconnectedMsg) is the ONLY place that installs
// the fresh client into the model — the cmd goroutine never touches
// shared Model state (see internal/ui/CLAUDE.md).
type daemonReconnectedMsg struct {
	client *daemonclient.Client
	err    error
}

// reconnectDaemonCmd re-dials openkanbankd and returns a
// daemonReconnectedMsg. It mirrors subscribeDaemonEventsCmd's bounded-
// async shape: a fresh context, a single RPC-ish call, one result msg.
//
// autostart mirrors the app.go startup choice so a launchd-managed /
// --no-launch-daemon setup is never force-autostarted by a reconnect —
// only a TUI that itself autostarted the daemon will fork a new one here.
//
// Building a fresh *Client (vs. re-dialing inside the old one) is
// deliberate: daemonclient.Client is terminal-on-EOF by design, and
// New/NewNoAutostart re-run the Hello + ProtocolVersion-skew handshake
// so a daemon that came back on a newer wire protocol is rejected with
// ErrProtocolVersionSkew rather than silently talked-to with a
// mismatched codec.
func reconnectDaemonCmd(autostart bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), reconnectDialTimeout)
		defer cancel()

		var (
			client *daemonclient.Client
			err    error
		)
		if autostart {
			client, err = daemonclient.New(ctx)
		} else {
			client, err = daemonclient.NewNoAutostart(ctx)
		}
		if err != nil {
			return daemonReconnectedMsg{err: err}
		}
		return daemonReconnectedMsg{client: client}
	}
}

// maybeReconnectDaemon returns a reconnect cmd and marks an attempt
// in-flight, or nil if one is already running. MUST be called from the
// Update handler (it mutates m.daemonReconnecting) — never from a cmd
// goroutine. Callers gate on m.daemonClient.Closed() so a live client is
// never replaced.
func (m *Model) maybeReconnectDaemon() tea.Cmd {
	if m.daemonReconnecting {
		return nil
	}
	m.daemonReconnecting = true
	log.Printf("openkanban: daemon control conn dead; attempting reconnect (autostart=%v)", m.daemonAutostart)
	return reconnectDaemonCmd(m.daemonAutostart)
}

// handleDaemonReconnectedMsg applies a reconnect result.
//
//   - Success: swap BOTH m.daemonClient and m.daemon to the fresh client
//     (before the next resync tick, so its List rebuilds panes against the
//     new client), then re-arm the push subscription. We do NOT set
//     daemonConnected here — handleDaemonSubscribeReady flips it true once
//     the bounded Subscribe handshake returns, which keeps the
//     daemon-wins precedence rule's single writer.
//   - Version skew: TERMINAL. A re-dial cannot fix a protocol mismatch —
//     the daemon won't change versions by being asked again — so we
//     surface the actionable hint and stop. The 30s resync chain keeps
//     running, so a later `openkanban daemon restart` (matching binary)
//     still recovers without a TUI restart.
//   - Other failure: log and stop; the still-running resync tick will see
//     the client is still Closed() and trigger the next attempt. No
//     re-arm here (that would race the resync-driven retry).
func (m *Model) handleDaemonReconnectedMsg(msg daemonReconnectedMsg) (tea.Model, tea.Cmd) {
	m.daemonReconnecting = false

	if msg.err != nil {
		if errors.Is(msg.err, daemonclient.ErrProtocolVersionSkew) {
			log.Printf("openkanban: daemon reconnect aborted (version skew): %v", msg.err)
			m.notify("Daemon version skew — run `openkanban daemon restart`")
			return m, nil
		}
		log.Printf("openkanban: daemon reconnect failed: %v", msg.err)
		return m, nil
	}

	m.daemonClient = msg.client
	m.daemon = msg.client
	log.Printf("openkanban: daemon reconnected (client id %d)", msg.client.ClientID())
	m.notify("Daemon reconnected")

	// Re-arm the push subscription against the fresh client. The periodic
	// resync chain is still running (it re-arms even on error), so panes
	// repopulate against the new client on the next tick — no immediate
	// List kick here, which would fork a second resync chain.
	return m, subscribeDaemonEventsCmd(m.daemonClient)
}
