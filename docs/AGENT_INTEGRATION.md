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

The daemon has two supported lifecycle modes:

1. **Default (TUI-managed).** The TUI autostarts the daemon on first invocation via `daemonclient.DialOrStart` (`internal/daemonclient/dial.go`), which forks `<self> daemon` with `Setsid` and stdio redirected to the log file, then polls the socket until it binds. The daemon's lifetime is bound to a connected TUI — see below. The user-facing model is "openkanban runs it for you, you mostly don't need to know."

2. **System-managed (macOS launchd).** Run `openkanban daemon install-service` to register openkanbankd as a LaunchAgent under `gui/<uid>`. The plist invokes `openkanban daemon --persistent`, which keeps the daemon alive across TUI restarts. Useful when iterating on TUI code or expecting agent sessions to outlive any one TUI process. `daemon uninstall-service` reverses it. Set `daemon.autostart: false` in `~/.config/openkanban/config.json` (or pass `--no-launch-daemon` at launch) so the TUI doesn't race the service for the pidlock. No Linux systemd backend yet; on Linux you can run `openkanban daemon --persistent` under your own supervisor (systemd user unit, tmux, etc.).

### Lifecycle: default vs persistent

**Default mode** is not a tmux-style long-running service. The daemon's contract is "be alive while there's a TUI that needs me." Concretely:

