package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/agent"
	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
)

// envHas reports whether the SpawnReq env carries an exact KEY=VALUE
// entry. Returns false on any partial match so the test fails loudly
// if the daemon contract is ever weakened (e.g. "OPENKANBAN_SESSION="
// with no value).
func envHas(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

// argsContain reports whether the SpawnReq.Args slice contains the
// exact string. The respawn-notice positional is searched via prefix
// match (the message format includes the slug) so a separate helper
// argsHavePrefix exists for that case.
func argsContain(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func argsHavePrefix(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

// baseClaudeInputs returns a spawnReqInputs value seeded for a claude
// agent and a ticket that has previously been spawned (AgentSpawnedAt
// set), so the "resume" branches fire. Tests override `plan` and the
// ticket's AgentSpawnedAt as needed.
func baseClaudeInputs(t *testing.T, ticketID, branchName string) spawnReqInputs {
	t.Helper()
	now := time.Now()
	tk := &board.Ticket{
		ID:             board.TicketID(ticketID),
		Title:          "Make spawn daemon-aware",
		BranchName:     branchName,
		AgentSpawnedAt: &now, // not nil → resume path
	}
	return spawnReqInputs{
		ticket:       tk,
		plan:         spawnPlan{},
		sessionName:  branchName,
		command:      "claude",
		workdir:      "/tmp/wt",
		cols:         120,
		rows:         40,
		agentType:    "claude",
		cleanArgs:    []string{},
		isNewSession: false, // mirrors tk.AgentSpawnedAt != nil
		ctxData:      agent.NewContextData(tk, "", false, false),
	}
}

// TestBuildSpawnReq_NeverForks is the dynamic counterpart to the
// static grep guard at forksession_guard_test.go. Tabled over the
// cartesian {isNewSession ∈ {true, false}} × {AgentSessionID ∈ {empty,
// valid UUID}} × {spawn-plan branches}, asserts the assembled argv
// NEVER contains the divergent-fork flag literal. The grep guard
// catches re-introduction in source; this catches a hypothetical
// dynamic addition (e.g., conditional logic that builds the string
// from parts). The forbidden literal is reconstructed at runtime so
// this test file itself doesn't trip the grep guard.
func TestBuildSpawnReq_NeverForks(t *testing.T) {
	forbidden := "--fork" + "-session"
	uuid := "abcdefab-cdef-4abc-8def-abcdefabcdef"

	cases := []struct {
		name         string
		isNewSession bool
		hasUUID      bool
		plan         spawnPlan
	}{
		{"new + uuid + force-fresh", true, true, spawnPlan{ForceFresh: true}},
		{"new + uuid + plain", true, true, spawnPlan{}},
		{"new + no-uuid + plain", true, false, spawnPlan{}},
		{"resume + uuid + plain", false, true, spawnPlan{}},
		{"resume + uuid + inject-notice", false, true, spawnPlan{InjectResumeNotice: true}},
		{"resume + uuid + skip-merge", false, true, spawnPlan{SkipMerge: true}},
		{"resume + no-uuid + plain", false, false, spawnPlan{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseClaudeInputs(t, "TICK-"+tc.name, "task/x")
			in.isNewSession = tc.isNewSession
			if tc.hasUUID {
				in.ticket.AgentSessionID = uuid
			}
			in.plan = tc.plan
			req := buildSpawnReq(in)
			for _, a := range req.Args {
				if strings.Contains(a, forbidden) {
					t.Errorf("Args[%q] contains forbidden literal %q (table row %s, full Args: %v)",
						a, forbidden, tc.name, req.Args)
				}
			}
		})
	}
}

// TestBuildSpawnReq_ForceFresh_FullPrime asserts the Discard branch:
// the caller has cleared AgentSpawnedAt before invoking, so
// buildSpawnReq treats this as a fresh spawn. argv must NOT contain
// --continue and must NOT carry the resume-notice positional.
func TestBuildSpawnReq_ForceFresh_FullPrime(t *testing.T) {
	in := baseClaudeInputs(t, "TICK-1", "task/foo")
	in.ticket.AgentSpawnedAt = nil // ForceFresh contract: caller already cleared it
	in.isNewSession = true
	in.plan = spawnPlan{ForceFresh: true}

	req := buildSpawnReq(in)

	if req.TicketID != "TICK-1" {
		t.Errorf("TicketID = %q, want %q", req.TicketID, "TICK-1")
	}
	if argsContain(req.Args, "--continue") {
		t.Errorf("ForceFresh argv must NOT contain --continue, got %v", req.Args)
	}
	if argsHavePrefix(req.Args, "Brief updated at tickets/") {
		t.Errorf("ForceFresh argv must NOT contain resume notice positional, got %v", req.Args)
	}
	// Sanity: -n flag with the ticket title should be present for new claude sessions
	if !argsContain(req.Args, "-n") {
		t.Errorf("ForceFresh argv expected -n flag for new claude session, got %v", req.Args)
	}
}

// TestBuildSpawnReq_InjectResumeNotice_AppendsPositional asserts the
// Resume branch: AgentSpawnedAt is still set, and the plan asks for
// the resume notice positional to be appended after --continue.
func TestBuildSpawnReq_InjectResumeNotice_AppendsPositional(t *testing.T) {
	in := baseClaudeInputs(t, "TICK-2", "task/bar")
	in.plan = spawnPlan{InjectResumeNotice: true}

	req := buildSpawnReq(in)

	if !argsContain(req.Args, "--continue") {
		t.Errorf("InjectResumeNotice argv must contain --continue, got %v", req.Args)
	}
	if !argsHavePrefix(req.Args, "Brief updated at tickets/bar.md") {
		t.Errorf("InjectResumeNotice argv must contain resume notice positional, got %v", req.Args)
	}
	// Order: --continue must precede the positional.
	contIdx, posIdx := -1, -1
	for i, a := range req.Args {
		if a == "--continue" {
			contIdx = i
		}
		if strings.HasPrefix(a, "Brief updated at tickets/") {
			posIdx = i
		}
	}
	if contIdx == -1 || posIdx == -1 || posIdx < contIdx {
		t.Errorf("expected --continue at idx<%d, got args=%v", posIdx, req.Args)
	}
}

// TestBuildSpawnReq_SkipMerge_NoResumeNotice asserts the Skip branch:
// --continue is appended (this is still a resume), but the resume-
// notice positional is NOT added — the user opted not to surface a
// "brief updated" notice to the agent.
func TestBuildSpawnReq_SkipMerge_NoResumeNotice(t *testing.T) {
	in := baseClaudeInputs(t, "TICK-3", "task/baz")
	in.plan = spawnPlan{SkipMerge: true}

	req := buildSpawnReq(in)

	if !argsContain(req.Args, "--continue") {
		t.Errorf("SkipMerge argv must contain --continue (still a resume), got %v", req.Args)
	}
	if argsHavePrefix(req.Args, "Brief updated at tickets/") {
		t.Errorf("SkipMerge argv must NOT contain resume notice positional, got %v", req.Args)
	}
}

// TestBuildSpawnReq_EnvCarriesSessionAndTicketID asserts the env-var
// contract from T2: every SpawnReq must carry both OPENKANBAN_SESSION
// and OPENKANBAN_TICKET_ID so the spawned agent's `openkanban ticket
// done` invocation can resolve back to its ticket. Runs across all
// three spawnPlan branches to confirm the contract holds regardless
// of plan.
func TestBuildSpawnReq_EnvCarriesSessionAndTicketID(t *testing.T) {
	cases := []struct {
		name string
		plan spawnPlan
	}{
		{"default", spawnPlan{}},
		{"force_fresh", spawnPlan{ForceFresh: true}},
		{"inject_resume_notice", spawnPlan{InjectResumeNotice: true}},
		{"skip_merge", spawnPlan{SkipMerge: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseClaudeInputs(t, "TICK-ENV", "task/env")
			if tc.plan.ForceFresh {
				in.ticket.AgentSpawnedAt = nil
				in.isNewSession = true
			}
			in.plan = tc.plan

			req := buildSpawnReq(in)

			if !envHas(req.Env, "OPENKANBAN_SESSION=task/env") {
				t.Errorf("env missing OPENKANBAN_SESSION=task/env, got %v", req.Env)
			}
			if !envHas(req.Env, "OPENKANBAN_TICKET_ID=TICK-ENV") {
				t.Errorf("env missing OPENKANBAN_TICKET_ID=TICK-ENV, got %v", req.Env)
			}
			// Wire-level contract: TicketID + SessionName are also
			// carried as top-level SpawnReq fields so the daemon's
			// terminal.Pane.SetSessionName / SetTicketID path can
			// synthesize env independently. The two must agree.
			if req.TicketID != "TICK-ENV" {
				t.Errorf("SpawnReq.TicketID = %q, want TICK-ENV", req.TicketID)
			}
			if req.SessionName != "task/env" {
				t.Errorf("SpawnReq.SessionName = %q, want task/env", req.SessionName)
			}
		})
	}
}

// TestBuildSpawnReq_InjectsAgentEnv pins the per-agent Env injection — the
// mechanism that lets a "Claude (Custom)" preset carry CLAUDE_CONFIG_DIR so a
// project launches a different Claude profile. A leading "~/" must be expanded
// to $HOME (env vars are not shell-expanded). Red-before-green: revert the
// agentEnv loop in buildSpawnReq and the custom-preset case fails.
func TestBuildSpawnReq_InjectsAgentEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Run("custom_preset_injects_expanded_config_dir", func(t *testing.T) {
		in := baseClaudeInputs(t, "TICK-CFGDIR", "task/cfgdir")
		in.agentEnv = map[string]string{"CLAUDE_CONFIG_DIR": "~/.claude-personal"}

		req := buildSpawnReq(in)

		want := "CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude-personal")
		if !envHas(req.Env, want) {
			t.Errorf("env missing %q, got %v", want, req.Env)
		}
	})

	t.Run("default_preset_no_config_dir", func(t *testing.T) {
		in := baseClaudeInputs(t, "TICK-NOCFG", "task/nocfg")
		// The "Claude (Default)" preset carries no Env.

		req := buildSpawnReq(in)

		for _, e := range req.Env {
			if strings.HasPrefix(e, "CLAUDE_CONFIG_DIR=") {
				t.Errorf("unexpected CLAUDE_CONFIG_DIR in env: %v", req.Env)
			}
		}
	})
}

// TestBuildSpawnReq_LeanPresetReachesSpawn is the authoritative test for the
// claude-lean preset: it feeds the SHIPPED preset's Env+Args through
// buildSpawnReq and asserts the token-optimizing knobs actually reach the
// SpawnReq the daemon executes — not merely that the config map contains the
// key (a config-only assertion passes vacuously even if the wiring breaks).
// Red-before-green: strip any of CLAUDE_CONFIG_DIR / CLAUDE_CODE_DISABLE_AUTO_MEMORY
// / --strict-mcp-config from the preset in config.go and the matching sub-check
// fails. See docs/TOKEN_OPTIMIZATION.md.
func TestBuildSpawnReq_LeanPresetReachesSpawn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	lean, ok := config.DefaultConfig().Agents["claude-lean"]
	if !ok {
		t.Fatal("claude-lean preset missing from default agents")
	}
	if lean.Command != "claude" {
		t.Fatalf("claude-lean Command = %q, want \"claude\" (must inherit Claude-class spawn behavior via basename switch)", lean.Command)
	}

	// Mirror model.go's cleanArgs derivation (strip empty entries).
	cleanArgs := make([]string, 0, len(lean.Args))
	for _, a := range lean.Args {
		if strings.TrimSpace(a) != "" {
			cleanArgs = append(cleanArgs, a)
		}
	}

	in := baseClaudeInputs(t, "TICK-LEAN", "task/lean")
	in.cleanArgs = cleanArgs
	in.agentEnv = lean.Env

	req := buildSpawnReq(in)

	wantCfgDir := "CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude-lean")
	if !envHas(req.Env, wantCfgDir) {
		t.Errorf("lean env missing %q, got %v", wantCfgDir, req.Env)
	}
	if !envHas(req.Env, "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1") {
		t.Errorf("lean env missing CLAUDE_CODE_DISABLE_AUTO_MEMORY=1, got %v", req.Env)
	}
	if !argsContain(req.Args, "--strict-mcp-config") {
		t.Errorf("lean args missing --strict-mcp-config, got %v", req.Args)
	}
	if !argsContain(req.Args, "--exclude-dynamic-system-prompt-sections") {
		t.Errorf("lean args missing --exclude-dynamic-system-prompt-sections, got %v", req.Args)
	}
}

