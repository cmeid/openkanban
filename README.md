<h1 align="center">
  <br>
  <img src="https://github.com/user-attachments/assets/14cde506-2091-4745-9349-2604d8ec5b32" alt="OpenKanban" width="600">
  <br>
</h1>

<h4 align="center">A TUI kanban board for orchestrating AI coding agents.</h4>

<p align="center">
  <a href="https://github.com/TechDufus/openkanban/releases/latest">
    <img src="https://img.shields.io/github/v/release/TechDufus/openkanban?style=flat-square&color=blue" alt="Release">
  </a>
  <a href="https://github.com/TechDufus/openkanban/blob/main/LICENSE">
    <img src="https://img.shields.io/github/license/TechDufus/openkanban?style=flat-square&color=green" alt="License">
  </a>
  <a href="https://github.com/TechDufus/openkanban">
    <img src="https://img.shields.io/github/go-mod/go-version/TechDufus/openkanban?style=flat-square" alt="Go Version">
  </a>
  <a href="https://github.com/TechDufus/openkanban/actions">
    <img src="https://img.shields.io/github/actions/workflow/status/TechDufus/openkanban/release.yaml?style=flat-square&label=build" alt="Build Status">
  </a>
</p>

<p align="center">
  <img src="./docs/assets/demo.gif" alt="OpenKanban Demo" width="800">
</p>

---

## Why?

AI coding agents are powerful, but managing multiple agents across projects gets messy fast. You end up with terminals everywhere, losing track of what's running where, and context-switching between tasks becomes a chore.

OpenKanban gives you a single view of all your work. Each ticket gets its own git worktree and embedded terminal. Spawn an agent, watch it work, jump between tasks. Everything stays organized.

## Features

- **Tickets as worktrees** - Each task gets an isolated git branch
- **Embedded terminals** - Agents run inside the TUI, not in random terminal tabs
- **File-based tickets** - Each ticket is a Markdown file with YAML frontmatter; edit in `$EDITOR`, version it in git, watch the TUI update live
- **Hot reload** - External edits to a ticket file are reflected in the TUI within ~150ms
- **Any agent** - OpenCode, Claude Code, Gemini, Codex, Aider, or whatever CLI tool you prefer
- **Multi-project** - Manage tickets across all your repositories from one board
- **Scriptable** - `openkanban ticket new` creates tickets from the command line, ideal for a running agent that wants to spin off a subtask without driving the TUI
- **Agent self-completion** - `openkanban ticket done` lets a spawned agent mark its own ticket as complete and exit gracefully; the TUI auto-stops the pane on completion

## Install

### From source (recommended)

```bash
git clone https://github.com/techdufus/openkanban.git
cd openkanban
./scripts/install.sh
```

`scripts/install.sh` checks prerequisites, builds and `go install`s the
binary into `$GOBIN`, and — if Claude Code is detected — offers to wire
session-status hooks into `~/.claude/settings.json`. It's idempotent;
safe to re-run.

Every launch of `openkanban` checks `origin/main` for newer commits and
prompts before applying them: **Enter** to update + relaunch, **Esc** to
skip, **Q** to quit. Skip the check entirely with `--no-update-check`
or by setting `behavior.check_for_updates_on_launch: false` in
`~/.config/openkanban/config.json`.

You can also update on demand: `openkanban update --check` to print
status, `openkanban update` to pull + rebuild + reinstall.

See [docs/INSTALL.md](docs/INSTALL.md) for prerequisites, troubleshooting,
and removal instructions.

### Homebrew (macOS/Linux)

```bash
brew install TechDufus/tap/openkanban
```

To update:

```bash
brew upgrade openkanban
```

### Go

```bash
go install github.com/techdufus/openkanban@latest
```

> Builds installed via Homebrew or `go install` are tagged as "release
> builds": the launch-time update check and `openkanban update` print
> upgrade instructions (`brew upgrade openkanban` / `go install ...@latest`)
> instead of attempting an in-place git pull.

## Quick Start

From a fresh clone to a running board in four commands:

```bash
git clone https://github.com/techdufus/openkanban.git
cd openkanban && ./scripts/install.sh   # build + install + (optional) Claude Code hooks
cd ~/projects/my-app                    # any git repo you want to track
openkanban new "My App"                 # register it as an openkanban project
openkanban                              # launch the TUI
```

On launch, openkanban checks `origin/main` for newer commits and
prompts before applying — **Enter** to update + relaunch, **Esc** to
skip. Skip permanently with `--no-update-check`.

Create tickets from the TUI with `n`, or from the command line — useful
when a running agent wants to spin off a subtask:

```bash
openkanban ticket new --project "My App" --title "Add login flow"
# prints the path of the resulting .md file to stdout
```

## Tickets on disk

Tickets live as Markdown files at:

```
~/.config/openkanban/tickets/<project_id>/<slug>-<uuid8>.md
```

Each file is self-contained: status, labels, branch, and metadata in
YAML frontmatter; description in the Markdown body. Edit a ticket in
any editor and the running TUI picks up the change automatically:

```bash
vim ~/.config/openkanban/tickets/$PROJECT/wire-fsnotify-7f3a9b2c.md
# change `status: backlog` to `status: in_progress`, save
# the TUI moves the card to the In Progress column within ~150ms
```

If you have a config from before this version, your existing
`tickets/<project_id>.json` is auto-migrated on first launch and
preserved as `.json.migrated` for rollback.

## Agent self-completion

When openkanban spawns an agent on a ticket, the child process can
mark the ticket complete and exit cleanly by running:

```bash
openkanban ticket done
```

This:

- Sets the ticket to `status: done` and stamps `completed_at`.
- Sets `agent_status: completed` so the badge sticks.
- Signals the TUI, which gracefully stops the pane (SIGTERM, 3s
  grace, SIGKILL) — the agent process exits, the ticket lands in
  the Done column, no manual `/quit` or column-move required.

Worktrees and branches are deliberately preserved — only ticket
deletion tears those down. See
[docs/AGENT_INTEGRATION.md](./docs/AGENT_INTEGRATION.md#agent-callable-commands-in-session)
for the env vars, status-file mechanics, and how this interacts
with Claude Code's `Stop` hook.

## Keybindings

| Key | Action |
|-----|--------|
| `j/k` | Navigate tickets up/down |
| `h/l` | Navigate between columns |
| `space` | Move ticket to next column |
| `n` | New ticket |
| `s` | Spawn agent |
| `enter` | Attach to agent |
| `?` | Full help |

## Configuration

OpenKanban is highly configurable. Agents, keybindings, branch naming, cleanup behavior - all customizable in `~/.config/openkanban/config.json`.

See [Configuration Guide](./docs/CONFIGURATION.md) for the full reference, or [Data Model](./docs/DATA_MODEL.md) for the on-disk layout.

## Architecture notes

Each agent pane is backed by a real PTY (`creack/pty`) wrapped with a virtual terminal emulator (`charmbracelet/x/vt`) that parses every escape sequence the agent emits, maintains the screen + scrollback state, and lets openkanban render it inside the TUI.

The emulator choice and the rationale for moving off the previous one (`hinshun/vt10x`) live in [docs/AGENT_INTEGRATION.md#architecture-terminal-emulator](./docs/AGENT_INTEGRATION.md#architecture-terminal-emulator) — short version: vt10x miscounted scrolls over long sessions, the bug was unpatched upstream, and charm/x/vt is correct + actively maintained in the same ecosystem as the rest of the TUI stack.

## License

[AGPL-3.0](LICENSE)
