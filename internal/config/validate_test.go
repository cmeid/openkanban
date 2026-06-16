package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate_ValidDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	result := cfg.Validate()

	if result.HasErrors() {
		t.Errorf("default config should be valid, got errors:\n%s", result.FormatErrors())
	}
}

func TestValidate_InvalidBranchNaming(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Defaults.BranchNaming = "invalid"

	result := cfg.Validate()

	if !result.HasErrors() {
		t.Error("expected validation error for invalid branch_naming")
	}

	found := false
	for _, e := range result.Errors {
		if e.Section == "defaults" && e.Field == "branch_naming" {
			found = true
			if !strings.Contains(e.Message, "template, ai, prompt") {
				t.Errorf("error message should list valid values; got %q", e.Message)
			}
		}
	}
	if !found {
		t.Error("expected error for defaults.branch_naming")
	}
}

func TestValidate_NegativeSlugMaxLength(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Defaults.SlugMaxLength = -1

	result := cfg.Validate()

	if !result.HasErrors() {
		t.Error("expected validation error for negative slug_max_length")
	}

	found := false
	for _, e := range result.Errors {
		if e.Section == "defaults" && e.Field == "slug_max_length" {
			found = true
		}
	}
	if !found {
		t.Error("expected error for defaults.slug_max_length")
	}
}

func TestValidate_NonexistentDefaultAgent(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Defaults.DefaultAgent = "nonexistent-agent"

	result := cfg.Validate()

	if !result.HasErrors() {
		t.Error("expected validation error for nonexistent default_agent")
	}

	found := false
	for _, e := range result.Errors {
		if e.Section == "defaults" && e.Field == "default_agent" {
			found = true
			if !strings.Contains(e.Message, "nonexistent-agent") {
				t.Errorf("error message should mention the agent name; got %q", e.Message)
			}
		}
	}
	if !found {
		t.Error("expected error for defaults.default_agent")
	}
}

func TestValidate_BranchTemplateMissingPlaceholders(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Defaults.BranchTemplate = "feature-branch"

	result := cfg.Validate()

	// This should be a warning, not an error
	if result.HasErrors() {
		t.Errorf("missing placeholders should be a warning, not error:\n%s", result.FormatErrors())
	}

	if !result.HasWarnings() {
		t.Error("expected warning for branch_template without placeholders")
	}

	found := false
	for _, w := range result.Warnings {
		if w.Section == "defaults" && w.Field == "branch_template" {
			found = true
		}
	}
	if !found {
		t.Error("expected warning for defaults.branch_template")
	}
}

func TestValidate_MissingAgentCommand(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Agents["custom"] = AgentConfig{
		Command: "",
		Args:    []string{},
	}

	result := cfg.Validate()

	if !result.HasErrors() {
		t.Error("expected validation error for missing agent command")
	}

	found := false
	for _, e := range result.Errors {
		if e.Section == "agents.custom" && e.Field == "command" {
			found = true
			if !strings.Contains(e.Message, "required") {
				t.Errorf("error message should mention 'required'; got %q", e.Message)
			}
		}
	}
	if !found {
		t.Error("expected error for agents.custom.command")
	}
}

func TestValidate_CommandNotInPath(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Agents["custom"] = AgentConfig{
		Command: "nonexistent-binary-12345",
		Args:    []string{},
	}
	cfg.Defaults.DefaultAgent = "custom"

	result := cfg.Validate()

	hasCommandWarning := false
	for _, w := range result.Warnings {
		if w.Section == "agents.custom" && w.Field == "command" {
			hasCommandWarning = true
			if !strings.Contains(w.Message, "not found in PATH") {
				t.Errorf("warning should mention PATH; got %q", w.Message)
			}
		}
	}
	if !hasCommandWarning {
		t.Error("expected warning for default agent command not in PATH")
	}
}

func TestValidate_NonDefaultAgentNotInPath_NoWarning(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Agents["custom"] = AgentConfig{
		Command: "nonexistent-binary-12345",
		Args:    []string{},
	}
	cfg.Defaults.DefaultAgent = "opencode"

	result := cfg.Validate()

	for _, w := range result.Warnings {
		if w.Section == "agents.custom" && w.Field == "command" {
			t.Error("should not warn about non-default agent missing from PATH")
		}
	}
}

