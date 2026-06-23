package project

import (
	"strings"
	"testing"

	"github.com/techdufus/openkanban/internal/board"
)

// makeFrontmatter builds a minimal valid YAML frontmatter with the
// given fields overridden, plus a body. Used for negative-test
// payloads that target one field at a time.
func makeFrontmatter(t *testing.T, overrides map[string]string) []byte {
	t.Helper()
	fields := map[string]string{
		"id":           "d1d2d3d4-e5e6-f7f8-a9a0-b1b2b3b4b5b6",
		"project_id":   "test-project",
		"title":        "Sample ticket",
		"status":       "backlog",
		"priority":     "3",
		"labels":       "[]",
		"created_at":   "2026-06-12T00:00:00Z",
		"updated_at":   "2026-06-12T00:00:00Z",
		"use_worktree": "true",
		"agent_status": "none",
		"blocked_by":   "[]",
		"meta":         "{}",
	}
	for k, v := range overrides {
		if v == "" {
			delete(fields, k)
		} else {
			fields[k] = v
		}
	}
	var b strings.Builder
	b.WriteString("---\n")
	for _, key := range []string{"id", "project_id", "title", "status", "priority", "labels", "created_at", "updated_at", "use_worktree", "worktree_path", "branch_name", "base_branch", "agent_type", "agent_status", "blocked_by", "meta"} {
		if v, ok := fields[key]; ok {
			b.WriteString(key + ": " + v + "\n")
		}
	}
	b.WriteString("---\n\nbody\n")
	return []byte(b.String())
}

func TestUnmarshalRejectsEmptyID(t *testing.T) {
	_, err := UnmarshalTicket(makeFrontmatter(t, map[string]string{"id": "\"\""}))
	if err == nil {
		t.Fatal("expected error for empty id, got nil")
	}
	if !strings.Contains(err.Error(), "id") {
		t.Errorf("error should mention 'id'; got: %v", err)
	}
}

func TestUnmarshalRejectsEmptyTitle(t *testing.T) {
	_, err := UnmarshalTicket(makeFrontmatter(t, map[string]string{"title": "\"\""}))
	if err == nil {
		t.Fatal("expected error for empty title, got nil")
	}
	if !strings.Contains(err.Error(), "title") {
		t.Errorf("error should mention 'title'; got: %v", err)
	}
}

func TestUnmarshalRejectsWhitespaceOnlyTitle(t *testing.T) {
	_, err := UnmarshalTicket(makeFrontmatter(t, map[string]string{"title": "\"   \""}))
	if err == nil {
		t.Fatal("expected error for whitespace-only title, got nil")
	}
}

func TestUnmarshalRejectsInvalidStatus(t *testing.T) {
	cases := []string{"in-progress", "InProgress", "doing", "pending", "BACKLOG"}
	for _, bad := range cases {
		t.Run(bad, func(t *testing.T) {
			_, err := UnmarshalTicket(makeFrontmatter(t, map[string]string{"status": bad}))
			if err == nil {
				t.Errorf("expected error for status=%q", bad)
				return
			}
			if !strings.Contains(err.Error(), "status") {
				t.Errorf("error should mention 'status'; got: %v", err)
			}
		})
	}
}

func TestUnmarshalAcceptsValidStatuses(t *testing.T) {
	for _, good := range []board.TicketStatus{board.StatusBacklog, board.StatusInProgress, board.StatusDone, board.StatusArchived} {
		t.Run(string(good), func(t *testing.T) {
			tk, err := UnmarshalTicket(makeFrontmatter(t, map[string]string{"status": string(good)}))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tk.Status != good {
				t.Errorf("got %q, want %q", tk.Status, good)
			}
		})
	}
}

func TestUnmarshalRejectsInvalidAgentStatus(t *testing.T) {
	_, err := UnmarshalTicket(makeFrontmatter(t, map[string]string{"agent_status": "running"}))
	if err == nil {
		t.Fatal("expected error for bogus agent_status")
	}
	if !strings.Contains(err.Error(), "agent_status") {
		t.Errorf("error should mention 'agent_status'; got: %v", err)
	}
}

// TestUnmarshalAcceptsSubagentsAgentStatus pins that the detection-derived
// "subagents" status round-trips through frontmatter. The daemon/poll paths
// set it in-memory and a later save serializes it; without it in
// validAgentStatuses, reload would hard-error the whole ticket.
func TestUnmarshalAcceptsSubagentsAgentStatus(t *testing.T) {
	tk, err := UnmarshalTicket(makeFrontmatter(t, map[string]string{"agent_status": "subagents"}))
	if err != nil {
		t.Fatalf("unexpected error for agent_status=subagents: %v", err)
	}
	if tk.AgentStatus != board.AgentSubagents {
		t.Errorf("got %q, want %q", tk.AgentStatus, board.AgentSubagents)
	}
}

func TestUnmarshalAcceptsValidAgentTypes(t *testing.T) {
	for _, good := range []string{"claude", "opencode", "aider", "gemini", "codex", ""} {
		label := good
		if label == "" {
			label = "empty"
		}
		t.Run(label, func(t *testing.T) {
			overrides := map[string]string{}
			if good != "" {
				overrides["agent_type"] = good
			}
			tk, err := UnmarshalTicket(makeFrontmatter(t, overrides))
			if err != nil {
				t.Fatalf("unexpected error for agent_type=%q: %v", good, err)
			}
			if tk.AgentType != good {
				t.Errorf("got %q, want %q", tk.AgentType, good)
			}
		})
	}
}

func TestUnmarshalRejectsInvalidAgentType(t *testing.T) {
	_, err := UnmarshalTicket(makeFrontmatter(t, map[string]string{"agent_type": "cursor"}))
	if err == nil {
		t.Fatal("expected error for bogus agent_type")
	}
	if !strings.Contains(err.Error(), "agent_type") {
		t.Errorf("error should mention 'agent_type'; got: %v", err)
	}
}

// TestUnmarshalLenientOnMissingDefaults: omitted status / agent_status
// / timestamps should fall back to sensible defaults rather than
// erroring -- a hand-created ticket with just id and title should
// load.
func TestUnmarshalLenientOnMissingDefaults(t *testing.T) {
	overrides := map[string]string{
		"status":       "",
		"agent_status": "",
		"created_at":   "",
		"updated_at":   "",
	}
	tk, err := UnmarshalTicket(makeFrontmatter(t, overrides))
	if err != nil {
		t.Fatalf("expected lenient load to succeed, got: %v", err)
	}
	if tk.Status != board.StatusBacklog {
		t.Errorf("Status should default to backlog, got %q", tk.Status)
	}
	if tk.AgentStatus != board.AgentNone {
		t.Errorf("AgentStatus should default to none, got %q", tk.AgentStatus)
	}
	if tk.CreatedAt.IsZero() {
		t.Error("CreatedAt should be backfilled when missing")
	}
	if tk.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should be backfilled when missing")
	}
}
