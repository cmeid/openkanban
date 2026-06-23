package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
)

// TestSpawnOverlayLabelFlip verifies the spawn overlay shows "Starting"
// during the bounded spawn-RPC window and flips to "Attaching" only once
// spawnAttachLabelDelay has elapsed — so a slow post-spawn attach reads
// honestly instead of a perpetual, misleading "Starting".
func TestSpawnOverlayLabelFlip(t *testing.T) {
	tests := []struct {
		name       string
		startedAgo time.Duration
		zeroStart  bool
		wantStart  bool // expect "Starting claude"
		wantAttach bool // expect "Attaching to claude"
	}{
		{name: "just started shows Starting", startedAgo: 100 * time.Millisecond, wantStart: true},
		{name: "below threshold still Starting", startedAgo: spawnAttachLabelDelay - time.Second, wantStart: true},
		{name: "past threshold flips to Attaching", startedAgo: spawnAttachLabelDelay + time.Second, wantAttach: true},
		{name: "zero start is defensive Starting", zeroStart: true, wantStart: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Model{
				mode:          ModeSpawning,
				spawningAgent: "claude",
				spinner:       spinner.New(),
				width:         80,
				height:        24,
			}
			if !tt.zeroStart {
				m.spawnStartedAt = time.Now().Add(-tt.startedAgo)
			}

			out := m.renderSpawning()

			hasStart := strings.Contains(out, "Starting claude")
			hasAttach := strings.Contains(out, "Attaching to claude")

			if tt.wantStart && !hasStart {
				t.Errorf("want 'Starting claude' in overlay, got:\n%s", out)
			}
			if tt.wantAttach && !hasAttach {
				t.Errorf("want 'Attaching to claude' in overlay, got:\n%s", out)
			}
			// The two labels are mutually exclusive — assert the other is absent.
			if tt.wantStart && hasAttach {
				t.Errorf("did not expect 'Attaching' while in the Starting window, got:\n%s", out)
			}
			if tt.wantAttach && hasStart {
				t.Errorf("did not expect 'Starting' after the flip, got:\n%s", out)
			}
		})
	}
}
