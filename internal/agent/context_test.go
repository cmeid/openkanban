package agent

import (
	"strings"
	"testing"

	"github.com/techdufus/openkanban/internal/board"
)

func TestBuildContextPrompt(t *testing.T) {
	tests := []struct {
		name           string
		template       string
		ticket         *board.Ticket
		briefRelPath   string
		hasBrief       bool
		isResume       bool
		expectContains []string
		expectEmpty    bool
	}{
		{
			name:        "empty template returns empty",
			template:    "",
			ticket:      &board.Ticket{Title: "Test"},
			expectEmpty: true,
		},
		{
			name:           "simple title substitution",
			template:       "Work on: {{.Title}}",
			ticket:         &board.Ticket{Title: "Fix the bug"},
			expectContains: []string{"Work on: Fix the bug"},
		},
		{
			name:     "multiple field substitution",
			template: "Title: {{.Title}}\nBranch: {{.BranchName}}\nBase: {{.BaseBranch}}",
			ticket: &board.Ticket{
				Title:      "New feature",
				BranchName: "feature/new-thing",
				BaseBranch: "main",
			},
			expectContains: []string{
				"Title: New feature",
				"Branch: feature/new-thing",
				"Base: main",
			},
		},
		{
			name:     "description field",
			template: "{{.Title}}: {{.Description}}",
			ticket: &board.Ticket{
				Title:       "Bug fix",
				Description: "Fix the critical issue",
			},
			expectContains: []string{"Bug fix: Fix the critical issue"},
		},
		{
			name:     "all fields",
			template: "ID={{.TicketID}} Title={{.Title}} Status={{.Status}} Path={{.WorktreePath}}",
			ticket: &board.Ticket{
				ID:           "abc-123",
				Title:        "Test",
				Status:       board.StatusInProgress,
				WorktreePath: "/path/to/worktree",
			},
			expectContains: []string{
				"ID=abc-123",
				"Title=Test",
				"Status=in_progress",
				"Path=/path/to/worktree",
			},
		},
		{
			name:     "handles empty fields gracefully",
			template: "Title={{.Title}} Desc={{.Description}}",
			ticket: &board.Ticket{
				Title:       "Only title",
				Description: "",
			},
			expectContains: []string{"Title=Only title", "Desc="},
		},
		{
			name:           "slug field rendered from branch",
			template:       "Slug={{.Slug}}",
			ticket:         &board.Ticket{Title: "T", BranchName: "task/foo-bar"},
			expectContains: []string{"Slug=foo-bar"},
		},
		{
			name:           "brief fields rendered",
			template:       "HasBrief={{.HasBrief}} BriefPath={{.BriefPath}}",
			ticket:         &board.Ticket{Title: "T"},
			briefRelPath:   "tickets/foo.md",
			hasBrief:       true,
			expectContains: []string{"HasBrief=true", "BriefPath=tickets/foo.md"},
		},
		{
			name:           "resume flag rendered",
			template:       "Resume={{.IsExternalResume}}",
			ticket:         &board.Ticket{Title: "T"},
			isResume:       true,
			expectContains: []string{"Resume=true"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := NewContextData(tt.ticket, tt.briefRelPath, tt.hasBrief, tt.isResume)
			result := BuildContextPrompt(tt.template, data)

			if tt.expectEmpty {
				if result != "" {
					t.Errorf("BuildContextPrompt() = %q; want empty", result)
				}
				return
			}

			for _, expected := range tt.expectContains {
				if !strings.Contains(result, expected) {
					t.Errorf("BuildContextPrompt() = %q; want to contain %q", result, expected)
				}
			}
		})
	}
}

func TestBuildContextPrompt_InvalidTemplate(t *testing.T) {
	ticket := &board.Ticket{
		Title:       "Test ticket",
		Description: "Some description",
	}

	result := BuildContextPrompt("{{.InvalidSyntax", NewContextData(ticket, "", false, false))

	if result == "" {
		t.Error("BuildContextPrompt with invalid template should return fallback, not empty")
	}
	if !strings.Contains(result, "Test ticket") {
		t.Errorf("fallback should contain ticket title; got %q", result)
	}
}

func TestBuildFallbackPrompt(t *testing.T) {
	tests := []struct {
		name           string
		data           ContextData
		expectContains []string
	}{
		{
			name: "title only",
			data: ContextData{
				Title: "Simple task",
			},
			expectContains: []string{"Task: Simple task"},
		},
		{
			name: "title and description",
			data: ContextData{
				Title:       "Complex task",
				Description: "Do these things",
			},
			expectContains: []string{"Task: Complex task", "Do these things"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildFallbackPrompt(tt.data)
			for _, expected := range tt.expectContains {
				if !strings.Contains(result, expected) {
					t.Errorf("buildFallbackPrompt() = %q; want to contain %q", result, expected)
				}
			}
		})
	}
}

