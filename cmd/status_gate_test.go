package cmd

import (
	"testing"

	"github.com/techdufus/openkanban/internal/board"
)

func TestInAgentSession(t *testing.T) {
	tests := []struct {
		name    string
		session string
		ticketID string
		want    bool
	}{
		{"neither set", "", "", false},
		{"session only", "sess-abc", "", true},
		{"ticketID only", "", "00000000-0000-0000-0000-000000000001", true},
		{"both set", "sess-abc", "00000000-0000-0000-0000-000000000001", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OPENKANBAN_SESSION", tc.session)
			t.Setenv("OPENKANBAN_TICKET_ID", tc.ticketID)
			if got := inAgentSession(); got != tc.want {
				t.Errorf("inAgentSession() = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestGuardAgentStatusChange(t *testing.T) {
	target := board.StatusDone
	tests := []struct {
		name     string
		session  string
		ticketID string
		force    bool
		wantErr  bool
	}{
		// Human context — no env vars set — always allowed.
		{"human context no force", "", "", false, false},
		{"human context with force", "", "", true, false},
		// Agent context without --force — blocked.
		{"agent session only no force", "sess-abc", "", false, true},
		{"agent ticketID only no force", "", "00000000-0000-0000-0000-000000000001", false, true},
		{"agent both set no force", "sess-abc", "00000000-0000-0000-0000-000000000001", false, true},
		// Agent context with --force — OR doesn't swallow force.
		{"agent both set with force", "sess-abc", "00000000-0000-0000-0000-000000000001", true, false},
		{"agent session only with force", "sess-abc", "", true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OPENKANBAN_SESSION", tc.session)
			t.Setenv("OPENKANBAN_TICKET_ID", tc.ticketID)
			err := guardAgentStatusChange(target, tc.force)
			if (err != nil) != tc.wantErr {
				t.Errorf("guardAgentStatusChange() error = %v; wantErr %v", err, tc.wantErr)
			}
		})
	}
}
