package config

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const defaultGlobalPrompt = `You have been spawned by OpenKanban to work on a ticket.

**Title:** {{.Title}}

**Description:**
{{.Description}}

**Branch:** {{.BranchName}} (from {{.BaseBranch}})

Focus on completing this ticket. Ask clarifying questions if the description is unclear.`

// defaultAgentPrompt is the canonical priming prompt OpenKanban sends to
// every spawned Claude-class agent. It is embedded from
// agent_prompt.tmpl so the source is editable markdown rather than
// backtick-escaped Go string literals. The binary ships with this
// content baked in; per-user customization still flows through
// config.json's `agents.<name>.init_prompt` override.
//
//go:embed agent_prompt.tmpl
var defaultAgentPrompt string

// DefaultAgentPrompt returns the shipped priming template. Exposed so
// tests in sibling packages can render it and assert invariants
// (e.g. that the leading phrases still match agent.ClaudePrimingPrefixes).
func DefaultAgentPrompt() string {
	return defaultAgentPrompt
}

const defaultAiderPrompt = `OpenKanban Ticket: {{.Title}}

Description:
{{.Description}}

Branch: {{.BranchName}} (from {{.BaseBranch}})

This is your assigned task. Implement what the description specifies.`

// AgentPriority defines the order in which agents are preferred when auto-detecting.
// The first available agent in this list becomes the default.
var AgentPriority = []string{"opencode", "claude", "gemini", "codex", "aider"}

// DetectAvailableAgent returns the first agent from the priority list
// whose command is available in PATH. Falls back to the first priority
// agent if none are found (user may install later).
func DetectAvailableAgent(agents map[string]AgentConfig) string {
	for _, name := range AgentPriority {
		if agent, exists := agents[name]; exists {
			if _, err := exec.LookPath(agent.Command); err == nil {
				return name
			}
		}
	}
	// Fallback to first in priority list
	return AgentPriority[0]
}

// Config holds the global application configuration
type Config struct {
	Defaults BoardSettings          `json:"defaults"`
	Agents   map[string]AgentConfig `json:"agents"`
	UI       UIConfig               `json:"ui"`
	Cleanup  CleanupSettings        `json:"cleanup"`
	Behavior BehaviorSettings       `json:"behavior"`
	Daemon   DaemonSettings         `json:"daemon"`
	Opencode OpencodeSettings       `json:"opencode"`
	Keys     map[string]string      `json:"keys,omitempty"`
}

// OpencodeSettings controls OpenCode server integration
type OpencodeSettings struct {
	ServerEnabled  bool `json:"server_enabled"`  // Start opencode server for enhanced status detection
	ServerPort     int  `json:"server_port"`     // Port for opencode server (default: 4096)
	PollInterval   int  `json:"poll_interval"`   // Status polling interval in seconds (default: 1)
	StartupTimeout int  `json:"startup_timeout"` // Server startup timeout in seconds (default: 10)
}

// BoardSettings contains default settings for boards
type BoardSettings struct {
	DefaultAgent     string `json:"default_agent"`
	WorktreeBase     string `json:"worktree_base"`
	AutoSpawnAgent   bool   `json:"auto_spawn_agent"`
	AutoCreateBranch bool   `json:"auto_create_branch"`
	BranchPrefix     string `json:"branch_prefix"`
	BranchNaming     string `json:"branch_naming"`   // "template" | "ai" | "prompt"
	BranchTemplate   string `json:"branch_template"` // e.g., "{prefix}{slug}"
	SlugMaxLength    int    `json:"slug_max_length"` // default: 40
	InitPrompt       string `json:"init_prompt"`
}

// AgentConfig defines how to spawn and monitor an AI agent
type AgentConfig struct {
	Command    string            `json:"command"`
	Args       []string          `json:"args"`
	Env        map[string]string `json:"env"`
	StatusFile string            `json:"status_file"`
	InitPrompt string            `json:"init_prompt"`
}

