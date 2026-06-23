package project

import (
	"testing"

	"github.com/techdufus/openkanban/internal/config"
)

// TestBranchNameForTitle pins the byte-identical output the TUI Model produced
// before the derivation was extracted out of internal/ui. The cascade is
// project.Settings > config.Defaults > hardcoded ("task/", "{prefix}{slug}", 40).
// If this test drifts, the CLI (`ticket new --worktree`) and TUI spawn can
// derive different branch names for the same ticket, breaking worktree reuse.
func TestBranchNameForTitle(t *testing.T) {
	// Hardcoded fallbacks: nil project + empty defaults.
	emptyDefaults := config.BoardSettings{}
	// The real global defaults (config.DefaultConfig()).
	globalDefaults := config.BoardSettings{
		BranchPrefix:   "task/",
		BranchTemplate: "{prefix}{slug}",
		SlugMaxLength:  40,
	}

	tests := []struct {
		name     string
		title    string
		proj     *Project
		defaults config.BoardSettings
		want     string
	}{
		{
			name:     "hardcoded fallback (nil proj, empty defaults)",
			title:    "Improve Ticket CLI",
			proj:     nil,
			defaults: emptyDefaults,
			want:     "task/improve-ticket-cli",
		},
		{
			name:     "global defaults tier",
			title:    "Improve Ticket CLI",
			proj:     &Project{},
			defaults: globalDefaults,
			want:     "task/improve-ticket-cli",
		},
		{
			name:  "project settings override defaults",
			title: "Improve Ticket CLI",
			proj: &Project{Settings: ProjectSettings{
				BranchPrefix:   "feature/",
				BranchTemplate: "{prefix}{slug}",
				SlugMaxLength:  40,
			}},
			defaults: globalDefaults,
			want:     "feature/improve-ticket-cli",
		},
		{
			name:  "custom template without prefix placeholder",
			title: "Fix Bug",
			proj: &Project{Settings: ProjectSettings{
				BranchTemplate: "wip/{slug}",
			}},
			defaults: globalDefaults,
			want:     "wip/fix-bug",
		},
		{
			name:  "slug truncation honors project slug_max_length",
			title: "This Is A Very Long Ticket Title That Exceeds The Limit",
			proj: &Project{Settings: ProjectSettings{
				SlugMaxLength: 10,
			}},
			defaults: globalDefaults,
			// board.Slugify(title, 10) -> "this-is-a" (trailing dash trimmed).
			want: "task/this-is-a",
		},
		{
			name:     "default slug max length 40 truncates",
			title:    "This Is A Very Long Ticket Title That Exceeds The Limit",
			proj:     &Project{},
			defaults: globalDefaults,
			// board.Slugify(title, 40) -> first 40 runes, trailing dash trimmed.
			want: "task/this-is-a-very-long-ticket-title-that-ex",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BranchNameForTitle(tt.title, tt.proj, tt.defaults)
			if got != tt.want {
				t.Errorf("BranchNameForTitle(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}
