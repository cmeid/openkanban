package ui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/daemonclient"
)

// classifierModel returns a minimal Model in ModeAgentView with a Detached pane
// (info=nil → Detached; HandleKey returns nil safely, no daemon I/O).
func classifierModel(id board.TicketID) *Model {
	pane := daemonclient.NewPaneView(nil, string(id), "", nil)
	return &Model{
		mode:        ModeAgentView,
		focusedPane: id,
		panes:       map[board.TicketID]*daemonclient.PaneView{id: pane},
	}
}

func keyUp() tea.KeyMsg   { return tea.KeyMsg{Type: tea.KeyUp} }
func keyDown() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyDown} }

func TestArrowClassifier_FirstArrowBuffered(t *testing.T) {
	m := classifierModel("C1")
	t0 := time.Unix(1000, 0)
	m.clock = func() time.Time { return t0 }

	_, cmd := m.handleAgentViewMode(keyUp())

	if !m.arrowPending {
		t.Fatal("first up: want arrowPending=true")
	}
	if cmd == nil {
		t.Error("first up: want non-nil Tick cmd")
	}
	if m.arrowSeq == 0 {
		t.Error("first up: want arrowSeq > 0")
	}
}

func TestArrowClassifier_TwoFastArrowsBecomesScroll(t *testing.T) {
	m := classifierModel("C2")
	t0 := time.Unix(1000, 0)
	m.clock = func() time.Time { return t0 }

	// First up — buffered.
	m.handleAgentViewMode(keyUp()) //nolint:errcheck
	if !m.arrowPending {
		t.Fatal("after first up: want arrowPending=true")
	}

	// Second up within arrowLeadWindow (5ms < 30ms) → scroll flood confirmed.
	m.clock = func() time.Time { return t0.Add(5 * time.Millisecond) }
	_, _ = m.handleAgentViewMode(keyUp())

	if m.arrowPending {
		t.Error("after second fast up: want arrowPending=false")
	}
	if m.arrowScrollUntil.IsZero() {
		t.Error("after second fast up: want arrowScrollUntil set")
	}
}

func TestArrowClassifier_AlreadyScrolling(t *testing.T) {
	m := classifierModel("C3")
	t0 := time.Unix(1000, 0)
	// Seed model as already in a scroll gesture.
	m.arrowScrollUntil = t0.Add(100 * time.Millisecond)
	m.clock = func() time.Time { return t0 }

	_, cmd := m.handleAgentViewMode(keyDown())

	if cmd != nil {
		t.Errorf("during scroll: want nil cmd, got non-nil")
	}
	if m.arrowPending {
		t.Error("during scroll: want arrowPending=false")
	}
	if m.arrowScrollUntil.IsZero() {
		t.Error("during scroll: want arrowScrollUntil still set after extend")
	}
}

func TestArrowClassifier_IsolatedArrowForwardedAfterRelease(t *testing.T) {
	m := classifierModel("C4")
	t0 := time.Unix(1000, 0)
	m.clock = func() time.Time { return t0 }

	// Buffer an arrow.
	_, _ = m.handleAgentViewMode(keyUp())
	seq := m.arrowSeq
	if !m.arrowPending {
		t.Fatal("pre-condition: want arrowPending=true")
	}

	// The arrowReleaseMsg fires with the matching seq.
	_, _ = m.handleArrowReleaseMsg(arrowReleaseMsg{seq: seq})

	if m.arrowPending {
		t.Error("after release: want arrowPending=false (isolated arrow forwarded)")
	}
}

func TestArrowClassifier_StaleReleaseIgnored(t *testing.T) {
	m := classifierModel("C5")
	t0 := time.Unix(1000, 0)
	m.clock = func() time.Time { return t0 }

	// Buffer a first arrow (seq = 1).
	_, _ = m.handleAgentViewMode(keyUp())
	oldSeq := m.arrowSeq

	// A second fast arrow arrives → scroll, clears pending.
	m.clock = func() time.Time { return t0.Add(5 * time.Millisecond) }
	_, _ = m.handleAgentViewMode(keyUp())
	if m.arrowPending {
		t.Fatal("pre-condition: want arrowPending=false after scroll flood")
	}

	// The stale tick from the first arrow fires — must be a no-op.
	_, _ = m.handleArrowReleaseMsg(arrowReleaseMsg{seq: oldSeq})
	if m.arrowPending {
		t.Error("stale release: want arrowPending still false")
	}
}

func TestArrowClassifier_NonArrowFlushesBuffered(t *testing.T) {
	m := classifierModel("C6")
	t0 := time.Unix(1000, 0)
	m.clock = func() time.Time { return t0 }

	// Buffer an up arrow.
	_, _ = m.handleAgentViewMode(keyUp())
	if !m.arrowPending {
		t.Fatal("pre-condition: want arrowPending=true")
	}

	// A non-arrow key arrives well after the lead window.
	m.clock = func() time.Time { return t0.Add(50 * time.Millisecond) }
	_, _ = m.handleAgentViewMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})

	if m.arrowPending {
		t.Error("after non-arrow: want arrowPending=false (buffered arrow flushed)")
	}
}

func TestReconcileMouseMode(t *testing.T) {
	tests := []struct {
		name      string
		prevMode  Mode
		currMode  Mode
		wantNil   bool
		wantClear bool // arrowPending should be cleared on transition
	}{
		{"normal→agent: cmd emitted", ModeNormal, ModeAgentView, false, true},
		{"agent→normal: cmd emitted", ModeAgentView, ModeNormal, false, true},
		{"normal→normal: no-op", ModeNormal, ModeNormal, true, false},
		{"agent→agent: no-op", ModeAgentView, ModeAgentView, true, false},
		{"agent→spawning: cmd emitted", ModeAgentView, ModeSpawning, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Model{mode: tt.currMode, arrowPending: true}
			cmd := m.reconcileMouseMode(tt.prevMode)
			if (cmd == nil) != tt.wantNil {
				t.Errorf("cmd nil=%v, want nil=%v", cmd == nil, tt.wantNil)
			}
			if tt.wantClear && m.arrowPending {
				t.Error("on transition: want arrowPending cleared")
			}
			if !tt.wantClear && !m.arrowPending {
				t.Error("no-op: want arrowPending unchanged (still true)")
			}
		})
	}
}
