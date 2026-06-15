package ui

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/techdufus/openkanban/internal/daemon"
)

// daemonGuardAPI is the subset of *daemonclient.Client used by the TUI
// exit-guard AND the spawn-path dead-session gate. Held as an interface
// (rather than a concrete type) so tests can swap in a fake without
// bringing up a real daemon. The real daemonclient.Client satisfies
// this interface by virtue of its PrepareExit / Kill / ClientID / Owns
// methods.
//
// Owns is used by spawnAgent to short-circuit the on-disk JSONL
// dead-session check when the daemon already has a live PTY for the
// session UUID — see internal/ui/model.go's spawnAgent.
type daemonGuardAPI interface {
	PrepareExit(ctx context.Context) (daemon.PrepareExitResp, error)
	CancelExit(ctx context.Context) error
	Kill(ctx context.Context, sessionID string, grace time.Duration) error
	ClientID() uint16
	Owns(ctx context.Context, sessionUUID string) (daemon.OwnsResp, error)
}

// confirmExitState carries the modal's transient bookkeeping. Lives on
// the Model so the View() render is a pure read.
type confirmExitState struct {
	// sessions is the live list as of the most recent PrepareExit. It
	// shrinks as sessionKilledMsg arrives for each session.
	sessions []daemon.SessionInfo

	// selectedIdx is the row the user has highlighted; defaults to 0.
	// Clamped to [0, len(sessions)-1] on every key event.
	selectedIdx int

	// killing tracks per-session in-flight Kill RPCs (keyed by
	// SessionID). We use it both to render a "killing…" marker and to
	// guard against double-firing if the user mashes x.
	killing map[string]bool
}

// reset zeros the modal state — call before re-entering ModeConfirmExit
// to avoid showing stale data.
func (s *confirmExitState) reset() {
	s.sessions = nil
	s.selectedIdx = 0
	s.killing = nil
}

// killGracePeriod is the SIGTERM-to-SIGKILL window we ask the daemon
// to use when the user terminates a session from the modal. Matches
// the value handleQuit's cleanupAsync path used historically.
const killGracePeriod = 3 * time.Second

// --- Tea messages used by the exit guard. ---

// QuitRequestedMsg is dispatched in place of returning tea.Quit
// directly from any quit code path. Routing through the message queue
// gives the guard a single chokepoint and keeps the PrepareExit RPC
// off the synchronous Update path.
//
// Exported so out-of-package senders (notably the signal handler in
// internal/app/app.go, which fires program.Send(ui.QuitRequestedMsg{})
// from a goroutine on SIGINT/SIGTERM) can dispatch it. With this
// wiring, Ctrl-C and termination signals flow through the same
// exit-guard modal as `q` — closing the previously-noted
// "signal-handler bypass" hole in [[openkanban-exit-guard-always-fires]].
type QuitRequestedMsg struct{}

// prepareExitResultMsg carries the daemon's PrepareExit response back
// to the Update loop, where the decision tree (ClientCount > 1 → exit;
// no sessions → exit; otherwise → modal) is applied.
type prepareExitResultMsg struct {
	Resp daemon.PrepareExitResp
}

// prepareExitFailedMsg fires when PrepareExit returned an error. We
// log and exit anyway — refusing to quit because the daemon is
// unreachable would trap the user.
type prepareExitFailedMsg struct {
	Err error
}

// sessionKilledMsg confirms one Kill RPC succeeded. The handler removes
// the session from confirmExitState.sessions and, if the list is now
// empty, returns tea.Quit.
type sessionKilledMsg struct {
	SessionID string
}

// sessionKillFailedMsg fires when a Kill RPC fails. We leave the
// session in the list with a "kill failed" marker so the user can
// retry or Esc out.
type sessionKillFailedMsg struct {
	SessionID string
	Err       error
}

// --- handlers ---

// handleQuitRequested is the entry point: it asks the daemon for a
// PrepareExit snapshot in a background tea.Cmd so the UI stays
// responsive. The actual decision is made in handlePrepareExitResult
// when the result arrives.
//
// When the guard API is nil (daemon never reachable), we exit
// immediately — the user must not be trapped.
func (m *Model) handleQuitRequested() (tea.Model, tea.Cmd) {
	if m.guardAPI == nil {
		return m, tea.Quit
	}
	api := m.guardAPI
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		resp, err := api.PrepareExit(ctx)
		if err != nil {
			return prepareExitFailedMsg{Err: err}
		}
		return prepareExitResultMsg{Resp: resp}
	}
}

