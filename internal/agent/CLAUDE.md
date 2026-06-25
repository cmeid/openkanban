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

### Session bucket normalization

`claude --resume <uuid>` is scoped to the project bucket of the *launch* cwd (and that repo's worktrees) — there is no flag to resume a session filed under a different cwd. `NormalizeSessionBucket(uuid, worktreePath)` makes resume directory-independent by relocating `<uuid>.jsonl` + its sibling `<uuid>/` artifact dir into `ProjectDirFor(worktreePath)` before spawn (called from `prepareSpawnWith` once the worktree path is finalized). Idempotent (no-op when already in the right bucket or no transcript), skips sessions a live process holds open (`SessionActive`/`lsof`), refuses same-UUID collisions, moves the `.jsonl` lookup key last, non-fatal at the call site. `ProjectDirFor(path)` is the shared bucket encoder — `EncodeClaudeBucket`: per-char `[^A-Za-z0-9]`→`-`, mirroring the Claude CLI as of 2.1.177, which now maps `_`, `.`, space, etc. to `-` (not just `/`; this drift silently broke resume of underscore-path worktrees until matched — see memory `reference_openkanban_claude_bucket_encoding_drift`). Also used by `latestClaudeJSONL`. `ResumeResolvable(uuid, worktreePath)` reports whether `--resume` would find the transcript in the launch bucket; `prepareSpawnWith` surfaces a visible notice when it can't, instead of a silent ~2s "No conversation found" exit. Only foreign/linked sessions move; openkanban-created sessions already start in the worktree. Full write-up: README "Changes vs upstream" §10 and `docs/AGENT_INTEGRATION.md` "Session Linking on Ticket Creation".

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

### Brief concurrency contract

- The **store** `ticket.Description` is the source of truth; the brief is a **one-way generated view** (store → brief, never brief → store).
- `MergeTicketBrief` rewrites **only** the managed-block fences (`upsertManagedBlock`). Content outside the block is agent-authored, preserved verbatim, and **worktree-only** — the store has no copy, so it is lost if the worktree is removed.
- The brief write is **atomic (temp+rename, mirroring `TicketStore.SaveTicket`)** so concurrent readers (the spawned agent, a second TUI) always see a complete brief, never a torn one. Keep `PreviewBriefMerge` strictly read-only; only `MergeTicketBrief` writes.

Template in config: `"init_prompt": "Work on: {{.Title}}"`

## Status Detection

`StatusDetector` monitors agent state:
- File-based detection (marker files)
- API-based (HTTP to localhost)
- Terminal content parsing (keyword matching)

Keywords: "waiting", "thinking", "error", etc.

### Refining a file-based "waiting" (DetectStatusWithActivity)

The hook status file pins "waiting" across the whole Notification→PostToolUse
gap (permission granted, tool running, no hook fires). `DetectStatusWithActivity`
refines that using **what's on the live PTY grid**, NOT byte-recency — a prompt
Claude is blocked on (permission box, AskUserQuestion, idle notice) re-renders
every couple of seconds and stamps fresh activity, so "bytes flowed recently"
cannot tell an active turn apart from a re-rendering prompt. Precedence when the
file says "waiting":

1. `permissionPromptVisible` — a recognized approval prompt on screen
   (`"do you want to"` / `"esc to cancel"` / the hook-silent
   `"would you like to …"` family: `proceed` (plan-approval / ExitPlanMode),
   `install`, `create a manifest`, `stash these changes`) → **waiting**
   (wins outright). The full signature list lives in
   `permissionPromptSignatures`; its detection ledger is
   `TestPermissionPromptVisible_SignatureCoverageLedger`.
2. `activeTurnVisible` — positive evidence of an active turn
   (`"esc to interrupt"` / braille spinner) → **working**.
3. otherwise → **waiting** (durable default).

There is no byte-recency fallback: the old `lastActivity < WaitingActivityTTL →
working` catch-all was removed (it mislabeled re-rendering prompts and
empty-grid unattached sessions as "working"). Detection now **fails safe to
"waiting"** — an unknown prompt type or a session with no grid is never shown as
"working". The load-bearing assumption is that Claude renders `"esc to interrupt"`
for the full duration of a tool run; if that footer string drifts, a busy session
degrades to "waiting" (annoying, not dangerous), never the reverse. `lastActivity`
is retained in the signature but no longer triggers promotion.

For a session **no TUI is attached to**, the client has no grid, so the daemon
supplies the verdict from its own live grid — see `internal/daemon`
`resolveSessionStatus`.

#### Background-sub-agent wait wins above all refinements (`AgentSubagents`)

Before the waiting/working refinements above, `DetectStatusWithActivity` runs
two top-precedence checks on the freshly-read `status`:

1. **terminal guard** — `AgentCompleted`/`AgentError` return immediately
   (authoritative; never overridden by a screen heuristic).
2. **`backgroundWaitVisible`** — when the live grid's tail shows Claude's
   `"✻ Waiting for N background agent(s) to finish"` line (signatures
   `"background agent to finish"` / `"background agents to finish"`, 15-line
   tail like `permissionPromptVisible`), return `board.AgentSubagents`. The
   foreground agent is idle-but-occupied — it delegated to sub-agents and is
   NOT blocked on the user. NO hook fires for this wait, so the file stays
   pinned at "working"/"waiting"; the leading "Waiting for…" text would
   otherwise classify as `AgentWaiting` (orange, needs-you). This check sits
   ABOVE the working-branch (which returns) so it wins regardless of the file
   value. Same fail-safe stance as `activeTurnMarkers`: if the wording drifts
   it stops matching and degrades to today's `AgentWaiting`.

`AgentSubagents` is deliberately excluded from `needsAttention` (Auto-mode
must not jump to it) and renders calm/gray. The `detectCodingAgentStatus`
terminal-content path carries the same signature arm before its `"waiting for"`
keyword scan, for hookless agents with no status file. See
[[reference_openkanban_subagents_status]] and the new-status surface map
[[reference_openkanban_agent_status_surface_map]].

#### Refining a stale file-based "working" (the symmetric case)

The mirror problem: the file stays pinned at `"working"` while the session is
actually blocked on the user. Claude's `Notification` hook does **not** reliably
fire for every input-needed state — an `AskUserQuestion` prompt was observed
holding a session's status file at `"working"` for hours. `DetectStatusWithActivity`
therefore also refines a file-based `"working"`: when the live grid shows a
recognized prompt (`permissionPromptVisible`) and **no** active-turn marker
(`activeTurnVisible`), it demotes to `"waiting"`.

Note the deliberate asymmetry vs the waiting-branch precedence above:
- **waiting-branch:** `permissionPromptVisible` wins outright (prompt-first).
- **working-branch:** `activeTurnVisible` **guards** — any active-turn evidence
  keeps `"working"`; only an unambiguous prompt-without-activity demotes. The
  combo is impossible in Claude's real UI; for a file already asserting
  `"working"`, not demoting on a coincidental prompt substring is the
  conservative choice. An empty grid fails SAFE to `"working"`
  (`permissionPromptVisible("")` is false), and the daemon supplies its own grid
  for unattached sessions. Pinned by
  `TestDetectStatusWithActivity_StaleWorkingDemotedOnPrompt`.

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

## 1:1 ticket↔session enforcement (`internal/ticketsvc`)

The package at `internal/ticketsvc/svc.go` is the **single sanctioned funnel** for any code that writes `ticket.AgentSessionID` or gates an attach to an existing Claude session. Both TUI (`internal/ui`) and CLI (`cmd/`) must call through here — direct `ticket.AgentSessionID = uuid` assignments are forbidden by policy.

- `LinkSession(store, requesting, uuid, opts)` — claims `uuid` for the requesting ticket after a uniqueness scan over `GlobalTicketStore.FindByAgentSessionID(uuid)`. `LinkOpts.BestEffort` is silent-noop-on-conflict (used by back-fill); `LinkOpts.Force` clears conflicting tickets first. Caller is responsible for `store.SaveTicket(requesting)` on success.
- `GateAttach(probe, uuid, requestingTicketID)` — refuses attach when `lsof` shows the JSONL is held, the daemon owns the session for a DIFFERENT ticket (`OwnsResp.OwnedByTicketID` ≠ ours), or the daemon reports a multi-match `Conflict`. Allows when the daemon owns for THIS ticket (idempotent re-attach) and degrades to "trust the single match" when `OwnedByTicketID` is empty (old-daemon wire compat). `SessionProbe` is a `func` type (not an interface); `NewRealProbe(ownsFn)` assembles the production probe from an `OwnsFunc` closure + `agent.SessionActive`.
- Spawn-side wiring: `prepareSpawnWith` runs `GateAttach` BEFORE the fast-path attach and BEFORE the daemon `Spawn` RPC, reusing the single `Owns` round-trip. The Unattached (ctrl+space) path still runs the gate — a foreign-held UUID refuses regardless — but skips the fast-path attach itself.
- The pre-2026-06-17 `Ticket.SessionOwned` bool was removed when forking was eliminated; every spawn is now migrate-on-resume. The YAML frontmatter field is dormant for old `.md` compatibility. The grep guard at `internal/ui/forksession_guard_test.go` makes `--fork-session` re-introduction a build-time failure.

See [[openkanban-one-to-one-ticket-session-invariant]] for the threat model.

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
