# Agent Package

Agent configuration, status detection, and spawn preparation.

## Agent Types

Configured in `config.json` under `agents` map:
- `claude`, `opencode`, `gemini`, `codex`, etc.
- Each has: `command`, `args`, `env`, `init_prompt`

## Session Detection

Find existing sessions to resume:
```go
FindClaudeSession(workdir) string   // freshest LIVE .jsonl UUID in ~/.claude/projects/<encoded-cwd>/
FindOpencodeSession(workdir) string
FindGeminiSession(workdir) string
FindCodexSession(workdir) string
```

Returns session ID or empty string. No error path — callers retry on the next status-poll tick. `FindClaudeSession` additionally gates on the `.jsonl` having at least one real assistant turn so the UI doesn't back-fill an abandoned UUID into `Ticket.AgentSessionID`.

Four parallel funcs by design — see `feedback_openkanban_no_premature_service_abstraction`; no `SessionDiscoverer` interface until a 5th caller exists.

### Claude history.jsonl purge

`PurgeClaudePrimingHistory(historyPath, uuid, prefixes...)` rewrites `~/.claude/history.jsonl` to drop entries whose `sessionId == uuid` AND whose `display` starts with one of the given prefixes. Atomic via temp file + `os.Rename`. Refuses to wildcard-purge: empty uuid OR empty prefixes returns nil.

Why this exists: openkanban delivers the priming prompt to claude as a positional argv argument (`internal/ui/model.go` at the `args = append(args, prompt)` line in the claude `case`). Claude Code records that argv prompt to `history.jsonl` as a normal up-arrow recall entry. Claude Code's input ring then surfaces it on `↑` instead of the user's real recent prompts — both for live sessions and after `DeleteClaudeSession` deletes the transcript and leaves the history entry orphaned.

Two call sites:
- `DeleteClaudeSession(sessionPath)` — derives the UUID from the basename, calls the purge after `os.Remove`. Catches the orphan case (transcript gone, history.jsonl entry would otherwise leak forever).
- `pollAgentStatusesAsync`'s claude back-fill branch (`internal/ui/model.go` near the `FindClaudeSession` call) — runs once per ticket the first time the UUID is back-filled. Catches the live-session case (session is fine but its priming would otherwise dominate the ring until deletion).

`ClaudePrimingPrefixes` holds the two template-leading sentences (fresh-spawn + external-resume). `TestClaudePrimingPrefixes_MatchTemplate` renders both branches and asserts the constants stay in sync — guards against drift if `agent_prompt.tmpl` is later edited.

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

### Review-and-prune

`ReviewAndPruneRepoSettings(repoPath)` runs after Promote on every `TicketStore.Move`. It walks `permissions.allow` of the repo file and removes noise `Bash(...)` entries — hard-deny verbs (`git push`, `gh pr create`, `op`, `sudo`, `aws`, etc.), hard-deny paths (`/.ssh/`, `/.aws/`, …), escape-soup (3+ backslashes), untrusted absolute paths (anything outside the workspace allowlist), and the long-no-glob catch-all (length > 30 with no `*`/`**`/`./...`). Skill / Read / Agent entries pass through.

The **idempotency contract** is load-bearing: if no entries would change, the function returns `(nil, nil)` without snapshotting, writing, or appending to `.pruned-log`. Repeated same-status transitions and already-clean files cost only a single read. Tests pin this with table-driven idempotency rows (`prune(prune(input)) == prune(input)` per row).

Tilde resolution lives in two `sync.Once`-memoized package globals (`getResolvedHome`, `resolvedPathAllowlist`). If `os.UserHomeDir()` returns an error, the path allowlist degrades to only `/tmp/`, `/private/tmp/`, `/var/folders/` — fail-closed, prune anything else under common home roots.

Recovery surfaces (both gitignored via the inner `.gitignore`):

- `<repo>/.claude/.pruned-log` — append-only, one RFC3339-stamped line per removal (`<ts> <reason> <entry>`).
- `<repo>/.claude/settings.local.json.bak.<unix-nanos>` — snapshot of pre-prune state. Rotation keeps the 3 most recent (sort by suffix; nanos suffix prevents same-second collisions).

**No verb-widening.** Widening `Bash(<verb> <args>)` to `Bash(<verb> *)` is deliberately out of scope — it would collide with the global push-gate rule and the secret-management surface, and the safe-to-widen verbs are too few to justify the complexity. Users who want broader entries hand-edit the repo file.

## Thread Safety

Use `sync.RWMutex` for cache access in StatusDetector.

## Anti-Patterns

- Don't hardcode agent commands - use config
- Don't block on session detection - return empty on timeout
- Don't skip mutex for shared cache
- Don't assume agent process exists - check first
