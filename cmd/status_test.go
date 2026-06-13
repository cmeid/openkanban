package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestStatusSet covers the RunE of `openkanban status set <state>`.
//
// HOME is redirected to a temp dir so agent.WriteStatusFile lands under
// the test's sandbox (~/.cache/openkanban-status/<session>.status).
func TestStatusSet(t *testing.T) {
	tests := []struct {
		name        string
		session     string // value of OPENKANBAN_SESSION; "" means unset
		state       string
		wantErr     bool
		wantFile    bool   // whether the status file should exist after running
		wantContent string // expected file body if wantFile
	}{
		{
			name:        "valid working",
			session:     "test-session",
			state:       "working",
			wantFile:    true,
			wantContent: "working\n",
		},
		{
			name:        "valid idle",
			session:     "test-session",
			state:       "idle",
			wantFile:    true,
			wantContent: "idle\n",
		},
		{
			name:        "valid waiting",
			session:     "test-session",
			state:       "waiting",
			wantFile:    true,
			wantContent: "waiting\n",
		},
		{
			name:        "valid completed",
			session:     "test-session",
			state:       "completed",
			wantFile:    true,
			wantContent: "completed\n",
		},
		{
			name:        "valid error",
			session:     "test-session",
			state:       "error",
			wantFile:    true,
			wantContent: "error\n",
		},
		{
			name:     "no session env: silent no-op, no error",
			session:  "",
			state:    "working",
			wantFile: false,
		},
		{
			name:    "unknown state errors",
			session: "test-session",
			state:   "borked",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			if tt.session != "" {
				t.Setenv("OPENKANBAN_SESSION", tt.session)
			} else {
				t.Setenv("OPENKANBAN_SESSION", "")
			}

			err := statusSetCmd.RunE(statusSetCmd, []string{tt.state})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			statusFile := filepath.Join(home, ".cache", "openkanban-status", tt.session+".status")
			data, readErr := os.ReadFile(statusFile)

			if !tt.wantFile {
				if readErr == nil {
					t.Fatalf("status file unexpectedly exists at %s", statusFile)
				}
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("unexpected read error: %v", readErr)
				}
				return
			}

			if readErr != nil {
				t.Fatalf("status file missing: %v", readErr)
			}
			if got := string(data); got != tt.wantContent {
				t.Errorf("status file body = %q; want %q", got, tt.wantContent)
			}
		})
	}
}

// TestParseAgentStatus verifies the string→AgentStatus mapping in isolation,
// independent of any file I/O.
func TestParseAgentStatus(t *testing.T) {
	tests := []struct {
		name    string
		state   string
		wantErr bool
	}{
		{name: "working", state: "working"},
		{name: "idle", state: "idle"},
		{name: "waiting", state: "waiting"},
		{name: "completed", state: "completed"},
		{name: "error", state: "error"},
		{name: "empty", state: "", wantErr: true},
		{name: "unknown", state: "running", wantErr: true},
		{name: "uppercase rejected", state: "Working", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseAgentStatus(tt.state)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error for %q, got nil", tt.state)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tt.state, err)
			}
		})
	}
}