func TestValidate_InvalidTemplatePrompt(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Agents["custom"] = AgentConfig{
		Command:    "echo",
		InitPrompt: "{{.Invalid syntax",
	}

	result := cfg.Validate()

	if !result.HasErrors() {
		t.Error("expected validation error for invalid template syntax")
	}

	found := false
	for _, e := range result.Errors {
		if e.Section == "agents.custom" && e.Field == "init_prompt" {
			found = true
		}
	}
	if !found {
		t.Error("expected error for agents.custom.init_prompt")
	}
}

func TestValidate_InvalidDefaultsInitPrompt(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Defaults.InitPrompt = "{{.Broken"

	result := cfg.Validate()

	if !result.HasErrors() {
		t.Error("expected validation error for invalid defaults.init_prompt")
	}

	found := false
	for _, e := range result.Errors {
		if e.Section == "defaults" && e.Field == "init_prompt" {
			found = true
		}
	}
	if !found {
		t.Error("expected error for defaults.init_prompt")
	}
}

func TestValidate_ZeroUIColumnWidth(t *testing.T) {
	cfg := DefaultConfig()
	cfg.UI.ColumnWidth = 0

	result := cfg.Validate()

	if !result.HasErrors() {
		t.Error("expected validation error for zero column_width")
	}

	found := false
	for _, e := range result.Errors {
		if e.Section == "ui" && e.Field == "column_width" {
			found = true
		}
	}
	if !found {
		t.Error("expected error for ui.column_width")
	}
}

func TestValidate_ZeroUITicketHeight(t *testing.T) {
	cfg := DefaultConfig()
	cfg.UI.TicketHeight = 0

	result := cfg.Validate()

	if !result.HasErrors() {
		t.Error("expected validation error for zero ticket_height")
	}

	found := false
	for _, e := range result.Errors {
		if e.Section == "ui" && e.Field == "ticket_height" {
			found = true
		}
	}
	if !found {
		t.Error("expected error for ui.ticket_height")
	}
}

func TestValidate_ZeroUIRefreshInterval(t *testing.T) {
	cfg := DefaultConfig()
	cfg.UI.RefreshInterval = 0

	result := cfg.Validate()

	if !result.HasErrors() {
		t.Error("expected validation error for zero refresh_interval")
	}

	found := false
	for _, e := range result.Errors {
		if e.Section == "ui" && e.Field == "refresh_interval" {
			found = true
		}
	}
	if !found {
		t.Error("expected error for ui.refresh_interval")
	}
}

func TestValidate_InvalidServerPort(t *testing.T) {
	tests := []struct {
		name string
		port int
	}{
		{"zero port", 0},
		{"negative port", -1},
		{"port too high", 70000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Opencode.ServerPort = tt.port

			result := cfg.Validate()

			if !result.HasErrors() {
				t.Error("expected validation error for invalid server_port")
			}

			found := false
			for _, e := range result.Errors {
				if e.Section == "opencode" && e.Field == "server_port" {
					found = true
				}
			}
			if !found {
				t.Errorf("expected error for opencode.server_port with value %d", tt.port)
			}
		})
	}
}

func TestValidate_NegativePollInterval(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Opencode.PollInterval = -1

	result := cfg.Validate()

	if !result.HasErrors() {
		t.Error("expected validation error for negative poll_interval")
	}

	found := false
	for _, e := range result.Errors {
		if e.Section == "opencode" && e.Field == "poll_interval" {
			found = true
		}
	}
	if !found {
		t.Error("expected error for opencode.poll_interval")
	}
}

func TestValidationResult_FormatErrors(t *testing.T) {
	r := &ValidationResult{}
	r.AddError("defaults", "branch_naming", "must be valid", "invalid")
	r.AddError("agents.custom", "command", "is required", nil)

	output := r.FormatErrors()

	if !strings.Contains(output, "defaults") {
		t.Error("formatted errors should contain section name")
	}
	if !strings.Contains(output, "branch_naming") {
		t.Error("formatted errors should contain field name")
	}
	if !strings.Contains(output, "must be valid") {
		t.Error("formatted errors should contain message")
	}
	if !strings.Contains(output, "invalid") {
		t.Error("formatted errors should contain value")
	}
	if !strings.Contains(output, "agents.custom") {
		t.Error("formatted errors should contain nested section")
	}
}

