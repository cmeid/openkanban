package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/agent"
)

// TestResolveStatusVerdict pins the daemon-side status resolution that
// rides on the activity heartbeat. The daemon owns the live PTY grid for
// every session it runs, so it — not the (possibly unattached) client —
// is the authority on working vs waiting. The verdict must:
//
//   - treat recent activity at a PROMPT as "waiting" (the false-working
//     bug: a re-rendering prompt looks like activity but isn't work);
//   - treat an active-turn marker on screen as "working" (a bg-spawned
//     session that's genuinely busy);
//   - default to "waiting" for an unnamed prompt / empty grid;
//   - stay silent ("") for opencode (UI resolves it via HTTP), for a
//     missing agent type (older client), and when it has no verdict.
//
// HOME is redirected so the detector's default status dir lands in the
// temp tree (the dir list is package-private to internal/agent, and
// NewStatusDetector reads HOME, not OPENKANBAN_STATUS_DIR). Writes land
// under the temp HOME, never the real ~/.cache, so the #88 write-guard
// (which captured the real HOME at init) does not fire.
func TestResolveStatusVerdict(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OPENKANBAN_CONFIG_DIR", t.TempDir())
	statusDir := filepath.Join(home, ".cache", "openkanban-status")
	if err := os.MkdirAll(statusDir, 0755); err != nil {
		t.Fatalf("mkdir status dir: %v", err)
	}

	d := agent.NewStatusDetector()

	const name = "branch-x"
	if err := os.WriteFile(filepath.Join(statusDir, name+".status"), []byte("waiting"), 0644); err != nil {
		t.Fatalf("write status: %v", err)
	}
	recent := time.Now().Add(-2 * time.Second)

	tests := []struct {
		name         string
		detector     *agent.StatusDetector
		agentType    string
		sessionName  string
		content      string
		lastActivity time.Time
		want         string
	}{
		{
			name:         "claude at a permission prompt resolves waiting",
			detector:     d,
			agentType:    "claude",
			sessionName:  name,
			content:      "│ Do you want to proceed?\n│ ❯ 1. Yes\n│ Esc to cancel",
			lastActivity: recent,
			want:         "waiting",
		},
		{
			// Durable default: an unnamed prompt (no permission keywords,
			// no work marker) still resolves waiting, not working.
			name:         "claude at an unnamed prompt resolves waiting",
			detector:     d,
			agentType:    "claude",
			sessionName:  name,
			content:      "Which key should trigger the action?\n  Option A\n  Option B",
			lastActivity: recent,
			want:         "waiting",
		},
		{
			name:         "claude with active-turn marker resolves working",
			detector:     d,
			agentType:    "claude",
			sessionName:  name,
			content:      "✻ Cogitating… (esc to interrupt)",
			lastActivity: recent,
			want:         "working",
		},
		{
			name:        "opencode yields no daemon verdict",
			detector:    d,
			agentType:   "opencode",
			sessionName: name,
			content:     "✻ Cogitating… (esc to interrupt)",
			want:        "",
		},
		{
			name:        "missing agent type yields no verdict",
			detector:    d,
			agentType:   "",
			sessionName: name,
			want:        "",
		},
		{
			name:         "no file and empty grid yields no verdict",
			detector:     d,
			agentType:    "claude",
			sessionName:  "unknown-session-no-file",
			content:      "",
			lastActivity: recent,
			want:         "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d.InvalidateCache(name)
			got := resolveStatusVerdict(tt.detector, tt.agentType, tt.sessionName, "", true, tt.content, tt.lastActivity)
			if got != tt.want {
				t.Errorf("resolveStatusVerdict = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveStatusVerdict_NilDetector confirms safe degradation when the
// server has no detector wired (defensive — should not happen in prod).
func TestResolveStatusVerdict_NilDetector(t *testing.T) {
	if got := resolveStatusVerdict(nil, "claude", "x", "", true, "", time.Now()); got != "" {
		t.Errorf("nil detector = %q, want \"\"", got)
	}
}