// TestBuildSpawnReq_ForceFresh_AgentSpawnedAtNilAtConstruction asserts
// the ForceFresh invariant: by the time buildSpawnReq runs, the
// caller (the Discard option's Fn closure in spawnAgent) must have
// already nilled ticket.AgentSpawnedAt and saved the ticket — otherwise
// the daemon's fresh-spawn branch wouldn't fire and the prompt would
// carry "this is a resume" framing.
//
// This test pins the contract at the helper level: in.ticket.AgentSpawnedAt
// must be nil AND in.isNewSession must be true when plan.ForceFresh is set,
// so isNewSession-gated argv (the -n flag, the prompt) is constructed
// correctly.
func TestBuildSpawnReq_ForceFresh_AgentSpawnedAtNilAtConstruction(t *testing.T) {
	in := baseClaudeInputs(t, "TICK-FRESH", "task/fresh")
	// Simulate the modal Fn closure having cleared AgentSpawnedAt + saved.
	in.ticket.AgentSpawnedAt = nil
	in.isNewSession = true
	in.plan = spawnPlan{ForceFresh: true}

	req := buildSpawnReq(in)

	if in.ticket.AgentSpawnedAt != nil {
		t.Fatalf("test setup invariant violated: AgentSpawnedAt must be nil for ForceFresh")
	}
	// argv shape should reflect "new session", not resume.
	if argsContain(req.Args, "--continue") || argsContain(req.Args, "-c") {
		t.Errorf("ForceFresh argv reflects resume despite AgentSpawnedAt=nil: %v", req.Args)
	}
}

