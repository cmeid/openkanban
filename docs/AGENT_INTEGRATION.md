# Agent Integration

This document describes how OpenKanban spawns, monitors, and manages AI coding agents.

## Overview

OpenKanban runs AI agents in embedded PTY terminals within the TUI. This approach provides:

- **Seamless UX**: No context switching to external terminals
- **Integrated view**: See agent output directly in the board
- **Full terminal emulation**: Colors, cursor movement, interactive prompts
- **Easy navigation**: `ctrl+g` returns to board view

## Daemon architecture (openkanbankd)

The PTY-owning machinery does not live in the TUI process. It lives in a per-user daemon, `openkanbankd`. The TUI is a client: it tells the daemon what to spawn, attaches to one pane at a time over a Unix socket, and renders bytes the daemon streams. Detach the TUI (or kill it, or `kill -9` it) and the agent keeps running inside the daemon. Reopen the TUI and reattach.

```
                    ┌──────────────────────────────┐
                    │       openkanbankd           │
                    │  (cmd/daemon.go, internal/   │
   ┌───────────┐    │   daemon/)                   │
   │ TUI A     │◄──►│                              │
   │ (model.go)│    │  per-session:                │
   └───────────┘    │   - *terminal.Pane (PTY +    │
                    │     vt + scrollback + drain) │◄── PTY ◄── claude/opencode/...
   ┌───────────┐    │   - subscriber list (≤1      │
   │ TUI B     │◄──►│     attached client + N      │
   │ (model.go)│    │     status subscribers)      │
   └───────────┘    │                              │
                    │  Unix socket:                │
                    │  ~/.cache/openkanban/        │
                    │    daemon.sock (0600)        │
                    └──────────────────────────────┘
```

### Why it exists

Two problems the old single-process design could not solve:

1. **Detach-survival across TUI restarts.** Before the daemon, every PTY was owned by the TUI goroutine. Closing the TUI killed the PTY, killed the agent. Iterating on the TUI itself — even a clean `q`-quit — meant losing the agent session. With the daemon, the TUI is a thin frontend; agents survive a TUI restart so you can run `go install` mid-session.
2. **Cross-instance visibility and Takeover.** Multiple TUI instances (e.g. one per worktree) need to see each other's spawned sessions and, occasionally, take one over. The daemon is the single source of truth; both TUIs subscribe to the same event stream.

### Where it runs

Per-user, single instance, locked by `flock(LOCK_EX|LOCK_NB)` on a pidfile.

- Socket: `~/.cache/openkanban/daemon.sock` (mode 0600, override via `OPENKANBAN_DAEMON_SOCK`)
- Pidfile: `~/.cache/openkanban/daemon.pid` (override via `OPENKANBAN_DAEMON_PID`)
- Log: `~/.cache/openkanban/daemon.log` (override via `OPENKANBAN_DAEMON_LOG`; tail with `openkanban daemon log`)

The daemon is autostarted on the first TUI invocation that needs it — `daemonclient.DialOrStart` (see `internal/daemonclient/dial.go`) forks `<self> daemon` with `Setsid` and stdio redirected to the log file, then polls the socket until it binds. No systemd unit, no launchd plist; the user-facing model is "openkanban runs it for you, you mostly don't need to know."

### Lifecycle: bound to ≥1 live TUI

This is **not** a tmux-style long-running service. The daemon's contract is "be alive while there's a TUI that needs me." Concretely:

