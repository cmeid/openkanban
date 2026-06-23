package config

import "testing"

// TestAgentConfigIsEnabled pins the tri-state: an explicit Enabled override
// wins over PATH; nil falls back to PATH auto-detection.
func TestAgentConfigIsEnabled(t *testing.T) {
	tr, fa := true, false

	if !(AgentConfig{Enabled: &tr, Command: "definitely-not-a-real-binary-xyzzy"}).IsEnabled() {
		t.Error("Enabled=&true must report enabled regardless of PATH")
	}
	if (AgentConfig{Enabled: &fa, Command: "sh"}).IsEnabled() {
		t.Error("Enabled=&false must report disabled regardless of PATH")
	}
	// Auto (nil): a command that exists on PATH vs one that doesn't. `sh` is
	// present on every POSIX test host.
	if !(AgentConfig{Command: "sh"}).IsEnabled() {
		t.Error("auto: 'sh' should resolve on PATH")
	}
	if (AgentConfig{Command: "definitely-not-a-real-binary-xyzzy"}).IsEnabled() {
		t.Error("auto: a nonexistent command must report disabled")
	}
}