// handlePrepareExitResult decides whether to exit immediately or warn
// the user. The decision tree is:
//
//   - OtherActiveClients > 0 → at least one peer TUI is still attached
//     and has NOT also called PrepareExit, so it will keep the daemon
//     (and its sessions) alive after we leave. Silent-quit is safe.
//   - Sessions empty → no live sessions to warn about; exit cleanly.
//   - Otherwise → we're the last one out and sessions are at stake;
//     open the modal.
//
// Note the load-bearing field is OtherActiveClients, NOT ClientCount.
// ClientCount races on simultaneous closes (multiple TUIs each see
// their own connection in the total and silent-quit, then the actual
// last one out trips the daemon's defensive kill). The daemon computes
// OtherActiveClients atomically under clientsMu by excluding self and
// any peer that has also called PrepareExit, so exactly one caller
// among N simultaneous closers sees 0 — see
// [[openkanban-exit-guard-always-fires]] for the rationale.
//
// Accepted edge cases (do NOT try to fix):
//   - Two TUIs can both observe OtherActiveClients == 0 if they flip
//     their exit-intent flags at near-identical instants and both then
//     read the count; both open the modal. Killing sessions in either
//     succeeds; daemon de-dupes session state. Non-crashing.
//   - Rapid Esc→q in the modal issues a new PrepareExit before the
//     fire-and-forget CancelExit reaches the daemon; a peer's next
//     PrepareExit may transiently see us as exiting and pop a spurious
//     modal. Same category as the above.
func (m *Model) handlePrepareExitResult(msg prepareExitResultMsg) (tea.Model, tea.Cmd) {
	resp := msg.Resp
	if resp.OtherActiveClients > 0 {
		return m, tea.Quit
	}
	if len(resp.Sessions) == 0 {
		return m, tea.Quit
	}

	// Defensive copy: the modal mutates this slice as sessions are
	// killed, and we don't want to scribble on the daemon's RPC reply.
	sessions := make([]daemon.SessionInfo, len(resp.Sessions))
	copy(sessions, resp.Sessions)

	m.confirmExit.reset()
	m.confirmExit.sessions = sessions
	m.confirmExit.killing = map[string]bool{}
	m.mode = ModeConfirmExit
	return m, nil
}

// handlePrepareExitFailed runs when the PrepareExit RPC errored or
// timed out. Without an authoritative session list from the daemon we
// must not silently tea.Quit if our local pane state shows running
// agents — that's the same silent-destruction path the modal exists to
// prevent. Fall back to the local pane snapshot: if any pane is
// Running(), synthesize a SessionInfo list and show the modal. The
// synthesized PID is 0 (we don't track it client-side); SessionID is
// the daemon-internal handle the kill RPC needs.
//
// Only when the local snapshot is also empty do we exit anyway —
// trapping the user when there's genuinely nothing live is worse than
// the residual chance of a daemon-side ghost session.
func (m *Model) handlePrepareExitFailed(msg prepareExitFailedMsg) (tea.Model, tea.Cmd) {
	if msg.Err != nil {
		log.Printf("openkanban: exit guard PrepareExit failed: %v", msg.Err)
	}

	sessions := m.localRunningSessions()
	if len(sessions) == 0 {
		return m, tea.Quit
	}

	m.confirmExit.reset()
	m.confirmExit.sessions = sessions
	m.confirmExit.killing = map[string]bool{}
	m.mode = ModeConfirmExit
	m.notify("Daemon RPC failed; showing locally-known sessions. Kill or cancel.")
	return m, nil
}

// localRunningSessions builds a SessionInfo list from m.panes where
// the pane reports Running(). Used as a fallback when the daemon's
// PrepareExit RPC is unavailable. PID is omitted (the client doesn't
// track it); the SessionID is the daemon-internal handle so a Kill
// RPC from the modal can still target the right session.
func (m *Model) localRunningSessions() []daemon.SessionInfo {
	if len(m.panes) == 0 {
		return nil
	}
	out := make([]daemon.SessionInfo, 0, len(m.panes))
	for ticketID, pane := range m.panes {
		if pane == nil || !pane.Running() {
			continue
		}
		out = append(out, daemon.SessionInfo{
			SessionID:   pane.SessionID(),
			TicketID:    string(ticketID),
			SessionName: pane.SessionName(),
			Title:       pane.Title(),
			Running:     true,
		})
	}
	return out
}

