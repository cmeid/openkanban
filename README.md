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

## Install

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

## Quick Start

Launch the TUI:

```bash
cd ~/projects/my-app
openkanban new "My App"
openkanban
```

Or create a ticket directly from the command line — useful when a
running agent wants to spin off a subtask:

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

## License

[AGPL-3.0](LICENSE)