// argsHavePair reports whether args contains `flag` immediately
// followed by `value` (space-separated CLI pair).
func argsHavePair(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// TestBuildSpawnReq_NewClaude_StartsInPlanMode pins the ticket: every
// new claude session must be launched with --permission-mode plan so
// the agent reviews the work before touching the tree. The default
// agent config carries --dangerously-skip-permissions; that flag
// conflicts with plan mode and must be filtered out on new sessions.
func TestBuildSpawnReq_NewClaude_StartsInPlanMode(t *testing.T) {
	in := baseClaudeInputs(t, "TICK-PLAN", "task/plan")
	in.ticket.AgentSpawnedAt = nil
	in.isNewSession = true
	in.cleanArgs = []string{"--dangerously-skip-permissions"}

	req := buildSpawnReq(in)

	if !argsHavePair(req.Args, "--permission-mode", "plan") {
		t.Errorf("new claude argv missing --permission-mode plan, got %v", req.Args)
	}
	if argsContain(req.Args, "--dangerously-skip-permissions") {
		t.Errorf("new claude argv must NOT contain --dangerously-skip-permissions (conflicts with plan mode), got %v", req.Args)
	}
}

// TestBuildSpawnReq_NewClaude_StripsExistingPermissionMode asserts the
// user's pre-existing --permission-mode <X> pair (e.g. acceptEdits) is
// replaced by --permission-mode plan on new sessions. The ticket
// guarantees plan mode unconditionally on new spawn.
func TestBuildSpawnReq_NewClaude_StripsExistingPermissionMode(t *testing.T) {
	in := baseClaudeInputs(t, "TICK-PLAN-OVERRIDE", "task/override")
	in.ticket.AgentSpawnedAt = nil
	in.isNewSession = true
	in.cleanArgs = []string{"--permission-mode", "acceptEdits"}

	req := buildSpawnReq(in)

	if argsHavePair(req.Args, "--permission-mode", "acceptEdits") {
		t.Errorf("user-configured permission-mode acceptEdits must be stripped on new session, got %v", req.Args)
	}
	if !argsHavePair(req.Args, "--permission-mode", "plan") {
		t.Errorf("new claude argv missing --permission-mode plan after stripping override, got %v", req.Args)
	}
	// Only one --permission-mode flag should remain.
	count := 0
	for _, a := range req.Args {
		if a == "--permission-mode" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one --permission-mode flag, got %d in %v", count, req.Args)
	}
}

// TestBuildSpawnReq_ResumeClaude_NoPlanMode pins that resumed claude
// sessions are not forced into plan mode — only new sessions get the
// flag. Resumes pick up wherever the user left off (the ticket title
// is "start all NEW sessions in plan mode", explicit on new).
func TestBuildSpawnReq_ResumeClaude_NoPlanMode(t *testing.T) {
	in := baseClaudeInputs(t, "TICK-RESUME", "task/resume")
	// baseClaudeInputs already sets AgentSpawnedAt != nil and isNewSession=false.
	in.cleanArgs = []string{"--dangerously-skip-permissions"}

	req := buildSpawnReq(in)

	if argsHavePair(req.Args, "--permission-mode", "plan") {
		t.Errorf("resumed claude argv must NOT carry --permission-mode plan, got %v", req.Args)
	}
	// And the user's resume-time args (incl. --dangerously-skip-permissions)
	// pass through untouched — only new-session argv is rewritten.
	if !argsContain(req.Args, "--dangerously-skip-permissions") {
		t.Errorf("resumed argv must preserve user's --dangerously-skip-permissions, got %v", req.Args)
	}
}

// TestResolveBrief_SkipMergePreservesBytes pins the SkipMerge brief
// contract: the file's bytes (and mtime) on disk are not touched by
// the spawn flow when the user picks option 'n'. The agent then sees
// whatever brief was on disk before — including any manual edits.
func TestResolveBrief_SkipMergePreservesBytes(t *testing.T) {
	worktree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(worktree, "tickets"), 0o755); err != nil {
		t.Fatal(err)
	}
	briefPath := filepath.Join(worktree, "tickets", "preserve.md")
	original := []byte("# Manual edits\n\nUser-curated content the agent must not touch.\n")
	if err := os.WriteFile(briefPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	// Backdate mtime so any rewrite would be detectable.
	past := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(briefPath, past, past); err != nil {
		t.Fatal(err)
	}
	beforeStat, err := os.Stat(briefPath)
	if err != nil {
		t.Fatal(err)
	}

	ticket := &board.Ticket{
		ID:          "TICK-PRESERVE",
		Title:       "Should not be merged",
		Description: "If SkipMerge respects the user, this never lands in the brief.",
		BranchName:  "task/preserve",
	}

	rel, hasBrief, err := resolveBrief(ticket, worktree, spawnPlan{SkipMerge: true})
	if err != nil {
		t.Fatalf("resolveBrief returned err: %v", err)
	}
	if !hasBrief {
		t.Errorf("hasBrief = false, want true (the file exists)")
	}
	if rel != "tickets/preserve.md" {
		t.Errorf("rel = %q, want %q", rel, "tickets/preserve.md")
	}

	got, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Errorf("brief bytes were modified under SkipMerge:\nbefore: %q\nafter:  %q", original, got)
	}
	afterStat, err := os.Stat(briefPath)
	if err != nil {
		t.Fatal(err)
	}
	if !afterStat.ModTime().Equal(beforeStat.ModTime()) {
		t.Errorf("brief mtime changed under SkipMerge: before=%v after=%v",
			beforeStat.ModTime(), afterStat.ModTime())
	}
}

