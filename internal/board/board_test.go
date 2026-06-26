package board

import (
	"testing"
	"time"
)

func TestSlugify(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxLen   int
		expected string
	}{
		{
			name:     "simple lowercase",
			input:    "hello world",
			maxLen:   40,
			expected: "hello-world",
		},
		{
			name:     "mixed case",
			input:    "Hello World",
			maxLen:   40,
			expected: "hello-world",
		},
		{
			name:     "special characters",
			input:    "Hello! World? How's it going?",
			maxLen:   40,
			expected: "hello-world-how-s-it-going",
		},
		{
			name:     "numbers preserved",
			input:    "Feature 123 Implementation",
			maxLen:   40,
			expected: "feature-123-implementation",
		},
		{
			name:     "multiple spaces become single dash",
			input:    "hello    world",
			maxLen:   40,
			expected: "hello-world",
		},
		{
			name:     "leading and trailing spaces trimmed",
			input:    "  hello world  ",
			maxLen:   40,
			expected: "hello-world",
		},
		{
			name:     "truncation at max length",
			input:    "this is a very long title that should be truncated",
			maxLen:   20,
			expected: "this-is-a-very-long",
		},
		{
			name:     "truncation removes trailing dash",
			input:    "hello world foo bar",
			maxLen:   12,
			expected: "hello-world",
		},
		{
			name:     "zero maxLen uses default 40",
			input:    "test",
			maxLen:   0,
			expected: "test",
		},
		{
			name:     "negative maxLen uses default 40",
			input:    "test",
			maxLen:   -5,
			expected: "test",
		},
		{
			name:     "unicode characters",
			input:    "café résumé",
			maxLen:   40,
			expected: "caf-r-sum",
		},
		{
			name:     "empty string",
			input:    "",
			maxLen:   40,
			expected: "",
		},
		{
			name:     "only special characters",
			input:    "!!!???",
			maxLen:   40,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Slugify(tt.input, tt.maxLen)
			if result != tt.expected {
				t.Errorf("Slugify(%q, %d) = %q; want %q", tt.input, tt.maxLen, result, tt.expected)
			}
		})
	}
}

func TestNewTicketID(t *testing.T) {
	id1 := NewTicketID()
	id2 := NewTicketID()

	if id1 == "" {
		t.Error("NewTicketID() returned empty string")
	}

	if id1 == id2 {
		t.Errorf("NewTicketID() returned duplicate IDs: %s", id1)
	}

	if len(id1) != 36 {
		t.Errorf("NewTicketID() returned ID with length %d; want 36", len(id1))
	}
}

func TestNewTicket(t *testing.T) {
	title := "Test Ticket"
	projectID := "project-123"

	before := time.Now()
	ticket := NewTicket(title, projectID)
	after := time.Now()

	if ticket.Title != title {
		t.Errorf("ticket.Title = %q; want %q", ticket.Title, title)
	}

	if ticket.ProjectID != projectID {
		t.Errorf("ticket.ProjectID = %q; want %q", ticket.ProjectID, projectID)
	}

	if ticket.Status != StatusBacklog {
		t.Errorf("ticket.Status = %q; want %q", ticket.Status, StatusBacklog)
	}

	if ticket.AgentStatus != AgentNone {
		t.Errorf("ticket.AgentStatus = %q; want %q", ticket.AgentStatus, AgentNone)
	}

	if ticket.Priority != 3 {
		t.Errorf("ticket.Priority = %d; want 3", ticket.Priority)
	}

	if ticket.ID == "" {
		t.Error("ticket.ID should not be empty")
	}

	if ticket.CreatedAt.Before(before) || ticket.CreatedAt.After(after) {
		t.Errorf("ticket.CreatedAt = %v; want between %v and %v", ticket.CreatedAt, before, after)
	}

	if ticket.UpdatedAt.Before(before) || ticket.UpdatedAt.After(after) {
		t.Errorf("ticket.UpdatedAt = %v; want between %v and %v", ticket.UpdatedAt, before, after)
	}

	if ticket.StatusChangedAt == nil {
		t.Fatal("ticket.StatusChangedAt should be set on new ticket")
	}
	if !ticket.StatusChangedAt.Equal(ticket.CreatedAt) {
		t.Errorf("ticket.StatusChangedAt = %v; want CreatedAt = %v", *ticket.StatusChangedAt, ticket.CreatedAt)
	}

	if ticket.Labels == nil {
		t.Error("ticket.Labels should not be nil")
	}

	if ticket.Meta == nil {
		t.Error("ticket.Meta should not be nil")
	}
}

func TestTicket_Touch(t *testing.T) {
	ticket := NewTicket("Test", "project-1")
	originalUpdatedAt := ticket.UpdatedAt

	time.Sleep(time.Millisecond)

	ticket.Touch()

	if !ticket.UpdatedAt.After(originalUpdatedAt) {
		t.Errorf("Touch() should update UpdatedAt; got %v, original was %v", ticket.UpdatedAt, originalUpdatedAt)
	}
}

