package ui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/techdufus/openkanban/internal/board"
)

// The stuck-action modal is the user-controllable recovery surface for a
// session the daemon's watchdog flagged AgentStuck (wedged on input
// backpressure — the child stopped draining stdin). It deliberately
// offers ONLY user-initiated actions; the daemon NEVER auto-kills a
// wedged session (input backpressure can't distinguish "busy" from
// "wedged"), so the human decides:
//
//	[r] recover — attach to the session so the user can manually unstick
//	              it (send Ctrl-C / Enter); the wedge clears once input
//	              drains.
//	[d] destroy — Kill the daemon-side session (SIGTERM→SIGKILL group).
//	[esc]       — dismiss; leave the session as-is.
//
// It follows the exit-guard modal pattern: shown via a bool flag
// (m.stuckActionPrompt) while m.mode stays ModeNormal, and routed in
// handleKey's global arms BEFORE the ModeNormal dispatch (the PR #70
// routing rule) so the global arms (esc/q/ctrl+c/?) still run first and
// a key the modal doesn't handle is swallowed rather than reaching a
// board binding.

// openStuckActionModal arms the stuck-action modal for the currently
// selected ticket, but only when that ticket is actually AgentStuck.
// A no-op otherwise so the key is inert on non-stuck cards.
func (m *Model) openStuckActionModal() (tea.Model, tea.Cmd) {
	ticket := m.selectedTicket()
	if ticket == nil || ticket.AgentStatus != board.AgentStuck {
		return m, nil
	}
	m.stuckActionPrompt = true
	m.stuckActionTicket = ticket.ID
	return m, nil
}

// dismissStuckActionModal clears the modal state.
func (m *Model) dismissStuckActionModal() {
	m.stuckActionPrompt = false
	m.stuckActionTicket = ""
}

// handleStuckActionKey dispatches keys while the stuck-action modal is
// open. r → recover (attach), d → destroy (Kill), esc → dismiss. Any
// other key is swallowed (the modal owns the keyboard while open).
func (m *Model) handleStuckActionKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.dismissStuckActionModal()
		return m, nil
	case "r":
		return m.recoverStuckSession()
	case "d":
		return m.destroyStuckSession()
	}
	// Swallow any other key while the modal is up.
	return m, nil
}

// recoverStuckSession attaches to the stuck session so the user can
// manually unstick it (e.g. Ctrl-C / Enter). It routes through the same
// single attach/spawn entry point the board uses (spawnAgent), after
// re-selecting the stuck ticket so the entry point acts on the right
// one. Clearing the wedge happens naturally once input drains.
func (m *Model) recoverStuckSession() (tea.Model, tea.Cmd) {
	target := m.stuckActionTicket
	m.dismissStuckActionModal()
	if target == "" {
		return m, nil
	}
	m.selectTicketByID(target)
	return m.spawnAgent()
}

// destroyStuckSession kills the daemon-side session for the stuck
// ticket via the same Kill RPC the exit modal uses (SIGTERM→SIGKILL
// group, killGracePeriod). The session id comes from the local pane
// handle; if there's no pane we can't target a Kill, so it's a no-op
// (the card will resolve on the daemon's next reconcile).
func (m *Model) destroyStuckSession() (tea.Model, tea.Cmd) {
	target := m.stuckActionTicket
	m.dismissStuckActionModal()
	if target == "" {
		return m, nil
	}
	pv, ok := m.panes[target]
	if !ok || pv == nil {
		return m, nil
	}
	sessionID := pv.SessionID()
	if sessionID == "" {
		return m, nil
	}
	return m, m.killSessionCmd(sessionID)
}

// renderStuckActionModal draws the recover/destroy dialog, matching the
// exit-guard modal's err-colored, rounded-border visual style.
func (m *Model) renderStuckActionModal() string {
	titleStyle := lipgloss.NewStyle().Foreground(m.colors.err).Bold(true)
	dim := m.dimStyle()

	var ticketLabel string
	if t, _ := m.globalStore.Get(m.stuckActionTicket); t != nil {
		ticketLabel = t.Title
	}
	if ticketLabel == "" {
		ticketLabel = string(m.stuckActionTicket)
	}

	var b strings.Builder
	b.WriteString(titleStyle.Render("⚠ Session stuck — the agent stopped reading input"))
	b.WriteString("\n\n")
	b.WriteString(lipgloss.NewStyle().Foreground(m.colors.text).Render(truncateMiddle(ticketLabel, 48)))
	b.WriteString("\n\n")
	b.WriteString(dim.Render("[r] recover (attach & unstick)   [d] destroy   Esc dismiss"))

	return lipgloss.NewStyle().
		Border(columnBorder).
		BorderForeground(m.colors.err).
		Padding(1, 2).
		Render(b.String())
}