// TestResolveBrief_DefaultPlanWritesBrief is the negative complement:
// when SkipMerge is false (default / ForceFresh / InjectResumeNotice),
// the brief file IS rewritten to reflect the ticket description. This
// pins that the SkipMerge gate is the only thing preventing the merge
// — no other plan accidentally suppresses it.
func TestResolveBrief_DefaultPlanWritesBrief(t *testing.T) {
	worktree := t.TempDir()
	ticket := &board.Ticket{
		ID:          "TICK-WRITE",
		Title:       "Card description should propagate",
		Description: "fresh contents from the card",
		BranchName:  "task/write",
	}

	rel, hasBrief, err := resolveBrief(ticket, worktree, spawnPlan{})
	if err != nil {
		t.Fatalf("resolveBrief returned err: %v", err)
	}
	if rel != "tickets/write.md" {
		t.Errorf("rel = %q, want tickets/write.md", rel)
	}
	if !hasBrief {
		t.Errorf("hasBrief = false, want true")
	}
	fullPath := filepath.Join(worktree, "tickets", "write.md")
	body, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("expected brief to be written at %s: %v", fullPath, err)
	}
	if !strings.Contains(string(body), "fresh contents from the card") {
		t.Errorf("brief body missing card description; got: %s", body)
	}
}

