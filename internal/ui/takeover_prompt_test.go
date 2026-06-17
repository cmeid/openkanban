package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/project"
)

// newTakeoverTestModel builds a minimal ModeAgentView Model with the
// maps the takeover-prompt path touches. daemonClient stays nil — the
// Attach-vs-Takeover wire distinction is proven in internal/daemonclient
// (TestPaneView_Attach_AlreadyAttachedReturnsSentinel); here we exercise
// the Update→arm→resolve propagation only.
func newTakeoverTestModel(t *testing.T) *Model {
	t.Helper()
	theme := config.DefaultConfig().GetTheme()
	return &Model{
		globalStore:   project.NewGlobalTicketStore(nil),
		panes:         map[board.TicketID]*daemonclient.PaneView{},
		daemonOwned:   map[board.TicketID]struct{}{},
		columnTickets: [][]*board.Ticket{},
		columnOffsets: []int{0, 0, 0, 0},
		mode:          ModeAgentView,
		width:         120,
		height:        40,
		theme:         theme,
		colors:        newUIColors(theme),
		config:        &config.Config{Agents: map[string]config.AgentConfig{}},
	}
}

func unattachedPane(id board.TicketID) *daemonclient.PaneView {
	info := &daemon.SessionInfo{
		SessionID: "sid-" + string(id),
		TicketID:  string(id),
		Running:   true,
		Cols:      80,
		Rows:      24,
	}
	return daemonclient.NewPaneView(nil, string(id), info.SessionID, info)
}

// TestAttachConflictMsg_ArmsTakeoverPrompt_RegistersPane covers the
// cold-start (P2) shape: the conflict carries a pv not yet in m.panes.
// Dispatching it through m.Update must register the pane and arm the
// modal. (Traverses the consuming side of the propagation path.)
func TestAttachConflictMsg_ArmsTakeoverPrompt_RegistersPane(t *testing.T) {
	m := newTakeoverTestModel(t)
	id := board.TicketID("T-CONFLICT")
	pv := unattachedPane(id)

	model, _ := m.Update(attachConflictMsg{ticketID: id, pv: pv})
	got := model.(*Model)

	if !got.takeoverPrompt {
		t.Fatal("takeoverPrompt = false, want true after attachConflictMsg")
	}
	if got.mode != ModeAgentView {
		t.Errorf("mode = %v, want ModeAgentView", got.mode)
	}
	if got.focusedPane != id {
		t.Errorf("focusedPane = %q, want %q", got.focusedPane, id)
	}
	if got.panes[id] != pv {
		t.Errorf("pane %q not registered from the conflict msg", id)
	}
	if _, ok := got.daemonOwned[id]; !ok {
		t.Errorf("daemonOwned[%q] not set", id)
	}
	if got.takeoverPending.ticketID != id || got.takeoverPending.pv != pv {
		t.Errorf("takeoverPending = %+v, want {%q, pv}", got.takeoverPending, id)
	}
}

// TestAttachConflictMsg_ArmsTakeoverPrompt_ExistingPane covers the P1
// shape: the pane is already registered, conflict carries the same pv.
func TestAttachConflictMsg_ArmsTakeoverPrompt_ExistingPane(t *testing.T) {
	m := newTakeoverTestModel(t)
	id := board.TicketID("T-EXIST")
	pv := unattachedPane(id)
	m.panes[id] = pv

	model, _ := m.Update(attachConflictMsg{ticketID: id, pv: pv})
	got := model.(*Model)

	if !got.takeoverPrompt {
		t.Fatal("takeoverPrompt = false, want true")
	}
	if got.takeoverPending.pv != pv {
		t.Error("takeoverPending.pv did not reuse the registered pane")
	}
}

// TestTakeoverPrompt_EscCancels: Esc dismisses the warning and returns
// to the board without attaching. Dispatched through m.Update so the
// handleKey → handleAgentViewMode → handleTakeoverPromptKey intercept
// wiring is actually traversed.
func TestTakeoverPrompt_EscCancels(t *testing.T) {
	m := newTakeoverTestModel(t)
	id := board.TicketID("T-ESC")
	pv := unattachedPane(id)
	// Arm via the real producing path.
	model, _ := m.Update(attachConflictMsg{ticketID: id, pv: pv})
	m = model.(*Model)
	if !m.takeoverPrompt {
		t.Fatal("precondition: takeoverPrompt not armed")
	}

	model, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got := model.(*Model)

	if got.takeoverPrompt {
		t.Error("takeoverPrompt = true after Esc, want false")
	}
	if got.mode != ModeNormal {
		t.Errorf("mode = %v after Esc, want ModeNormal", got.mode)
	}
	if got.focusedPane != "" {
		t.Errorf("focusedPane = %q after Esc, want empty", got.focusedPane)
	}
	if got.takeoverPending.pv != nil {
		t.Error("takeoverPending.pv not cleared after Esc")
	}
	_ = cmd
}

// TestTakeoverPrompt_EnterTakesTakeoverBranch: Enter clears the modal
// and takes the takeover branch. With a nil daemonClient doAttach is a
// no-op (nil cmd), but the branch — and the pending clear — must run.
func TestTakeoverPrompt_EnterTakesTakeoverBranch(t *testing.T) {
	m := newTakeoverTestModel(t)
	id := board.TicketID("T-ENTER")
	pv := unattachedPane(id)
	model, _ := m.Update(attachConflictMsg{ticketID: id, pv: pv})
	m = model.(*Model)

	model, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := model.(*Model)

	if got.takeoverPrompt {
		t.Error("takeoverPrompt = true after Enter, want false")
	}
	// Enter commits — unlike Esc it must NOT bounce back to the board.
	if got.mode != ModeAgentView {
		t.Errorf("mode = %v after Enter, want ModeAgentView (committing the takeover)", got.mode)
	}
	if got.takeoverPending.pv != nil {
		t.Error("takeoverPending.pv not cleared after Enter")
	}
}

// TestRetryAttach_AlreadyAttachedShortCircuits proves the Owns fast-path
// retry loop bails immediately on a deterministic ErrAlreadyAttached
// instead of burning the full backoff schedule.
func TestRetryAttach_AlreadyAttachedShortCircuits(t *testing.T) {
	calls := 0
	err := retryAttach(func(ctx context.Context) error {
		calls++
		return daemonclient.ErrAlreadyAttached
	})
	if calls != 1 {
		t.Errorf("attach called %d times, want 1 (no retries on already_attached)", calls)
	}
	if !errors.Is(err, daemonclient.ErrAlreadyAttached) {
		t.Errorf("err = %v, want ErrAlreadyAttached", err)
	}
}

// TestRenderTakeoverModal asserts the warning body carries the
// destructive-action wording and the take-over / cancel keys.
func TestRenderTakeoverModal(t *testing.T) {
	m := newTakeoverTestModel(t)
	m.focusedPane = board.TicketID("T-RENDER")
	m.takeoverPrompt = true

	out := m.renderTakeoverModal()
	for _, want := range []string{
		"Session open elsewhere",
		"attached in another openkanban window",
		"detach it there",
		"Take over",
		"Cancel",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("renderTakeoverModal missing %q\n--- got ---\n%s", want, out)
		}
	}
}
