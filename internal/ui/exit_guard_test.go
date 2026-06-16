package ui

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
)

// fakeGuardAPI is a tiny in-memory stand-in for *daemonclient.Client
// that records every exit-guard-relevant call and returns canned
// responses. Built solely to test the exit-guard's decision tree
// without a real daemon process. Safe for concurrent use because
// tea.Cmd callbacks run on a separate goroutine.
//
// Embeds daemonAPINoop so the methods the exit-guard never touches
// (Owns / TicketDone / List / Spawn) resolve to zero-value returns
// without having to stub them here.
type fakeGuardAPI struct {
	daemonAPINoop

	mu sync.Mutex

	prepareExitResp daemon.PrepareExitResp
	prepareExitErr  error
	cancelExitErr   error

	killErrs map[string]error // by SessionID; nil = success

	killCalls    []string
	prepareCalls int
	cancelCalls  int
}

// newFakeGuardAPI seeds a PrepareExit response derived from the
// provided clientCount: ClientCount mirrors clientCount (legacy field),
// and OtherActiveClients defaults to max(0, clientCount-1) — the
// natural interpretation when no peer has also called PrepareExit.
// Tests that want to exercise the atomic-exit-intent decision tree
// (e.g. "last TUI sees 0 even when ClientCount is stale") should set
// fields directly on the returned struct's prepareExitResp.
func newFakeGuardAPI(sessions []daemon.SessionInfo, clientCount int) *fakeGuardAPI {
	other := clientCount - 1
	if other < 0 {
		other = 0
	}
	return &fakeGuardAPI{
		prepareExitResp: daemon.PrepareExitResp{
			ClientCount:        clientCount,
			OtherActiveClients: other,
			Sessions:           sessions,
		},
		killErrs: map[string]error{},
	}
}

func (f *fakeGuardAPI) PrepareExit(_ context.Context) (daemon.PrepareExitResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepareCalls++
	if f.prepareExitErr != nil {
		return daemon.PrepareExitResp{}, f.prepareExitErr
	}
	return f.prepareExitResp, nil
}

func (f *fakeGuardAPI) CancelExit(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelCalls++
	return f.cancelExitErr
}

func (f *fakeGuardAPI) Kill(_ context.Context, sessionID string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killCalls = append(f.killCalls, sessionID)
	return f.killErrs[sessionID]
}

// cancelCallCount returns the number of CancelExit invocations.
func (f *fakeGuardAPI) cancelCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancelCalls
}

func (f *fakeGuardAPI) ClientID() uint16 { return 1 }

func (f *fakeGuardAPI) killCallsCopy() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.killCalls))
	copy(out, f.killCalls)
	return out
}

// minimalModel returns a Model wired with just enough state to drive
// the exit guard. It deliberately leaves panes / store / config zero
// so the test can't accidentally exercise the legacy quit path — the
// guard runs entirely off m.daemon.
func minimalModel(api daemonAPI) *Model {
	return &Model{
		mode:   ModeNormal,
		daemon: api,
	}
}

// isQuitCmd returns true iff cmd is tea.Quit (the *function value*
// returned by tea.Quit is a quitMsg-producing closure). The cheapest
// portable way to detect it is to invoke it and check the message
// type.
func isQuitCmd(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	msg := cmd()
	_, ok := msg.(tea.QuitMsg)
	return ok
}