// handleSessionKilled removes the named session from the modal's list.
// If the list is now empty, the user has finished cleanup and we exit.
func (m *Model) handleSessionKilled(msg sessionKilledMsg) (tea.Model, tea.Cmd) {
	if m.confirmExit.killing != nil {
		delete(m.confirmExit.killing, msg.SessionID)
	}
	m.confirmExit.removeSession(msg.SessionID)

	// Only auto-exit if we're still in the modal — a previous Esc
	// cancellation should NOT race a confirmation back into tea.Quit.
	if m.mode == ModeConfirmExit && len(m.confirmExit.sessions) == 0 {
		m.confirmExit.reset()
		m.mode = ModeNormal
		return m, tea.Quit
	}
	return m, nil
}

// handleSessionKillFailed leaves the session in the list and notifies
// the user. We don't auto-retry — the user can press x again or Esc
// out and investigate.
func (m *Model) handleSessionKillFailed(msg sessionKillFailedMsg) (tea.Model, tea.Cmd) {
	if m.confirmExit.killing != nil {
		delete(m.confirmExit.killing, msg.SessionID)
	}
	if msg.Err != nil {
		log.Printf("openkanban: exit guard Kill(%s) failed: %v", msg.SessionID, msg.Err)
		m.notify("Kill failed: " + msg.Err.Error())
	}
	return m, nil
}

// handleConfirmExitMode is the modal's key handler. The full keymap
// is documented next to the case statements below.
//
// Keymap (final):
//
//	↑ / k        — move selection up
//	↓ / j        — move selection down
//	x / Enter    — terminate the highlighted session
//	X            — terminate ALL live sessions
//	Esc / q      — cancel; stay in openkanban
//
// `k` here means up (vim-style), not "kill" — kill is x. This
// intentionally avoids the j/k vs. k/K conflict noted in the task.
func (m *Model) handleConfirmExitMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.confirmExit.reset()
		m.mode = ModeNormal
		// Tell the daemon we're no longer planning to exit, so peer
		// TUIs see this client as active again on their next
		// PrepareExit. Fire-and-forget — UI must not freeze if the
		// daemon is wedged. A failure is logged but otherwise ignored
		// (worst case: a peer transiently sees a spurious modal, which
		// falls under the accepted-edge-cases set documented in
		// handlePrepareExitResult).
		return m, m.cancelExitCmd()

	case "up", "k":
		if m.confirmExit.selectedIdx > 0 {
			m.confirmExit.selectedIdx--
		}
		return m, nil

	case "down", "j":
		if m.confirmExit.selectedIdx < len(m.confirmExit.sessions)-1 {
			m.confirmExit.selectedIdx++
		}
		return m, nil

	case "x", "enter":
		return m, m.killSelectedSession()

	case "X":
		return m, m.killAllSessions()
	}
	return m, nil
}

// killSelectedSession fires a Kill RPC for the highlighted row and
// marks it as in-flight so the UI shows a "killing…" indicator and
// double-presses are dropped.
func (m *Model) killSelectedSession() tea.Cmd {
	if len(m.confirmExit.sessions) == 0 {
		return nil
	}
	idx := m.confirmExit.selectedIdx
	if idx < 0 || idx >= len(m.confirmExit.sessions) {
		return nil
	}
	s := m.confirmExit.sessions[idx]
	if m.confirmExit.killing == nil {
		m.confirmExit.killing = map[string]bool{}
	}
	if m.confirmExit.killing[s.SessionID] {
		return nil
	}
	m.confirmExit.killing[s.SessionID] = true
	return m.killSessionCmd(s.SessionID)
}