func TestTicket_SetStatus(t *testing.T) {
	t.Run("transition to in_progress sets StartedAt", func(t *testing.T) {
		ticket := NewTicket("Test", "project-1")

		if ticket.StartedAt != nil {
			t.Error("new ticket should have nil StartedAt")
		}

		before := time.Now()
		ticket.SetStatus(StatusInProgress)
		after := time.Now()

		if ticket.Status != StatusInProgress {
			t.Errorf("ticket.Status = %q; want %q", ticket.Status, StatusInProgress)
		}

		if ticket.StartedAt == nil {
			t.Error("StartedAt should be set after transition to in_progress")
		}

		if ticket.StartedAt.Before(before) || ticket.StartedAt.After(after) {
			t.Errorf("ticket.StartedAt = %v; want between %v and %v", *ticket.StartedAt, before, after)
		}
	})

	t.Run("transition to done sets CompletedAt", func(t *testing.T) {
		ticket := NewTicket("Test", "project-1")

		if ticket.CompletedAt != nil {
			t.Error("new ticket should have nil CompletedAt")
		}

		before := time.Now()
		ticket.SetStatus(StatusDone)
		after := time.Now()

		if ticket.Status != StatusDone {
			t.Errorf("ticket.Status = %q; want %q", ticket.Status, StatusDone)
		}

		if ticket.CompletedAt == nil {
			t.Error("CompletedAt should be set after transition to done")
		}

		if ticket.CompletedAt.Before(before) || ticket.CompletedAt.After(after) {
			t.Errorf("ticket.CompletedAt = %v; want between %v and %v", *ticket.CompletedAt, before, after)
		}
	})

	t.Run("transition to backlog does not set timestamps", func(t *testing.T) {
		ticket := NewTicket("Test", "project-1")
		ticket.SetStatus(StatusBacklog)

		if ticket.StartedAt != nil {
			t.Error("StartedAt should remain nil for backlog status")
		}

		if ticket.CompletedAt != nil {
			t.Error("CompletedAt should remain nil for backlog status")
		}
	})

	// Regression guard: Next is a benign staging status, so SetStatus must
	// NOT stamp StartedAt/CompletedAt for it (only in_progress/done do).
	// This passes with SetStatus unchanged; it fails only if a future edit
	// wrongly adds a `case StatusNext` that stamps a timestamp.
	t.Run("transition to next does not set timestamps", func(t *testing.T) {
		ticket := NewTicket("Test", "project-1")
		ticket.SetStatus(StatusNext)

		if ticket.StartedAt != nil {
			t.Error("StartedAt should remain nil for next status")
		}

		if ticket.CompletedAt != nil {
			t.Error("CompletedAt should remain nil for next status")
		}
	})

	t.Run("updates UpdatedAt", func(t *testing.T) {
		ticket := NewTicket("Test", "project-1")
		originalUpdatedAt := ticket.UpdatedAt

		time.Sleep(time.Millisecond)
		ticket.SetStatus(StatusInProgress)

		if !ticket.UpdatedAt.After(originalUpdatedAt) {
			t.Error("SetStatus should update UpdatedAt")
		}
	})

	t.Run("stamps StatusChangedAt on transition", func(t *testing.T) {
		ticket := NewTicket("Test", "project-1")
		originalStatusChangedAt := *ticket.StatusChangedAt

		time.Sleep(time.Millisecond)
		ticket.SetStatus(StatusInProgress)

		if ticket.StatusChangedAt == nil {
			t.Fatal("StatusChangedAt should be non-nil after SetStatus")
		}
		if !ticket.StatusChangedAt.After(originalStatusChangedAt) {
			t.Errorf("SetStatus should advance StatusChangedAt; got %v, original %v",
				*ticket.StatusChangedAt, originalStatusChangedAt)
		}
	})
}

func TestTicket_SetAgentStatus(t *testing.T) {
	t.Run("no-op when status unchanged", func(t *testing.T) {
		ticket := NewTicket("Test", "project-1")
		ticket.AgentStatus = AgentWorking
		originalUpdatedAt := ticket.UpdatedAt
		originalStatusChangedAt := *ticket.StatusChangedAt

		time.Sleep(time.Millisecond)
		changed := ticket.SetAgentStatus(AgentWorking)

		if changed {
			t.Error("SetAgentStatus should return false on no-op")
		}
		if !ticket.UpdatedAt.Equal(originalUpdatedAt) {
			t.Error("UpdatedAt should not change on no-op")
		}
		if !ticket.StatusChangedAt.Equal(originalStatusChangedAt) {
			t.Error("StatusChangedAt should not change on no-op")
		}
	})

	t.Run("real change stamps and returns true", func(t *testing.T) {
		ticket := NewTicket("Test", "project-1")
		originalUpdatedAt := ticket.UpdatedAt
		originalStatusChangedAt := *ticket.StatusChangedAt

		time.Sleep(time.Millisecond)
		changed := ticket.SetAgentStatus(AgentWaiting)

		if !changed {
			t.Fatal("SetAgentStatus should return true on real change")
		}
		if ticket.AgentStatus != AgentWaiting {
			t.Errorf("AgentStatus = %q; want %q", ticket.AgentStatus, AgentWaiting)
		}
		if !ticket.UpdatedAt.After(originalUpdatedAt) {
			t.Error("UpdatedAt should advance on change")
		}
		if ticket.StatusChangedAt == nil {
			t.Fatal("StatusChangedAt should be non-nil after change")
		}
		if !ticket.StatusChangedAt.After(originalStatusChangedAt) {
			t.Errorf("StatusChangedAt should advance; got %v, original %v",
				*ticket.StatusChangedAt, originalStatusChangedAt)
		}
	})

	t.Run("Touch does not bump StatusChangedAt", func(t *testing.T) {
		ticket := NewTicket("Test", "project-1")
		originalStatusChangedAt := *ticket.StatusChangedAt

		time.Sleep(time.Millisecond)
		ticket.Touch()

		if !ticket.StatusChangedAt.Equal(originalStatusChangedAt) {
			t.Errorf("Touch should not bump StatusChangedAt; got %v, original %v",
				*ticket.StatusChangedAt, originalStatusChangedAt)
		}
	})
}