// runCmds invokes every non-nil tea.Cmd produced by the model under
// test and feeds each resulting message back into m.Update until no
// more commands are produced. Returns the final model and the
// concatenated message log. Helpful for exercising the kill-then-exit
// flow which involves an RPC tea.Cmd → sessionKilledMsg → tea.Quit
// pipeline.
func runCmds(t *testing.T, m *Model, cmd tea.Cmd) (*Model, []tea.Msg) {
	t.Helper()
	var msgs []tea.Msg
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		msg := c()
		if msg == nil {
			continue
		}
		// A tea.BatchMsg wraps multiple cmds — drain it into the queue.
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, sub := range batch {
				queue = append(queue, sub)
			}
			continue
		}
		// tea.QuitMsg terminates the run — record and stop.
		if _, ok := msg.(tea.QuitMsg); ok {
			msgs = append(msgs, msg)
			return m, msgs
		}
		msgs = append(msgs, msg)
		newModel, newCmd := m.Update(msg)
		if mm, ok := newModel.(*Model); ok {
			m = mm
		}
		if newCmd != nil {
			queue = append(queue, newCmd)
		}
	}
	return m, msgs
}

// containsQuitMsg reports whether any tea.QuitMsg appears in msgs.
func containsQuitMsg(msgs []tea.Msg) bool {
	for _, msg := range msgs {
		if _, ok := msg.(tea.QuitMsg); ok {
			return true
		}
	}
	return false
}

// When sessions are live but the daemon reports at least one OTHER
// active (non-exiting) client, silent-quit is safe — the peer will
// keep the daemon (and its sessions) alive. This is the race-free
// successor to the v1 stopgap "always show modal regardless of
// ClientCount", now keyed on the daemon's atomic per-client exit-intent
// flag instead of the snapshot client count. See
// [[openkanban-exit-guard-always-fires]].
func TestExitGuard_OtherActiveClientsPositive_SilentQuitsEvenWithSessions(t *testing.T) {
	sessions := []daemon.SessionInfo{
		{SessionID: "s1", TicketID: "t1", PID: 100, Running: true},
	}
	api := newFakeGuardAPI(sessions, /*clientCount=*/ 5) // → OtherActiveClients = 4
	m := minimalModel(api)

	_, cmd := m.handleQuitRequested()
	finalModel, msgs := runCmds(t, m, cmd)
	if !containsQuitMsg(msgs) {
		t.Fatalf("expected tea.Quit when OtherActiveClients > 0; got msgs=%v", msgs)
	}
	if finalModel.mode == ModeConfirmExit {
		t.Errorf("expected NOT to enter ModeConfirmExit; got mode=%v", finalModel.mode)
	}
}

// Conversely, when the daemon's authoritative count says we're the
// last active TUI (OtherActiveClients=0) AND sessions are live, the
// modal must fire — even if the legacy ClientCount field is stale
// (e.g. peer disconnects mid-RPC). This asserts that the new field is
// the load-bearing signal, not ClientCount.
func TestExitGuard_LastTUI_ShowsModalEvenWithStaleClientCount(t *testing.T) {
	api := newFakeGuardAPI(nil, 0)
	api.prepareExitResp = daemon.PrepareExitResp{
		ClientCount:        5, // intentionally stale / inconsistent
		OtherActiveClients: 0,
		Sessions: []daemon.SessionInfo{
			{SessionID: "s1", TicketID: "t1", PID: 100, Running: true},
		},
	}
	m := minimalModel(api)

	_, cmd := m.handleQuitRequested()
	msg := cmd()
	resMsg, ok := msg.(prepareExitResultMsg)
	if !ok {
		t.Fatalf("expected prepareExitResultMsg; got %T", msg)
	}
	newModel, followup := m.Update(resMsg)
	mm := newModel.(*Model)
	if mm.mode != ModeConfirmExit {
		t.Fatalf("expected ModeConfirmExit when OtherActiveClients=0; got %v", mm.mode)
	}
	if isQuitCmd(followup) {
		t.Errorf("expected NOT to quit; got tea.Quit")
	}
	if len(mm.confirmExit.sessions) != 1 {
		t.Errorf("expected 1 session in modal; got %d", len(mm.confirmExit.sessions))
	}
}

