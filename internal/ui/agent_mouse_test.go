package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestRouteAgentViewMouse pins the routing contract for mouse events
// in agent view. Regression: b61c3c5 introduced a chrome-offset Y
// subtraction that dropped ALL events landing on the chrome rows,
// including wheel scrolls. Wheel events are position-insensitive
// and must be forwarded to the pane even when the cursor sits on
// the header or deps line.
func TestRouteAgentViewMouse(t *testing.T) {
	const width = 120

	cases := []struct {
		name     string
		msg      tea.MouseMsg
		chrome   int
		wantAct  agentViewMouseAction
		wantY    int
	}{
		{
			name: "close-button click on row 0 right edge",
			msg: tea.MouseMsg{
				X: width - 10, Y: 0,
				Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
			},
			chrome:  1,
			wantAct: agentViewMouseCloseModal,
		},
		{
			name: "left-click inside pane area",
			msg: tea.MouseMsg{
				X: 10, Y: 5,
				Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
			},
			chrome:  1,
			wantAct: agentViewMouseForward,
			wantY:   4,
		},
		{
			name: "left-click on chrome (header) — drop",
			msg: tea.MouseMsg{
				X: 10, Y: 0,
				Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
			},
			chrome:  1,
			wantAct: agentViewMouseDrop,
		},
		{
			name: "left-click on chrome (deps line) — drop",
			msg: tea.MouseMsg{
				X: 10, Y: 1,
				Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
			},
			chrome:  2,
			wantAct: agentViewMouseDrop,
		},
		{
			name: "wheel-up on chrome — forward with clamped Y",
			msg: tea.MouseMsg{
				X: 10, Y: 0,
				Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp,
			},
			chrome:  1,
			wantAct: agentViewMouseForward,
			wantY:   0,
		},
		{
			name: "wheel-down on chrome — forward with clamped Y",
			msg: tea.MouseMsg{
				X: 10, Y: 1,
				Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown,
			},
			chrome:  2,
			wantAct: agentViewMouseForward,
			wantY:   0,
		},
		{
			name: "wheel-up inside pane area — forward with adjusted Y",
			msg: tea.MouseMsg{
				X: 10, Y: 5,
				Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp,
			},
			chrome:  1,
			wantAct: agentViewMouseForward,
			wantY:   4,
		},
		{
			name: "motion (drag) on chrome — drop",
			msg: tea.MouseMsg{
				X: 10, Y: 0,
				Action: tea.MouseActionMotion, Button: tea.MouseButtonNone,
			},
			chrome:  1,
			wantAct: agentViewMouseDrop,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, action := routeAgentViewMouse(tc.msg, width, tc.chrome)
			if action != tc.wantAct {
				t.Fatalf("action = %v, want %v", action, tc.wantAct)
			}
			if action == agentViewMouseForward && got.Y != tc.wantY {
				t.Errorf("forwarded Y = %d, want %d", got.Y, tc.wantY)
			}
		})
	}
}
