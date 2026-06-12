package ui

import "testing"

// TestAgentChromeHeight pins the contract used by handleAgentViewMouse
// to translate host-terminal mouse Y coords into pane-relative coords.
// Off-by-one bugs in selection / forwarded mouse events trace back to
// this mapping being wrong.
func TestAgentChromeHeight(t *testing.T) {
	cases := []struct {
		name    string
		hasDeps bool
		want    int
	}{
		{"no deps line, header only", false, 1},
		{"deps line present", true, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentChromeHeight(tc.hasDeps); got != tc.want {
				t.Errorf("agentChromeHeight(%v) = %d, want %d", tc.hasDeps, got, tc.want)
			}
		})
	}
}