- Last-client-disconnect triggers shutdown. When the connected-clients count drops to zero, the daemon kills any still-live sessions (defensively — the TUI's exit-guard should have caught this) and exits immediately. See `internal/daemon/server.go:handleLastClientDisconnect`.
- The TUI's exit-guard (see `internal/ui/exit_guard.go`) uses an *atomic exit-intent* mechanism in the daemon to decide between silent-quit and modal. When a TUI is about to exit, it calls `PrepareExit`; the daemon atomically marks that connection as `exiting` under `clientsMu` and returns `OtherActiveClients` (the count of peer clients that have NOT also called PrepareExit). The TUI silent-quits when `OtherActiveClients > 0` (a peer will keep the daemon alive); otherwise — and if sessions are live — it opens the exit-confirm modal. Exactly one caller among N simultaneous closers observes `OtherActiveClients == 0`, so the multi-TUI close race is closed without prompting non-final TUIs. If the user dismisses the modal (Esc/q), the TUI calls `CancelExit` to clear the flag so peer TUIs see this client as active again on subsequent PrepareExit calls. SIGINT/SIGTERM are routed through the same guard via `program.Send(ui.QuitRequestedMsg{})` — Ctrl-C with live sessions opens the modal just like `q`.

This means a default-mode daemon process never outlives its useful work. It also means `openkanban daemon` from a fresh shell with no TUI running will start and immediately exit — that's expected; pair it with a TUI or a long-lived `openkanban daemon list` to keep it up for debugging.

**Persistent mode** (`--persistent`, used by `daemon install-service`) inverts the last-client gate: the daemon stays alive when all clients disconnect, and only exits via explicit `openkanban daemon stop`, SIGTERM, or its own staleness watcher (after a binary upgrade with no sessions attached — see below). To avoid silently orphaning live sessions, `daemon stop` prompts before shutting down whenever sessions are alive (pass `--force` to skip; matches `daemon restart`'s gate).

For per-session control without tearing down the whole daemon, `openkanban daemon close <id>` gracefully terminates a single session. The `<id>` argument resolves in this order: exact SessionID, then 4+ char SessionID prefix (matches the leftmost column of `daemon list`), then exact TicketID. SessionID matches route through `KillReq`; TicketID matches route through `TicketDoneReq` so duplicate-TicketID defense-in-depth still kills every match. `--grace <duration>` controls the SIGTERM-to-SIGKILL window (default 3s, `--grace 0` for hard-kill); `-y` skips the interactive confirm. This is the recovery hatch for cases the orphan-prevention fixes at the construction sites — `handleSpawn` refuses empty TicketID, `DeleteProject`/`ticket delete`/`performTicketCleanup` notify the daemon — don't cover.

### One attacher per session, with Takeover

The PTY's output stream is a single producer (the agent) and the daemon multiplexes it. Only one client is the *attached* client at a time — the one whose keystrokes reach stdin and whose viewport sets the resize. Additional clients can subscribe to *status* events without attaching.

Takeover is explicit: a second TUI sends `AttachReq{Takeover: true}`, the daemon sends a `TypeDetach` signal to the current attacher, then accepts the new one. The agent process is untouched; only the wire-level attachment swaps. See `internal/daemon/session.go` and PR6's commit message for the design notes.

### TUI viewing signal (distinct from attach)

The board card's `◉` indicator does NOT track attach — see [UI_DESIGN.md → TUI Viewing Indicator](UI_DESIGN.md#tui-viewing-indicator) for the visual. It tracks **viewing**: a separate per-client signal sent via `SetViewing` RPC whenever a TUI enters or leaves `ModeAgentView` on a given session. The two are intentionally orthogonal:

- *Attached* = "this client owns the binary PTY stream right now." In practice this stays true from spawn until `PaneView.Close`, because spawn auto-attaches and nothing detaches when the user Esc's back to the board. So `attached` answers the question "could a TUI receive keystrokes on this session" — useful for takeover decisions, useless as a "someone is watching" indicator (every running session would light up).
- *Viewing* = "this client is in agent-view mode on this session." Flips on Enter / off on Esc. Multiple TUIs can view the same session simultaneously (no enforced exclusion). When the count is >0 on any subscriber's board, the card shows `◉`.

`SetViewing(sessionID, viewing bool)` returns the post-call viewer count and (when the call changed state — duplicates are idempotent no-ops) broadcasts `SessionEvent{Event: "viewing" | "unviewing"}` to every subscriber. The TUI maintains a `daemonViewing map[TicketID]int` populated at startup from `SessionInfo.ViewerCount` and updated by events. Crashed-client safety: on connection disconnect the daemon scrubs the client out of every session's viewers set and emits `"unviewing"` for each, so a `kill -9`'d TUI does not leave zombie indicator counts on sibling boards (`internal/daemon/server.go` `cleanupViewersForClient`).

The receiver still uses a counter rather than a bool — accumulated viewer count is genuinely the signal we want (so the indicator shows for "any TUI viewing", not "this specific TUI viewing"), and the count makes "two viewers, one leaves" produce the right `1 → still showing` state without extra bookkeeping.

The TUI's outbound side is centralized: `Update` wraps the inner dispatch with a `reconcileViewing` step that diffs current `(mode, focusedPane)` against the last value sent to the daemon and fires `SetViewing(prev,false)` / `SetViewing(new,true)` when they differ. No individual case branch in the giant `dispatchUpdate` switch has to remember to ping the daemon — every mutation gets caught by the post-dispatch reconcile. See `internal/ui/model.go` (`reconcileViewing` / `setViewingCmd`) and `internal/ui/daemon_subscribe_test.go::TestHandleDaemonSessionEventViewingCounter`.

### Snapshot redraw on attach

When a client attaches, the daemon ships a synthetic ANSI byte stream over the binary connection before any new live bytes: first the serialized scrollback history (SGR-batched per row, terminated by `\r\n`), then a `SerializeRedraw` of the current screen state — emulator cell grid, cursor position, alt-screen flag, mouse-mode flag, cursor visibility, title. The client's local `xvt.SafeEmulator` consumes it and ends up cell-grid-equivalent to the daemon's; the client's snapshot-apply path also drives `CaptureTopRow`/`PushScrolledLine` per `\r\n`-segment so the history rows land in its own scrollback ring as they scroll off the top. See `internal/daemon/redraw.go` (`SerializeRedraw`, `SerializeScrollback`) and `internal/daemonclient/paneview.go::applySnapshotChunk`.

The redraw portion uses CUP positioning rather than `\n` scrolling, so it does not push extra rows into client-side scrollback. If the redraw flips alt-screen on, the client's alt-screen detector keeps subsequent rows out of the primary-screen ring (matching the live-mode contract).

### `--migrate` and the 3×3 matrix

`openkanban ticket new --session <uuid> --migrate` declares "this Claude/opencode session belongs to openkanban now." The daemon participates: if the daemon already owns a session for that UUID, migrate proceeds as a re-link (the existing daemon-owned session stands; the ticket gets the UUID and `session_owned: true`). Across the three orthogonal axes — `--migrate` set?, daemon up?, daemon owns this UUID? — the CLI exhibits nine concrete behaviors covered in `cmd/ticket_daemon_test.go`. Summary:

|              | daemon down | daemon up, doesn't own | daemon up, owns |
|--------------|-------------|------------------------|-----------------|
| **link**     | record uuid | record uuid            | record uuid     |
| **migrate**  | lsof probe + stamp | lsof probe + stamp | re-link only (no kill) |
| **migrate --force** | SIGTERM holders + stamp | SIGTERM holders + stamp | re-link only (no kill) |

Migrating an openkanban-owned session is the case the daemon makes safer: the daemon knows it holds the JSONL open, so the CLI does not need to lsof the world and SIGTERM strangers.

### Architectural decisions (do not regress without consulting this section)

The persistent-mode / launchd integration shipped 2026-06-15 has a small set of load-bearing design choices. Future contributors editing this area should preserve each unless the original constraint genuinely no longer applies.

1. **Two modes only: default (TUI-bound) and persistent (`--persistent`).** No auto-detection of "am I under launchd?" via PPID or env. The flag is explicit. Why: PPID-based detection is fragile across re-execs and process supervisors, and hides intent.
2. **Last-client-disconnect = exit ONLY in default mode.** `handleLastClientDisconnect` (`internal/daemon/server.go`) gates `initiateShutdown` on `!s.persistent`. Don't conditionally re-introduce shutdown in persistent mode without addressing the "rapid TUI iteration loses sessions" use case that motivated this whole refactor.
3. **Persistent + stale binary + 0 sessions = STILL exit.** `watchBinaryStaleness` deliberately ignores the `persistent` flag at the zero-sessions branch. Why: under launchd the exit triggers a clean respawn with the new binary; without launchd, exit beats "stale forever." Adding a `persistent` gate here would create a stale-binary footgun.
4. **No `service.Backend` interface — launchd is inline, build-tagged.** `internal/service/launchd_darwin.go` + `launchd_other.go` (stub returning `ErrUnsupported`). Why: with one concrete backend, an abstraction is guessing at the seam. When systemd lands as `systemd_linux.go`, *that's* the moment to extract a contract from the actual diff between the two implementations. Don't pre-abstract.
5. **`launchctl bootstrap` / `bootout`, NOT `load` / `unload`.** The deprecated verbs work but are noisy on Sonoma+ and break `launchctl print` scrapes. If you "fix" this back to `load`, you're undoing a deliberate Sonoma-era choice — read the man pages first.
6. **`KeepAlive = {SuccessfulExit: false}`** in the plist. A clean `openkanban daemon stop` (exit 0) leaves the service down. Only crashes / signals / `launchctl bootout` trigger respawn. This is the user's escape hatch from "the service refuses to stop."
7. **`install-service` REFUSES if a daemon is currently running.** Liveness check via `daemonclient.Dial(2s-timeout)`. No "auto-stop the existing one" magic. Why: transactional stop-then-install can fail mid-way (sessions live, user not prompted); two clear user steps beats one magical action.
8. **`install-service` does NOT modify `~/.config/openkanban/config.json`.** Only the interactive `scripts/install.sh` prompt path is allowed to flip `daemon.autostart=false`. The bare subcommand prints a hint and lets the user choose. Why: separation of concerns; re-running install-service shouldn't silently reconfigure the user.
9. **`install-service` rejects `os.TempDir()` / `/go-build` binary paths.** `sanityCheckBinPath` catches the "I ran from `go run`" footgun where the plist points at a path the OS will GC in minutes.
10. **`--no-launch-daemon` is ONE-WAY.** `=true` suppresses autostart; `=false` does NOT force autostart on (config controls). There's a code comment at `cmd/root.go` enforcing this so a future contributor doesn't refactor it into a tri-state.
11. **TUI clients identify themselves as `"openkanban-tui"`; CLI subcommands as `"openkanban-cli"`.** Constants live in `internal/daemon/protocol.go` (`ClientNameTUI` / `ClientNameCLI`). `PrepareExitResp.OtherTUIClients` exposes a peer-TUI count for clients that want to differentiate "I'm the last one out" from "another TUI is still watching" (the TUI's exit-guard uses it). `daemon stop` used to gate its own kill-confirm prompt on `OtherTUIClients == 0`, but that turned a watching-TUI into implicit consent — a stop invoked from `scripts/install.sh` (or any other separate shell) would silently kill in-flight agent work the user never consented to. The prompt now fires on `liveSessions > 0` alone, matching `daemon restart`. Don't drop the ClientName tracking — the TUI exit-guard still depends on `OtherTUIClients`; don't reuse those string values for non-TUI/non-CLI clients.
12. **Existing default-mode integration tests stay default-mode.** Persistent-mode behavior has its own siblings (`TestServerLifecycle_PersistentSurvivesLastDisconnect`, `TestPrepareExit_OtherTUIClients`). Don't add a `persistent` knob to `startServer()`; copy-paste a new test fixture instead — keeps the test harness from accidentally coupling the two modes.
13. **TUI-forked daemons ALWAYS pass `--persistent`.** Both fork sites (`internal/daemon/autostart.go`, `internal/daemonclient/dial.go`) must invoke `exec.Command(exe, "daemon", "--persistent")`. The "ephemeral daemon that dies with the TUI" variant looks innocuous but kills every live agent session on TUI close — the headline bug behind this hardening pass. Verified by the second integration test that asserts a session survives a TUI fork-then-quit cycle (see brief at `tickets/review-exit-handling-when-using-launch-d.md`). Orphan-daemon concern is addressed by `watchBinaryStaleness` recycling on next update.
14. **Background daemon goroutines recover from panics; `handleConn` does NOT.** `broadcastEvents`, `watchBinaryStaleness`, and `watchSessionExit` wrap their bodies in `defer recover()` so a panic logs + exits the goroutine rather than crashing the whole process (which would take every live PTY with it). `handleConn` is deliberately *unwrapped*: a panic there indicates wire-format or session-map corruption that should surface — recovering would let the daemon limp on with inconsistent state. `watchSessionExit` additionally preserves the invariant that `removeSession()` + the "exited" emit always run, via a deferred-cleanup pattern with an inner recover around the emit.
15. **SIGHUP is a clean-shutdown trigger alongside SIGINT/SIGTERM.** `cmd/daemon.go` registers all three via `signal.NotifyContext` so the daemon runs `cleanup()` on each. Go's default disposition for SIGHUP is process termination (exit 129); for a daemon that owns live PTYs, that's the wrong default — orphaned children get SIGHUP'd to death without the per-session grace window.
16. **Plist `ExitTimeOut=30` is sized for the sequential `cleanup()` kill loop.** Both `cleanup()` and `handleShutdown` kill sessions sequentially with `shutdownGraceSeconds=3` each, so worst-case wall clock is `s.wg.Wait() + 3N + overhead`. 30s covers ~8 concurrent sessions. If `shutdownGraceSeconds` changes, recompute the plist budget or parallelize the kill loop — the two values must move together. launchd's default is 20s; we override explicitly so a future contributor can't silently change either.
17. **`OPENKANBAN_DAEMON_SOURCE` env var tags every daemon's origin.** Set to `tui-fork` at both fork sites and to `launchd` via plist `EnvironmentVariables`. The daemon logs it on startup (`persistent=true source=launchd` etc.) so postmortems on session-loss events can identify *which* daemon was the victim. Three valid values: `tui-fork`, `launchd`, anything-else-reported-as `manual`.

### What is *not* supported

- **Concurrent shared attach.** One attacher; status subscribers do not see keystrokes.
- **Daemon survives its own upgrade.** Replacing the `openkanban` binary requires the daemon to restart. In default mode, that's `openkanban daemon restart` (or just quit and re-launch the TUI — the next invocation autostarts a fresh daemon from the new binary). In persistent / launchd-managed mode, run `openkanban daemon stop`; launchd's `KeepAlive = {SuccessfulExit: false}` keeps a clean stop down rather than respawning, so the user can re-launch the TUI (or `launchctl bootstrap` the service again) when ready. Either way the protocol-version check in the client fails loudly with an actionable hint if the user attaches a new-binary client to an old-binary daemon. "Upgrade in place" was considered and rejected — every reasonable implementation requires either ABI freezing or an in-band handshake migration, both of which cost more than the user does by re-killing N sessions once a release.
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

### Design intent of the shipped template

The embedded `agent_prompt.tmpl` is deliberately **generic about cross-cutting workflow discipline**. It points at categories of help — "code-review, validation, cross-stack risk subagents", "adversarial / multi-role doc reviewer" — without naming specific agents, plugins, or skills that may not be installed in the spawned environment. Personal subagent loadouts and named-agent guidance belong in the user's own `~/.claude/CLAUDE.md`, which the template explicitly defers to. When extending the template, prefer naming a role over naming a tool; if you want to name a specific agent, the right home is your global CLAUDE.md.

`validateInitPromptOverlap` (in `internal/config/validate.go`) backs this discipline up with a warning when a user-supplied `init_prompt` restates rules from their global CLAUDE.md. Its `strongRuleMarkers` are universal patterns (`HARD RULE`, `NEVER gh pr create / git push`, `\bglobal rule\b`) — not phrases lifted from any one user's CLAUDE.md.

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
  `openkanban ticket done` / `in-review` / `in-progress` to resolve the
  .md file authoritatively.

Several CLI subcommands are designed to be invoked from inside a spawned
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

#### PTY-activity override (`waiting` → `working`)

The hook-driven file is authoritative for "what state did the agent
just enter", but it has a known gap: between Claude Code's
`Notification` hook (permission prompt → file=`waiting`) and the
eventual `PostToolUse` hook (tool done → file=`working`), no hook
fires. A long-running tool that the user has already approved leaves
the file pinned at `waiting` for the whole duration — even though the
agent's spinner is animating and tool output is streaming.

To close that gap, the daemon timestamps every non-empty `vt.Write` on
a session's pane (`Pane.LastActivity()`), and a 2-second ticker emits
`SessionEvent{Event: "activity", LastActivityAt: ...}` whenever the
timestamp advances. The same value rides on lifecycle events
(`started`, `attached`, `detached`, `exited`) so subscribers get a
baseline before the first heartbeat lands. The status detector layers
an override on top of `DetectStatusWithPort`: when the file says
`waiting` but `LastActivityAt` is within `WaitingActivityTTL` (60s),
report `working` instead. The override is intentionally narrow —
other file states pass through untouched, and zero `LastActivityAt`
(no daemon report yet) also passes through.

The cost is bounded by the activity broadcaster's "only emit when
advanced" check: an idle session generates zero traffic. Spinner-
animating sessions emit one event per tick.

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

### `openkanban ticket in-review`

Same motion as `ticket done`, but the ticket lands in
`Status=in_review` instead of `Status=done`. Used when the agent has
finished its implementation work and is handing the ticket off to a
human reviewer — the reviewer picks it up from the board, not from
inside the (now-dead) agent session. `AgentStatus=completed`, status
file write, and daemon-side PTY termination all happen exactly as with
`ticket done`; the daemon doesn't distinguish between the two.

Idempotent on a ticket already `in_review` — no second `UpdatedAt`
drift, but the status file and daemon RPC still fire.

### `openkanban ticket in-progress`

Flips `Status=in_progress`. Unlike `ticket done` / `in-review`, this
command does NOT signal the daemon — `AgentStatus` is left untouched
and the live PTY keeps running. Use it when a session needs to flag
itself as actively working (e.g. an agent resuming from a paused
state and wanting the board to reflect that).

## Claude approval persistence across tickets

Each new openkanban ticket gets a clean worktree, and the new-session policy in `prepareSpawnWith` forces `--permission-mode plan` (so the user reviews the proposed approach before any tree mutation). The combination produced significant repeat-approval friction: a user who clicked **"Yes, and don't ask again"** for `Bash(go test *)` in one ticket would be re-prompted in the next, because Claude Code persists approvals to `./.claude/settings.local.json` — a path that, inside a worktree, dies with the worktree.

The fork wires Claude's local-settings file into the ticket lifecycle so approvals persist per-source-repo without giving up the per-ticket trust isolation:

### Seed on worktree create

After every successful `CreateWorktree` (`setupWorktree` for in-progress-on-demand, the closure inside `prepareSpawnWith` for spawn-time creation), the UI calls `agent.SeedClaudeSettings(worktreePath, proj.RepoPath)`. The helper merges `<repo>/.claude/settings.local.json` into `<worktree>/.claude/settings.local.json` — source-repo entries land in the worktree, worktree-local entries are preserved. The ticket's first Claude session opens with every approval the user has ever promoted for that source repo already in place.

A defensive `<repo>/.claude/.gitignore` (containing `settings.local.json`) is created when the repo's existing ignore stack — root `.gitignore`, nested ignores, global excludesFile — doesn't already cover `.claude/`. The local settings file holds user-specific approvals and must never be committed; the inner gitignore is a belt-and-suspenders guarantee.

### Promote on `→ in_review` / `→ done`

`project.TicketStore.Move` is the single funnel for UI-driven status transitions (drag-drop, `space` quick-move, `backspace` quick-move-back). After delegating to `SetStatus`, it calls `agent.PromoteClaudeSettingsOnTransition(t.WorktreePath, s.repoPath, oldStatus, newStatus)`. The transition gate fires only when `newStatus ∈ {in_review, done}` and `oldStatus != newStatus` — moves into `in_progress` or `backlog` are explicit no-ops. The CLI path `wrapUpSessionTicketAt` (used by `openkanban ticket in-review` and `ticket done`) routes through `store.Move` rather than `ticket.SetStatus` directly so the same promotion fires for in-session self-completion.

The newly-promoted entries surface as a UI status-bar toast — `Moved to in_review · promoted 2 approvals to repo defaults` — so silent trust escalation isn't possible. The CLI prints the equivalent line to stderr (`openkanban: promoted N claude approval(s) to repo defaults`).

### Trust boundary

Approvals collected in a ticket only escape its worktree if the user **consciously advances the ticket to in_review or done**. Tickets abandoned, archived, or deleted from the backlog never promote — exploratory `Bash(curl ...)` approvals granted during an ill-fated investigation stay confined to the throwaway worktree. The human-mediated review gate is the trust boundary, not the act of approving.

### Merge semantics

`agent.mergeSettingsLocal` is a pure additive merge over the `permissions.{allow,ask,deny}` arrays. No deletes, no reorders, no duplicates. Every other top-level key in the destination file is untouched, and unknown keys in the source are ignored. This means future Claude Code settings keys round-trip safely as long as they don't share the `permissions` name. All three helpers (`Seed`, `Promote`, `PromoteOnTransition`) are idempotent: running them twice in a row is a no-op on the second call.

### What is not changing

- `--permission-mode plan` stays forced for new sessions — design intent is unchanged.
- User-level `~/.claude/settings.json` and the openkanban-installed hook entries are not touched.
- Committed `<repo>/.claude/settings.json` is not touched.
- Errors at any layer are non-fatal: a settings-write failure logs and degrades to today's per-worktree allowlist behavior, it never blocks a spawn or a status transition.

See `internal/agent/claude_settings.go` (helpers + 28 subtests covering merge, seed, promote, transition-gate, round-trip).

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
