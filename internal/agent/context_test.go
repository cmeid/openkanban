package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/board"
)

func TestBuildContextPrompt(t *testing.T) {
	tests := []struct {
		name           string
		template       string
		ticket         *board.Ticket
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := BuildContextPrompt(tt.template, tt.ticket)

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

	result := BuildContextPrompt("{{.InvalidSyntax", ticket)

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
		ticket         *board.Ticket
		expectContains []string
	}{
		{
			name: "title only",
			ticket: &board.Ticket{
				Title: "Simple task",
			},
			expectContains: []string{"Task: Simple task"},
		},
		{
			name: "title and description",
			ticket: &board.Ticket{
				Title:       "Complex task",
				Description: "Do these things",
			},
			expectContains: []string{"Task: Complex task", "Do these things"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildFallbackPrompt(tt.ticket)
			for _, expected := range tt.expectContains {
				if !strings.Contains(result, expected) {
					t.Errorf("buildFallbackPrompt() = %q; want to contain %q", result, expected)
				}
			}
		})
	}
}

func TestShouldInjectContext(t *testing.T) {
	tests := []struct {
		name     string
		ticket   *board.Ticket
		expected bool
	}{
		{
			name:     "new ticket without spawn time",
			ticket:   &board.Ticket{AgentSpawnedAt: nil},
			expected: true,
		},
		{
			name: "previously spawned ticket",
			ticket: &board.Ticket{
				AgentSpawnedAt: func() *time.Time { t := time.Now(); return &t }(),
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ShouldInjectContext(tt.ticket)
			if result != tt.expected {
				t.Errorf("ShouldInjectContext() = %v; want %v", result, tt.expected)
			}
		})
	}
}

func TestScrubMissingFileReferences(t *testing.T) {
	worktree := t.TempDir()
	// Create one real file the scrubber should consider present.
	presentRel := "docs/present.md"
	if err := os.MkdirAll(filepath.Join(worktree, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, presentRel), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name           string
		input          string
		expectContains []string
		expectAbsent   []string
	}{
		{
			name:           "no md references is no-op",
			input:          "## Heading\n\nA paragraph with no file refs.\n\nAnother paragraph.",
			expectContains: []string{"## Heading", "A paragraph", "Another paragraph"},
		},
		{
			name: "present file path leaves paragraph intact",
			input: "## Brief\n\nRead `docs/present.md` for context.\n\n" +
				"## Next\n\nDo the thing.",
			expectContains: []string{"## Brief", "docs/present.md", "## Next", "Do the thing"},
		},
		{
			name: "missing file path strips containing paragraph",
			input: "## Locating brief\n\nRead `tickets/missing.md` immediately.\n\n" +
				"## Next steps\n\nProceed when ready.",
			expectContains: []string{"## Next steps", "Proceed when ready"},
			expectAbsent:   []string{"tickets/missing.md", "Read `tickets/missing.md`"},
		},
		{
			name: "orphan header dropped when body fully scrubbed",
			input: "## Locating brief\n\nThe brief is at `tickets/missing.md`.\n\n" +
				"If `tickets/missing.md` is absent, stop.\n\n## Next steps\n\nGo.",
			expectContains: []string{"## Next steps", "Go"},
			expectAbsent:   []string{"## Locating brief", "tickets/missing.md"},
		},
		{
			name: "literal placeholder path is treated as missing",
			input: "## Read brief\n\nOpen `tickets/<slug>.md`.\n\n## Next\n\nContinue.",
			expectContains: []string{"## Next", "Continue"},
			expectAbsent:   []string{"## Read brief", "tickets/<slug>.md"},
		},
		{
			name:           "absolute missing path stripped",
			input:          "## A\n\nSee `/definitely/does/not/exist.md`.\n\n## B\n\nKeep me.",
			expectContains: []string{"## B", "Keep me"},
			expectAbsent:   []string{"/definitely/does/not/exist.md"},
		},
		{
			name: "multiple paragraphs referencing missing file each stripped",
			input: "## H\n\nFirst para about `tickets/x.md`.\n\n" +
				"Second para about `tickets/x.md`.\n\nThird unrelated para.",
			expectContains: []string{"Third unrelated para"},
			expectAbsent:   []string{"First para", "Second para", "tickets/x.md"},
		},
		{
			name: "paragraph with both present and missing dropped (any missing wins)",
			input: "## Mix\n\nRead `docs/present.md` and `tickets/missing.md`.\n\n" +
				"## Tail\n\nEnd.",
			expectContains: []string{"## Tail", "End"},
			expectAbsent:   []string{"tickets/missing.md", "docs/present.md"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scrubMissingFileReferences(tt.input, worktree)
			for _, want := range tt.expectContains {
				if !strings.Contains(got, want) {
					t.Errorf("missing expected substring %q in:\n%s", want, got)
				}
			}
			for _, absent := range tt.expectAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("unexpected substring %q still present in:\n%s", absent, got)
				}
			}
		})
	}
}

func TestScrubMissingFileReferences_HomeExpansion(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir available")
	}
	// Create a temp file under home for the present case.
	tmp, err := os.CreateTemp(home, "okb-scrub-test-*.md")
	if err != nil {
		t.Skip("cannot create file in home dir")
	}
	t.Cleanup(func() { os.Remove(tmp.Name()) })
	tmp.Close()
	rel := "~/" + filepath.Base(tmp.Name())

	prompt := "## A\n\nRead `" + rel + "` for the brief.\n\n## B\n\nKeep."
	got := scrubMissingFileReferences(prompt, "")
	if !strings.Contains(got, rel) {
		t.Errorf("present ~/-path should be kept; got:\n%s", got)
	}

	missingPrompt := "## A\n\nRead `~/does-not-exist-okb-test.md`.\n\n## B\n\nKeep."
	got = scrubMissingFileReferences(missingPrompt, "")
	if strings.Contains(got, "~/does-not-exist") {
		t.Errorf("missing ~/-path should be stripped; got:\n%s", got)
	}
	if !strings.Contains(got, "## B") {
		t.Errorf("unrelated section should remain; got:\n%s", got)
	}
}

func TestBuildContextPrompt_ScrubsMissingFileReferences(t *testing.T) {
	worktree := t.TempDir()
	ticket := &board.Ticket{
		Title:        "Test",
		BranchName:   "task/foo",
		WorktreePath: worktree,
	}
	template := "## Brief\n\nRead `tickets/foo.md` carefully.\n\n## Tail\n\n{{.Title}} continues."
	got := BuildContextPrompt(template, ticket)
	if strings.Contains(got, "tickets/foo.md") {
		t.Errorf("missing-file directive should be scrubbed; got:\n%s", got)
	}
	if !strings.Contains(got, "Test continues") {
		t.Errorf("unrelated content should remain; got:\n%s", got)
	}
}

func TestContextData_AllFieldsMapped(t *testing.T) {
	ticket := &board.Ticket{
		ID:           "test-id-123",
		Title:        "Test Title",
		Description:  "Test Description",
		BranchName:   "feature/test",
		BaseBranch:   "main",
		Status:       board.StatusInProgress,
		WorktreePath: "/home/user/project-worktrees/test",
	}

	template := "{{.TicketID}}|{{.Title}}|{{.Description}}|{{.BranchName}}|{{.BaseBranch}}|{{.Status}}|{{.WorktreePath}}"
	result := BuildContextPrompt(template, ticket)

	expected := "test-id-123|Test Title|Test Description|feature/test|main|in_progress|/home/user/project-worktrees/test"
	if result != expected {
		t.Errorf("All fields mapping:\ngot:  %q\nwant: %q", result, expected)
	}
}
