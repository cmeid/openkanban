<h1 align="center">
  <br>
  <img src="https://github.com/user-attachments/assets/14cde506-2091-4745-9349-2604d8ec5b32" alt="OpenKanban" width="600">
  <br>
</h1>

<h4 align="center">A TUI kanban board for orchestrating AI coding agents.</h4>

<p align="center">
  <a href="https://github.com/cmeid/openkanban/blob/main/LICENSE">
    <img src="https://img.shields.io/github/license/cmeid/openkanban?style=flat-square&color=green" alt="License">
  </a>
  <a href="https://github.com/cmeid/openkanban">
    <img src="https://img.shields.io/github/go-mod/go-version/cmeid/openkanban?style=flat-square" alt="Go Version">
  </a>
  <a href="https://github.com/TechDufus/openkanban">
    <img src="https://img.shields.io/badge/fork%20of-TechDufus%2Fopenkanban-blue?style=flat-square" alt="Fork">
  </a>
</p>

<p align="center">
  <img src="./docs/assets/demo.gif" alt="OpenKanban Demo" width="800">
</p>

---

> **This is a fork.** OpenKanban was created by [@TechDufus](https://github.com/TechDufus). This fork (`cmeid/openkanban`) extends the original with per-ticket Markdown storage, hot reload, a non-interactive CLI, a long-running daemon for multi-window state, session linking for Claude Code, and a swap of the underlying terminal emulator. It also fixes a handful of correctness bugs in the upstream pane and input layers. See [Changes vs upstream](#changes-vs-upstream) for the full diff at the conceptual level.

## What it is

AI coding agents are powerful, but managing several of them across projects gets messy — terminals everywhere, no shared view of what's running where, context-switching as a tax.

OpenKanban gives you one TUI with kanban columns. Each ticket is a git worktree with an embedded terminal. Spawn an agent into a ticket, watch it work, jump between tasks. Tickets are flat Markdown files on disk so any editor or script can read, edit, and create them.

## Changes vs upstream

This fork sits **56 commits ahead** of `TechDufus/main`. The changes fall into six themes.

### 1. Per-ticket Markdown storage (was: one JSON blob per project)

Upstream stored every ticket of a project in a single `tickets/<project_id>.json` map. Two TUIs editing different tickets in the same project could clobber each other on save; `$EDITOR` was useless because the file was machine-shaped.

This fork stores each ticket as its own Markdown file with YAML frontmatter:

```
~/.config/openkanban/tickets/<project_id>/<slug>-<uuid8>.md
```

Identity is the frontmatter `id`, so renaming the title preserves the ticket. Enum fields (`status`, `agent_status`, `agent_type`) are validated on load with loud errors instead of silent drops. Duplicate IDs are broken by mtime.

A one-shot migration runs on first launch, preserving the legacy `tickets/<project_id>.json` as `.json.migrated` for rollback. The CLI refuses to race a pending migration.

See [`internal/project/ticket_md.go`](internal/project/ticket_md.go), [`internal/project/migration.go`](internal/project/migration.go).

### 2. Hot reload

External edits to ticket `.md` files — by an agent, a CLI invocation, or `$EDITOR` — appear in the running TUI within ~100 ms. `fsnotify` watches the config dir and each project's `tickets/<id>/` subdir, events are debounced 100 ms, editor swap files are filtered, and the TUI's own writes are suppressed by `(mtime, size)` for 5 s to silence echoes. Reload errors surface as a toast and persist in `~/.config/openkanban/watch-errors.log`.

See [`internal/watch/watcher.go`](internal/watch/watcher.go), [`internal/ui/reload.go`](internal/ui/reload.go).

### 3. Non-interactive CLI (`openkanban ticket …`)

Upstream's CLI was effectively project-management only (`new` / `list` / `delete`). This fork adds a scriptable ticket lifecycle, designed for a running agent to spawn child tickets:

| Command | What it does |
|---|---|
| `openkanban ticket new` | Create a ticket; prints the `.md` path to stdout so a parent agent can capture it. Honors `--description-file` / stdin, `--labels`, `--priority`, `--status`, `--session`, `--migrate`, `--force`, `--created-by`. |
| `openkanban ticket done` | Mark the current ticket done (reads `$OPENKANBAN_TICKET_ID` injected at spawn). The TUI sees the transition and gracefully stops the pane. |
| `openkanban ticket delete` | Daemon-aware. If the daemon owns the session, sends a `KillReq` first, then unlinks the `.md`. |
| `openkanban daemon {list,stop,restart,log}` | Daemon lifecycle. |
| `openkanban hooks install` | Wires Claude Code `SessionStart` / `UserPromptSubmit` / `Stop` / `Notification` hooks into `~/.claude/settings.json`. Atomic write, timestamped backup, preserves foreign keys, dedupes by command prefix. |
| `openkanban hooks uninstall` | Inverse of `hooks install`. Strips entries recognized by the `openkanban status set ` command prefix; foreign hooks on the same events are preserved. No-op (no rewrite, no backup) when no openkanban entries are present. |
| `openkanban status set <state>` | Used by the installed hooks to drive ticket `agent_status` from inside a session. |
| `openkanban config {validate,generate,path}` | Config tooling, including a validator that warns when an agent's `init_prompt` restates rules already in `~/.claude/CLAUDE.md`. |
| `openkanban update` | Self-update from the source clone; `origin/main` check, `ff-only` pull, `go install` with `SourcePath` preserved. Also ff-fast-forwards local `main` toward `origin/main` even when run from a feature-branch worktree, so the next branch you cut doesn't start from a stale base. Diverged local `main` (has commits not on `origin/main`) is left untouched. Refuses with an actionable message when the source clone is on a non-`main` branch, detached HEAD, a linked git worktree, or not a git repo at all — and on a non-`main` branch offers an opt-in "switch to main & update" prompt rather than silently building from the wrong tree. |
| `openkanban uninstall` | Removes install artifacts only: the running binary, the openkanban Claude Code hook entries, and the legacy `~/.local/bin/update-openkanban` script if present. Data dirs (`~/.config/openkanban`, `~/.cache/openkanban`, `~/.cache/openkanban-status`) are listed in the summary but never touched, so a reinstall finds projects and config where they were. `--dry-run` previews; `-y` skips the confirmation. |

See [`cmd/`](cmd/).

### 4. Session linking for Claude Code

A spawned ticket can be tied to a specific Claude Code session UUID:

- `--session <uuid>` — link in **fork-on-spawn** mode (safe default): the ticket appends `--fork-session` so the parent session is undisturbed.
- `--session <uuid> --migrate` — resume the session in place under the ticket's ownership. Guarded by an `lsof`-style probe that refuses if another process holds the JSONL open.
- `--force` — override the `lsof` guard. Refused if the daemon currently owns the session as a live pane.

If the daemon is running, the same paths route through `daemonclient.Dial` → `KillReq` so the live pane is taken down cleanly before the session changes hands.

See [`internal/agent/sessions.go`](internal/agent/sessions.go), [`cmd/ticket.go`](cmd/ticket.go) (`applySessionFlags`).

### 5. `openkanbankd` daemon for multi-window state

Upstream is single-window: one TUI process owns its panes. This fork splits process ownership across a long-running daemon:

- **Server.** `openkanbankd` runs as a detached process auto-started on the first TUI's socket dial. Unix socket at `~/.cache/openkanban/daemon.sock`, advisory `flock` pidfile, line-delimited JSON-RPC (`Spawn` / `List` / `Kill` / `Subscribe` / `Attach` / `PrepareExit` / `Shutdown`).
- **Ownership.** The daemon owns PTYs. The TUI holds a `daemonclient.PaneView` handle whose API shape matches the old `*terminal.Pane` — UI code didn't have to change.
- **Multi-TUI.** Multiple TUI windows can view the same board and take turns being attached to a given pane. Single-attacher with snapshot redraw on attach.
- **Last-client-shutdown.** The daemon **must not outlive its last TUI**. When client count hits zero with live sessions, it tears them down defensively and exits — this is an explicit anti-tmux design choice.
- **Exit guard.** Closing the last TUI while sessions are live opens a vim-nav modal listing the sessions: `x`/`Enter` to kill selected, `X` for all, `Esc` to cancel.
- **Version skew.** If the TUI's `daemonclient` protocol version doesn't match the running daemon, the TUI starts in degraded mode and prints an actionable `daemon restart` hint instead of crashing.

See [`internal/daemon/`](internal/daemon/) (server, protocol, autostart, lifecycle) and [`internal/daemonclient/`](internal/daemonclient/) (TUI-side handle, version-skew tests).

### 6. Terminal emulator swap: `hinshun/vt10x` → `charmbracelet/x/vt`

Upstream used `hinshun/vt10x`, which is unmaintained and miscounts scrolls over long sessions — measured **46 rows divergent** vs `charm/x/vt` on the same 22-second captured PTY trace. The visible symptom was Claude Code's input bar and `AskUserQuestion` menu rendering at the wrong row inside the pane.

This fork migrates to `charm/x/vt` + `charmbracelet/ultraviolet`, which is correct and lives in the same ecosystem as the rest of the TUI stack (`bubbletea`, `bubbles`, `lipgloss`).

See [`internal/terminal/pane.go`](internal/terminal/pane.go), commit `7a787a3`.

### 7. macOS desktop notifications

When Claude Code runs directly in Ghostty / iTerm2 / Wezterm, the terminal surfaces a native desktop notification each time the agent enters a "waiting" state. Inside an openkanban-wrapped session that signal was lost — the daemon's terminal emulator consumed the OSC 9 escape sequence and the TUI's stderr (often redirected to `/dev/null` by launch wrappers) wasn't a reliable delivery channel.

This fork installs a macOS `.app` bundle at `~/Applications/OpenKanban.app` (`CFBundleIdentifier=dev.cmeid.openkanban`, `LSUIElement=true` so no Dock icon) and routes notifications through `NSUserNotification` from inside the daemon process running from that bundle. The result is a native macOS notification with the OpenKanban icon, the agent's exact text passed through 1:1, and a manageable entry under System Settings → Notifications.

The bundle is assembled by `dist/macos/build-bundle.sh` and installed alongside the binary by `scripts/install.sh`. Daemon binary lookup (`internal/daemon.ResolveBinary`) prefers the bundled path so notifications keep the OpenKanban identity even under launchd (`openkanban daemon install-service`). Toggleable via `config.behavior.forward_agent_notifications` (default on).

See [`internal/notify/`](internal/notify/), [`internal/terminal/pane.go`](internal/terminal/pane.go), and [`dist/macos/`](dist/macos/).

## Bugs fixed in upstream

Seven correctness bugs that exist on `TechDufus/main` today are fixed in this fork. Each is verified against the upstream tree.

1. **`StopGraceful` sent SIGINT, not SIGTERM.** [`pane.go:250`](https://github.com/TechDufus/openkanban/blob/main/internal/terminal/pane.go#L250) calls `proc.Signal(os.Interrupt)` despite the doc comment promising SIGTERM. Claude Code traps SIGINT as "abort current operation," so panes never closed until the 3 s SIGKILL backstop. Fix: send the real `syscall.SIGTERM`. (Commit `1cf9d1a`.)

2. **PTY size raced the child fork.** Upstream calls `pty.Start(cmd)` then `pty.Setsize(...)` milliseconds later ([`pane.go:196,207`](https://github.com/TechDufus/openkanban/blob/main/internal/terminal/pane.go)). If the child rendered its first frame in that window, layout was computed against the OS default 80×24 — bottom-anchored UI pinned to row 24 of the child's coordinate space landed at the wrong row in the actual pane. Fix: `pty.StartWithSize` (atomic with the fork). (Commit `1aff166`.)

3. **Unconditional `SetSize` on agent-view entry caused render drift.** Every entry into agent view sent a fresh SIGWINCH; Ink (which Claude Code uses) invalidates layout on SIGWINCH. Cycle leave/re-enter or open/close `AskUserQuestion` enough times and bottom-anchored UI ended up off by rows. Fix: dimension-equality short-circuit. (Commit `5f99307`.)

4. **`vt10x` scroll-counting drift corrupted cursor row over a long session.** Covered above in [Terminal emulator swap](#6-terminal-emulator-swap-hinshunvt10x--charmbraceletxvt). Commit `7a787a3`.

5. **Shift+Tab in spawned panes was delivered as plain Tab.** Upstream's `translateKey` ([`pane.go:813`](https://github.com/TechDufus/openkanban/blob/main/internal/terminal/pane.go#L813)) only handles `tea.KeyTab` and tries to detect Shift via `msg.Alt`. BubbleTea v1 parses `\x1b[Z` into a distinct `tea.KeyShiftTab` whose struct has no `Alt` field at all, so the Alt branch never fired. Reverse-tab silently broke in every embedded TUI. Fix: handle `KeyShiftTab` explicitly and emit `\x1b[Z`. (Commit `7847d24`.)

6. **Mouse Y offset wrong in agent view.** Upstream's [`handleAgentViewMouse`](https://github.com/TechDufus/openkanban/blob/main/internal/ui/model.go#L1001) passed `tea.MouseMsg.Y` straight to `pane.HandleMouse`, but BubbleTea reports host-terminal-relative coords and the pane sits 1–2 rows below the chrome. Selections and forwarded clicks were off by 1 (or 2 with the deps line). Throws off Claude Code's interactive menus. Fix: subtract the chrome height before forwarding. (Commit `b61c3c5`.)

7. **Drag-to-select was disabled whenever the child enabled mouse tracking.** When the child turned on mouse tracking (Claude does for its menus), upstream's `HandleMouse` forwarded every event to the PTY and short-circuited the selection state machine — `Cmd+C` had nothing to copy unless Shift was held, and the host terminal's native selection was disabled by the alt-screen escapes. Fix: always track selection state for left-button events regardless of `mouseEnabled`; Shift becomes an explicit forward-bypass. (Commits `b61c3c5`, `312037d`.)

A handful of additional bugs were **introduced by this fork's refactor and fixed in the same branch** (per-ticket migration race, owned-session JSONL leak on delete, `init_prompt` referencing non-existent ticket `.md`s, column-overflow math). They are tracked in commits `6a6acf1`, `0875ff9`, `58784e4` / `8e2b647`, and `7d4e8de` respectively. They are not upstream bugs — they only matter as honest framing for the fork's history.

## Install

### From source (recommended)

```bash
git clone https://github.com/cmeid/openkanban.git
cd openkanban
./scripts/install.sh
```

`scripts/install.sh` checks prerequisites, builds and `go install`s the binary into `$GOBIN`, and — if Claude Code is detected — offers to wire session-status hooks into `~/.claude/settings.json`. Idempotent; safe to re-run. (`scripts/install.sh` is fork-only; upstream doesn't ship one.)

> ℹ️ **Use the script, not bare `go install .`.** The install script injects three ldflags (`SourcePath`, `Commit`, `BuildMarker`) that the binary depends on at runtime. A bare `go install .` skips them; the resulting binary refuses to run anything except `openkanban version` and prints a hint pointing back at the script. See [`docs/INSTALL.md`](docs/INSTALL.md#why-scriptsinstallsh-and-not-bare-go-install) for the rationale.

Every launch checks `origin/main` for newer commits and prompts before applying: **Enter** to update + relaunch, **Esc** to skip, **Q** to quit. Disable with `--no-update-check` or `behavior.check_for_updates_on_launch: false` in `~/.config/openkanban/config.json`. The launch-time check also surfaces every status on stderr ("up to date", "ahead", "diverged", refusals) so it's never silent. When the source clone is parked on a non-`main` branch, the prompt instead offers to switch back to `main` first; detached HEAD / linked-worktree / non-git-repo source clones refuse with an actionable message rather than building from the wrong tree.

You can also update on demand: `openkanban update --check` to print status, `openkanban update` to pull + rebuild + reinstall.

To remove openkanban, run `openkanban uninstall`. It removes the binary, the Claude Code hook entries, and the legacy `~/.local/bin/update-openkanban` script if present. Data directories (`~/.config/openkanban`, `~/.cache/openkanban*`) are listed in the closing summary but never touched — a reinstall finds projects and config exactly where they were. Pass `--dry-run` to preview or `-y` to skip the prompt. See [`docs/INSTALL.md`](docs/INSTALL.md) for prerequisites, troubleshooting, and manual data cleanup.

### `go install` (upstream binary)

```bash
go install github.com/techdufus/openkanban@latest
```

> ⚠️ This installs **upstream's** latest tagged release, **not this fork**. The Go module path in this fork is still `github.com/techdufus/openkanban` to preserve import compatibility, but `@latest` resolves through the upstream remote. To get the fork's behavior, use the source install above.

## Quick start

From a fresh clone to a running board in four commands:

```bash
git clone https://github.com/cmeid/openkanban.git
cd openkanban && ./scripts/install.sh   # build + install + (optional) Claude Code hooks
cd ~/projects/my-app                    # any git repo you want to track
openkanban new "My App"                 # register it as an openkanban project
openkanban                              # launch the TUI
```

Create tickets from inside the TUI with `n`, or from the command line — useful when a running agent wants to spin off a subtask:

```bash
openkanban ticket new --project "My App" --title "Add login flow"
# prints the path of the resulting .md file to stdout
```

## Tickets on disk

```
~/.config/openkanban/tickets/<project_id>/<slug>-<uuid8>.md
```

Each file is self-contained: status, labels, branch, and metadata in YAML frontmatter; description in the body. Edit a ticket in any editor and the running TUI picks up the change automatically:

```bash
vim ~/.config/openkanban/tickets/$PROJECT/wire-fsnotify-7f3a9b2c.md
# change `status: backlog` to `status: in_progress`, save
# the TUI moves the card to the In Progress column within ~150ms
```

If you have a config from before this version, your existing `tickets/<project_id>.json` is auto-migrated on first launch and preserved as `.json.migrated` for rollback.

## Agent self-completion

When openkanban spawns an agent into a ticket, the child can mark the ticket done and exit cleanly:

```bash
openkanban ticket done
```

This sets `status: done`, stamps `completed_at`, sets `agent_status: completed`, and signals the TUI to gracefully stop the pane (SIGTERM, 3 s grace, SIGKILL). The agent exits, the card lands in Done, no manual `/quit` or column-move required.

Worktrees and branches are preserved on `done`. Only `openkanban ticket delete` tears them down. See [`docs/AGENT_INTEGRATION.md`](./docs/AGENT_INTEGRATION.md#agent-callable-commands-in-session) for env vars, status-file mechanics, and Claude Code `Stop` hook interaction.

## Daemon

`openkanbankd` is auto-started on first TUI launch and exits when the last TUI closes. You normally never interact with it.

```bash
openkanban daemon list      # what sessions are alive
openkanban daemon log       # tail the daemon log
openkanban daemon restart   # restart (prompts before killing live sessions; --force to skip)
openkanban daemon stop      # clean shutdown
```

Override the socket path with `OPENKANBAN_DAEMON_SOCK` if you need to.

The daemon will **never** outlive the TUI as a tmux-style detached session. If you want kanban state to persist across reboots, the on-disk `.md` files are the source of truth — relaunch the TUI and respawn the agents.

## Keybindings

| Key | Action |
|-----|--------|
| `j/k` | Navigate tickets up/down |
| `h/l` | Navigate between columns |
| `space` | Move ticket to next column |
| `n` | New ticket |
| `s` | Spawn agent |
| `enter` | Attach to agent |
| `o` | Cycle sort order (default → name → age → priority) |
| `w` | Toggle session filter (all ⇄ open) |
| `?` | Full help |

## Configuration

OpenKanban is heavily configurable. Agents, keybindings, branch naming, cleanup behavior — all customizable in `~/.config/openkanban/config.json`.

See [Configuration Guide](./docs/CONFIGURATION.md) for the full reference, [Data Model](./docs/DATA_MODEL.md) for the on-disk layout, and [Architecture](./ARCHITECTURE.md) for the system overview.

## Architecture notes

Each agent pane is backed by a real PTY (`creack/pty`) wrapped with a virtual terminal emulator (`charmbracelet/x/vt`) that parses every escape sequence the agent emits, maintains the screen + scrollback state, and lets openkanban render it inside the TUI.

Since the daemon split (PR7 in this fork), PTYs are owned by `openkanbankd`, not the TUI process. The TUI holds a thin `daemonclient.PaneView` that mimics the old `*terminal.Pane` API. See [`internal/terminal/CLAUDE.md`](internal/terminal/CLAUDE.md) and [`internal/daemon/`](internal/daemon/).

The emulator choice and rationale for moving off `hinshun/vt10x` live in [`docs/AGENT_INTEGRATION.md#architecture-terminal-emulator`](./docs/AGENT_INTEGRATION.md#architecture-terminal-emulator).

## License & attribution

[AGPL-3.0](LICENSE), inherited from [TechDufus/openkanban](https://github.com/TechDufus/openkanban). All upstream copyright applies; this fork's changes are likewise AGPL-3.0.