func TestValidationResult_FormatWarnings(t *testing.T) {
	r := &ValidationResult{}
	r.AddWarning("agents.custom", "command", "not found in PATH", "custom-agent")

	output := r.FormatWarnings()

	if !strings.Contains(output, "agents.custom") {
		t.Error("formatted warnings should contain section name")
	}
	if !strings.Contains(output, "command") {
		t.Error("formatted warnings should contain field name")
	}
	if !strings.Contains(output, "not found in PATH") {
		t.Error("formatted warnings should contain message")
	}
}

func TestValidationResult_HasErrors(t *testing.T) {
	r := &ValidationResult{}

	if r.HasErrors() {
		t.Error("empty result should not have errors")
	}

	r.AddError("test", "field", "message", nil)

	if !r.HasErrors() {
		t.Error("result with error should have errors")
	}
}

func TestValidationResult_HasWarnings(t *testing.T) {
	r := &ValidationResult{}

	if r.HasWarnings() {
		t.Error("empty result should not have warnings")
	}

	r.AddWarning("test", "field", "message", nil)

	if !r.HasWarnings() {
		t.Error("result with warning should have warnings")
	}
}

func TestValidate_MultipleErrors(t *testing.T) {
	cfg := &Config{
		Defaults: BoardSettings{
			DefaultAgent:   "nonexistent",
			BranchNaming:   "invalid",
			SlugMaxLength:  -1,
			BranchTemplate: "no-placeholders",
		},
		Agents: map[string]AgentConfig{
			"bad": {Command: ""},
		},
		UI: UIConfig{
			ColumnWidth:     0,
			TicketHeight:    0,
			RefreshInterval: 0,
		},
		Opencode: OpencodeSettings{
			ServerPort:   -1,
			PollInterval: -1,
		},
	}

	result := cfg.Validate()

	// Should have multiple errors
	if len(result.Errors) < 5 {
		t.Errorf("expected at least 5 errors, got %d:\n%s", len(result.Errors), result.FormatErrors())
	}

	// Should have at least one warning (branch_template)
	if len(result.Warnings) < 1 {
		t.Error("expected at least 1 warning for branch_template")
	}
}

func TestValidate_EmptyBranchNamingIsValid(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Defaults.BranchNaming = ""

	result := cfg.Validate()

	for _, e := range result.Errors {
		if e.Field == "branch_naming" {
			t.Error("empty branch_naming should be valid (uses default)")
		}
	}
}

func TestValidate_ValidTemplatePrompt(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Agents["custom"] = AgentConfig{
		Command:    "echo",
		InitPrompt: "Working on: {{.Title}}\nDescription: {{.Description}}",
	}

	result := cfg.Validate()

	for _, e := range result.Errors {
		if e.Section == "agents.custom" && e.Field == "init_prompt" {
			t.Errorf("valid template should not produce error: %s", e.Message)
		}
	}
}

// writeGlobalClaudeMd writes a fake global CLAUDE.md into a temp dir and
// points OPENKANBAN_GLOBAL_CLAUDE_MD at it for the duration of the test.
// Pass an empty content string to simulate the file being absent.
func writeGlobalClaudeMd(t *testing.T, content string) {
	t.Helper()
	if content == "" {
		t.Setenv("OPENKANBAN_GLOBAL_CLAUDE_MD", filepath.Join(t.TempDir(), "nonexistent.md"))
		return
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fake CLAUDE.md: %v", err)
	}
	t.Setenv("OPENKANBAN_GLOBAL_CLAUDE_MD", path)
}

