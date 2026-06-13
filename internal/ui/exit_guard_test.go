package ui

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/daemon"
)

// fakeGuardAPI is a tiny in-memory stand-in for *daemonclient.Client
// that records every call and returns canned responses. Built solely
// to test the exit-guard's decision tree without a real daemon
// process. Safe for concurrent use because tea.Cmd callbacks run on a
// separate goroutine.
type fakeGuardAPI struct {
	mu sync.Mutex

	prepareExitResp daemon.PrepareExitResp
	prepareExitErr  error

	killErrs map[string]error // by SessionID; nil = success

	killCalls    []string
	prepareCalls int
}

func newFakeGuardAPI(sessions []daemon.SessionInfo, clientCount int) *fakeGuardAPI {
	return &fakeGuardAPI{
		prepareExitResp: daemon.PrepareExitResp{
			ClientCount: clientCount,
			Sessions:    sessions,
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

func (f *fakeGuardAPI) Kill(_ context.Context, sessionID string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killCalls = append(f.killCalls, sessionID)
	return f.killErrs[sessionID]
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
// guard runs entirely off m.guardAPI.
func minimalModel(api daemonGuardAPI) *Model {
	return &Model{
		mode:     ModeNormal,
		guardAPI: api,
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

func TestExitGuard_ClientCountGreaterThanOne_ExitsImmediately(t *testing.T) {
	api := newFakeGuardAPI([]daemon.SessionInfo{
		{SessionID: "s1", TicketID: "t1", PID: 100, Running: true},
	}, /*clientCount=*/ 2)
	m := minimalModel(api)

	_, cmd := m.handleQuitRequested()
	finalModel, msgs := runCmds(t, m, cmd)
	if !containsQuitMsg(msgs) {
		t.Fatalf("expected tea.Quit; got msgs=%v", msgs)
	}
	if finalModel.mode == ModeConfirmExit {
		t.Errorf("expected NOT to enter ModeConfirmExit; got mode=%v", finalModel.mode)
	}
	if api.prepareCalls != 1 {
		t.Errorf("expected 1 PrepareExit call, got %d", api.prepareCalls)
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

	// Esc → cancel
	newModel, followup := m.handleConfirmExitMode(tea.KeyMsg{Type: tea.KeyEsc})
	mm := newModel.(*Model)
	if mm.mode != ModeNormal {
		t.Errorf("expected mode=ModeNormal after Esc; got %v", mm.mode)
	}
	if isQuitCmd(followup) {
		t.Errorf("expected NOT to exit; got tea.Quit")
	}
	if calls := api.killCallsCopy(); len(calls) != 0 {
		t.Errorf("expected zero Kill calls; got %v", calls)
	}
}

func TestExitGuard_PrepareExitFails_ExitsAnyway(t *testing.T) {
	api := newFakeGuardAPI(nil, 1)
	api.prepareExitErr = errors.New("daemon broke")
	m := minimalModel(api)

	_, cmd := m.handleQuitRequested()
	finalModel, msgs := runCmds(t, m, cmd)
	if !containsQuitMsg(msgs) {
		t.Fatalf("expected tea.Quit despite RPC error; got msgs=%v", msgs)
	}
	if finalModel.mode == ModeConfirmExit {
		t.Errorf("expected NOT to enter ModeConfirmExit when PrepareExit failed; mode=%v", finalModel.mode)
	}
}

func TestExitGuard_NilGuardAPI_ExitsImmediately(t *testing.T) {
	m := minimalModel(nil)
	_, cmd := m.handleQuitRequested()
	if !isQuitCmd(cmd) {
		t.Fatalf("expected tea.Quit when guardAPI is nil; got %T", cmd)
	}
}

// mustModel asserts that the returned tea.Model is *Model and returns
// it alongside the cmd. Keeps each test step a one-liner.
func mustModel(model tea.Model, cmd tea.Cmd) (*Model, tea.Cmd) {
	mm := model.(*Model)
	return mm, cmd
}