- Last-client-disconnect triggers shutdown. When the connected-clients count drops to zero, the daemon waits a short grace period; if it stays at zero, it kills any still-live sessions (defensively — the TUI's exit-guard should have caught this) and exits. See `internal/daemon/server.go:handleLastClientDisconnect`.
- The TUI's exit-guard (see `internal/ui/exit_guard.go`) prompts the user before quitting if doing so would leave the daemon as the last client with live sessions.

This means a daemon process never outlives its useful work. It also means `openkanban daemon` from a fresh shell with no TUI running will start and immediately exit — that's expected; pair it with a TUI or a long-lived `openkanban daemon list` to keep it up for debugging.

### One attacher per session, with Takeover

The PTY's output stream is a single producer (the agent) and the daemon multiplexes it. Only one client is the *attached* client at a time — the one whose keystrokes reach stdin and whose viewport sets the resize. Additional clients can subscribe to *status* events without attaching.

Takeover is explicit: a second TUI sends `AttachReq{Takeover: true}`, the daemon sends a `TypeDetach` signal to the current attacher, then accepts the new one. The agent process is untouched; only the wire-level attachment swaps. See `internal/daemon/session.go` and PR6's commit message for the design notes.

The attached/detached state is broadcast to every subscribed client via `SessionEvent{Event: "attached" | "detached"}`. The TUI surfaces it on the board: each card's header badge row renders a `◉` glyph (in info color) while any TUI is attached to that ticket's daemon session — see [UI_DESIGN.md → Daemon Attach Indicator](UI_DESIGN.md#daemon-attach-indicator) for the visual contract.

**Receiver-side counter, not a bool.** The two events come from different goroutines: the new-attach emit is in the attach handler (`internal/daemon/attach.go:84`), while the matching old-detach emit fires from the displaced client's binaryLoop completion (`internal/daemon/attach.go:122`). On takeover both are in flight at once and arrival order at the broadcaster — and therefore at subscribers — is not deterministic. A bool toggle (`true` on attached, `false` on detached) lands in the wrong terminal state for one of the two orderings. The TUI tracks `daemonAttached map[TicketID]int`, increments on attached, decrements (underflow-guarded) on detached, and renders the indicator while the count is >0. The pair nets to +1 in either ordering, matching the daemon's single-attacher truth. See `internal/ui/daemon_subscribe.go` and `internal/ui/daemon_subscribe_test.go::TestHandleDaemonSessionEventAttachedCounter`.

### Snapshot redraw on attach

When a client attaches, the daemon serializes the current screen state — emulator cell grid, cursor position, alt-screen flag, mouse-mode flag, cursor visibility, title — into a synthetic ANSI redraw blob and sends it before any new live bytes. The client's local `xvt.SafeEmulator` consumes it and ends up in a state cell-grid-equivalent to the daemon's. See `internal/daemon/redraw.go` and its tests for fixture-driven round-trip verification.

This is why a TUI reattach "just shows the current screen" — there is no scrollback replay, no cold start. Scrollback bytes prior to the snapshot are not transmitted across a detach/reattach.

### `--migrate` and the 3×3 matrix

`openkanban ticket new --session <uuid> --migrate` declares "this Claude/opencode session belongs to openkanban now." The daemon participates: if the daemon already owns a session for that UUID, migrate proceeds as a re-link (the existing daemon-owned session stands; the ticket gets the UUID and `session_owned: true`). Across the three orthogonal axes — `--migrate` set?, daemon up?, daemon owns this UUID? — the CLI exhibits nine concrete behaviors covered in `cmd/ticket_daemon_test.go`. Summary:

|              | daemon down | daemon up, doesn't own | daemon up, owns |
|--------------|-------------|------------------------|-----------------|
| **link**     | record uuid | record uuid            | record uuid     |
| **migrate**  | lsof probe + stamp | lsof probe + stamp | re-link only (no kill) |
| **migrate --force** | SIGTERM holders + stamp | SIGTERM holders + stamp | re-link only (no kill) |

Migrating an openkanban-owned session is the case the daemon makes safer: the daemon knows it holds the JSONL open, so the CLI does not need to lsof the world and SIGTERM strangers.

### What is *not* supported

- **Concurrent shared attach.** One attacher; status subscribers do not see keystrokes.
- **Daemon survives its own upgrade.** Replacing the `openkanban` binary requires `openkanban daemon restart`. The protocol-version check in the client fails loudly with that exact hint if the user forgets. "Upgrade in place" was considered and rejected — every reasonable implementation requires either ABI freezing or an in-band handshake migration, both of which cost more than the user does by re-killing N sessions once a release.
- **Persistent scrollback across restarts.** Scrollback lives in the per-session ring buffer in the daemon process. Daemon restart loses it. (The agent's own conversation history is in its session JSONL, which is independent.)
- **Networking.** The socket is `AF_UNIX`, mode 0600, in the user's `~/.cache`. There is no TCP listener and no auth layer; the security model is "you trust everything else under your uid."

## Supported Agents

### Tier 1: Full Support

Agents with native support and session continuation.

| Agent | Command | Session Resume | Notes |
|-------|---------|----------------|-------|
| OpenCode | `opencode` | `--session` flag | Native session lookup |
| Claude Code | `claude` | `--continue` flag | Continues last session |
| Gemini CLI | `gemini` | `--resume` flag | Auto-approve with `--yolo` |
| Codex CLI | `codex` | `resume --last` | Auto-approve with `--full-auto` |
| Aider | `aider` | N/A | Use `--yes` flag |

### Tier 2: Generic Support

Any CLI tool that runs interactively.

```json
{
  "agents": {
    "custom-agent": {
      "command": "/path/to/agent",
      "args": ["--interactive"]
    }
  }
}
```

## Agent Lifecycle

### Spawning an Agent

```
User presses 's' on in-progress ticket
       │
       ▼
┌─────────────────────────────────────────┐
│ 1. Check ticket status                  │
│    Must be "in_progress"                │
└─────────────────────────────────────────┘
       │
       ▼
┌─────────────────────────────────────────┐
│ 2. Ensure worktree exists               │
│    Create if missing                    │
└─────────────────────────────────────────┘
       │
       ▼
┌─────────────────────────────────────────┐
│ 3. Create terminal pane                 │
│    terminal.New(ticketID, width, height)│
│    pane.SetWorkdir(worktreePath)        │
└─────────────────────────────────────────┘
       │
       ▼
┌─────────────────────────────────────────┐
│ 4. Build agent command                  │
│    Add context prompt for new sessions  │
│    Add --continue/--session for resume  │
└─────────────────────────────────────────┘
       │
       ▼
┌─────────────────────────────────────────┐
│ 5. Start PTY                            │
│    pane.Start(command, args...)         │
└─────────────────────────────────────────┘
       │
       ▼
┌─────────────────────────────────────────┐
│ 6. Enter agent view                     │
│    mode = ModeAgentView                 │
│    Full-screen terminal display         │
└─────────────────────────────────────────┘
```

### Implementation

The following is an illustrative sketch — the canonical implementation
lives in `internal/ui/model.go` and varies in detail (settings cascade,
opencode server, status-detector wiring, etc.).

```go
// internal/ui/model.go - spawnAgent() — illustrative

func (m *Model) spawnAgent() (tea.Model, tea.Cmd) {
    ticket := m.selectedTicket()
    if ticket.Status != board.StatusInProgress {
        m.notify("Move ticket to In Progress first")
        return m, nil
    }

    // Ensure worktree exists
    if ticket.WorktreePath == "" {
        if err := m.setupWorktree(ticket); err != nil {
            m.notify("Failed to create worktree: " + err.Error())
            return m, nil
        }
    }

    // Get agent config (ticket override -> global default)
    agentType := ticket.AgentType
    if agentType == "" {
        agentType = m.config.Defaults.DefaultAgent
    }
    agentCfg := m.config.Agents[agentType]

    // Create terminal pane
    pane := terminal.New(string(ticket.ID), m.width, m.height-2)
    pane.SetWorkdir(ticket.WorktreePath)
    m.panes[ticket.ID] = pane

    // Build args with context
    isNewSession := agent.ShouldInjectContext(ticket)
    args := m.buildAgentArgs(agentCfg, ticket, isNewSession)

    // Enter agent view
    m.mode = ModeAgentView
    m.focusedPane = ticket.ID

    return m, pane.Start(agentCfg.Command, args...)
}
```

### Context Injection

For new sessions, OpenKanban injects ticket context:

```go
// internal/agent/context.go

func BuildContextPrompt(template string, ticket *board.Ticket) string {
    // Template variables:
    // {{.Title}}       - Ticket title
    // {{.Description}} - Ticket description
    // {{.BranchName}}  - Git branch name
    // {{.BaseBranch}}  - Base branch (e.g., main)
    
    result := strings.ReplaceAll(template, "{{.Title}}", ticket.Title)
    result = strings.ReplaceAll(result, "{{.Description}}", ticket.Description)
    // ...
    return result
}

func ShouldInjectContext(ticket *board.Ticket) bool {
    // New session if never spawned before
    return ticket.AgentSpawnedAt == nil
}
```

Default prompt template:

```
You have been spawned by OpenKanban to work on a ticket.

**Title:** {{.Title}}

**Description:**
{{.Description}}

**Branch:** {{.BranchName}} (from {{.BaseBranch}})

Focus on completing this ticket. Ask clarifying questions if needed.
```

### Session Continuation

For returning to an existing session:

**OpenCode:**
```go
case "opencode":
    if !isNewSession {
        if sessionID := agent.FindOpencodeSession(ticket.WorktreePath); sessionID != "" {
            args = append(args, "--session", sessionID)
        }
    }
```

**Claude Code:**
```go
case "claude":
    if isNewSession {
        // New claude sessions always start in plan mode so the
        // agent reviews the proposed approach before touching
        // the tree. Any conflicting permission flag from the
        // user's agent config (--dangerously-skip-permissions
        // or another --permission-mode pair) is stripped first.
        args = stripPermissionFlags(args)
        args = append(args, "--permission-mode", "plan")
        // Title the Claude session after the ticket so it's
        // identifiable in `claude --resume`'s session picker and in
        // the terminal title bar. Only on new sessions — resumes
        // inherit the existing name. Skipped if the user already
        // configured -n / --name in their agent args.
        if !hasClaudeNameFlag(args) && strings.TrimSpace(ticket.Title) != "" {
            args = append(args, "-n", ticket.Title)
        }
        // Inject the init-prompt as a positional argument
        // (see Context Injection above).
    } else {
        // Resumed sessions keep whatever permission mode they
        // had at exit — only new sessions are forced into plan.
        args = append(args, "--continue")
    }
```

## Terminal Pane

### PTY Architecture

```go
// internal/terminal/pane.go — illustrative

type Pane struct {
    id           string
    vt           *xvt.SafeEmulator   // charm/x/vt emulator, mutex-wrapped
    pty          *os.File            // PTY master file descriptor
    cmd          *exec.Cmd           // Running process
    workdir      string
    width        int
    height       int

    cursorHidden atomic.Bool         // tracks DECTCEM via charm callback
    drainStop    chan struct{}       // shuts down the response-drain goroutine
    drainWG      sync.WaitGroup
}
```

### Starting a Process

```go
func (p *Pane) Start(command string, args ...string) tea.Cmd {
    return func() tea.Msg {
        p.cmd = exec.Command(command, args...)
        p.cmd.Env = buildCleanEnv(p.sessionName)
        p.cmd.Dir = p.workdir

        // Fork with size atomically (avoids the TIOCSWINSZ race that
        // used to leave bottom-anchored UI rendered at the top).
        ptmx, err := pty.StartWithSize(p.cmd, &pty.Winsize{
            Rows: uint16(p.height), Cols: uint16(p.width),
        })
        if err != nil {
            return ExitMsg{PaneID: p.id, Err: err}
        }
        p.pty = ptmx

        // Spin up the emulator. charm/x/vt emits responses
        // (DA queries, cursor reports, ...) via Read() — we MUST drain
        // those bytes back to the PTY or the emulator deadlocks.
        p.vt = xvt.NewSafeEmulator(p.width, p.height)
        p.vt.SetCallbacks(xvt.Callbacks{
            CursorVisibility: func(visible bool) {
                p.cursorHidden.Store(!visible)
            },
        })
        p.startDrainUnlocked()  // goroutine: for { Read; pty.Write }

        return p.readOutputUnlocked()()
    }
}
```

### Input Handling

```go
func (p *Pane) HandleKey(msg tea.KeyMsg) tea.Msg {
    // ctrl+g exits agent view
    if msg.String() == "ctrl+g" {
        return ExitFocusMsg{}
    }

    // Convert key to PTY escape sequence
    input := p.translateKey(msg)
    p.pty.Write(input)
    return nil
}

func (p *Pane) translateKey(msg tea.KeyMsg) []byte {
    switch msg.Type {
    case tea.KeyEnter:
        return []byte("\r")
    case tea.KeyUp:
        return []byte("\x1b[A")
    case tea.KeyDown:
        return []byte("\x1b[B")
    // ... etc
    }
    return []byte(string(msg.Runes))
}
```

### Rendering

```go
func (p *Pane) View() string {
    // Iterate cells from the emulator, translate each to our internal
    // Glyph type via cellToGlyph(p.vt.CellAt(x, y)), batch runs of
    // identical SGR, emit ANSI for the host terminal.
}
```

## Architecture: Terminal Emulator

OpenKanban currently uses `github.com/charmbracelet/x/vt` (specifically `SafeEmulator`) for in-pane terminal emulation. This section explains why, and what it cost to move there.

### Previous: hinshun/vt10x

The original implementation used `github.com/hinshun/vt10x`. It was small (~2k LOC), legible, and broadly correct for plain output. Two material problems surfaced over time:

1. **Bottom-edge scroll counting drifts.** vt10x handles the cursor-down command (`CSI N B`) by clamping the cursor at the bottom row without scrolling. Line feed (`\n`) at the bottom scrolls separately. The two paths don't share state perfectly: over many cycles of "draw menu, scroll N lines, redraw," vt10x's cursor row diverged from what a correct terminal computes. The captured-PTY repro showed up to **46 rows of drift** in a 22-second session, with claude's "thinking" indicator landing at the wrong row in vt10x's grid. Rendered to the host terminal, the symptom is the input bar (or AskUserQuestion menu cursor) appearing at the top of the pane instead of the bottom, non-deterministically.

2. **Unmaintained.** Last release in 2022. The bug above is unlikely to ever be upstream-fixed.

### Chosen: charmbracelet/x/vt

`charmbracelet/x/vt` is part of the same ecosystem as the rest of openkanban's TUI dependencies (Bubble Tea, lipgloss, ultraviolet). Verified against the same captured-PTY trace, charm/x/vt produces a cursor position consistent with a correct terminal, and content lands at the expected rows.

Other candidates considered:
- **Fork-and-fix vt10x.** Patching the bottom-edge clamp/scroll interaction is ~50 LOC. Cheap up-front but leaves us owning a fork of an unmaintained library; future bugs we haven't hit yet (and there will be some — escape-sequence surfaces are large) would all need patching.
- **Build a screen buffer on top of `charmbracelet/x/ansi`.** Parser-only; we'd have written the screen state machine ourselves. Too much surface area for too little payoff.
- **gdamore/tcell.** A UI rendering library — it generates ANSI, doesn't parse it. Wrong direction.

### What the migration cost

- **New internal `Glyph` type** (`internal/terminal/glyph.go`). The scrollback ring, selection state, and render path all use `Glyph` — they don't touch the emulator's native `*uv.Cell`. Only `pane.go` translates at the boundary (`cellToGlyph`). This makes the emulator a swap-out detail.
- **Response-pipe drain goroutine.** charm/x/vt emits replies to terminal queries (DA, cursor position, etc.) through `Emulator.Read()`. Without a consumer, the emulator deadlocks on the first query. `Pane.startDrainUnlocked` runs the consumer for the lifetime of the pane; `stopDrainUnlocked` closes the emulator, unblocking `Read` with EOF.
- **Cursor visibility hook.** charm/x/vt's public API does not expose a `CursorVisible()` getter (the `Cursor.Hidden` flag lives on a private `Screen`). We register a `Callbacks.CursorVisibility` callback that flips an `atomic.Bool` on the Pane; the renderer reads it lock-free.
- **Dependency-tree shift.** Brought transitive bumps to Bubble Tea (1.3.4 → 1.3.10), bubbles (0.21.0 → 1.0.0), and a new direct dep on `charmbracelet/ultraviolet` and `charmbracelet/x/ansi`. All same-ecosystem upgrades.

### Known limitations carried forward

- **Single-rune cells.** `cellToGlyph` collapses a grapheme cluster (charm's `Cell.Content` is a UTF-8 string supporting ZWJ, combining marks, etc.) to its first rune. The rest of the renderer assumes 1 rune per cell. Double-wide CJK still renders narrowly; combining marks drop. Cell width is captured (`Glyph.Width`) but unused. This matches the prior vt10x behavior.
- **Mouse-mode detection.** openkanban keeps its own byte scanner for `?1000h` / `?1049h` because we need the state synchronously during input handling. charm/x/vt tracks these internally too — we don't read from it for these specific flags.

### When to revisit

- If charm/x/vt's API surface stabilizes enough to expose `CursorVisible()` directly, we can drop the callback machinery.
- If grapheme-width support becomes a felt issue (CJK users, emoji-heavy output), the rendering path needs to be reworked to iterate `Glyph.Width` for spacing — that's a larger change touching the column-major loops in pane.go.

## Session Linking on Ticket Creation

`openkanban ticket new` can attach an existing Claude Code session UUID to a ticket via `--session <uuid>`. The session JSONL lives at `~/.claude/projects/<encoded-cwd>/<uuid>.jsonl`; the CLI globs that path to verify the session exists before recording it. Two operational sub-modes are exposed: **link mode** (default — recorded with `session_owned: false` in the frontmatter) and **migrate mode** (set via `--migrate`, recorded as `session_owned: true`). Link mode is the safe default: the original session is left running untouched. Migrate mode declares "this session belongs to openkanban now," so any further driving of the session happens through the ticket. Before stamping migrate mode the CLI probes with `lsof` for processes holding the JSONL open; if anything still has it, it refuses unless `--force` is also set, in which case it sends SIGTERM with a 3s grace window and then SIGKILL. A separate `--created-by <name>` flag stores a free-form audit string in `created_by_session` — provenance only, never read by the spawn logic.

The spawn flow consumes these two frontmatter fields together. On a ticket's first agent spawn (`AgentSpawnedAt == nil`), the Claude branch in `internal/ui/model.go` appends `--resume <AgentSessionID>` to the command line and additionally appends `--fork-session` when `SessionOwned` is false. The UUID is re-validated against `agent.SessionUUIDPattern` at spawn time as a defensive belt-and-braces check; non-UUID-shaped values are skipped silently rather than passed to `claude` as garbage. Subsequent spawns of the same ticket follow the existing `--continue` resume path and ignore the linkage fields. See the `--session` / `--migrate` / `--force` / `--created-by` flag descriptions in CONFIGURATION.md for the CLI surface.

## Status Detection

### Status Types

```go
type AgentStatus string

const (
    AgentNone      AgentStatus = "none"      // No session spawned
    AgentIdle      AgentStatus = "idle"      // Waiting for input
    AgentWorking   AgentStatus = "working"   // Processing
    AgentWaiting   AgentStatus = "waiting"   // Waiting for user
    AgentCompleted AgentStatus = "completed"
    AgentError     AgentStatus = "error"
)
```

These values are enum-validated when ticket Markdown files are loaded.
A hand-edited file with `agent_status: running` (not on the allowlist)
will be rejected at parse with a clear error and surfaced via
`watch-errors.log` — see [DATA_MODEL.md](DATA_MODEL.md#file-watcher-integration).

### Detection Methods

**1. Process State**
```go
func (p *Pane) Running() bool {
    return p.running && p.cmd != nil && p.cmd.Process != nil
}
```

**2. Status Files** (for OpenCode/Claude)
```go
func (d *StatusDetector) DetectStatus(agentType, sessionID string, running bool) AgentStatus {
    if !running {
        return AgentNone
    }
    
    // Check agent-specific status file
    switch agentType {
    case "opencode":
        return d.checkOpencodeStatus(sessionID)
    case "claude":
        return d.checkClaudeStatus(sessionID)
    }
    
    return AgentIdle
}
```

### Polling

Status is polled at configurable intervals:

```go
func tickAgentStatus(d time.Duration) tea.Cmd {
    return tea.Tick(d, func(t time.Time) tea.Msg {
        return agentStatusMsg(t)
    })
}

// In Update():
case agentStatusMsg:
    return m, tea.Batch(
        m.pollAgentStatusesAsync(),
        tickAgentStatus(m.agentMgr.StatusPollInterval()),
    )
```

## Configuration

### Agent Config

```json
{
  "agents": {
    "opencode": {
      "command": "opencode",
      "args": [],
      "status_file": ".opencode/status.json",
      "init_prompt": "Custom prompt for OpenCode..."
    },
    "claude": {
      "command": "claude",
      "args": ["--dangerously-skip-permissions"],
      "status_file": ".claude/status.json",
      "init_prompt": "Custom prompt for Claude..."
    },
    "gemini": {
      "command": "gemini",
      "args": ["--yolo"],
      "init_prompt": "Custom prompt for Gemini..."
    },
    "codex": {
      "command": "codex",
      "args": ["--full-auto"],
      "init_prompt": "Custom prompt for Codex..."
    },
    "aider": {
      "command": "aider",
      "args": ["--yes"],
      "init_prompt": "Custom prompt for Aider..."
    }
  },
  "defaults": {
    "default_agent": "opencode",
    "init_prompt": "Default prompt for all agents..."
  }
}
```

### Prompt Priority

1. Agent-specific `init_prompt` in config
2. Global `defaults.init_prompt` in config
3. Built-in default prompt — embedded from [`internal/config/agent_prompt.tmpl`](../internal/config/agent_prompt.tmpl) via `//go:embed`. Edit the markdown file, not a Go string constant. On `Load`, `mergeAgentDefaults` restores the embedded default when a user's `init_prompt` field is empty or absent, so clearing the override falls through to the binary's shipped content (not to the much shorter generic `defaultGlobalPrompt`).

## Environment Isolation

When spawning agents, OpenKanban filters environment variables to
prevent nested-session detection and inherited identity leakage, and
injects the two openkanban-specific vars the child can use to report
back (`OPENKANBAN_SESSION`, `OPENKANBAN_TICKET_ID`):

```go
func buildCleanEnv(sessionName, ticketID string) []string {
    var env []string
    for _, e := range os.Environ() {
        key := strings.Split(e, "=")[0]
        // Strip agent-specific vars so the child agent starts clean.
        if key == "OPENCODE" || strings.HasPrefix(key, "OPENCODE_") {
            continue
        }
        if key == "CLAUDE" || strings.HasPrefix(key, "CLAUDE_") {
            continue
        }
        if key == "GEMINI" || strings.HasPrefix(key, "GEMINI_") {
            continue
        }
        if key == "CODEX" || strings.HasPrefix(key, "CODEX_") {
            continue
        }
        // Strip any inherited OPENKANBAN_* so nested spawns can't
        // leak an outer pane's session/ticket identity to the child.
        if strings.HasPrefix(key, "OPENKANBAN_") {
            continue
        }
        env = append(env, e)
    }
    env = append(env, "TERM=xterm-256color")
    if sessionName != "" {
        env = append(env, "OPENKANBAN_SESSION="+sessionName)
    }
    if ticketID != "" {
        env = append(env, "OPENKANBAN_TICKET_ID="+ticketID)
    }
    return env
}
```

## Agent-callable commands (in-session)

When openkanban spawns an agent, it injects two env vars the child can
use to report back:

- `OPENKANBAN_SESSION` — session identifier used as the basename of
  `~/.cache/openkanban-status/<session>.status`. Used by Claude Code
  hooks (see `openkanban hooks install`) to write working / idle /
  waiting status that the TUI polls.
- `OPENKANBAN_TICKET_ID` — the ticket's frontmatter UUID. Used by
  `openkanban ticket done` to resolve the .md file authoritatively.

Two CLI subcommands are designed to be invoked from inside a spawned
session:

### `openkanban status set <state>`

Writes the session's status file. `state` is one of `working`, `idle`,
`waiting`, `completed`, `error`. Silently no-ops when
`$OPENKANBAN_SESSION` is unset (safe for globally-installed hooks).

Once the status file holds `completed`, a subsequent `status set idle`
/ `working` / `waiting` is silently dropped — only `completed` or
`error` (terminal states) may overwrite it. This prevents Claude's
`Stop` hook from clobbering the completion signal during the SIGTERM
grace window that follows `openkanban ticket done`.

### `openkanban ticket done`

The agent-side "/quit equivalent." Marks the current session's ticket
as `Status=done` + `AgentStatus=completed` (atomic .md write), then
writes `completed` to the status file. Reads `$OPENKANBAN_TICKET_ID`;
exits non-zero if unset or if the ticket .md is missing.

When the TUI sees the resulting `AgentCompleted` transition on a ticket
whose `Status == StatusDone`, it gracefully stops the pane
(SIGTERM → 3s grace → SIGKILL) — the Claude process exits cleanly and
the ticket lands in the Done column with the completed badge.

Idempotent: a second invocation does not re-stamp `CompletedAt`, but
the status file is re-written so a freshly-spawned pane (re-opened
after the previous completion) re-arms the auto-stop transition.

Worktree, branch, and session-JSONL teardown remain reserved for
ticket deletion — `ticket done` does not touch them.

## Adding New Agents

### 1. Add Configuration

```json
{
  "agents": {
    "new-agent": {
      "command": "new-agent-cli",
      "args": ["--mode", "interactive"],
      "init_prompt": "You are working on: {{.Title}}"
    }
  }
}
```

### 2. Handle Session Resume (Optional)

If the agent supports session continuation, add logic to `buildAgentArgs()`:

```go
case "new-agent":
    if !isNewSession {
        // Add session resume flag
        args = append(args, "--resume", ticket.ID)
    }
```

### 3. Add Status Detection (Optional)

If the agent writes status files:

```go
func (d *StatusDetector) checkNewAgentStatus(sessionID string) AgentStatus {
    path := filepath.Join(os.Getenv("HOME"), ".new-agent", "status", sessionID)
    // Read and parse status
}
```

## Error Handling

### Spawn Failures

```go
// PTY start fails
if err != nil {
    return ExitMsg{PaneID: p.id, Err: err}
}

// Handled in Update():
case terminal.ExitMsg:
    delete(m.panes, board.TicketID(msg.PaneID))
    m.notify("Agent exited")
```

### Agent Crashes

When the agent process exits:

```go
// In pane read loop - EOF means process exited
n, err := ptyFile.Read(buf)
if err != nil {
    return ExitMsg{PaneID: paneID, Err: err}
}
```

### Recovery

User can restart with `s` key on the ticket.

## Security Considerations

### Command Sources

Agent commands come only from config, never user input:

```go
// SAFE: From validated config
agentCfg := m.config.Agents[agentType]
pane.Start(agentCfg.Command, args...)

// NEVER: From user input
// pane.Start(userInput, ...)
```

### Worktree Validation

Worktrees are always within the project's designated directory:

```go
worktreePath := filepath.Join(m.worktreeDir, branchName)
// Path is always under worktreeDir, can't escape
```

### Environment Filtering

Prevents sensitive environment variables from leaking to agents and prevents nested session issues.
