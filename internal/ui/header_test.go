package ui

import (
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/board"
)

func TestAgentStatusGlyph(t *testing.T) {
	tests := []struct {
		name     string
		status   board.AgentStatus
		wantIcon string
		wantLbl  string
	}{
		{"working", board.AgentWorking, "●", "working"},
		{"waiting", board.AgentWaiting, "◐", "waiting"},
		{"idle", board.AgentIdle, "◆", "idle"},
		{"completed", board.AgentCompleted, "✓", "done"},
		{"error", board.AgentError, "✗", "error"},
		{"none -> empty", board.AgentNone, "", ""},
		{"unknown -> empty", board.AgentStatus("garbage"), "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			icon, label := agentStatusGlyph(tt.status)
			if icon != tt.wantIcon {
				t.Errorf("icon: got %q, want %q", icon, tt.wantIcon)
			}
			if label != tt.wantLbl {
				t.Errorf("label: got %q, want %q", label, tt.wantLbl)
			}
		})
	}
}

func TestPriorityGlyph(t *testing.T) {
	tests := []struct {
		priority int
		want     string
	}{
		{1, "⌃⌃"},
		{2, "⌃⎯"},
		{3, "⎯⎯"},
		{4, "⎯⌄"},
		{5, "⌄⌄"},
		{0, ""},
		{6, ""},
		{-1, ""},
	}
	for _, tt := range tests {
		got := priorityGlyph(tt.priority)
		if got != tt.want {
			t.Errorf("priorityGlyph(%d): got %q, want %q", tt.priority, got, tt.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name string
		secs int
		want string
	}{
		{"0 seconds", 0, "0s"},
		{"45 seconds", 45, "45s"},
		{"1 minute", 60, "1m"},
		{"5 minutes", 5 * 60, "5m"},
		{"59 minutes", 59 * 60, "59m"},
		{"1 hour exact", 3600, "1h"},
		{"2h 15m", 2*3600 + 15*60, "2h15m"},
		{"3h 0m", 3 * 3600, "3h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := time.Duration(tt.secs) * time.Second
			got := formatDuration(d)
			if got != tt.want {
				t.Errorf("formatDuration(%v): got %q, want %q", d, got, tt.want)
			}
		})
	}
}
