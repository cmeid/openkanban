package project

import (
	"strings"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
)

// BranchNameForTitle derives the git branch name for a ticket title using the
// settings cascade: a project's own Settings override the global config
// Defaults, which fall back to hardcoded values ("task/", "{prefix}{slug}",
// 40). This is the single source of truth shared by the TUI spawn path
// (internal/ui) and the `ticket new --worktree` CLI path (cmd) so the two can
// never derive different names for the same ticket — a divergence would defeat
// WorktreeManager.CreateWorktree's path/branch reuse check and produce a
// duplicate worktree at spawn.
//
// Only the "template" naming mode is handled here (prefix+slug substitution),
// matching the historical Model behavior; BranchNaming "ai"/"prompt" modes are
// intentionally out of scope.
func BranchNameForTitle(title string, proj *Project, defaults config.BoardSettings) string {
	maxLen := branchSlugMaxLength(proj, defaults)
	slug := board.Slugify(title, maxLen)

	template := branchTemplate(proj, defaults)
	prefix := branchPrefix(proj, defaults)

	result := strings.ReplaceAll(template, "{prefix}", prefix)
	result = strings.ReplaceAll(result, "{slug}", slug)

	return result
}

func branchPrefix(proj *Project, defaults config.BoardSettings) string {
	if proj != nil && proj.Settings.BranchPrefix != "" {
		return proj.Settings.BranchPrefix
	}
	if defaults.BranchPrefix != "" {
		return defaults.BranchPrefix
	}
	return "task/"
}

func branchTemplate(proj *Project, defaults config.BoardSettings) string {
	if proj != nil && proj.Settings.BranchTemplate != "" {
		return proj.Settings.BranchTemplate
	}
	if defaults.BranchTemplate != "" {
		return defaults.BranchTemplate
	}
	return "{prefix}{slug}"
}

func branchSlugMaxLength(proj *Project, defaults config.BoardSettings) int {
	if proj != nil && proj.Settings.SlugMaxLength > 0 {
		return proj.Settings.SlugMaxLength
	}
	if defaults.SlugMaxLength > 0 {
		return defaults.SlugMaxLength
	}
	return 40
}