func TestContextData_AllFieldsMapped(t *testing.T) {
	ticket := &board.Ticket{
		ID:           "test-id-123",
		Title:        "Test Title",
		Description:  "Test Description",
		BranchName:   "task/feature-test",
		BaseBranch:   "main",
		Status:       board.StatusInProgress,
		WorktreePath: "/home/user/project-worktrees/test",
	}

	data := NewContextData(ticket, "tickets/test.md", true, true)

	template := "{{.TicketID}}|{{.Title}}|{{.Description}}|{{.BranchName}}|{{.BaseBranch}}|{{.Status}}|{{.WorktreePath}}|{{.Slug}}|{{.HasBrief}}|{{.BriefPath}}|{{.IsExternalResume}}"
	result := BuildContextPrompt(template, data)

	expected := "test-id-123|Test Title|Test Description|task/feature-test|main|in_progress|/home/user/project-worktrees/test|feature-test|true|tickets/test.md|true"
	if result != expected {
		t.Errorf("All fields mapping:\ngot:  %q\nwant: %q", result, expected)
	}
}

func TestNewContextData(t *testing.T) {
	tests := []struct {
		name         string
		ticket       *board.Ticket
		briefRelPath string
		hasBrief     bool
		isResume     bool
		want         ContextData
	}{
		{
			name:         "nil ticket sets only brief and resume fields",
			ticket:       nil,
			briefRelPath: "tickets/x.md",
			hasBrief:     true,
			isResume:     true,
			want: ContextData{
				BriefPath:        "tickets/x.md",
				HasBrief:         true,
				IsExternalResume: true,
			},
		},
		{
			name: "non-nil ticket derives slug from branch",
			ticket: &board.Ticket{
				Title:      "T",
				BranchName: "task/foo-bar",
			},
			briefRelPath: "",
			hasBrief:     false,
			isResume:     false,
			want: ContextData{
				Title:      "T",
				BranchName: "task/foo-bar",
				Slug:       "foo-bar",
			},
		},
		{
			name: "hasBrief false yields HasBrief false",
			ticket: &board.Ticket{
				Title:      "T",
				BranchName: "task/abc",
			},
			briefRelPath: "tickets/abc.md",
			hasBrief:     false,
			isResume:     false,
			want: ContextData{
				Title:      "T",
				BranchName: "task/abc",
				Slug:       "abc",
				BriefPath:  "tickets/abc.md",
				HasBrief:   false,
			},
		},
		{
			name: "isExternalResume true is propagated",
			ticket: &board.Ticket{
				Title:      "T",
				BranchName: "task/zz",
			},
			briefRelPath:     "",
			hasBrief:         false,
			isResume:         true,
			want: ContextData{
				Title:            "T",
				BranchName:       "task/zz",
				Slug:             "zz",
				IsExternalResume: true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewContextData(tt.ticket, tt.briefRelPath, tt.hasBrief, tt.isResume)
			if got != tt.want {
				t.Errorf("NewContextData() = %+v; want %+v", got, tt.want)
			}
		})
	}
}

func TestBuildContextPrompt_BriefConditional(t *testing.T) {
	tmpl := "{{if .HasBrief}}brief: {{.BriefPath}}{{else}}no brief{{end}}"

	t.Run("has brief renders path", func(t *testing.T) {
		data := ContextData{HasBrief: true, BriefPath: "tickets/x.md"}
		got := BuildContextPrompt(tmpl, data)
		if got != "brief: tickets/x.md" {
			t.Errorf("got %q; want %q", got, "brief: tickets/x.md")
		}
	})

	t.Run("no brief renders else branch", func(t *testing.T) {
		data := ContextData{HasBrief: false}
		got := BuildContextPrompt(tmpl, data)
		if got != "no brief" {
			t.Errorf("got %q; want %q", got, "no brief")
		}
	})
}

func TestBuildContextPrompt_ResumeConditional(t *testing.T) {
	tmpl := "{{if .IsExternalResume}}resume{{else}}fresh{{end}}"

	t.Run("resume true", func(t *testing.T) {
		data := ContextData{IsExternalResume: true}
		got := BuildContextPrompt(tmpl, data)
		if got != "resume" {
			t.Errorf("got %q; want %q", got, "resume")
		}
	})

	t.Run("resume false", func(t *testing.T) {
		data := ContextData{IsExternalResume: false}
		got := BuildContextPrompt(tmpl, data)
		if got != "fresh" {
			t.Errorf("got %q; want %q", got, "fresh")
		}
	})
}