func TestValidateInitPromptOverlap(t *testing.T) {
	const globalWithPushSection = `# Global config

## Sending code out (pushes and PRs)

Pushes are gated on the destination repo.

## Git commit signing on this machine

SSH-signed via 1Password.
`

	tests := []struct {
		name           string
		prompt         string
		globalContent  string // "" means no global file
		wantWarnSubstr []string
		wantNoWarn     bool
	}{
		{
			name:           "strong marker: HARD RULE",
			prompt:         "## PR guardrail (HARD RULE)\nDo not push.",
			globalContent:  globalWithPushSection,
			wantWarnSubstr: []string{"HARD RULE"},
		},
		{
			name:           "strong marker: NEVER run gh pr create",
			prompt:         "Do this: NEVER run `gh pr create` without approval.",
			globalContent:  globalWithPushSection,
			wantWarnSubstr: []string{"NEVER", "gh pr create"},
		},
		{
			name:           "strong marker: global rule",
			prompt:         "Following the global rule, never push without approval.",
			globalContent:  globalWithPushSection,
			wantWarnSubstr: []string{"global rule"},
		},
		{
			name:           "strong marker: global rule fires even when global is absent",
			prompt:         "Per my global rule, ask before pushing.",
			globalContent:  "",
			wantWarnSubstr: []string{"global rule"},
		},
		{
			name:           "section header overlap on PR",
			prompt:         "## PR guardrail\nSome rule.",
			globalContent:  globalWithPushSection,
			wantWarnSubstr: []string{"PR guardrail", "Sending code out"},
		},
		{
			name:           "no overlap, clean prompt with global present",
			prompt:         "## First action\nRun /prime.\n\n## Your assignment\nTitle here.",
			globalContent:  globalWithPushSection,
			wantNoWarn:     true,
		},
		{
			name:           "no overlap, clean prompt with global absent",
			prompt:         "## First action\nRun /prime.\n\n## Your assignment\nTitle here.",
			globalContent:  "",
			wantNoWarn:     true,
		},
		{
			name:           "strong marker fires even when global is absent",
			prompt:         "## Guardrail (HARD RULE)\nNo PRs.",
			globalContent:  "",
			wantWarnSubstr: []string{"HARD RULE"},
		},
		{
			name:           "section header overlap on signing",
			prompt:         "## Commit signing\nUse PGP.",
			globalContent:  globalWithPushSection,
			wantWarnSubstr: []string{"Commit signing", "Git commit signing"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeGlobalClaudeMd(t, tt.globalContent)

			cfg := DefaultConfig()
			cfg.Agents["custom"] = AgentConfig{
				Command:    "echo",
				InitPrompt: tt.prompt,
			}
			result := cfg.Validate()

			var overlapWarnings []ValidationError
			for _, w := range result.Warnings {
				if w.Section == "agents.custom" && w.Field == "init_prompt" {
					overlapWarnings = append(overlapWarnings, w)
				}
			}

			if tt.wantNoWarn {
				if len(overlapWarnings) > 0 {
					t.Errorf("expected no init_prompt overlap warnings, got %d:\n%s",
						len(overlapWarnings), formatWarnings(overlapWarnings))
				}
				return
			}

			if len(overlapWarnings) == 0 {
				t.Fatalf("expected warning(s) containing %v, got none", tt.wantWarnSubstr)
			}

			joined := joinWarningMessages(overlapWarnings)
			for _, want := range tt.wantWarnSubstr {
				if !strings.Contains(joined, want) {
					t.Errorf("warning text missing %q\ngot:\n%s", want, joined)
				}
			}
		})
	}
}

func TestValidateInitPromptOverlap_OnDefaults(t *testing.T) {
	writeGlobalClaudeMd(t, "## Sending code out\nPushes gated by destination.\n")

	cfg := DefaultConfig()
	cfg.Defaults.InitPrompt = "## PR guardrail (HARD RULE)\nDo not push."

	result := cfg.Validate()

	found := false
	for _, w := range result.Warnings {
		if w.Section == "defaults" && w.Field == "init_prompt" {
			found = true
			if !strings.Contains(w.Message, "HARD RULE") && !strings.Contains(w.Message, "PR guardrail") {
				t.Errorf("expected message to mention HARD RULE or PR guardrail; got %q", w.Message)
			}
		}
	}
	if !found {
		t.Error("expected warning on defaults.init_prompt for HARD RULE marker")
	}
}

func TestValidateInitPromptOverlap_NeverError(t *testing.T) {
	writeGlobalClaudeMd(t, "## Sending code out\nPushes gated.\n")

	cfg := DefaultConfig()
	cfg.Agents["custom"] = AgentConfig{
		Command:    "echo",
		InitPrompt: "## PR guardrail (HARD RULE)\nNEVER run `gh pr create`. Following the global rule, every context.",
	}

	result := cfg.Validate()

	for _, e := range result.Errors {
		if e.Section == "agents.custom" && e.Field == "init_prompt" {
			t.Errorf("contradiction check should warn, never error; got error: %s", e.Message)
		}
	}
}

// formatWarnings is a test helper for readable failure output.
func formatWarnings(ws []ValidationError) string {
	var sb strings.Builder
	for _, w := range ws {
		sb.WriteString("  - ")
		sb.WriteString(w.Message)
		sb.WriteString("\n")
	}
	return sb.String()
}

func joinWarningMessages(ws []ValidationError) string {
	var sb strings.Builder
	for _, w := range ws {
		sb.WriteString(w.Message)
		sb.WriteString("\n")
	}
	return sb.String()
}