func TestExitGuard_NoSessions_ExitsImmediately(t *testing.T) {
	api := newFakeGuardAPI(nil, /*clientCount=*/ 1)
	m := minimalModel(api)

	_, cmd := m.handleQuitRequested()
	finalModel, msgs := runCmds(t, m, cmd)
	if !containsQuitMsg(msgs) {
		t.Fatalf("expected tea.Quit; got msgs=%v", msgs)
	}
	if finalModel.mode == ModeConfirmExit {
		t.Errorf("expected NOT to enter ModeConfirmExit; got mode=%v", finalModel.mode)
	}
}

func TestExitGuard_HasSessions_EntersConfirmMode(t *testing.T) {
	sessions := []daemon.SessionInfo{
		{SessionID: "s1", TicketID: "t1", SessionName: "task/foo", PID: 100, Running: true},
		{SessionID: "s2", TicketID: "t2", SessionName: "task/bar", PID: 101, Running: true},
	}
	api := newFakeGuardAPI(sessions, 1)
	m := minimalModel(api)

	_, cmd := m.handleQuitRequested()
	if cmd == nil {
		t.Fatal("expected a tea.Cmd from handleQuitRequested")
	}

	// Invoke PrepareExit synchronously.
	msg := cmd()
	res, ok := msg.(prepareExitResultMsg)
	if !ok {
		t.Fatalf("expected prepareExitResultMsg; got %T (%v)", msg, msg)
	}
	if len(res.Resp.Sessions) != 2 {
		t.Fatalf("expected 2 sessions in response; got %d", len(res.Resp.Sessions))
	}

	newModel, followup := m.Update(msg)
	mm := newModel.(*Model)
	if mm.mode != ModeConfirmExit {
		t.Fatalf("expected mode=ModeConfirmExit; got %v", mm.mode)
	}
	if len(mm.confirmExit.sessions) != 2 {
		t.Errorf("expected 2 sessions stored in modal; got %d", len(mm.confirmExit.sessions))
	}
	if followup != nil {
		// handlePrepareExitResult should return nil cmd — purely a
		// mode transition.
		t.Errorf("expected nil cmd from result handler; got %T", followup)
	}
}

func TestExitGuard_KillAllThenExit(t *testing.T) {
	sessions := []daemon.SessionInfo{
		{SessionID: "s1", TicketID: "t1", PID: 100, Running: true},
		{SessionID: "s2", TicketID: "t2", PID: 101, Running: true},
	}
	api := newFakeGuardAPI(sessions, 1)
	m := minimalModel(api)

	// Drive through PrepareExit → ModeConfirmExit.
	_, cmd := m.handleQuitRequested()
	msg := cmd()
	resMsg := msg.(prepareExitResultMsg)
	newModel, _ := m.Update(resMsg)
	m = newModel.(*Model)
	if m.mode != ModeConfirmExit {
		t.Fatalf("setup: expected ModeConfirmExit; got %v", m.mode)
	}

	// Simulate `X` (kill all) — runCmds drives the full kill-then-exit
	// chain. Each Kill returns sessionKilledMsg → handler drops it from
	// the list; when empty, handler emits tea.Quit.
	_, cmd = m.handleConfirmExitMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'X'}})
	finalModel, msgs := runCmds(t, m, cmd)

	calls := api.killCallsCopy()
	if len(calls) != 2 {
		t.Errorf("expected 2 Kill calls; got %d (%v)", len(calls), calls)
	}
	if !containsQuitMsg(msgs) {
		t.Fatalf("expected tea.Quit at end; msgs=%v", msgs)
	}
	if got := len(finalModel.confirmExit.sessions); got != 0 {
		t.Errorf("expected empty session list at exit; got %d", got)
	}
}