// killAllSessions fires Kill RPCs for every live session in the modal.
// Each RPC's success/failure is reported via sessionKilledMsg /
// sessionKillFailedMsg so the modal updates incrementally as kills land.
func (m *Model) killAllSessions() tea.Cmd {
	if len(m.confirmExit.sessions) == 0 {
		return nil
	}
	if m.confirmExit.killing == nil {
		m.confirmExit.killing = map[string]bool{}
	}
	cmds := make([]tea.Cmd, 0, len(m.confirmExit.sessions))
	for _, s := range m.confirmExit.sessions {
		if m.confirmExit.killing[s.SessionID] {
			continue
		}
		m.confirmExit.killing[s.SessionID] = true
		cmds = append(cmds, m.killSessionCmd(s.SessionID))
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// cancelExitCmd builds a fire-and-forget tea.Cmd that tells the daemon
// this client is no longer planning to exit (clearing the per-client
// exiting flag set by PrepareExit). The result message is nil so the
// Update loop ignores it; failures are logged but never surfaced to
// the user — trapping the modal-cancel path on a wedged daemon would
// be worse than the rare spurious-modal a peer might see if the
// CancelExit is lost.
//
// Short timeout: 1s is more than enough for a same-process daemon
// (handler just flips a bool under clientsMu) and bounds the goroutine
// lifetime if the daemon is gone.
func (m *Model) cancelExitCmd() tea.Cmd {
	if m.guardAPI == nil {
		return nil
	}
	api := m.guardAPI
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		if err := api.CancelExit(ctx); err != nil {
			log.Printf("openkanban: exit guard CancelExit failed: %v", err)
		}
		return nil
	}
}

// killSessionCmd builds the tea.Cmd that performs a single Kill RPC
// against the daemon. The grace period is the standard SIGTERM window
// the daemon enforces before SIGKILL — see daemon.KillReq.GraceSeconds.
func (m *Model) killSessionCmd(sessionID string) tea.Cmd {
	if m.guardAPI == nil {
		return func() tea.Msg {
			return sessionKillFailedMsg{
				SessionID: sessionID,
				Err:       fmt.Errorf("daemon unavailable"),
			}
		}
	}
	api := m.guardAPI
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), killGracePeriod+2*time.Second)
		defer cancel()
		if err := api.Kill(ctx, sessionID, killGracePeriod); err != nil {
			return sessionKillFailedMsg{SessionID: sessionID, Err: err}
		}
		return sessionKilledMsg{SessionID: sessionID}
	}
}

// removeSession drops sessionID from s.sessions, preserving order, and
// adjusts selectedIdx so it stays in range.
func (s *confirmExitState) removeSession(sessionID string) {
	out := s.sessions[:0]
	for _, si := range s.sessions {
		if si.SessionID == sessionID {
			continue
		}
		out = append(out, si)
	}
	s.sessions = out
	if s.selectedIdx >= len(s.sessions) {
		s.selectedIdx = len(s.sessions) - 1
		if s.selectedIdx < 0 {
			s.selectedIdx = 0
		}
	}
}

// --- view ---

// renderConfirmExitDialog draws the exit-guard modal. Matches the
// existing renderConfirmDialog visual style (rounded-corner-ish
// columnBorder, err-colored title, dim hint footer) so the modal
// doesn't look out of place.
func (m *Model) renderConfirmExitDialog() string {
	titleStyle := lipgloss.NewStyle().
		Foreground(m.colors.err).
		Bold(true)
	rowStyle := lipgloss.NewStyle().Foreground(m.colors.text)
	dim := m.dimStyle()
	sel := lipgloss.NewStyle().Foreground(m.colors.warning).Bold(true)
	killing := lipgloss.NewStyle().Foreground(m.colors.muted).Italic(true)

	var rows strings.Builder
	for i, s := range m.confirmExit.sessions {
		marker := "  "
		if i == m.confirmExit.selectedIdx {
			marker = sel.Render("▶ ")
		}
		ticket := truncateMiddle(s.TicketID, 18)
		name := truncateMiddle(s.SessionName, 22)
		title := s.Title
		if title != "" {
			title = " — " + truncateMiddle(title, 28)
		}
		state := "running"
		if m.confirmExit.killing != nil && m.confirmExit.killing[s.SessionID] {
			state = "killing…"
		}
		stateRendered := dim.Render(state)
		if state == "killing…" {
			stateRendered = killing.Render(state)
		}
		line := fmt.Sprintf("%sticket=%s  session=%s  pid=%d  %s%s",
			marker,
			rowStyle.Render(ticket),
			rowStyle.Render(name),
			s.PID,
			stateRendered,
			dim.Render(title),
		)
		rows.WriteString(line)
		rows.WriteString("\n")
	}

	footer := dim.Render("↑/↓ select   x = kill   X = kill all   Esc = cancel")

	content := titleStyle.Render("⚠ Live agent sessions — must terminate before exit") + "\n\n" +
		rows.String() + "\n" +
		footer

	return lipgloss.NewStyle().
		Border(columnBorder).
		BorderForeground(m.colors.err).
		Padding(1, 2).
		Render(content)
}

// truncateMiddle clips s to at most n runes with an ellipsis in the
// middle so both the prefix (often the ticket-ID slug) and suffix
// (often the trailing UUID fragment) stay legible.
func truncateMiddle(s string, n int) string {
	if n <= 1 {
		if len(s) > 0 {
			return "…"
		}
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	head := (n - 1) / 2
	tail := n - 1 - head
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}
