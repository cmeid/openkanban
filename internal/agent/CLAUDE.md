# Agent Package

Agent configuration, status detection, and spawn preparation.

## Agent Types

Configured in `config.json` under `agents` map:
- `claude`, `opencode`, `gemini`, `codex`, etc.
- Each has: `command`, `args`, `env`, `init_prompt`

## Session Detection

Find existing sessions to resume:
```go
FindOpencodeSession(workdir) string
FindGeminiSession(workdir) string
FindCodexSession(workdir) string
```

Returns session ID or empty string.

## Context Prompts

`BuildContextPrompt(promptTemplate string, data ContextData) string` renders Go templates:
```go
type ContextData struct {
    Title            string
    Description      string
    BranchName       string
    BaseBranch       string
    TicketID         string
    Status           string
    WorktreePath     string
    Slug             string
    HasBrief         bool
    BriefPath        string
    IsExternalResume bool
}
```

Use `NewContextData(ticket, briefRelPath, hasBrief, isExternalResume) ContextData` to construct.

`HasBrief` / `BriefPath` are populated by `MergeTicketBrief` at spawn time. The brief lives at `<worktree>/tickets/<slug>.md` and contains a managed block (`<!-- openkanban:card-notes ... -->`) carrying the openkanban card's Description.

Template in config: `"init_prompt": "Work on: {{.Title}}"`

## Status Detection

`StatusDetector` monitors agent state:
- File-based detection (marker files)
- API-based (HTTP to localhost)
- Terminal content parsing (keyword matching)

Keywords: "waiting", "thinking", "error", etc.

## OpenCode Server

Lifecycle management for opencode:
- `Start()` / `Stop()`
- `waitForReady()` with timeout
- HTTP client queries status API

## Claude Settings Persistence

`claude_settings.go` carries the seed-on-spawn / promote-on-review machinery that makes Claude Code's `"yes, and don't ask again"` approvals survive a ticket's worktree lifecycle.

- `SeedClaudeSettings(worktreePath, repoPath)` — called at every successful `CreateWorktree`. Merges `<repo>/.claude/settings.local.json` into the new `<worktree>/.claude/settings.local.json` (additively, worktree-local entries preserved). Writes a defensive `<repo>/.claude/.gitignore` if the repo's ignore stack doesn't already cover `.claude/`.
- `PromoteClaudeSettings(worktreePath, repoPath)` — reverse merge. Returns the slice of entries newly promoted into the repo file.
- `PromoteClaudeSettingsOnTransition(worktreePath, repoPath, oldStatus, newStatus)` — the policy gate. No-op unless `newStatus ∈ {in_review, done}` and `oldStatus != newStatus`. This is the **only** function status-mutating call sites should reach for.

Callers — keep them in sync if you add a new transition path:

- `project.TicketStore.Move` (single funnel for UI drag/drop, quickMove, quickMoveBackward via `GlobalTicketStore.Move`).
- `cmd.wrapUpSessionTicketAt` (CLI `ticket in-review` / `ticket done` — routes through `store.Move` rather than `ticket.SetStatus` directly so promotion fires).

Pure JSON-level merges — the helpers only touch `permissions.{allow,ask,deny}` and leave every other top-level key untouched. They do NOT claim to validate Claude Code's full settings schema; new top-level keys Claude adds in the future round-trip safely as long as they aren't named `permissions`.

Errors are non-fatal at all call sites: a settings-write failure logs and degrades to today's behavior (per-worktree allowlist), it never blocks a spawn or a status transition.

## Thread Safety

Use `sync.RWMutex` for cache access in StatusDetector.

## Anti-Patterns

- Don't hardcode agent commands - use config
- Don't block on session detection - return empty on timeout
- Don't skip mutex for shared cache
- Don't assume agent process exists - check first
