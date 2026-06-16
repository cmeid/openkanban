package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

// ValidationError represents a single config validation issue
type ValidationError struct {
	Section string // "defaults", "agents.claude", "ui", etc.
	Field   string // "command", "branch_naming", etc.
	Message string // Human-readable error
	Value   any    // The invalid value (for display)
}

// ValidationResult holds all validation errors and warnings
type ValidationResult struct {
	Errors   []ValidationError
	Warnings []ValidationError
}

// HasErrors returns true if there are any validation errors
func (r *ValidationResult) HasErrors() bool {
	return len(r.Errors) > 0
}

// HasWarnings returns true if there are any validation warnings
func (r *ValidationResult) HasWarnings() bool {
	return len(r.Warnings) > 0
}

// AddError adds a validation error
func (r *ValidationResult) AddError(section, field, message string, value any) {
	r.Errors = append(r.Errors, ValidationError{
		Section: section,
		Field:   field,
		Message: message,
		Value:   value,
	})
}

// AddWarning adds a validation warning
func (r *ValidationResult) AddWarning(section, field, message string, value any) {
	r.Warnings = append(r.Warnings, ValidationError{
		Section: section,
		Field:   field,
		Message: message,
		Value:   value,
	})
}

// FormatErrors returns a formatted string of all errors for CLI output
func (r *ValidationResult) FormatErrors() string {
	var sb strings.Builder
	for _, e := range r.Errors {
		if e.Field != "" {
			sb.WriteString(fmt.Sprintf("  [%s] %s\n", e.Section, e.Field))
		} else {
			sb.WriteString(fmt.Sprintf("  [%s]\n", e.Section))
		}
		sb.WriteString(fmt.Sprintf("    %s\n", e.Message))
		if e.Value != nil {
			sb.WriteString(fmt.Sprintf("    got: %v\n", e.Value))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// FormatWarnings returns a formatted string of all warnings for CLI output
func (r *ValidationResult) FormatWarnings() string {
	var sb strings.Builder
	for _, w := range r.Warnings {
		if w.Field != "" {
			sb.WriteString(fmt.Sprintf("  [%s] %s\n", w.Section, w.Field))
		} else {
			sb.WriteString(fmt.Sprintf("  [%s]\n", w.Section))
		}
		sb.WriteString(fmt.Sprintf("    %s\n", w.Message))
		if w.Value != nil {
			sb.WriteString(fmt.Sprintf("    got: %v\n", w.Value))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// Validate performs full config validation and returns all errors and warnings
func (c *Config) Validate() *ValidationResult {
	result := &ValidationResult{}
	c.validateDefaults(result)
	c.validateAgents(result)
	c.validateUI(result)
	c.validateOpencode(result)
	return result
}

// validateDefaults validates the defaults section
func (c *Config) validateDefaults(r *ValidationResult) {
	// BranchNaming must be a valid enum value
	validNaming := map[string]bool{"template": true, "ai": true, "prompt": true, "": true}
	if !validNaming[c.Defaults.BranchNaming] {
		r.AddError("defaults", "branch_naming",
			fmt.Sprintf("must be one of: template, ai, prompt (got %q)", c.Defaults.BranchNaming),
			c.Defaults.BranchNaming)
	}

	// SlugMaxLength must be positive if set
	if c.Defaults.SlugMaxLength < 0 {
		r.AddError("defaults", "slug_max_length",
			"must be a positive number",
			c.Defaults.SlugMaxLength)
	}

	// DefaultAgent must reference a defined agent (if set)
	if c.Defaults.DefaultAgent != "" {
		if _, exists := c.Agents[c.Defaults.DefaultAgent]; !exists {
			r.AddError("defaults", "default_agent",
				fmt.Sprintf("references undefined agent %q", c.Defaults.DefaultAgent),
				c.Defaults.DefaultAgent)
		}
	}

	// BranchTemplate should contain placeholders (warning only)
	if c.Defaults.BranchTemplate != "" {
		if !strings.Contains(c.Defaults.BranchTemplate, "{slug}") &&
			!strings.Contains(c.Defaults.BranchTemplate, "{prefix}") {
			r.AddWarning("defaults", "branch_template",
				"should contain {slug} or {prefix} placeholder",
				c.Defaults.BranchTemplate)
		}
	}

	// Validate InitPrompt template syntax
	if c.Defaults.InitPrompt != "" {
		if err := validateTemplate(c.Defaults.InitPrompt); err != nil {
			r.AddError("defaults", "init_prompt",
				fmt.Sprintf("invalid Go template syntax: %v", err),
				nil)
		}
		for _, msg := range validateInitPromptOverlap(c.Defaults.InitPrompt, findGlobalClaudeMd()) {
			r.AddWarning("defaults", "init_prompt", msg, nil)
		}
	}
}

func (c *Config) validateAgents(r *ValidationResult) {
	for name, agent := range c.Agents {
		section := fmt.Sprintf("agents.%s", name)

		if agent.Command == "" {
			r.AddError(section, "command", "is required but missing", nil)
		} else if name == c.Defaults.DefaultAgent {
			if _, err := exec.LookPath(agent.Command); err != nil {
				r.AddWarning(section, "command",
					fmt.Sprintf("executable %q not found in PATH", agent.Command),
					agent.Command)
			}
		}

		if agent.InitPrompt != "" {
			if err := validateTemplate(agent.InitPrompt); err != nil {
				r.AddError(section, "init_prompt",
					fmt.Sprintf("invalid Go template syntax: %v", err),
					nil)
			}
			for _, msg := range validateInitPromptOverlap(agent.InitPrompt, findGlobalClaudeMd()) {
				r.AddWarning(section, "init_prompt", msg, nil)
			}
		}
	}
}

// validateUI validates the UI section
func (c *Config) validateUI(r *ValidationResult) {
	if c.UI.Theme != "" && !IsValidTheme(c.UI.Theme) {
		r.AddWarning("ui", "theme",
			fmt.Sprintf("unknown theme %q, falling back to catppuccin-mocha. Available: %v",
				c.UI.Theme, ThemeNames()),
			c.UI.Theme)
	}

	if c.UI.ColumnWidth <= 0 {
		r.AddError("ui", "column_width",
			"must be a positive number",
			c.UI.ColumnWidth)
	}

	if c.UI.TicketHeight <= 0 {
		r.AddError("ui", "ticket_height",
			"must be a positive number",
			c.UI.TicketHeight)
	}

	if c.UI.RefreshInterval <= 0 {
		r.AddError("ui", "refresh_interval",
			"must be a positive number",
			c.UI.RefreshInterval)
	}
}

// validateOpencode validates the opencode server settings
func (c *Config) validateOpencode(r *ValidationResult) {
	if c.Opencode.ServerPort <= 0 || c.Opencode.ServerPort > 65535 {
		r.AddError("opencode", "server_port",
			"must be between 1 and 65535",
			c.Opencode.ServerPort)
	}

	if c.Opencode.PollInterval < 0 {
		r.AddError("opencode", "poll_interval",
			"must be a positive number",
			c.Opencode.PollInterval)
	}
}

// validateTemplate checks if a string is a valid Go template
func validateTemplate(tmpl string) error {
	_, err := template.New("check").Parse(tmpl)
	return err
}

// findGlobalClaudeMd returns the path to the user's global CLAUDE.md if it
// exists, or "" otherwise. The OPENKANBAN_GLOBAL_CLAUDE_MD env var, when set,
// overrides the default path lookup — useful for tests and for users who
// want to opt out of the contradiction check by pointing at a nonexistent
// file. A returned "" tells callers to skip global-dependent checks
// silently; it's the expected state for openkanban users who don't run
// Claude Code.
func findGlobalClaudeMd() string {
	if override, ok := os.LookupEnv("OPENKANBAN_GLOBAL_CLAUDE_MD"); ok {
		if _, err := os.Stat(override); err == nil {
			return override
		}
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(home, ".claude", "CLAUDE.md")
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// strongRuleMarkers are textual patterns whose presence in an init_prompt
// signals that the prompt is restating a global rule. Each entry is a
// compiled regexp + a human-friendly label used in the warning message.
var strongRuleMarkers = []struct {
	pattern *regexp.Regexp
	label   string
}{
	{regexp.MustCompile(`(?i)HARD RULE`), "HARD RULE"},
	{regexp.MustCompile("(?i)NEVER\\s+(?:run\\s+)?[`']?(?:gh pr create|git push)"), "NEVER gh pr create / git push"},
	{regexp.MustCompile(`(?i)every repo,\s*every project`), "every repo, every project"},
	{regexp.MustCompile(`(?i)Per\s+\w+'s\s+global\s+rule`), "Per <name>'s global rule"},
}

// globalRuleKeywords are topic keywords used to flag when a local
// init_prompt H2 section overlaps with a global CLAUDE.md H2 section.
// Match is case-insensitive on whole-word boundaries. Kept short and
// high-confidence — false positives are tolerable; missed contradictions
// are not.
var globalRuleKeywords = []string{
	"push",
	"PR",
	"pull request",
	"gh pr",
	"git push",
	"signing",
	"gpg",
	"ssh",
	"ticket creation",
}

// h2Pattern matches Markdown level-2 headers at the start of a line.
var h2Pattern = regexp.MustCompile(`(?m)^##\s+(.+)$`)

// validateInitPromptOverlap returns zero or more warning messages
// describing places where prompt appears to restate or contradict rules
// in the global CLAUDE.md at claudeMdPath. Pass an empty claudeMdPath to
// skip the header-overlap stage; the strong-marker stage runs in either
// case so a poisoned prompt is caught even without the global file.
//
// Detection is heuristic (regexp / keyword based) on purpose: this runs
// at config load and must be fast and warning-only. False positives are
// tolerable; users can suppress by editing the prompt or pointing
// OPENKANBAN_GLOBAL_CLAUDE_MD at a nonexistent file.
func validateInitPromptOverlap(prompt, claudeMdPath string) []string {
	var warnings []string

	// Stage 1: strong textual markers. Runs regardless of whether the
	// global file is present — these patterns are diagnostic on their
	// own.
	for _, m := range strongRuleMarkers {
		if m.pattern.FindString(prompt) != "" {
			warnings = append(warnings,
				fmt.Sprintf("init_prompt contains %q — looks like a restated global rule; "+
					"prefer to defer to ~/.claude/CLAUDE.md", m.label))
		}
	}

	// Stage 2: H2 section-header overlap. Needs the global file.
	if claudeMdPath == "" {
		return warnings
	}
	data, err := os.ReadFile(claudeMdPath)
	if err != nil {
		// File disappeared between findGlobalClaudeMd() and now (rare).
		// Skip silently; the strong-marker stage above already ran.
		return warnings
	}

	localHeaders := extractH2Headers(prompt)
	globalHeaders := extractH2Headers(string(data))

	seen := map[string]bool{}
	for _, lh := range localHeaders {
		for _, gh := range globalHeaders {
			kw := sharedRuleKeyword(lh, gh)
			if kw == "" {
				continue
			}
			key := lh + "|" + gh
			if seen[key] {
				continue
			}
			seen[key] = true
			warnings = append(warnings, fmt.Sprintf(
				"init_prompt section %q overlaps with global CLAUDE.md section %q "+
					"(shared topic %q) — consider deferring to global",
				lh, gh, kw))
		}
	}

	return warnings
}

func extractH2Headers(md string) []string {
	matches := h2Pattern.FindAllStringSubmatch(md, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

// sharedRuleKeyword returns the first keyword from globalRuleKeywords
// that appears (case-insensitive, as a substring) in BOTH headers, or ""
// if none. Substring rather than whole-word so headers like
// "PR guardrail" and "Sending code out (pushes and PRs)" both match "PR"
// and "push".
func sharedRuleKeyword(a, b string) string {
	la := strings.ToLower(a)
	lb := strings.ToLower(b)
	for _, kw := range globalRuleKeywords {
		lk := strings.ToLower(kw)
		if strings.Contains(la, lk) && strings.Contains(lb, lk) {
			return kw
		}
	}
	return ""
}
