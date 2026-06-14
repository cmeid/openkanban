package project

import (
	"hash/fnv"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/google/uuid"
)

// ColorPalette is the set of terminal-color names a project's Color field may take.
// Names are the canonical lowercase form; validation is case-sensitive.
var ColorPalette = []string{
	"red", "green", "yellow", "blue", "magenta", "cyan",
	"brightred", "brightgreen", "brightyellow", "brightblue", "brightmagenta", "brightcyan",
}

// Project represents a git repository registered with OpenKanban.
// Each git repo is exactly one Project - this is the fundamental unit of organization.
type Project struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	RepoPath    string    `json:"repo_path"`    // Absolute path to git repo root
	WorktreeDir string    `json:"worktree_dir"` // Where worktrees go (default: {repo}-worktrees)
	Color       string    `json:"color,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	// Project-specific settings (overrides global defaults)
	Settings ProjectSettings `json:"settings"`
}

// ProjectSettings contains project-specific configuration.
// These override global defaults from config.Config.
type ProjectSettings struct {
	AutoSpawnAgent   bool   `json:"auto_spawn_agent"`
	AutoCreateBranch bool   `json:"auto_create_branch"`
	BranchPrefix     string `json:"branch_prefix,omitempty"`
	BranchNaming     string `json:"branch_naming,omitempty"`   // "template" | "ai" | "prompt"
	BranchTemplate   string `json:"branch_template,omitempty"` // e.g., "{prefix}{slug}"
	SlugMaxLength    int    `json:"slug_max_length,omitempty"` // default: 40
}

// NewProject creates a new project for a repository
func NewProject(name, repoPath string) *Project {
	now := time.Now()

	// Default worktree dir is sibling to repo: /path/to/repo -> /path/to/repo-worktrees
	worktreeDir := repoPath + "-worktrees"

	return &Project{
		ID:          uuid.New().String(),
		Name:        name,
		RepoPath:    repoPath,
		WorktreeDir: worktreeDir,
		CreatedAt:   now,
		UpdatedAt:   now,
		Settings: ProjectSettings{
			AutoSpawnAgent:   true,
			AutoCreateBranch: true,
			// String/int settings left empty to cascade to global config.
			// Use Model.getBranchPrefix(), getBranchTemplate(), getSlugMaxLength() for resolution.
		},
	}
}

// GetWorktreeDir returns the worktree directory, using default if not set
func (p *Project) GetWorktreeDir() string {
	if p.WorktreeDir != "" {
		return p.WorktreeDir
	}
	return p.RepoPath + "-worktrees"
}

// GetBranchPrefix returns the branch prefix, using default if not set
func (p *Project) GetBranchPrefix() string {
	if p.Settings.BranchPrefix != "" {
		return p.Settings.BranchPrefix
	}
	return "task/"
}

// GetBranchTemplate returns the branch template, using default if not set
func (p *Project) GetBranchTemplate() string {
	if p.Settings.BranchTemplate != "" {
		return p.Settings.BranchTemplate
	}
	return "{prefix}{slug}"
}

// GetSlugMaxLength returns the slug max length, using default if not set
func (p *Project) GetSlugMaxLength() int {
	if p.Settings.SlugMaxLength > 0 {
		return p.Settings.SlugMaxLength
	}
	return 40
}

// Touch updates the UpdatedAt timestamp
func (p *Project) Touch() {
	p.UpdatedAt = time.Now()
}

// IsValidColor reports whether name is a member of ColorPalette.
// Comparison is case-sensitive: palette names are the canonical lowercase form.
func IsValidColor(name string) bool {
	for _, c := range ColorPalette {
		if c == name {
			return true
		}
	}
	return false
}

// GetColor returns the project's color, either the explicitly-set Color field
// (when valid) or a deterministic auto-derived palette entry based on the
// project's ID. An invalid explicit Color falls back to auto-derivation so a
// bad value in JSON doesn't break rendering.
func (p *Project) GetColor() string {
	if p.Color != "" && IsValidColor(p.Color) {
		return p.Color
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(p.ID))
	return ColorPalette[int(h.Sum32())%len(ColorPalette)]
}

// ResolveLipglossColor maps a palette name to a lipgloss.Color using the
// ANSI 16-color number space. Unknown names fall back to ANSI white ("7")
// rather than panicking so callers can render safely on bad input.
func ResolveLipglossColor(name string) lipgloss.Color {
	switch name {
	case "red":
		return lipgloss.Color("1")
	case "green":
		return lipgloss.Color("2")
	case "yellow":
		return lipgloss.Color("3")
	case "blue":
		return lipgloss.Color("4")
	case "magenta":
		return lipgloss.Color("5")
	case "cyan":
		return lipgloss.Color("6")
	case "brightred":
		return lipgloss.Color("9")
	case "brightgreen":
		return lipgloss.Color("10")
	case "brightyellow":
		return lipgloss.Color("11")
	case "brightblue":
		return lipgloss.Color("12")
	case "brightmagenta":
		return lipgloss.Color("13")
	case "brightcyan":
		return lipgloss.Color("14")
	default:
		return lipgloss.Color("7")
	}
}