// TestResolveBrief_SkipMergeNoFileReportsAbsent: when SkipMerge is on
// and the brief file doesn't yet exist, hasBrief must be false (the
// stat-failure branch) — buildSpawnReq's prompt template relies on
// {{.HasBrief}} being accurate for the conditional preamble.
func TestResolveBrief_SkipMergeNoFileReportsAbsent(t *testing.T) {
	worktree := t.TempDir()
	ticket := &board.Ticket{
		ID:          "TICK-MISSING",
		Title:       "No brief on disk",
		Description: "non-empty so MergeTicketBrief would write — but SkipMerge blocks it",
		BranchName:  "task/missing",
	}

	rel, hasBrief, err := resolveBrief(ticket, worktree, spawnPlan{SkipMerge: true})
	if err != nil {
		t.Fatalf("resolveBrief returned err: %v", err)
	}
	if hasBrief {
		t.Errorf("hasBrief = true, want false (file does not exist on disk)")
	}
	if rel != "" {
		t.Errorf("rel = %q, want \"\" when file absent", rel)
	}
	// And the file must NOT have been written.
	fullPath := filepath.Join(worktree, "tickets", "missing.md")
	if _, statErr := os.Stat(fullPath); statErr == nil {
		t.Errorf("brief file %s was created despite SkipMerge", fullPath)
	}
}