func TestExitGuard_KillSelected_EnterShortcut(t *testing.T) {
	sessions := []daemon.SessionInfo{
		{SessionID: "s1", TicketID: "t1", PID: 100, Running: true},
		{SessionID: "s2", TicketID: "t2", PID: 101, Running: true},
	}
	api := newFakeGuardAPI(sessions, 1)
	m := minimalModel(api)

	_, cmd := m.handleQuitRequested()
	msg := cmd()
	m, _ = mustModel(m.Update(msg.(prepareExitResultMsg)))
	if m.mode != ModeConfirmExit {
		t.Fatalf("setup: expected ModeConfirmExit; got %v", m.mode)
	}

	// j → move down to s2
	m, _ = mustModel(m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}}))
	if m.confirmExit.selectedIdx != 1 {
		t.Fatalf("expected selectedIdx=1 after j; got %d", m.confirmExit.selectedIdx)
	}

	// Enter → kill s2
	_, cmd = m.handleConfirmExitMode(tea.KeyMsg{Type: tea.KeyEnter})
	_, msgs := runCmds(t, m, cmd)
	calls := api.killCallsCopy()
	if len(calls) != 1 || calls[0] != "s2" {
		t.Errorf("expected single Kill(s2); got %v", calls)
	}
	// s1 still alive → modal must NOT auto-exit.
	if containsQuitMsg(msgs) {
		t.Errorf("expected to stay in modal; saw tea.Quit")
	}
	if len(m.confirmExit.sessions) != 1 {
		t.Errorf("expected 1 session remaining; got %d", len(m.confirmExit.sessions))
	}
}

func TestExitGuard_EscCancels(t *testing.T) {
	sessions := []daemon.SessionInfo{
		{SessionID: "s1", TicketID: "t1", PID: 100, Running: true},
	}
	api := newFakeGuardAPI(sessions, 1)
	m := minimalModel(api)

	_, cmd := m.handleQuitRequested()
	msg := cmd()
	m, _ = mustModel(m.Update(msg.(prepareExitResultMsg)))
	if m.mode != ModeConfirmExit {
		t.Fatalf("setup: expected ModeConfirmExit; got %v", m.mode)
	}

	// Esc → cancel. handleConfirmExitMode returns a fire-and-forget
	// cancelExitCmd; invoke it once so the CancelExit RPC fires, then
	// assert: (a) the returned message is nil (not tea.QuitMsg — we are
	// not exiting), (b) exactly one CancelExit call was recorded.
	newModel, followup := m.handleConfirmExitMode(tea.KeyMsg{Type: tea.KeyEsc})
	mm := newModel.(*Model)
	if mm.mode != ModeNormal {
		t.Errorf("expected mode=ModeNormal after Esc; got %v", mm.mode)
	}
	if followup == nil {
		t.Fatalf("expected non-nil cancelExitCmd; got nil")
	}
	resultMsg := followup()
	if _, isQuit := resultMsg.(tea.QuitMsg); isQuit {
		t.Errorf("expected cancelExitCmd to NOT emit tea.Quit; got QuitMsg")
	}
	if resultMsg != nil {
		t.Errorf("expected cancelExitCmd to return nil msg; got %T", resultMsg)
	}
	if got := api.cancelCallCount(); got != 1 {
		t.Errorf("expected exactly 1 CancelExit call; got %d", got)
	}
	if calls := api.killCallsCopy(); len(calls) != 0 {
		t.Errorf("expected zero Kill calls; got %v", calls)
	}
}

// TestExitGuard_KillAllThenExit_NoCancel confirms CancelExit is NOT
// called on the kill-all-then-exit path. The user committed to
// exiting; the daemon will see the disconnect and the exit-intent
// flag is moot. Firing CancelExit there would be wrong (the peer's
// next PrepareExit would briefly see us as active again, which is
// confusing).
func TestExitGuard_KillAllThenExit_NoCancel(t *testing.T) {
	sessions := []daemon.SessionInfo{
		{SessionID: "s1", TicketID: "t1", PID: 100, Running: true},
		{SessionID: "s2", TicketID: "t2", PID: 101, Running: true},
	}
	api := newFakeGuardAPI(sessions, 1)
	m := minimalModel(api)

	_, cmd := m.handleQuitRequested()
	msg := cmd()
	resMsg := msg.(prepareExitResultMsg)
	newModel, _ := m.Update(resMsg)
	m = newModel.(*Model)
	if m.mode != ModeConfirmExit {
		t.Fatalf("setup: expected ModeConfirmExit; got %v", m.mode)
	}

	_, cmd = m.handleConfirmExitMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'X'}})
	_, msgs := runCmds(t, m, cmd)

	if !containsQuitMsg(msgs) {
		t.Fatalf("expected tea.Quit at end; msgs=%v", msgs)
	}
	if got := api.cancelCallCount(); got != 0 {
		t.Errorf("expected NO CancelExit calls on kill-all path; got %d", got)
	}
}

