package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// TestDetectStatusWithPort_PreservesTerminalStatusWhenNotRunning verifies that
// when a status file holds a terminal status (completed/error) it survives the
// process having exited, while transient states (working) do not.
func TestDetectStatusWithPort_PreservesTerminalStatusWhenNotRunning(t *testing.T) {
	tmpDir := t.TempDir()
	d := NewStatusDetector()
	d.statusDirs = []string{tmpDir}

	tests := []struct {
		name           string
		sessionID      string
		fileContent    string
		processRunning bool
		expected       board.AgentStatus
	}{
		{
			name:           "completed survives process exit",
			sessionID:      "terminal-completed",
			fileContent:    "completed",
			processRunning: false,
			expected:       board.AgentCompleted,
		},
		{
			name:           "error survives process exit",
			sessionID:      "terminal-error",
			fileContent:    "error",
			processRunning: false,
			expected:       board.AgentError,
		},
		{
			name:           "working is suppressed when process exited",
			sessionID:      "transient-working-exited",
			fileContent:    "working",
			processRunning: false,
			expected:       board.AgentNone,
		},
		{
			name:           "working returned when process still running",
			sessionID:      "transient-working-running",
			fileContent:    "working",
			processRunning: true,
			expected:       board.AgentWorking,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessionID := tt.sessionID
			statusFile := filepath.Join(tmpDir, sessionID+".status")
			if err := os.WriteFile(statusFile, []byte(tt.fileContent), 0644); err != nil {
				t.Fatalf("failed to write status file: %v", err)
			}
			// Clear cache between subtests so the file is re-read every time.
			d.InvalidateCache(sessionID)

			// agentType/port irrelevant when status file resolves the status,
			// and processRunning=false short-circuits the API call anyway.
			result := d.DetectStatusWithPort("opencode", sessionID, "", 0, tt.processRunning, "")
			if result != tt.expected {
				t.Errorf("DetectStatusWithPort(file=%q, running=%v) = %q; want %q",
					tt.fileContent, tt.processRunning, result, tt.expected)
			}
		})
	}
}

// TestDetectStatusWithPort_ScopesOpencodeSession verifies that a single busy
// session on a shared opencode port does NOT mark other sessions on the same
// port as working — the lookup is keyed on sessionID.
func TestDetectStatusWithPort_ScopesOpencodeSession(t *testing.T) {
	server := httptest.NewServer(testOpencodeStatusHandler(t, `{"busy-session":{"type":"busy"}}`))
	defer server.Close()

	port := testServerPort(t, server.URL)

	// Use a fresh detector and a per-test status dir to avoid any
	// cross-test interference via the user's real ~/.cache.
	tmpDir := t.TempDir()
	d := NewStatusDetector()
	d.statusDirs = []string{tmpDir}

	tests := []struct {
		name      string
		sessionID string
		expected  board.AgentStatus
	}{
		{
			name:      "idle session on same port is not marked working",
			sessionID: "idle-session",
			expected:  board.AgentIdle,
		},
		{
			name:      "busy session is marked working",
			sessionID: "busy-session",
			expected:  board.AgentWorking,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear any cached value from a previous subtest.
			d.InvalidateCache(tt.sessionID)

			result := d.DetectStatusWithPort("opencode", tt.sessionID, "", port, true, "")
			if result != tt.expected {
				t.Errorf("DetectStatusWithPort(session=%q, port=%d) = %q; want %q",
					tt.sessionID, port, result, tt.expected)
			}
		})
	}
}

// testOpencodeStatusHandler returns an http.Handler that responds to
// GET /session/status with the supplied JSON body. Any other path fails
// the test loudly.
func testOpencodeStatusHandler(t *testing.T, body string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session/status" {
			t.Errorf("unexpected request path: %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, body); err != nil {
			t.Errorf("write response: %v", err)
		}
	}
}

// testServerPort extracts the port number from an httptest.Server URL.
func testServerPort(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse test server URL %q: %v", rawURL, err)
	}
	// httptest binds to 127.0.0.1, which queryOpencodeAPIOnPort doesn't
	// know about — it always dials localhost. On every supported dev
	// platform 127.0.0.1 and localhost resolve to the same loopback, so
	// the port is enough.
	if !strings.HasPrefix(u.Host, "127.0.0.1:") && !strings.HasPrefix(u.Host, "[::1]:") {
		t.Fatalf("unexpected test server host %q (need loopback)", u.Host)
	}
	portStr := u.Port()
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	return port
}
