package ui

import (
	"errors"
	"testing"

	"github.com/techdufus/openkanban/internal/daemonclient"
)

// TestShouldRetryAttachOnEnter covers the predicate that gates Enter-
// interception in handleAgentViewMode. The matrix mirrors the user-
// facing failure-overlay semantics in PaneView.View(): the overlay
// renders exactly when this returns true, and the Enter retry fires
// exactly when this returns true. Keeping them locked to the same
// predicate prevents the "I see the retry hint but pressing Enter
// does nothing" failure mode.
func TestShouldRetryAttachOnEnter(t *testing.T) {
	tests := []struct {
		name string
		pane func() *daemonclient.PaneView
		want bool
	}{
		{
			name: "nil pane",
			pane: func() *daemonclient.PaneView { return nil },
			want: false,
		},
		{
			name: "fresh Detached pane, no attach attempted",
			pane: func() *daemonclient.PaneView {
				return daemonclient.NewPaneView(nil, "T-FRESH", "", nil)
			},
			want: false,
		},
		{
			name: "Detached + lastAttachErr set (the stuck case)",
			pane: func() *daemonclient.PaneView {
				pv := daemonclient.NewPaneView(nil, "T-STUCK", "", nil)
				pv.SetLastAttachErr(errors.New("dial timeout"))
				return pv
			},
			want: true,
		},
		{
			name: "lastAttachErr cleared (post-retry success)",
			pane: func() *daemonclient.PaneView {
				pv := daemonclient.NewPaneView(nil, "T-CLEARED", "", nil)
				pv.SetLastAttachErr(errors.New("transient"))
				pv.SetLastAttachErr(nil)
				return pv
			},
			want: false,
		},
		{
			name: "Attached state with stale lastAttachErr should still skip retry",
			// State()=Attached is the override regardless of error
			// staleness — Attach()'s success path clears the error,
			// but defensively we don't want to fire a redundant
			// retry against a live attached pane.
			pane: func() *daemonclient.PaneView {
				pv := daemonclient.NewPaneView(nil, "T-ATTACHED", "", nil)
				pv.SetLastAttachErr(errors.New("ignored"))
				pv.SetPaneStateForTest(daemonclient.PaneViewAttached)
				return pv
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldRetryAttachOnEnter(tt.pane())
			if got != tt.want {
				t.Errorf("shouldRetryAttachOnEnter() = %v, want %v", got, tt.want)
			}
		})
	}
}
