package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/board"
)

func TestMapOpencodeStatus(t *testing.T) {
	d := NewStatusDetector()

	tests := []struct {
		name     string
		input    opencodeSessionStatus
		expected board.AgentStatus
	}{
		{
			name:     "busy maps to working",
			input:    opencodeSessionStatus{Type: "busy"},
			expected: board.AgentWorking,
		},
		{
			name:     "idle maps to idle",
			input:    opencodeSessionStatus{Type: "idle"},
			expected: board.AgentIdle,
		},
		{
			name:     "retry maps to error",
			input:    opencodeSessionStatus{Type: "retry"},
			expected: board.AgentError,
		},
		{
			name:     "unknown type maps to none",
			input:    opencodeSessionStatus{Type: "unknown"},
			expected: board.AgentNone,
		},
		{
			name:     "empty type maps to none",
			input:    opencodeSessionStatus{Type: ""},
			expected: board.AgentNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := d.mapOpencodeStatus(tt.input)
			if result != tt.expected {
				t.Errorf("mapOpencodeStatus(%+v) = %q; want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestDetectCodingAgentStatus(t *testing.T) {
	d := NewStatusDetector()

	tests := []struct {
		name        string
		recentLower string
		fullLower   string
		expected    board.AgentStatus
	}{
		{
			name:        "waiting for user input",
			recentLower: "waiting for your response",
			fullLower:   "",
			expected:    board.AgentWaiting,
		},
		{
			name:        "yes/no prompt",
			recentLower: "do you want to proceed? [y/n]",
			fullLower:   "",
			expected:    board.AgentWaiting,
		},
		{
			name:        "permission request",
			recentLower: "approve this change?",
			fullLower:   "",
			expected:    board.AgentWaiting,
		},
		{
			name:        "thinking indicator",
			recentLower: "thinking about the problem...",
			fullLower:   "",
			expected:    board.AgentWorking,
		},
		{
			name:        "processing",
			recentLower: "processing your request",
			fullLower:   "",
			expected:    board.AgentWorking,
		},
		{
			name:        "progress bar",
			recentLower: "downloading ━━━━━━━━",
			fullLower:   "",
			expected:    board.AgentWorking,
		},
		{
			name:        "error message",
			recentLower: "error: failed to compile",
			fullLower:   "",
			expected:    board.AgentError,
		},
		{
			name:        "rate limit error",
			recentLower: "rate limit exceeded, please wait",
			fullLower:   "",
			expected:    board.AgentError,
		},
		{
			name:        "idle at prompt",
			recentLower: "ready for input >",
			fullLower:   "",
			expected:    board.AgentNone,
		},
		{
			name:        "no clear status",
			recentLower: "some random output",
			fullLower:   "",
			expected:    board.AgentNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := d.detectCodingAgentStatus(tt.recentLower, tt.fullLower)
			if result != tt.expected {
				t.Errorf("detectCodingAgentStatus(%q, %q) = %q; want %q",
					tt.recentLower, tt.fullLower, result, tt.expected)
			}
		})
	}
}

func TestDetectGenericAgentStatus(t *testing.T) {
	d := NewStatusDetector()

	tests := []struct {
		name        string
		recentLower string
		expected    board.AgentStatus
	}{
		{
			name:        "error in output",
			recentLower: "something went wrong, error occurred",
			expected:    board.AgentError,
		},
		{
			name:        "failed message",
			recentLower: "operation failed",
			expected:    board.AgentError,
		},
		{
			name:        "processing dots",
			recentLower: "loading...",
			expected:    board.AgentWorking,
		},
		{
			name:        "processing keyword",
			recentLower: "processing data",
			expected:    board.AgentWorking,
		},
		{
			name:        "normal output",
			recentLower: "completed successfully",
			expected:    board.AgentNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := d.detectGenericAgentStatus(tt.recentLower)
			if result != tt.expected {
				t.Errorf("detectGenericAgentStatus(%q) = %q; want %q",
					tt.recentLower, result, tt.expected)
			}
		})
	}
}

func TestDetectStatusWithPort_NotRunning(t *testing.T) {
	d := NewStatusDetector()

	result := d.DetectStatusWithPort("opencode", "session-1", "/path", 4097, false, "")
	if result != board.AgentNone {
		t.Errorf("DetectStatusWithPort with processRunning=false should return AgentNone; got %q", result)
	}
}

func TestDetectStatusWithPort_UnknownStatus(t *testing.T) {
	d := NewStatusDetector()

	result := d.DetectStatusWithPort("opencode", "nonexistent-session", "/nonexistent/path", 0, true, "some random output with no patterns")
	if result != board.AgentNone {
		t.Errorf("DetectStatusWithPort with undetermined status should return AgentNone; got %q", result)
	}
}

func TestStatusDetectorCaching(t *testing.T) {
	d := NewStatusDetector()

	d.statusCacheMu.Lock()
	d.statusCache["file:test-session"] = cachedStatus{
		status:    board.AgentWorking,
		timestamp: time.Now(),
	}
	d.statusCacheMu.Unlock()

	result := d.readStatusFile("test-session")
	if result != board.AgentWorking {
		t.Errorf("readStatusFile should return cached status; got %q, want %q", result, board.AgentWorking)
	}
}

func TestReadStatusFile_FromDisk(t *testing.T) {
	tmpDir := t.TempDir()

	d := NewStatusDetector()
	d.statusDirs = []string{tmpDir}

	statusFile := filepath.Join(tmpDir, "test-session.status")
	if err := os.WriteFile(statusFile, []byte("working"), 0644); err != nil {
		t.Fatalf("failed to create status file: %v", err)
	}

	result := d.readStatusFile("test-session")
	if result != board.AgentWorking {
		t.Errorf("readStatusFile should return AgentWorking; got %q", result)
	}
}

// TestDetectStatusWithActivity_OverridesWaiting pins the core override
// behavior. The file-based detector returns AgentWaiting (Notification
// hook fired); when the daemon-reported PTY activity is recent, the
// override flips it to AgentWorking. Stale or absent activity leaves
// the file's verdict alone.
func TestDetectStatusWithActivity_OverridesWaiting(t *testing.T) {
	tmpDir := t.TempDir()
	d := NewStatusDetector()
	d.statusDirs = []string{tmpDir}

	statusFile := filepath.Join(tmpDir, "sess.status")
	if err := os.WriteFile(statusFile, []byte("waiting"), 0644); err != nil {
		t.Fatalf("write status: %v", err)
	}

	tests := []struct {
		name         string
		lastActivity time.Time
		want         board.AgentStatus
	}{
		{
			name:         "recent activity overrides waiting",
			lastActivity: time.Now().Add(-2 * time.Second),
			want:         board.AgentWorking,
		},
		{
			name:         "activity past TTL boundary is stale",
			lastActivity: time.Now().Add(-(WaitingActivityTTL + time.Second)),
			want:         board.AgentWaiting,
		},
		{
			name:         "stale activity leaves waiting",
			lastActivity: time.Now().Add(-5 * time.Minute),
			want:         board.AgentWaiting,
		},
		{
			name:         "zero activity (no daemon report) leaves waiting",
			lastActivity: time.Time{},
			want:         board.AgentWaiting,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Invalidate the 500ms cache between subtests so each call
			// re-reads the (unchanged) file rather than serving a hit.
			d.InvalidateCache("sess")
			got := d.DetectStatusWithActivity("claude", "sess", "", 0, true, "", tt.lastActivity)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDetectStatusWithActivity_NoDowngrade ensures the override never
// downgrades a non-waiting status. Even when activity is present, a
// file saying "working" / "idle" / "completed" passes through.
func TestDetectStatusWithActivity_NoDowngrade(t *testing.T) {
	tmpDir := t.TempDir()
	d := NewStatusDetector()
	d.statusDirs = []string{tmpDir}
	statusFile := filepath.Join(tmpDir, "sess.status")

	tests := []struct {
		name     string
		fileBody string
		want     board.AgentStatus
	}{
		{"working file is preserved", "working", board.AgentWorking},
		{"idle file is preserved", "idle", board.AgentIdle},
		{"completed file is preserved", "completed", board.AgentCompleted},
		{"error file is preserved", "error", board.AgentError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(statusFile, []byte(tt.fileBody), 0644); err != nil {
				t.Fatalf("write status: %v", err)
			}
			d.InvalidateCache("sess")
			// Recent activity is present; the override must NOT touch
			// non-waiting verdicts.
			got := d.DetectStatusWithActivity("claude", "sess", "", 0, true, "", time.Now())
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDetectStatusWithActivity_ProcessNotRunning confirms the
// activity override doesn't paper over a dead pane — when
// processRunning is false, DetectStatusWithPort short-circuits to
// AgentNone and the override must respect that.
func TestDetectStatusWithActivity_ProcessNotRunning(t *testing.T) {
	d := NewStatusDetector()
	got := d.DetectStatusWithActivity("claude", "sess", "", 0, false, "", time.Now())
	if got != board.AgentNone {
		t.Errorf("got %q, want AgentNone when process not running", got)
	}
}
