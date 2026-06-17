// Package finishskill ships the standardized openkanban ticket close-out
// skill. The SKILL.md is the version-controlled source of truth, embedded
// via //go:embed (mirroring how internal/config owns agent_prompt.tmpl)
// and written into the user's global ~/.claude/skills/ on launch so it
// stays in sync with the binary — and so `openkanban update` propagates
// skill changes on the next start.
package finishskill

import (
	_ "embed"
	"os"
	"path/filepath"
)

//go:embed SKILL.md
var skillMarkdown string

// SkillName is the directory name under ~/.claude/skills/ that holds the
// skill, and the slug agents invoke it by.
const SkillName = "finishing-an-openkanban-ticket"

// Markdown returns the embedded SKILL.md content.
func Markdown() string { return skillMarkdown }

// InstallPath is where the skill is written under the given home dir.
func InstallPath(home string) string {
	return filepath.Join(home, ".claude", "skills", SkillName, "SKILL.md")
}

// EnsureInstalled writes the embedded SKILL.md to ~/.claude/skills/<name>/
// when it is missing or differs from the embed, and reports whether it
// wrote. The repo embed is the source of truth (the skill is vendored
// into openkanban), so a differing global copy is overwritten — this is
// how skill edits propagate on update. Best-effort: callers should treat
// a non-nil error as non-fatal (the spawned agent can still finish work
// manually; the skill is a convenience, not a hard dependency).
func EnsureInstalled(home string) (wrote bool, err error) {
	if home == "" {
		return false, nil
	}
	dest := InstallPath(home)
	if existing, readErr := os.ReadFile(dest); readErr == nil && string(existing) == skillMarkdown {
		return false, nil // already current
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return false, err
	}
	// Atomic write: temp file in the same dir, then rename.
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, []byte(skillMarkdown), 0o644); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return false, err
	}
	return true, nil
}

// RequiredAgents lists the review/validation subagent roles the close-out
// skill leans on for its readiness self-evaluation. These are the agent
// names provided by the oh-my-claude plugin in this fork's environment.
//
// They are recommended, not required: the skill explicitly degrades to
// self-review when they're absent, so a missing agent only costs rigor,
// never correctness. The launch-time check (see cmd.warnMissingAgents...)
// surfaces a one-line, non-blocking hint when they can't be resolved.
func RequiredAgents() []string {
	return []string{"code-reviewer", "validator"}
}