// UIConfig holds UI-related preferences
type UIConfig struct {
	Theme           string       `json:"theme"`
	CustomColors    *ThemeColors `json:"custom_colors,omitempty"`
	ShowAgentStatus bool         `json:"show_agent_status"`
	RefreshInterval int          `json:"refresh_interval"`
	ColumnWidth     int          `json:"column_width"`
	TicketHeight    int          `json:"ticket_height"`
	SidebarVisible  bool         `json:"sidebar_visible"`
	ScrollbackLines int          `json:"scrollback_lines"`
}

// CleanupSettings controls cleanup behavior when deleting tickets
type CleanupSettings struct {
	DeleteWorktree       bool `json:"delete_worktree"`        // Remove git worktree on ticket delete
	DeleteBranch         bool `json:"delete_branch"`          // Delete git branch after worktree removal
	ForceWorktreeRemoval bool `json:"force_worktree_removal"` // Force removal even with uncommitted changes
}

// BehaviorSettings controls application behavior preferences
type BehaviorSettings struct {
	ConfirmQuitWithAgents     bool `json:"confirm_quit_with_agents"`      // Prompt before quitting with running agents
	CheckForUpdatesOnLaunch   bool `json:"check_for_updates_on_launch"`   // Quick update check before entering the TUI
	ForwardAgentNotifications bool `json:"forward_agent_notifications"`   // Re-emit OSC 9 notifications from wrapped agents to the host terminal
}

// DaemonSettings controls how the TUI interacts with openkanbankd at
// launch. Defaults preserve historical behavior — the TUI forks a
// daemon on demand. Set Autostart=false (or pass --no-launch-daemon)
// when openkanbankd is managed externally (e.g. by launchd) so the
// TUI does not race the service for the pidlock.
type DaemonSettings struct {
	Autostart bool `json:"autostart"` // TUI autostarts the daemon if not already running
}

func defaultAgents() map[string]AgentConfig {
	return map[string]AgentConfig{
		"claude": {
			Command:    "claude",
			Args:       []string{"--dangerously-skip-permissions"},
			Env:        map[string]string{},
			StatusFile: ".claude/status.json",
			InitPrompt: defaultAgentPrompt,
		},
		"opencode": {
			Command:    "opencode",
			Args:       []string{},
			Env:        map[string]string{},
			StatusFile: ".opencode/status.json",
			InitPrompt: defaultAgentPrompt,
		},
		"aider": {
			Command:    "aider",
			Args:       []string{"--yes"},
			Env:        map[string]string{},
			StatusFile: "",
			InitPrompt: defaultAiderPrompt,
		},
		"gemini": {
			Command:    "gemini",
			Args:       []string{"--yolo"},
			Env:        map[string]string{},
			StatusFile: "",
			InitPrompt: defaultAgentPrompt,
		},
		"codex": {
			Command:    "codex",
			Args:       []string{"--full-auto"},
			Env:        map[string]string{},
			StatusFile: "",
			InitPrompt: defaultAgentPrompt,
		},
	}
}

// DefaultConfig returns the default configuration
func DefaultConfig() *Config {
	agents := defaultAgents()
	return &Config{
		Defaults: BoardSettings{
			DefaultAgent:     DetectAvailableAgent(agents),
			WorktreeBase:     "",
			AutoSpawnAgent:   true,
			AutoCreateBranch: true,
			BranchPrefix:     "task/",
			BranchNaming:     "template",
			BranchTemplate:   "{prefix}{slug}",
			SlugMaxLength:    40,
			InitPrompt:       defaultGlobalPrompt,
		},
		Agents: agents,
		UI: UIConfig{
			Theme:           "catppuccin-mocha",
			ShowAgentStatus: true,
			RefreshInterval: 5,
			ColumnWidth:     40,
			TicketHeight:    4,
			SidebarVisible:  true,
			ScrollbackLines: 10000,
		},
		Cleanup: CleanupSettings{
			DeleteWorktree:       true,
			DeleteBranch:         false,
			ForceWorktreeRemoval: false,
		},
		Behavior: BehaviorSettings{
			ConfirmQuitWithAgents:     true,
			CheckForUpdatesOnLaunch:   true,
			ForwardAgentNotifications: true,
		},
		Daemon: DaemonSettings{
			Autostart: true,
		},
		Opencode: OpencodeSettings{
			ServerEnabled:  true,
			ServerPort:     4096,
			PollInterval:   1,
			StartupTimeout: 10,
		},
	}
}

