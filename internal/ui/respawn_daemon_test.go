package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/agent"
	"github.com/techdufus/openkanban/internal/board"
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
