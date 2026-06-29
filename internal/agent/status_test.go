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
			// The core regression: the leading "waiting for" must NOT win —
			// a foreground agent awaiting background sub-agents is occupied,
			// not blocked on the user.
			name:        "background agent wait (singular)",
			recentLower: "✻ waiting for 1 background agent to finish",
			fullLower:   "",
			expected:    board.AgentSubagents,
		},
		{
			name:        "background agents wait (plural)",
			recentLower: "✻ waiting for 3 background agents to finish",
			fullLower:   "",
			expected:    board.AgentSubagents,
		},
		{
			// Precision guard: "background agent" mentioned in prose without
			// the "to finish" status-line tail must NOT trigger sub-agents.
			name:        "prose mention of background agent is not the status line",
			recentLower: "i'll spawn a background agent to handle the search",
			fullLower:   "",
			expected:    board.AgentNone,
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

// TestDetectStatusWithActivity_NoWorkEvidenceStaysWaiting pins the core
// of the false-"working" fix: with the file at "waiting" and NO on-screen
// work evidence (empty grid — e.g. an unattached / bg-spawned session),
// the verdict stays "waiting" REGARDLESS of how recent the PTY activity
// is. This is the behavior change vs the old byte-recency override, which
// flipped any recent-activity case to "working" and so mislabeled a
// session parked at a prompt it was re-rendering.
func TestDetectStatusWithActivity_NoWorkEvidenceStaysWaiting(t *testing.T) {
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
			// The key change: recent activity no longer promotes to working
			// without on-screen evidence of an active turn.
			name:         "recent activity with empty grid stays waiting",
			lastActivity: time.Now().Add(-2 * time.Second),
			want:         board.AgentWaiting,
		},
		{
			name:         "activity past TTL boundary stays waiting",
			lastActivity: time.Now().Add(-(WaitingActivityTTL + time.Second)),
			want:         board.AgentWaiting,
		},
		{
			name:         "stale activity stays waiting",
			lastActivity: time.Now().Add(-5 * time.Minute),
			want:         board.AgentWaiting,
		},
		{
			name:         "zero activity (no daemon report) stays waiting",
			lastActivity: time.Time{},
			want:         board.AgentWaiting,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Invalidate the 500ms cache between subtests so each call
			// re-reads the (unchanged) file rather than serving a hit.
			d.InvalidateCache("sess")
			got := d.DetectStatusWithActivity("claude", "sess", "sess", "", 0, true, "", tt.lastActivity)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDetectStatusWithActivity_PermissionPromptStaysWaiting pins the
// fix for the permission-prompt masking bug. Rendering Claude Code's
// tool-approval box is itself PTY output, so the box's render stamps
// fresh activity at the same instant the Notification hook writes
// "waiting". Without the on-screen-prompt guard, the activity override
// (meant for a tool running AFTER the user grants permission) flips
// "waiting" to "working" and the card never shows the blocked state
// for the whole approve-within-TTL window. The prompt's on-screen text
// must hold the verdict at "waiting" despite recent activity, while a
// genuinely-running tool (no prompt) still overrides to "working".
func TestDetectStatusWithActivity_PermissionPromptStaysWaiting(t *testing.T) {
	tmpDir := t.TempDir()
	d := NewStatusDetector()
	d.statusDirs = []string{tmpDir}
	statusFile := filepath.Join(tmpDir, "sess.status")
	if err := os.WriteFile(statusFile, []byte("waiting"), 0644); err != nil {
		t.Fatalf("write status: %v", err)
	}

	bashPrompt := strings.Join([]string{
		" Bash command",
		"   echo \"=== MY session id ===\"",
		"   Confirm session id + read linking memories",
		" Contains simple_expansion",
		" Do you want to proceed?",
		" ❯ 1. Yes",
		"   2. No",
		" Esc to cancel · Tab to amend · ctrl+e to explain",
	}, "\n")

	tests := []struct {
		name            string
		terminalContent string
		want            board.AgentStatus
	}{
		{
			name:            "open prompt holds waiting despite fresh activity",
			terminalContent: bashPrompt,
			want:            board.AgentWaiting,
		},
		{
			name: "plan-approval prompt holds waiting",
			terminalContent: strings.Join([]string{
				" Claude has written up a plan and is ready to execute. Would you like to proceed?",
				" ❯ 1. Yes, and auto-accept edits",
				"   2. No, keep planning",
			}, "\n"),
			want: board.AgentWaiting,
		},
		{
			name:            "running tool (no prompt) still overrides to working",
			terminalContent: "⠹ Running bash command… (esc to interrupt)",
			want:            board.AgentWorking,
		},
		{
			// Behavior change vs the original #84: with the byte-recency
			// catch-all removed, an empty/unavailable grid (e.g. an
			// unattached session with no local PTY view) holds "waiting"
			// instead of being promoted to "working" on activity alone.
			name:            "empty content holds waiting (no work evidence)",
			terminalContent: "",
			want:            board.AgentWaiting,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d.InvalidateCache("sess")
			// Activity is fresh in every case — the discriminator is the
			// on-screen prompt, not the timer.
			got := d.DetectStatusWithActivity("claude", "sess", "sess", "", 0, true, tt.terminalContent, time.Now())
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDetectStatusWithActivity_BusyTurnNotWaiting pins the inverse of
// the prompt guard: a session busy on an already-approved tool (no
// prompt on screen) must not show "waiting" just because the file is
// pinned there and the tool happens to be silent. An active-turn marker
// on screen ("esc to interrupt" or a braille spinner) reclassifies to
// "working". The prompt guard still wins when both a prompt and a turn
// marker are present (the false-negative the critic flagged), and an
// idle screen with neither marker stays "waiting".
func TestDetectStatusWithActivity_BusyTurnNotWaiting(t *testing.T) {
	tmpDir := t.TempDir()
	d := NewStatusDetector()
	d.statusDirs = []string{tmpDir}
	statusFile := filepath.Join(tmpDir, "sess.status")
	if err := os.WriteFile(statusFile, []byte("waiting"), 0644); err != nil {
		t.Fatalf("write status: %v", err)
	}

	stale := time.Now().Add(-(WaitingActivityTTL + time.Second))
	recent := time.Now()

	runningFooter := strings.Join([]string{
		"⎿ Running…",
		"  $ go test ./...",
		"",
		" esc to interrupt",
	}, "\n")
	spinnerFooter := strings.Join([]string{
		"  exploring the codebase",
		"",
		" ⠹ Thinking…",
	}, "\n")
	idleBox := strings.Join([]string{
		"  Done. Anything else?",
		"",
		" │ >                                  │",
		" ? for shortcuts",
	}, "\n")
	// Adversarial: a prompt AND a turn marker on one screen. Cannot occur
	// in Claude's real UI, but proves the prompt guard is ordered first.
	promptPlusInterrupt := strings.Join([]string{
		" Do you want to proceed?",
		" ❯ 1. Yes",
		"   2. No",
		" Esc to cancel · Tab to amend",
		" esc to interrupt",
	}, "\n")
	promptPlusSpinner := strings.Join([]string{
		" Do you want to proceed?",
		" ❯ 1. Yes",
		"   2. No",
		" ⠹ Esc to cancel",
	}, "\n")
	// 2.1.181+ footer: " · x to stop". No esc/braille/permission text so
	// this row exercises ONLY the " x to stop" marker (non-vacuous proof).
	// Red-before-green: remove only " x to stop" from activeTurnMarkers
	// (keep "esc to interrupt") and this row must go RED while all
	// esc-to-interrupt rows stay GREEN.
	xToStopFooter := strings.Join([]string{
		"⎿ Running…",
		"  $ go test ./...",
		"",
		" · x to stop",
	}, "\n")

	tests := []struct {
		name            string
		terminalContent string
		lastActivity    time.Time
		want            board.AgentStatus
	}{
		// RED before the fix: stale activity, so the flip can only come
		// from the new active-turn marker, not the activity fallback.
		{"running footer with esc-to-interrupt is working", runningFooter, stale, board.AgentWorking},
		{"braille spinner footer is working", spinnerFooter, stale, board.AgentWorking},
		// 2.1.181 marker — RED when only " x to stop" is removed from
		// activeTurnMarkers (per-marker revert, not combined).
		{"x-to-stop footer is working (2.1.181)", xToStopFooter, stale, board.AgentWorking},
		// Ordering guards: recent activity, so WITHOUT the prompt guard
		// these would return working — proving prompt-first wins over both
		// the turn marker and the activity fallback (not vacuous).
		{"prompt plus interrupt marker stays waiting", promptPlusInterrupt, recent, board.AgentWaiting},
		{"prompt plus braille glyph stays waiting", promptPlusSpinner, recent, board.AgentWaiting},
		// Precision guard: idle screen, no marker — must not over-match.
		{"idle input box stays waiting", idleBox, stale, board.AgentWaiting},
		// 2.1.181+ activity-counter spinner — live captures from two running sessions
		// (Opus plan-mode and Sonnet auto-mode, 2026-06-29). The "· ↓ N tokens"
		// separator is the drift-resistant anchor added in this fix. RED when only
		// "· ↓ " is removed from activeTurnMarkers (per-marker revert, same discipline
		// as xToStopFooter above).
		{"plan-mode spinner promotes waiting to working (Opus, 2.1.181+)",
			"· Considering… (2m 17s · ↓ 6.4k tokens · still thinking)",
			stale, board.AgentWorking},
		{"auto-mode spinner promotes waiting to working (Sonnet, 2.1.181+)",
			"✢ Razzle-dazzling… (9m 5s · ↓ 16.4k tokens)",
			stale, board.AgentWorking},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d.InvalidateCache("sess")
			got := d.DetectStatusWithActivity("claude", "sess", "sess", "", 0, true, tt.terminalContent, tt.lastActivity)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDetectStatusWithActivity_StaleWorkingDemotedOnPrompt pins the
// stale-"working" fix. Claude's Notification hook does not reliably fire
// for every input-needed state — an AskUserQuestion prompt was observed
// pinning a session's status file at "working" for hours while it sat
// blocked on the user. When the file says "working" but the live grid
// shows a recognized approval/question prompt and NO active-turn marker,
// the session is needs-you and must surface as "waiting". The
// activeTurnVisible guard keeps a genuinely busy session at "working".
//
// The askUserQuestionGrid footer is the verbatim string Claude Code
// renders for AskUserQuestion ("… · Esc to cancel"), captured from a live
// daemon Peek of the specimen that motivated this fix.
func TestDetectStatusWithActivity_StaleWorkingDemotedOnPrompt(t *testing.T) {
	tmpDir := t.TempDir()
	d := NewStatusDetector()
	d.statusDirs = []string{tmpDir}
	statusFile := filepath.Join(tmpDir, "sess.status")
	if err := os.WriteFile(statusFile, []byte("working"), 0644); err != nil {
		t.Fatalf("write status: %v", err)
	}

	askUserQuestionGrid := strings.Join([]string{
		" ❯ 1. Default + visible (Recommended)",
		"   2. Lock to project default",
		"   3. Confirm to deviate",
		"   4. Type something.",
		"",
		" Enter to select · Tab/Arrow keys to navigate · Esc to cancel",
	}, "\n")

	permissionBox := strings.Join([]string{
		" Do you want to proceed?",
		" ❯ 1. Yes",
		"   2. No",
		" Esc to cancel · Tab to amend",
	}, "\n")

	// The ExitPlanMode / plan-approval prompt. Note it carries neither
	// "do you want to" nor an "esc to cancel" footer — its only stable
	// signature is the body text "Would you like to proceed?", so this
	// case fails unless that string is in permissionPromptSignatures.
	planApprovalGrid := strings.Join([]string{
		" Claude has written up a plan and is ready to execute. Would you like to proceed?",
		" ❯ 1. Yes, and auto-accept edits",
		"   2. Yes, and manually approve edits",
		"   3. No, keep planning",
	}, "\n")

	// Additional hook-silent "Would you like to …?" confirmation boxes that
	// carry neither "do you want to" nor an "esc to cancel" footer — each
	// needs its own body signature, same as planApprovalGrid above. Wordings
	// captured verbatim from the bundled binary (claude-code 2.1.179);
	// re-verified present in 2.1.181 binary grep.
	pluginInstallGrid := strings.Join([]string{
		" Would you like to install this LSP plugin?",
		" ❯ 1. Yes",
		"   2. No",
	}, "\n")
	manifestGrid := strings.Join([]string{
		" Would you like to create a manifest?",
		" ❯ 1. Yes",
		"   2. No",
	}, "\n")
	stashGrid := strings.Join([]string{
		" Would you like to stash these changes and continue with teleport?",
		" ❯ 1. Yes",
		"   2. No",
	}, "\n")

	tests := []struct {
		name            string
		terminalContent string
		want            board.AgentStatus
	}{
		// The fix: a blocked-on-user prompt on screen demotes the stale
		// "working" file to "waiting".
		{"AskUserQuestion prompt demotes stale working", askUserQuestionGrid, board.AgentWaiting},
		{"permission box demotes stale working", permissionBox, board.AgentWaiting},
		{"plan-approval prompt demotes stale working", planApprovalGrid, board.AgentWaiting},
		{"plugin install prompt demotes stale working", pluginInstallGrid, board.AgentWaiting},
		{"manifest prompt demotes stale working", manifestGrid, board.AgentWaiting},
		{"stash prompt demotes stale working", stashGrid, board.AgentWaiting},
		// Guards against over-demotion — these must STAY "working":
		{"active turn alone preserves working", "⠹ Running bash command… (esc to interrupt)", board.AgentWorking},
		{"x-to-stop alone preserves working (2.1.181)", " · x to stop", board.AgentWorking},
		{"no prompt + no marker preserves working (file authoritative)", "some streamed tool output\nrunning tests", board.AgentWorking},
		{"empty grid preserves working (fails safe)", "", board.AgentWorking},
		// Asymmetry vs the waiting-branch: in the working-branch the
		// activeTurn marker GUARDS (stays working) rather than the prompt
		// winning — the combo is impossible in Claude's real UI, and the
		// conservative choice for a file already saying "working" is to not
		// demote when any active-turn evidence is present.
		{"prompt plus interrupt marker stays working (guard)", askUserQuestionGrid + "\n (esc to interrupt)", board.AgentWorking},
		{"prompt plus x-to-stop stays working (guard, 2.1.181)", askUserQuestionGrid + "\n · x to stop", board.AgentWorking},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d.InvalidateCache("sess")
			got := d.DetectStatusWithActivity("claude", "sess", "sess", "", 0, true, tt.terminalContent, time.Now())
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPermissionPromptVisible_SignatureCoverageLedger is the authoritative
// ledger of the real Claude Code prompt strings permissionPromptVisible must
// recognize as a blocked-on-user state. Each row is a wording captured
// verbatim from the bundled binary (claude-code 2.1.179); re-verified
// present in 2.1.181 binary grep (Do you want to proceed?, Would you like
// to …, Esc to cancel families all confirmed). A row flipping to false means
// EITHER a signature was dropped from permissionPromptSignatures OR Claude's
// wording drifted — both are real regressions, NOT expectations to "fix" by
// editing want. When refreshing for a new Claude version, update the fixture
// string AND the matching signature in lockstep (verify against the binary
// per memory reference_verify_claude_scraper_signatures_via_binary).
//
// The AskUserQuestion row is the load-bearing drift guard: that prompt has no
// distinctive "?"-body of its own and is recognized ONLY via its "Esc to
// cancel" footer, so if a future Claude build changes that footer this row is
// what catches the silent regression to stale-"working".
func TestPermissionPromptVisible_SignatureCoverageLedger(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"AskUserQuestion footer (drift guard)", " Enter to select · Tab/Arrow keys to navigate · Esc to cancel", true},
		{"plan-approval body", " Claude has written up a plan and is ready to execute. Would you like to proceed?", true},
		{"tool permission body", " Do you want to proceed?", true},
		{"plugin install prompt", " Would you like to install this LSP plugin?", true},
		{"manifest creation prompt", " Would you like to create a manifest?", true},
		{"teleport stash prompt", " Would you like to stash these changes and continue with teleport?", true},
		{"plain streamed output (no prompt)", "running tests\nediting status.go\nall checks passed", false},
		// Negative control: a "Would you like to …?" prompt whose tail matches
		// NONE of the discriminating signatures must NOT match. Guards against a
		// future edit truncating e.g. "would you like to install" back toward a
		// bare "would you like to" (which would then catch agent narration).
		{"would-you-like near-miss (no discriminating tail)", " Would you like to review the changes?", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := permissionPromptVisible(tt.content); got != tt.want {
				t.Errorf("permissionPromptVisible(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

// TestDetectStatusWithActivity_BackgroundAgentWait pins the sub-agents
// status. A foreground Claude turn that delegated to background sub-agents
// shows "✻ Waiting for N background agent(s) to finish" while NO hook fires
// for the wait — so the status file stays pinned at its last value
// ("working" from the turn, or "waiting" from the delegating turn's
// permission prompt). The leading "Waiting for ..." would otherwise classify
// as AgentWaiting (orange, needs-you). The screen is the discriminator: this
// is idle-but-occupied, not blocked on the user. The check is placed ABOVE
// the working/waiting branches so it wins regardless of the file value, but
// MUST NOT override a terminal completed/error.
//
// These cases seed a real status file + processRunning=true so
// DetectStatusWithPortAPI returns the file status and the new high-precedence
// block is the thing under test (not a coincidental AgentNone fallthrough).
func TestDetectStatusWithActivity_BackgroundAgentWait(t *testing.T) {
	bgWaitGrid := strings.Join([]string{
		"  Spawning helpers…",
		"",
		" ✻ Waiting for 1 background agent to finish",
	}, "\n")
	bgWaitPlural := " ✻ Waiting for 3 background agents to finish"

	tests := []struct {
		name            string
		fileBody        string
		terminalContent string
		want            board.AgentStatus
	}{
		// (a) file=waiting + bg line → subagents (the primary production case:
		// the delegating turn left a "waiting" permission verdict behind).
		{"waiting file plus bg line is subagents", "waiting", bgWaitGrid, board.AgentSubagents},
		// (b) file=working + bg line → subagents. Proves the new block sits
		// ABOVE the working-branch return; otherwise this would stay working.
		{"working file plus bg line is subagents", "working", bgWaitGrid, board.AgentSubagents},
		{"working file plus plural bg line is subagents", "working", bgWaitPlural, board.AgentSubagents},
		// (c) GUARD: a terminal status is authoritative even with the bg line
		// still in scrollback. processRunning=true so it's the new terminal
		// guard doing the work, not the not-running short-circuit.
		{"completed file is not overridden", "completed", bgWaitGrid, board.AgentCompleted},
		{"error file is not overridden", "error", bgWaitGrid, board.AgentError},
		// (d) GUARD: prose mentioning "background agent" without the "to
		// finish" status-line tail must NOT trigger sub-agents.
		{"prose mention does not trigger", "working", "i'll use a background agent for this", board.AgentWorking},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			d := NewStatusDetector()
			d.statusDirs = []string{tmpDir}
			statusFile := filepath.Join(tmpDir, "sess.status")
			if err := os.WriteFile(statusFile, []byte(tt.fileBody), 0644); err != nil {
				t.Fatalf("write status: %v", err)
			}
			d.InvalidateCache("sess")
			got := d.DetectStatusWithActivity("claude", "sess", "sess", "", 0, true, tt.terminalContent, time.Now())
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDetectStatusWithActivity_NoDowngrade ensures the override never
// changes a non-waiting status when the terminal content is empty (no
// on-screen evidence of either a prompt or an active turn). Files saying
// "working" / "idle" / "completed" / "error" all pass through unchanged.
// Intentional exceptions tested elsewhere:
//   - stale "working" + visible prompt → "waiting" (StaleWorkingDemotedOnPrompt)
//   - stale "idle" + visible spinner → "working" (IdlePromotedOnSpinner)
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
			got := d.DetectStatusWithActivity("claude", "sess", "sess", "", 0, true, "", time.Now())
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
	got := d.DetectStatusWithActivity("claude", "sess", "", "", 0, false, "", time.Now())
	if got != board.AgentNone {
		t.Errorf("got %q, want AgentNone when process not running", got)
	}
}

// TestDetectStatusWithActivity_FileKeyVsAPIKey pins the fix for the
// bug where the file-lookup key drifted from OPENKANBAN_SESSION after
// the Claude UUID back-fill in pollAgentStatusesAsync. The hook writes
// to ~/.cache/openkanban-status/<OPENKANBAN_SESSION>.status (baked at
// spawn — usually the branch name). The detector must look up the
// same file regardless of any UUID we may also be tracking for
// --resume / opencode HTTP. Pre-fix, the poll passed the UUID as
// sessionID, the file was missing under that key, and the detector
// fell through to terminal-content scraping, which misclassifies
// Claude's input-prompt border (━) as "working".
func TestDetectStatusWithActivity_FileKeyVsAPIKey(t *testing.T) {
	tmpDir := t.TempDir()
	d := NewStatusDetector()
	d.statusDirs = []string{tmpDir}

	// Hook wrote the truth under the branch-keyed name (what
	// OPENKANBAN_SESSION carries in the live agent's env).
	branchKey := "feat/x"
	if err := os.MkdirAll(filepath.Join(tmpDir, "feat"), 0755); err != nil {
		t.Fatalf("mkdir feat: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, branchKey+".status"), []byte("idle"), 0644); err != nil {
		t.Fatalf("write idle: %v", err)
	}

	// Terminal content is the noisy fallback that mis-classifies an
	// idle Claude prompt as "working" via the ━ border heuristic.
	// Including it asserts the detector REACHED the file (skipping
	// the fallback) rather than coincidentally returning idle.
	terminalContent := "tools: read, write, bash\n━━━━━━━━━━━━━━━━━━━━━━━━━━━\n> "

	// API session ID (the Claude UUID back-filled by FindClaudeSession)
	// is DIFFERENT from the file key. The detector must not use it
	// to look up the file.
	apiSessionID := "11111111-2222-3333-4444-555555555555"

	got := d.DetectStatusWithActivity("claude", branchKey, apiSessionID, "", 0, true, terminalContent, time.Now())
	if got != board.AgentIdle {
		t.Errorf("got %q, want AgentIdle (file lookup must use the branch-keyed name, not the API UUID)", got)
	}
}

// TestDetectStatusWithActivity_IdlePromotedOnSpinner pins the fix for the
// second observed failure: a session in auto-mode whose Stop hook wrote
// "idle", then continued generating without a new UserPromptSubmit (no hook
// fires during pure generation). The status file stays pinned at "idle"
// while the spinner shows an active turn. An idle→working promotion arm —
// symmetric to the waiting→working arm — lifts the status when positive
// active-turn evidence is on screen.
//
// Fail-safes (both must hold):
//   - empty grid → stays idle (no active evidence)
//   - approval prompt on screen → stays idle (prompt guard runs first)
//
// Fixtures are the two real-world live captures that triggered this fix.
func TestDetectStatusWithActivity_IdlePromotedOnSpinner(t *testing.T) {
	stale := time.Now().Add(-(WaitingActivityTTL + time.Second))

	sonnetSpinner := "✢ Razzle-dazzling… (9m 5s · ↓ 16.4k tokens)"
	opusSpinner := "· Considering… (2m 17s · ↓ 6.4k tokens · still thinking)"
	planApprovalPrompt := strings.Join([]string{
		" Claude has written up a plan and is ready to execute. Would you like to proceed?",
		" ❯ 1. Yes, and auto-accept edits",
		"   2. No, keep planning",
	}, "\n")

	tests := []struct {
		name            string
		terminalContent string
		lastActivity    time.Time
		want            board.AgentStatus
	}{
		// The fix: spinner promotes idle→working.
		// RED before adding the idle arm in DetectStatusWithActivity (the
		// "status != AgentWaiting" guard returns idle before any refinement).
		{"Sonnet auto-mode spinner promotes idle to working", sonnetSpinner, stale, board.AgentWorking},
		{"Opus plan-mode spinner promotes idle to working", opusSpinner, stale, board.AgentWorking},
		// Fail-safe: empty grid → no promotion.
		{"empty grid keeps idle", "", stale, board.AgentIdle},
		// Fail-safe: approval prompt present → stays idle (prompt guard first).
		{"plan-approval prompt keeps idle", planApprovalPrompt, stale, board.AgentIdle},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			d := NewStatusDetector()
			d.statusDirs = []string{tmpDir}
			statusFile := filepath.Join(tmpDir, "sess.status")
			if err := os.WriteFile(statusFile, []byte("idle"), 0644); err != nil {
				t.Fatalf("write status: %v", err)
			}
			d.InvalidateCache("sess")
			got := d.DetectStatusWithActivity("claude", "sess", "sess", "", 0, true, tt.terminalContent, tt.lastActivity)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
