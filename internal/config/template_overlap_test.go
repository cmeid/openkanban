package config

import "testing"

// TestDefaultTemplateNoRuleOverlap guards that the shipped agent prompt
// template never trips validateInitPromptOverlap. The template is wired in
// as every agent's default init_prompt (see defaultAgents), so it IS run
// through this validator on config load — a regression here would print a
// spurious "restated global rule" warning on every launch / `config
// validate`.
//
// Two independent guarantees, matching the validator's two stages:
//   - Stage 1 (strong markers): the template must contain none of the
//     HARD RULE / NEVER gh pr create|git push / "global rule" patterns.
//   - Stage 2 (H2 overlap): no H2 heading in the template may carry a
//     globalRuleKeyword. If none do, the template can never overlap a
//     user's global CLAUDE.md section regardless of what that file holds.
func TestDefaultTemplateNoRuleOverlap(t *testing.T) {
	tmpl := DefaultAgentPrompt()
	if tmpl == "" {
		t.Fatal("DefaultAgentPrompt returned empty string")
	}

	// Stage 1: strong-marker scan runs even with no global file.
	if w := validateInitPromptOverlap(tmpl, ""); len(w) != 0 {
		t.Errorf("shipped template trips strong rule markers: %v", w)
	}

	// Stage 2: no template H2 header carries a rule keyword. We test each
	// header against itself: sharedRuleKeyword(h, h) is non-empty exactly
	// when h contains a globalRuleKeyword, which is the precondition for
	// any overlap warning against a real CLAUDE.md.
	for _, h := range extractH2Headers(tmpl) {
		if kw := sharedRuleKeyword(h, h); kw != "" {
			t.Errorf("template H2 header %q contains rule keyword %q — "+
				"it can overlap a global CLAUDE.md section; reword the heading",
				h, kw)
		}
	}
}