// TestBuildSpawnReq_ResumeUUID_PreferredOverContinue asserts the
// deterministic-resume contract: when a re-spawn happens for a claude
// ticket whose AgentSessionID has been back-filled with a valid UUID,
// argv carries `--resume <uuid>` instead of the positional `--continue`
// heuristic. This is what closes the "ForceFresh-then-re-spawn picks
// the wrong journal" failure mode `--continue` has.
func TestBuildSpawnReq_ResumeUUID_PreferredOverContinue(t *testing.T) {
	const uuid = "7f3a9b2c-1d8e-4a5b-9c3d-2f1e0a8b9c4d"

	in := baseClaudeInputs(t, "TICK-UUID", "task/uuid")
	in.ticket.AgentSessionID = uuid

	req := buildSpawnReq(in)

	if !argsHavePair(req.Args, "--resume", uuid) {
		t.Errorf("argv must contain --resume %s pair, got %v", uuid, req.Args)
	}
	if argsContain(req.Args, "--continue") {
		t.Errorf("argv must NOT contain --continue when --resume <uuid> is present, got %v", req.Args)
	}
}

// TestBuildSpawnReq_ResumeUUID_WithInjectResumeNotice asserts the
// InjectResumeNotice positional still lands AFTER --resume <uuid>,
// not after a non-existent --continue. Pins the order so the notice
// is parsed as the first new user turn by claude.
func TestBuildSpawnReq_ResumeUUID_WithInjectResumeNotice(t *testing.T) {
	const uuid = "7f3a9b2c-1d8e-4a5b-9c3d-2f1e0a8b9c4d"

	in := baseClaudeInputs(t, "TICK-UUID-NOTICE", "task/notice")
	in.ticket.AgentSessionID = uuid
	in.plan = spawnPlan{InjectResumeNotice: true}

	req := buildSpawnReq(in)

	if !argsHavePair(req.Args, "--resume", uuid) {
		t.Errorf("argv must contain --resume %s pair, got %v", uuid, req.Args)
	}
	resumeIdx, posIdx := -1, -1
	for i, a := range req.Args {
		if a == "--resume" {
			resumeIdx = i
		}
		if strings.HasPrefix(a, "Brief updated at tickets/") {
			posIdx = i
		}
	}
	if resumeIdx == -1 || posIdx == -1 || posIdx <= resumeIdx+1 {
		// posIdx must be at least resumeIdx + 2 (skip the UUID value)
		t.Errorf("expected resume-notice positional after --resume <uuid>, got args=%v", req.Args)
	}
}