func TestDefaultColumns(t *testing.T) {
	columns := DefaultColumns()

	if len(columns) != 5 {
		t.Fatalf("DefaultColumns() returned %d columns; want 5", len(columns))
	}

	expected := []struct {
		id     string
		name   string
		status TicketStatus
	}{
		{"backlog", "Backlog", StatusBacklog},
		{"next", "Next", StatusNext},
		{"in-progress", "In Progress", StatusInProgress},
		{"in-review", "In Review", StatusInReview},
		{"done", "Done", StatusDone},
	}

	for i, exp := range expected {
		if columns[i].ID != exp.id {
			t.Errorf("columns[%d].ID = %q; want %q", i, columns[i].ID, exp.id)
		}
		if columns[i].Name != exp.name {
			t.Errorf("columns[%d].Name = %q; want %q", i, columns[i].Name, exp.name)
		}
		if columns[i].Status != exp.status {
			t.Errorf("columns[%d].Status = %q; want %q", i, columns[i].Status, exp.status)
		}
		if columns[i].Color == "" {
			t.Errorf("columns[%d].Color should not be empty", i)
		}
	}
}

func TestBoardError(t *testing.T) {
	err := &BoardError{Message: "test error"}

	if err.Error() != "test error" {
		t.Errorf("BoardError.Error() = %q; want %q", err.Error(), "test error")
	}
}

func TestErrTicketNotFound(t *testing.T) {
	if ErrTicketNotFound.Error() != "ticket not found" {
		t.Errorf("ErrTicketNotFound.Error() = %q; want %q", ErrTicketNotFound.Error(), "ticket not found")
	}
}

func TestTicketStatus_Constants(t *testing.T) {
	if StatusBacklog != "backlog" {
		t.Errorf("StatusBacklog = %q; want %q", StatusBacklog, "backlog")
	}
	if StatusNext != "next" {
		t.Errorf("StatusNext = %q; want %q", StatusNext, "next")
	}
	if StatusInProgress != "in_progress" {
		t.Errorf("StatusInProgress = %q; want %q", StatusInProgress, "in_progress")
	}
	if StatusInReview != "in_review" {
		t.Errorf("StatusInReview = %q; want %q", StatusInReview, "in_review")
	}
	if StatusDone != "done" {
		t.Errorf("StatusDone = %q; want %q", StatusDone, "done")
	}
	if StatusArchived != "archived" {
		t.Errorf("StatusArchived = %q; want %q", StatusArchived, "archived")
	}
}

func TestParseStatus(t *testing.T) {
	valid := []struct {
		in   string
		want TicketStatus
	}{
		{"backlog", StatusBacklog},
		{"next", StatusNext},
		{"in_progress", StatusInProgress},
		{"in_review", StatusInReview},
		{"done", StatusDone},
		{"archived", StatusArchived},
	}
	for _, tt := range valid {
		got, err := ParseStatus(tt.in)
		if err != nil {
			t.Errorf("ParseStatus(%q) unexpected error: %v", tt.in, err)
		}
		if got != tt.want {
			t.Errorf("ParseStatus(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}

	invalid := []string{"", "BACKLOG", "in-progress", "pending", "unknown"}
	for _, s := range invalid {
		if _, err := ParseStatus(s); err == nil {
			t.Errorf("ParseStatus(%q) expected error; got nil", s)
		}
	}
}

func TestAgentStatus_Constants(t *testing.T) {
	if AgentNone != "none" {
		t.Errorf("AgentNone = %q; want %q", AgentNone, "none")
	}
	if AgentIdle != "idle" {
		t.Errorf("AgentIdle = %q; want %q", AgentIdle, "idle")
	}
	if AgentWorking != "working" {
		t.Errorf("AgentWorking = %q; want %q", AgentWorking, "working")
	}
	if AgentWaiting != "waiting" {
		t.Errorf("AgentWaiting = %q; want %q", AgentWaiting, "waiting")
	}
	if AgentCompleted != "completed" {
		t.Errorf("AgentCompleted = %q; want %q", AgentCompleted, "completed")
	}
	if AgentError != "error" {
		t.Errorf("AgentError = %q; want %q", AgentError, "error")
	}
}
