# UI Package

BubbleTea-based terminal UI with vim-style navigation.

## Model Structure

Single `Model` struct implements `tea.Model`:
- `Init()` - startup commands
- `Update(msg) (Model, Cmd)` - message handling
- `View() string` - render output

## Mode State Machine

`Mode` type controls behavior routing:
```go
type Mode string
const (
    ModeNormal    Mode = "normal"
    ModeAgentView Mode = "agent_view"
    ModeSettings  Mode = "settings"
    // ...
)
```

Key handlers dispatch by mode: `handleNormalMode()`, `handleAgentViewMode()`

## Key Bindings

Vim-style navigation:
- `h/j/k/l` - movement
- `g/G` - jump to start/end
- `n` - new item (in_review/done columns route the new ticket to in_progress)
- `d` - delete
- `Enter` - select/confirm
- `Esc` - cancel/back

Inside ModeAgentView, these keys are intercepted before the PTY child (claude, etc.) sees them:
- `Ctrl+]` / `Ctrl+\` - cycle focus to next / prev open, unattached session
- `Ctrl+g` - exit back to the board
- `Enter` - **conditionally** intercepted via `shouldRetryAttachOnEnter`: only when the focused pane has `LastAttachErr() != nil` AND `State() != PaneViewAttached`. In that state Enter retries `attachExisting`; otherwise it falls through to the PTY child as normal. The predicate is gated the same way as `PaneView.View()`'s failure-overlay branch — keep them locked together, otherwise the user sees the "Enter retries" hint but pressing Enter does nothing.

`Ctrl+[` cannot be used: in bubbletea v1.3.x (no Kitty keyboard protocol enabled here) it is bytewise indistinguishable from `Esc`. Any new ctrl-combo binding should be verified against `~/golang/pkg/mod/github.com/charmbracelet/bubbletea@<ver>/key.go` before promising it.

The cycle-attach modal renders OVER the focused pane's agent view (chrome stays visible behind), via `renderAgentViewWithCycleModal`. Do not switch it back to `renderWithOverlay`, which uses a blank background and hides the state needed to make the cycle decision. `cycleUnattachedSession` auto-attaches the target peer if it's Unattached so the modal backdrop shows live PTY content (not just chrome); the cycle iterates ALL open peers, not just Unattached ones.

## View Composition

Separate render methods composed in `View()`:
- `renderHeader()`, `renderBoard()`, `renderColumn()`
- `renderTicket()`, `renderStatusBar()`

## Styling

All styling via lipgloss with theme-based `uiColors` struct.
Never use raw ANSI codes in UI rendering.

## Messages

Custom messages for async operations:
```go
type spawnReadyMsg struct {...}
type agentStatusMsg struct {...}
```

Return `tea.Cmd` from `Update()` for async work.

## Column Viewport Scopes

Vertical scroll per column lives in `m.columnOffsets[i]`. Three functions touch it, each with a different scope — don't confuse them:

- `refreshColumnTickets` (model.go) rebuilds `m.columnTickets` (all columns) — the chokepoint that every filter mutator AND ticket move flows through. Its last step calls `compactColumnOffsets`.
- `compactColumnOffsets` walks **all columns** and reduces stale offsets so filtered columns fill the screen instead of stranding cards behind `▲ N more`. Only reduces — never pushes the user down.
- `ensureTicketVisible` operates on the **active column only**, scrolling to keep `m.activeTicket` in view (used on cursor move and `selectTicketByID`).

Card-height arithmetic in any path that runs *inside or after* `refreshColumnTickets` (and before the next render) must use the `ticketHeight` constant — NOT the `columnTicketHeights` cache. The cache is keyed to pre-refresh indices; after a filter shifts the ticket list, index `j` likely points at a different card. Reading it post-refresh is actively wrong, not just stale.

## Terminal Panes

`panes map[board.TicketID]*daemonclient.PaneView` — one per spawned agent.

PaneView is the client-side handle; the PTY itself lives in openkanbankd. Lifecycle is daemon-driven: `Spawn` happens server-side at construction time, `Attach` / `Detach` swap which TUI is the one attached client, and `daemonclient.PaneViewAttached` vs `PaneViewUnattached` describe what this TUI sees, not whether the agent is alive (the agent can be alive in the daemon while every TUI is `Unattached`). Methods preserve the old `*terminal.Pane` surface — see `internal/daemonclient/paneview.go` for the full 13-method shape and the unattached-state behavior table.

### Attach-failure overlay

When `attachWithRetry` (post-spawn or B4 fast-path) exhausts its retries, the closure calls `pv.SetLastAttachErr(err)` before returning the `spawnReadyMsg`. `PaneView.View()` then renders an actionable overlay instead of `blankPaneView` — same `cols × rows` contract so the chrome composition doesn't shift, pure ASCII so byte count == display cell count. Successful `Attach()` clears `lastAttachErr` automatically, so the overlay disappears on the next View() pass. The `shouldRetryAttachOnEnter` predicate (see Key Bindings above) gates Enter-retry on the SAME state pair, so the overlay's "Enter retries" hint is actually wired up.

## Pull-back chooser

The brief-chooser modal in `spawnAgent` fires on TWO signals (gate is `wouldChange || pulledBack`):

1. `wouldChange == true` (existing trigger) — the openkanban card's Description has diverged from the on-disk `<worktree>/tickets/<slug>.md` managed block.
2. `pulledBack == true` (new) — `ticket.StatusChangedAt.After(*ticket.AgentSpawnedAt)`. The user explicitly moved the card back into `in_progress` AFTER the prior session ran, e.g. drag-back from `in_review` or `done`. The message is context-sensitive: "pulled back" vs "brief was updated" vs combined.

Why the second signal exists: a shipped, pulled-back ticket typically has an empty Description (work is done) and no in-repo brief file, so `wouldChange=false`. Without the `pulledBack` arm, launching it silently `--resume`s the prior (often `/exit`-ed) JSONL with no agency over resume-vs-fresh. Routine re-attach (Ctrl+g → re-enter on the same in_progress card) doesn't trip this because `StatusChangedAt` is unchanged in that flow — confirmed by the negative-control test.

The three choice closures (`d`/`u`/`n` → `spawnPlan{ForceFresh}`/`{InjectResumeNotice}`/`{SkipMerge}`) are unchanged; only the trigger condition and the modal message vary.

## Status-mutation wrap-up

When the user moves a ticket OUT of `in_progress` to a terminal status (`in_review` or `done`) via the **board** (quick-move keys or drag), `wrapUpSessionForTicket` runs BEFORE `m.globalStore.Move(...)`. It mirrors the CLI's `cmd/ticket_done.go:wrapUpSessionTicketAt`: stops the local pane, sends `TicketDone` to the daemon to terminate the live PTY, and stamps `AgentStatus=Completed` via `SetAgentStatus`. The pre-Move ordering matters — the helper's gate ("is the ticket leaving in_progress?") reads the **current** status, which `Move`'s call to `SetStatus` mutates in place.

This closes the historical asymmetry where the CLI tore down sessions on transition but the TUI didn't — leaving a live daemon PTY whose ticket's status no longer matched.

Two seams worth knowing about:

- **`daemonGuardAPI` interface** (`exit_guard.go`) — extended with `TicketDone(ctx, ticketID)` so UI tests can substitute a fake without spinning up a real daemon. New daemon RPCs needed from UI code should be added here for the same reason.
- **`handleDaemonSessionEvent("exited")` conditional clear** (`daemon_subscribe.go`) — clears `ticket.AgentSessionID` / `AgentSpawnedAt` only when `ev.Expected == true`. Unexpected exits (daemon crash, transient PTY tear-down) deliberately preserve the residue so `--resume` can still pick up a JSONL that's still on disk. The persistence work in commit `c718699` (`feat(session): persist UUID, prefer --resume`) is what makes this matter — clearing on every exit would un-do it.

## tea.Cmd goroutines must not touch shared Model state

Returning a `tea.Cmd` from Update causes the framework to run it in a goroutine. That goroutine can run concurrently with subsequent Update calls, so it MUST NOT touch state that Update mutates — in particular `m.projectRegistry` and `m.globalStore`. Reading them is also a race if Update writes them (e.g. `handleFsChanged`'s `projectRegistry.ReloadFromDisk`, ticket-creation paths).

Discipline:

- **Goroutine:** read-only filesystem work; load registry/state from disk into *local* fresh copies (e.g. `project.LoadRegistry()`).
- **Update handler:** the only place that mutates `m.projectRegistry`, `m.globalStore`, `m.panes`, etc.

The race detector only catches observed concurrency. A test that drives the cmd synchronously (`cmd()` inline) will miss the race. See `board_resync.go` for the canonical shape — goroutine loads its own `*ProjectRegistry`; the handler reloads the model's registry. `daemon_resync.go` follows the same rule: its goroutine reads only `api` (externally synchronized) and never touches `m`.

## Anti-Patterns

- Don't block in Update() - use Cmd for async
- Don't render directly in Update() - only in View()
- Don't store computed strings - recompute in View()
- Don't access panes without nil check
- Don't mutate `m.*` from a tea.Cmd goroutine — see the section above