// PrepareExit RPC failure with NO known local sessions: we have
// nothing to warn about, so falling through to tea.Quit is the right
// move (trapping the user would be worse).
func TestExitGuard_PrepareExitFails_NoLocalSessions_ExitsAnyway(t *testing.T) {
	api := newFakeGuardAPI(nil, 1)
	api.prepareExitErr = errors.New("daemon broke")
	m := minimalModel(api)

	_, cmd := m.handleQuitRequested()
	finalModel, msgs := runCmds(t, m, cmd)
	if !containsQuitMsg(msgs) {
		t.Fatalf("expected tea.Quit despite RPC error; got msgs=%v", msgs)
	}
	if finalModel.mode == ModeConfirmExit {
		t.Errorf("expected NOT to enter ModeConfirmExit when PrepareExit failed and no local sessions; mode=%v", finalModel.mode)
	}
}

// PrepareExit RPC failure WITH known local sessions: fall back to the
// local pane snapshot and surface the modal so the user can decide,
// rather than silently exiting and letting the daemon kill them on
// disconnect.
func TestExitGuard_PrepareExitFails_LocalSessions_ShowsModal(t *testing.T) {
	api := newFakeGuardAPI(nil, 1)
	api.prepareExitErr = errors.New("daemon broke")

	// PaneView with info.Running=true sits in PaneViewUnattached and
	// reports Running()=true even with a nil daemon client.
	info := &daemon.SessionInfo{
		SessionID:   "s-alive",
		TicketID:    "t-alive",
		SessionName: "task/foo",
		Running:     true,
	}
	pv := daemonclient.NewPaneView(nil, "t-alive", "s-alive", info)

	m := minimalModel(api)
	m.panes = map[board.TicketID]*daemonclient.PaneView{
		board.TicketID("t-alive"): pv,
	}

	_, cmd := m.handleQuitRequested()
	finalModel, msgs := runCmds(t, m, cmd)
	if containsQuitMsg(msgs) {
		t.Fatalf("expected NOT to exit when local sessions exist; got tea.Quit")
	}
	if finalModel.mode != ModeConfirmExit {
		t.Fatalf("expected ModeConfirmExit on RPC failure with local sessions; got %v", finalModel.mode)
	}
	if got := len(finalModel.confirmExit.sessions); got != 1 {
		t.Errorf("expected 1 synthesized session; got %d", got)
	}
	if got := finalModel.confirmExit.sessions[0].SessionID; got != "s-alive" {
		t.Errorf("expected SessionID=s-alive in fallback; got %q", got)
	}
}

func TestExitGuard_NilDaemonAPI_ExitsImmediately(t *testing.T) {
	m := minimalModel(nil)
	_, cmd := m.handleQuitRequested()
	if !isQuitCmd(cmd) {
		t.Fatalf("expected tea.Quit when m.daemon is nil; got %T", cmd)
	}
}

// mustModel asserts that the returned tea.Model is *Model and returns
// it alongside the cmd. Keeps each test step a one-liner.
func mustModel(model tea.Model, cmd tea.Cmd) (*Model, tea.Cmd) {
	mm := model.(*Model)
	return mm, cmd
}