// ConfigDir returns the configuration directory path.
// Priority: OPENKANBAN_CONFIG_DIR > XDG_CONFIG_HOME/openkanban > ~/.config/openkanban
func ConfigDir() (string, error) {
	// Explicit override (testing, CI, multiple instances)
	if dir := os.Getenv("OPENKANBAN_CONFIG_DIR"); dir != "" {
		return dir, nil
	}

	// XDG standard
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "openkanban"), nil
	}

	// Default fallback
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "openkanban"), nil
}

// ConfigPath returns the default config file path
func ConfigPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads configuration from file or returns defaults
func Load(path string) (*Config, error) {
	if path == "" {
		var err error
		path, err = ConfigPath()
		if err != nil {
			return DefaultConfig(), nil
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		return nil, err
	}

	cfg := DefaultConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	cfg.mergeAgentDefaults()

	return cfg, nil
}

// ReloadFromDisk re-reads the config from disk and replaces the
// contents of c in place. Existing pointers to c remain valid.
// Existing agent processes are unaffected; only newly spawned agents
// see updated AgentConfig values.
//
// Safe to call from a Bubble Tea Update goroutine.
func (c *Config) ReloadFromDisk() error {
	fresh, err := Load("")
	if err != nil {
		return err
	}
	*c = *fresh
	return nil
}

func (c *Config) mergeAgentDefaults() {
	defaults := DefaultConfig()

	for name, defaultCfg := range defaults.Agents {
		if userCfg, exists := c.Agents[name]; exists {
			if userCfg.StatusFile == "" {
				userCfg.StatusFile = defaultCfg.StatusFile
			}
			if userCfg.Env == nil {
				userCfg.Env = defaultCfg.Env
			}
			if userCfg.InitPrompt == "" {
				userCfg.InitPrompt = defaultCfg.InitPrompt
			}
			c.Agents[name] = userCfg
		}
	}
}

func (c *Config) GetEffectiveInitPrompt(agentType string) string {
	if agentCfg, ok := c.Agents[agentType]; ok && agentCfg.InitPrompt != "" {
		return agentCfg.InitPrompt
	}
	if c.Defaults.InitPrompt != "" {
		return c.Defaults.InitPrompt
	}
	return defaultGlobalPrompt
}

func (c *Config) GetTheme() Theme {
	return GetTheme(c.UI.Theme, c.UI.CustomColors)
}

// Save writes configuration to file
func (c *Config) Save(path string) error {
	if path == "" {
		var err error
		path, err = ConfigPath()
		if err != nil {
			return err
		}
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}

// LoadWithValidation loads config and returns structured validation result
func LoadWithValidation(path string) (*Config, *ValidationResult, error) {
	if path == "" {
		var err error
		path, err = ConfigPath()
		if err != nil {
			return DefaultConfig(), nil, nil
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := DefaultConfig()
			return cfg, cfg.Validate(), nil
		}
		return nil, nil, err
	}

	cfg := DefaultConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		result := &ValidationResult{}
		if jsonErr := formatJSONError(err); jsonErr != "" {
			result.AddError("json", "", jsonErr, nil)
		} else {
			result.AddError("json", "", err.Error(), nil)
		}
		return nil, result, err
	}

	cfg.mergeAgentDefaults()
	result := cfg.Validate()

	return cfg, result, nil
}

// formatJSONError attempts to provide better JSON error context
func formatJSONError(err error) string {
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return fmt.Sprintf("invalid JSON at byte %d: %s", syntaxErr.Offset, syntaxErr.Error())
	}

	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return fmt.Sprintf("field %q expects %s but got %s", typeErr.Field, typeErr.Type, typeErr.Value)
	}

	return ""
}