// TestBuildSpawnReq_NoAgentSessionID_FallsBackToContinue asserts the
// graceful-degradation contract for tickets predating the back-fill
// (Task 1/2): if AgentSessionID isn't populated yet, claude still
// resumes via --continue (the legacy heuristic). Newer status-poll
// ticks will populate AgentSessionID and subsequent re-spawns will
// upgrade to --resume <uuid>.
func TestBuildSpawnReq_NoAgentSessionID_FallsBackToContinue(t *testing.T) {
	in := baseClaudeInputs(t, "TICK-LEGACY", "task/legacy")
	// AgentSessionID intentionally left empty.

	req := buildSpawnReq(in)

	if !argsContain(req.Args, "--continue") {
		t.Errorf("argv must fall back to --continue when AgentSessionID is empty, got %v", req.Args)
	}
	if argsContain(req.Args, "--resume") {
		t.Errorf("argv must NOT contain --resume when AgentSessionID is empty, got %v", req.Args)
	}
}

// TestBuildSpawnReq_InvalidAgentSessionID_FallsBackToContinue asserts
// defensive validation: a malformed UUID (e.g. an opencode-style ref
// accidentally stamped into AgentSessionID, or a corrupted value) must
// not be passed as --resume. Falls back to --continue.
func TestBuildSpawnReq_InvalidAgentSessionID_FallsBackToContinue(t *testing.T) {
	in := baseClaudeInputs(t, "TICK-BAD", "task/bad")
	in.ticket.AgentSessionID = "not-a-uuid"

	req := buildSpawnReq(in)

	if !argsContain(req.Args, "--continue") {
		t.Errorf("argv must fall back to --continue for invalid AgentSessionID, got %v", req.Args)
	}
	if argsContain(req.Args, "--resume") {
		t.Errorf("argv must NOT pass invalid AgentSessionID as --resume, got %v", req.Args)
	}
}

// TestBuildSpawnReq_DisablesPromptSuggestionForClaude pins the fix for
// "remove the suggestions that are inserted into claude's input box": every
// claude-class spawn (new or resumed) must carry
// CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=false in SpawnReq.Env so Claude Code's
// model-generated next-prompt drafts never appear. Non-claude agents must NOT
// get the CLAUDE_* var. (buildSpawnReq takes agentType already-resolved to the
// command basename — see model.go's agentType = filepath.Base(agentCfg.Command);
// this test exercises the gate, not that derivation.)
func TestBuildSpawnReq_DisablesPromptSuggestionForClaude(t *testing.T) {
	const want = "CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=false"

	t.Run("claude_resume", func(t *testing.T) {
		req := buildSpawnReq(baseClaudeInputs(t, "TICK-PS-R", "task/ps-r"))
		if !envHas(req.Env, want) {
			t.Errorf("resumed claude spawn must carry %q, got env=%v", want, req.Env)
		}
	})

	t.Run("claude_new_session", func(t *testing.T) {
		in := baseClaudeInputs(t, "TICK-PS-N", "task/ps-n")
		in.isNewSession = true
		req := buildSpawnReq(in)
		if !envHas(req.Env, want) {
			t.Errorf("new claude spawn must carry %q, got env=%v", want, req.Env)
		}
	})

	t.Run("non_claude_excluded", func(t *testing.T) {
		in := baseClaudeInputs(t, "TICK-PS-G", "task/ps-g")
		in.agentType = "gemini"
		in.command = "gemini"
		req := buildSpawnReq(in)
		if envHas(req.Env, want) {
			t.Errorf("non-claude spawn must NOT carry %q, got env=%v", want, req.Env)
		}
	})
}

