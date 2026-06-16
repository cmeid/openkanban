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

## Thread Safety

Use `sync.RWMutex` for cache access in StatusDetector.

## Anti-Patterns

- Don't hardcode agent commands - use config
- Don't block on session detection - return empty on timeout
- Don't skip mutex for shared cache
- Don't assume agent process exists - check first
