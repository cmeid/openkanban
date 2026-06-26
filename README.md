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
---

> **This is a fork.** OpenKanban was created by [@TechDufus](https://github.com/TechDufus). This fork (`cmeid/openkanban`) extends the original with per-ticket Markdown storage, hot reload, a non-interactive CLI, a long-running daemon for multi-window state, session linking for Claude Code, and a swap of the underlying terminal emulator. It also fixes a handful of correctness bugs in the upstream pane and input layers. See [Changes vs upstream](#changes-vs-upstream) for the full diff at the conceptual level.

## What it is

AI coding agents are powerful, but managing several of them across projects gets messy — terminals everywhere, no shared view of what's running where, context-switching as a tax.

OpenKanban gives you one TUI with kanban columns. Each ticket is a git worktree with an embedded terminal. Spawn an agent into a ticket, watch it work, jump between tasks. Tickets are flat Markdown files on disk so any editor or script can read, edit, and create them.

**New here?** Start with the [Getting Started guide](docs/GETTING_STARTED.md).

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
| `openkanban ticket new` | Create a ticket. Prints an `id=<uuid>` line and then the `.md` path (path stays the final line for back-compat). `--json` emits a stable object `{id, path, slug, status, project_id, worktree_path, branch_name, base_branch}`. `--worktree` provisions the git worktree + branch now (same derivation the TUI uses at spawn, so spawn reuses it) — distinct from the lazy `--no-worktree` hint. Also honors `--description-file` / stdin, `--labels`, `--priority`, `--status`, `--session`, `--migrate`, `--force`, `--created-by`. |
| `openkanban ticket list` | Enumerate tickets across all projects. Filters: `--project`, `--status` (comma/repeatable), `--title-contains`. Default is a human table (short id, title, status, project, updated); `--json` emits a stable array (every key present, `labels` always an array, RFC3339 timestamps). Read-only — skips migration-pending projects (noted on stderr) rather than migrating them. The canonical id-discovery path. |
| `openkanban ticket done` | Mark the current ticket done (reads `$OPENKANBAN_TICKET_ID` injected at spawn). The TUI sees the transition and gracefully stops the pane. |
| `openkanban ticket delete` | Daemon-aware. If the daemon owns the session, sends a `KillReq` first, then unlinks the `.md`. `--id` accepts the full id, a unique 4+ char id prefix (or filename short-hash), or a unique title slug; passing a **project** id by mistake returns a hint pointing at `ticket list` instead of a bare "not found". |
| `openkanban daemon {list,stop,restart,log}` | Daemon lifecycle. |
| `openkanban hooks install` | Wires Claude Code `SessionStart` / `UserPromptSubmit` / `Stop` / `Notification` hooks into `~/.claude/settings.json`. Atomic write, timestamped backup, preserves foreign keys, dedupes by command prefix. |
| `openkanban hooks uninstall` | Inverse of `hooks install`. Strips entries recognized by the `openkanban status set ` command prefix; foreign hooks on the same events are preserved. No-op (no rewrite, no backup) when no openkanban entries are present. |
| `openkanban status set <state>` | Used by the installed hooks to drive ticket `agent_status` from inside a session. |
| `openkanban config {validate,generate,path}` | Config tooling, including a validator that warns when an agent's `init_prompt` restates rules already in `~/.claude/CLAUDE.md`. |
| `openkanban update` | Self-update from the source clone; `origin/main` check, `ff-only` pull, `go install` with `SourcePath` preserved. Also ff-fast-forwards local `main` toward `origin/main` even when run from a feature-branch worktree, so the next branch you cut doesn't start from a stale base. Diverged local `main` (has commits not on `origin/main`) is left untouched. Refuses with an actionable message when the source clone is on a non-`main` branch, detached HEAD, a linked git worktree, or not a git repo at all — and on a non-`main` branch offers an opt-in "switch to main & update" prompt rather than silently building from the wrong tree. |
| `openkanban uninstall` | Removes install artifacts only: the running binary, the openkanban Claude Code hook entries, and the legacy `~/.local/bin/update-openkanban` script if present. Data dirs (`~/.config/openkanban`, `~/.cache/openkanban`, `~/.cache/openkanban-status`) are listed in the summary but never touched, so a reinstall finds projects and config where they were. `--dry-run` previews; `-y` skips the confirmation. |
| `openkanban backup` | Snapshot all openkanban config (everything under `~/.config/openkanban/`) plus each registered project's `<RepoPath>/tickets/` directory into a timestamped zip. Default output `~/backup/openkanban/openkanban-<ts>.zip`; `--output` accepts a directory or a `.zip` path. `--dry-run` previews; `-y` skips the confirmation. Warns if `config.json` contains non-empty `agents.*.env` (potential plaintext secrets in the archive). |
| `openkanban restore <archive>` | Apply a backup zip. Per-file diff-and-prompt for conflicts; identical files are skipped without touching mtime. `--on-conflict=skip\|overwrite\|prompt` (default `prompt`); `-y` skips the initial confirmation only. Path-traversal-safe (zip-slip defended). When a project's `RepoPath` is missing on this machine, prompts to abort or skip — `projects.json` is restored verbatim either way. |

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

### 8. Claude Code approvals persist across tickets

Upstream spawns each ticket's agent in a fresh worktree with an empty `.claude/settings.local.json`. Tools the user has approved in previous tickets ("Yes, and don't ask again") don't carry over — every new ticket re-prompts for `Bash(go test *)`, `Skill(...)`, the same handful of repeats. The fork's new-session policy forces `--permission-mode plan`, which makes the re-approval friction worse, not better.

This fork wires Claude's local-settings file into a per-source-repo lifecycle:

- **On worktree create**, openkanban merges entries from `<repo>/.claude/settings.local.json` into the new `<worktree>/.claude/settings.local.json`. The ticket starts pre-approved for everything you've ever promoted in this repo.
- **On `→ in_review` / `→ done` transition**, openkanban does the reverse merge: any approvals the ticket's agent collected get promoted into the source repo's file so the next ticket inherits them. Abandoned or archived tickets that never reach in-review/done don't pollute the per-repo defaults — the trust gate is human-mediated.
- Merges are additive and idempotent; `permissions.{allow,ask,deny}` are all covered. A defensive `<repo>/.claude/.gitignore` is auto-written when the repo's existing ignore stack doesn't already cover `.claude/`, so the file can never be committed.

A status-bar toast surfaces how many approvals just went global (`Moved to in_review · promoted 2 approvals to repo defaults`) so silent trust escalation isn't possible.

**Auto-prune of stale entries.** On every ticket transition (regardless of direction), openkanban also runs a noise-filter pass over `<repo>/.claude/settings.local.json` and removes entries that look one-shot — long no-glob Bash commands with embedded timestamps, escape-soup grep patterns, absolute paths outside an allowlist of trusted locations (`~/manifold/dev/**`, `~/.claude/projects/**`, etc.). A hard-deny list also blocks any auto-introduction of high-risk patterns (`Bash(git push *)`, `Bash(gh pr create *)`, `Bash(op *)`, paths under `~/.ssh/**` / `~/.aws/**`) — those collide with the global push-gate rule in `~/.claude/CLAUDE.md` and the 1Password CLI's secret-management surface. Every removal is appended to `<repo>/.claude/.pruned-log` with a timestamp + reason, and the pre-write file is snapshotted to `settings.local.json.bak.<unix-nanos>` (rotation keeps the last 3) so any false-positive prune is recoverable. The toast extends with `· pruned N stale entries`. **Verb-widening is explicitly out of scope** — collapsing `Bash(awk '/2026-.../' log)` to `Bash(awk *)` would auto-approve `awk 'BEGIN{system(...)}'`, which the global push-gate's threat model relies on the user denying per-call. Users who want explicit widening can do it by hand in the repo file.

This promotion is **independent of the standardized close-out** (the `finishing-an-openkanban-ticket` skill): promotion fires only on the human-driven `→ in_review` / `→ done` status transition, and the close-out skill never changes ticket status. So an agent landing its work via the skill's commit → PR → merge never triggers (or bypasses) the trust-gate — the two mechanisms are orthogonal.

See [`internal/agent/claude_settings.go`](internal/agent/claude_settings.go) and wiring in [`internal/ui/model.go`](internal/ui/model.go), [`internal/project/tickets.go`](internal/project/tickets.go), [`cmd/ticket_done.go`](cmd/ticket_done.go).

### 9. PTY-activity overrides "waiting" state

Claude Code emits an OSC 9 notification when it enters a permission-prompt state — the file-based status detector picks that up and renders the ticket card as **waiting**. After the user approves, the prompt clears and the tool runs, but no hook fires until `PostToolUse` returns N seconds later. During that gap the spinner is animating and the agent is doing autonomous work, but the card still reads **waiting**. The status is misleading exactly when the user most needs to know what's happening.

This fork closes the gap with a PTY-activity heartbeat:

- **Daemon side.** `terminal.Pane` timestamps every non-empty `vt.Write` — bytes-flowed, not grid-hash. (Cursor blinks are terminal-side; a displayed prompt then sits quiet — though its *initial render* is a byte burst, which is what the prompt guard below exists to handle; the spinner emits ~10 Hz throughout tool execution.) A 2-second broadcaster ticks `SessionEvent{Event:"activity", LastActivityAt:...}` only when the timestamp advanced, so idle sessions produce zero traffic. The same field rides on `started`/`exited`/`attached`/`detached` events so subscribers seed before the first heartbeat.
- **UI side.** `DetectStatusWithActivity` layers an override on top of `DetectStatusWithPort`: when the status file reads `waiting` but the session had PTY activity within the last 60 seconds (`WaitingActivityTTL`), the card renders **working** instead. Other states (idle, completed, error) pass through untouched.
- **Prompt guard.** Rendering the permission prompt is *itself* a burst of PTY bytes, landing at the same instant the `Notification` hook writes `waiting` — so the raw override would mask a genuinely-blocked session as **working** for the entire approve-within-60s window (i.e. almost always). `DetectStatusWithActivity` therefore suppresses the override while the approval prompt is still on screen: `permissionPromptVisible` matches the prompt's own text in the tail of the pane (`do you want to…` / `esc to cancel` — narrower than the generic keyword list, so a running tool's output won't trip it). Unlike the 60s timer, the on-screen text holds for the whole wait and clears the moment the user answers and the tool starts streaming.
- **Busy guard (the inverse).** The file is *also* pinned at `waiting` through the whole run of an already-approved tool. The activity heartbeat bridges this for a streaming tool, but a **silent** one — e.g. a quiet `go test` in a Bash tool, where Claude shows the command's output region instead of its own spinner — emits no bytes, so the card sat at `waiting` with nothing for the user to do. `activeTurnVisible` lifts that to `working` when the live screen shows an active-turn marker (`esc to interrupt`, or a braille spinner glyph) and no prompt. It is ordered strictly **after** the prompt guard, so an on-screen prompt always wins — the marker set is mutually exclusive with a prompt in Claude's UI, and if the footer string ever drifts the check fails *safe* (back to showing `waiting` while busy, never hiding a needs-you).
- **Backward compat.** `SessionEvent.LastActivityAt` is `omitempty`, so older clients that don't understand the field just see today's events.

The net effect: while the approval prompt is up the card reads **waiting**; once Claude is busy on a granted tool — streaming *or* silent — it reads **working**, so a `waiting` card always means the session genuinely needs you.

See [`internal/terminal/pane.go`](internal/terminal/pane.go) (`LastActivity`, write-timestamping), [`internal/daemon/server.go`](internal/daemon/server.go) (`broadcastActivity`), [`internal/agent/status.go`](internal/agent/status.go) (`DetectStatusWithActivity`, `WaitingActivityTTL`), and [`internal/ui/daemon_subscribe.go`](internal/ui/daemon_subscribe.go) / [`internal/ui/model.go`](internal/ui/model.go) (`m.lastPTYActivity` map + override wiring).

### 10. Directory-independent session resume

Claude Code stores each session transcript in a project bucket keyed to the directory the session was *started* in (`~/.claude/projects/<encoded-cwd>/<uuid>.jsonl`, where the CLI encodes the cwd by replacing every non-alphanumeric character with `-`), and `claude --resume <uuid>` only searches the bucket for the current directory and that repo's worktrees — there's no flag to resume a session from elsewhere. A session openkanban *creates* always starts in the ticket's worktree, so it resumes cleanly. But a session you started manually in some other directory and then linked to a ticket (`ticket new --session`) lives in a different bucket than the one openkanban launches the ticket from, so resuming it reported `No conversation found with session ID`.

This fork makes resume directory-independent by normalizing the transcript into the launch directory's bucket on every spawn:

- Before launching, `agent.NormalizeSessionBucket` relocates `<uuid>.jsonl` and its sibling `<uuid>/` artifact directory (subagent transcripts, tool-results) into the bucket for the directory openkanban is about to launch from.
- It's idempotent (a no-op in the common case where the session already lives there), skips any session a live process is holding open (so it never yanks a transcript out from under a running `claude`), refuses to overwrite a same-UUID collision, and moves the `.jsonl` lookup key last.
- It's non-fatal: a failure logs and degrades to the prior behavior rather than blocking the spawn. A relocation surfaces a status-bar toast.

The invariant: a ticket's transcript always lives in its launch directory's bucket, so reopening a ticket resumes the exact session regardless of where it — or openkanban — was originally started.

See [`internal/agent/sessions.go`](internal/agent/sessions.go) (`ProjectDirFor`, `NormalizeSessionBucket`) and the call site in [`internal/ui/model.go`](internal/ui/model.go) (`prepareSpawnWith`).

### 11. 1:1 ticket↔session enforcement

The daemon enforces 1:1 ticket↔session at the PTY layer (`Spawn` is idempotent per `TicketID`), but the Claude session UUID layer was permissive: two tickets could end up linked to the same UUID via `ticket new --session <already-claimed>`, divergent forks via `--fork-session` on every re-spawn, or the post-spawn back-fill writing the same UUID across tickets. The result was silent data/session loss — re-spawning a ticket landed in a stale fork, not the live conversation.

This fork closes the gap with three layered defenses:

- **Creation gate** (`ticket new --session`): refuses to claim a UUID already linked to a different ticket. `--force` claims by clearing the conflicting ticket's `agent_session_id` first.
- **Back-fill gate** (post-spawn UUID discovery via `FindClaudeSession`): silently no-ops when the discovered UUID is already claimed by a different ticket. No save, no `~/.claude/history.jsonl` purge.
- **Forking eliminated entirely**: `--fork-session` is no longer appended to any Claude spawn argv. A build-time grep guard at `internal/ui/forksession_guard_test.go` makes re-introduction structurally impossible.
- **Daemon `handleOwns` multi-match**: instead of silently returning the first match, the daemon now surfaces `Conflict=true` with all matching session IDs so upper layers refuse to route to one arbitrarily.

The shared funnel is `internal/ticketsvc`: a small package of free functions (`LinkSession`, `GateAttach`) that both TUI and CLI call for any `agent_session_id` mutation. Storage tolerates duplicates by policy (existing on-disk duplicates aren't auto-migrated), but the runtime gates ensure no NEW duplicate ever launches a session. `openkanban ticket in-progress` also routes through `TicketStore.Move` now, closing a pre-existing harmonization gap where the CLI verb bypassed the promote/prune side-effects the UI was firing.

See [`internal/ticketsvc/svc.go`](internal/ticketsvc/svc.go), [`internal/agent/CLAUDE.md`](internal/agent/CLAUDE.md), and the bug ticket `enforce-1-1-ticket-session` for the multi-vector analysis.

### 12. Per-project Claude selection (work vs personal)

If you run more than one Claude Code install — e.g. a work account and a personal one, distinguished only by `CLAUDE_CONFIG_DIR` — OpenKanban can launch the right one per project. Two presets ship by default, **Claude (Default)** and **Claude (Custom)**; the custom one carries a `CLAUDE_CONFIG_DIR` env you point at your alternate config dir (any agent's `env` is now injected at spawn, with a leading `~/` expanded).

Selection is **per project and nowhere else** — there is no per-ticket or global agent picker. Focus a project in the sidebar and press `g` to cycle its pinned agent; the pin shows under the project name and governs every spawn (including `Ctrl+Space` background spawns). A project with no pin **refuses to spawn** with an actionable message — so you can't accidentally launch the wrong Claude as long as you stay in your projects. See [Configuration → Agents](./docs/CONFIGURATION.md#agents).

You can also set a **per-project model** (claude only): open the project editor with `e`, tab to the **Model** field, and press `←`/`→` to cycle the presets (`opus`/`opusplan`/`sonnet`) or type any full model ID. OpenKanban passes `--model <value>` to the `claude` CLI on every spawn for that project. Leave it blank to let claude use its own configured default. See [Configuration → Per-project model override](./docs/CONFIGURATION.md#per-project-model-override-claude-only).

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
openkanban daemon start     # start it detached in the background (no-op if already running)
openkanban daemon restart   # restart (prompts before killing live sessions; --force to skip)
openkanban daemon stop      # clean shutdown (prompts before killing live sessions; --force to skip; no-op if already stopped)
```

`daemon start` returns immediately — it backgrounds the daemon (preferring the launchd service when installed) rather than holding it in the foreground like bare `openkanban daemon` (the internal entry point launchd/autostart use). `daemon restart` works whether or not a daemon is already running: if one is up it is shut down first, and either way `restart` ends with a fresh daemon running.

Override the socket path with `OPENKANBAN_DAEMON_SOCK` if you need to.

The daemon will **never** outlive the TUI as a tmux-style detached session in default mode. If you want kanban state to persist across reboots, the on-disk `.md` files are the source of truth — relaunch the TUI and respawn the agents. Persistent ([launchd-managed](./docs/AGENT_INTEGRATION.md#lifecycle-default-vs-persistent)) mode is opt-in via `openkanban daemon install-service`.

### Knowing the daemon's version

There is no `openkanban daemon version` command — the daemon reports its build info on every client handshake (`HelloResp.BinaryVersion`, see [`internal/daemon/protocol.go`](./internal/daemon/protocol.go)), but the easiest way to inspect it is the log:

```bash
openkanban daemon log | head    # startup banner includes binary version + commit
```

`openkanban version` reports the CLI binary on `$PATH`. If the daemon was started from an older binary, the two can drift for the lifetime of that daemon — see below.

### Updates and the running daemon

`openkanban update` rebuilds and reinstalls the binary on disk. It does **not** stop or restart the running daemon. The daemon polls its own on-disk binary (`watchBinaryStaleness` in [`internal/daemon/server.go`](./internal/daemon/server.go)) and reacts based on whether sessions are attached:

| State when binary changes | Daemon behavior |
|---|---|
| Zero live sessions | Exits immediately; next TUI launch autostarts a fresh daemon on the new binary. |
| Live sessions, default mode | Logs a `WARN` and keeps running; picks up the new binary on the next last-client-disconnect (which already terminates sessions). |
| Live sessions, persistent mode | Logs a `WARN` and keeps running indefinitely. Pick up the new binary with `openkanban daemon restart` once your sessions are expendable. |

Normal flow: `openkanban update` → finish your work → close the TUI → the daemon auto-recycles on the next launch. No explicit daemon command needed in default mode.

The handshake reports both `ProtocolVersion` and `BinaryVersion`. The daemon does not refuse mismatched clients at the protocol level — `daemonclient` warns and degrades to "no agent spawn" mode instead of crashing (see [version-skew note above](#5-openkanbankd-daemon-for-multi-window-state)).

### Forcing a daemon refresh with live sessions

`openkanban daemon restart` terminates the daemon, which SIGTERMs all live PTYs with a 3 s grace then SIGKILL — running agent sessions die. Use it only when you actually want the new daemon binary right now and the running sessions are expendable. The interactive prompt protects you by default; `--force` skips it.

## Backup & restore

`openkanban backup` produces a single zip containing everything you'd need to reconstruct an openkanban setup on a fresh machine (modulo `go install` of the binary itself):

- `~/.config/openkanban/` in full — `config.json`, `projects.json`, every per-ticket `.md`, archived tickets, anything else you've left under that directory.
- For each registered project, `<RepoPath>/tickets/` — the canonical, committed-or-uncommitted ticket briefs that live alongside the repo (see [`feedback_openkanban_store_volatile`](https://github.com/cmeid/openkanban) for why these matter — the volatile store is operational state, not canonical).

What it deliberately does **not** capture: the openkanban binary itself (reinstall via the source clone), Claude Code hook entries in `~/.claude/settings.json` (`openkanban hooks install` recreates them), the launchd plist if you ran `openkanban daemon install-service` (host-specific paths; restore reminds you to re-run that), and the ephemeral cache dirs under `~/.cache/openkanban*` (sockets, pidfiles, logs — all recreated on next launch).

```bash
openkanban backup                    # writes ~/backup/openkanban/openkanban-<ts>.zip
openkanban backup --dry-run          # preview, no archive written
openkanban backup --output ~/snap/   # alternate directory, auto-named inside
openkanban backup --output /tmp/foo.zip  # exact path
```

The archive contains real paths and your project list — if that's sensitive, pipe through `gpg` (no built-in encryption by design).

`openkanban restore <archive>` is symmetric and safe-by-default: every conflict is surfaced before bytes are written.

```bash
openkanban restore ~/backup/openkanban/openkanban-20260616-094530.zip
```

Per-file conflict handling:

- **Identical files** (byte-for-byte match with the archive entry) — skipped silently, mtime preserved.
- **Different files** — interactive `git add -p`-style prompt: `y` restore this one, `n` skip, `d` show a unified diff (shells out to `diff -u` if available), `a` yes-to-all-remaining, `A` no-to-all-remaining.
- `--on-conflict=skip` or `--on-conflict=overwrite` skips the prompt entirely. `-y` only suppresses the initial "about to extract N files, proceed?" confirmation — it does NOT pick a conflict policy.

Cross-machine recovery: when a project in the archive references a `RepoPath` that doesn't exist on this machine, restore stops and asks whether to abort (so you can clone the repo and retry) or skip that repo's `tickets/` extraction. Either way, `projects.json` is restored verbatim — the registry entry survives so you can fix the path inside openkanban after the fact.

Restore writes to a small, explicit allowlist of roots (`~/.config/openkanban/` and each existing `<RepoPath>/tickets/`) and rejects any archive entry whose cleaned destination falls outside them — a defense against zip-slip-style malicious archives.

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

See [Getting Started](./docs/GETTING_STARTED.md) for a task-oriented walkthrough, [Configuration Guide](./docs/CONFIGURATION.md) for the full reference, [Data Model](./docs/DATA_MODEL.md) for the on-disk layout, and [Architecture](./ARCHITECTURE.md) for the system overview.

## Architecture notes

Each agent pane is backed by a real PTY (`creack/pty`) wrapped with a virtual terminal emulator (`charmbracelet/x/vt`) that parses every escape sequence the agent emits, maintains the screen + scrollback state, and lets openkanban render it inside the TUI.

Since the daemon split (PR7 in this fork), PTYs are owned by `openkanbankd`, not the TUI process. The TUI holds a thin `daemonclient.PaneView` that mimics the old `*terminal.Pane` API. See [`internal/terminal/CLAUDE.md`](internal/terminal/CLAUDE.md) and [`internal/daemon/`](internal/daemon/).

The emulator choice and rationale for moving off `hinshun/vt10x` live in [`docs/AGENT_INTEGRATION.md#architecture-terminal-emulator`](./docs/AGENT_INTEGRATION.md#architecture-terminal-emulator).

## License & attribution

[AGPL-3.0](LICENSE), inherited from [TechDufus/openkanban](https://github.com/TechDufus/openkanban). All upstream copyright applies; this fork's changes are likewise AGPL-3.0.