// TestBuildSpawnReq_UserConfigContinue_NotOverridden asserts the user-
// config escape hatch: if --continue or --resume is already in the
// caller's cleanArgs (from a user's agent config), buildSpawnReq must
// NOT add another resume flag. Today's hasFlag guard at the !isNewSession
// branch handles this; the test pins it across both --continue and
// --resume preset variants.
func TestBuildSpawnReq_UserConfigContinue_NotOverridden(t *testing.T) {
	const uuid = "7f3a9b2c-1d8e-4a5b-9c3d-2f1e0a8b9c4d"

	t.Run("user_preset_continue", func(t *testing.T) {
		in := baseClaudeInputs(t, "TICK-PRESET-C", "task/preset-c")
		in.ticket.AgentSessionID = uuid // would otherwise trigger --resume
		in.cleanArgs = []string{"--continue"}

		req := buildSpawnReq(in)

		// Exactly one --continue, no --resume.
		var continueCount, resumeCount int
		for _, a := range req.Args {
			if a == "--continue" {
				continueCount++
			}
			if a == "--resume" {
				resumeCount++
			}
		}
		if continueCount != 1 {
			t.Errorf("--continue count = %d, want 1 (preset preserved), args=%v", continueCount, req.Args)
		}
		if resumeCount != 0 {
			t.Errorf("--resume must NOT be added when preset contains --continue, args=%v", req.Args)
		}
	})

	t.Run("user_preset_resume", func(t *testing.T) {
		in := baseClaudeInputs(t, "TICK-PRESET-R", "task/preset-r")
		in.ticket.AgentSessionID = uuid
		// Simulate a user who hand-pinned --resume <other-uuid> in their config.
		in.cleanArgs = []string{"--resume", "11111111-1111-1111-1111-111111111111"}

		req := buildSpawnReq(in)

		// Exactly one --resume (theirs), no auto-added --resume or --continue.
		var continueCount, resumeCount int
		for _, a := range req.Args {
			if a == "--continue" {
				continueCount++
			}
			if a == "--resume" {
				resumeCount++
			}
		}
		if resumeCount != 1 {
			t.Errorf("--resume count = %d, want 1 (preset preserved), args=%v", resumeCount, req.Args)
		}
		if continueCount != 0 {
			t.Errorf("--continue must NOT be added when preset contains --resume, args=%v", req.Args)
		}
	})
}

// TestBuildSpawnReq_Model pins that the per-project model override is threaded
// into the claude argv as --model <value> in both new and resume arms, and is
// absent when empty or when the agent is not claude.
func TestBuildSpawnReq_Model(t *testing.T) {
	t.Run("new_session_model_present", func(t *testing.T) {
		in := baseClaudeInputs(t, "TICK-MODEL-NEW", "task/model-new")
		in.isNewSession = true
		in.model = "opus"
		req := buildSpawnReq(in)
		if !argsHavePair(req.Args, "--model", "opus") {
			t.Errorf("new session: expected --model opus in argv, got %v", req.Args)
		}
	})

	t.Run("resume_model_present", func(t *testing.T) {
		in := baseClaudeInputs(t, "TICK-MODEL-RESUME", "task/model-resume")
		in.isNewSession = false
		in.model = "opus"
		req := buildSpawnReq(in)
		if !argsHavePair(req.Args, "--model", "opus") {
			t.Errorf("resume: expected --model opus in argv, got %v", req.Args)
		}
	})

	t.Run("new_session_model_absent_when_empty", func(t *testing.T) {
		in := baseClaudeInputs(t, "TICK-MODEL-EMPTY", "task/model-empty")
		in.isNewSession = true
		in.model = ""
		req := buildSpawnReq(in)
		if argsContain(req.Args, "--model") {
			t.Errorf("new session: --model must be absent when model is empty, got %v", req.Args)
		}
	})

	t.Run("opencode_model_ignored", func(t *testing.T) {
		in := baseClaudeInputs(t, "TICK-MODEL-OC", "task/model-oc")
		in.agentType = "opencode"
		in.command = "opencode"
		in.model = "opus"
		req := buildSpawnReq(in)
		if argsContain(req.Args, "--model") {
			t.Errorf("opencode: --model must be absent for non-claude agents, got %v", req.Args)
		}
	})
}
