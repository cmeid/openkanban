# OpenKanban

Terminal-based kanban board with integrated AI agent spawning for ticket work.

## Stack

Go 1.21+, BubbleTea (TUI), creack/pty, charmbracelet/x/vt (terminal emulation; see [Terminal Emulator](docs/AGENT_INTEGRATION.md#architecture-terminal-emulator))

## Development

```bash
go build ./...    # Build
go test ./...     # Test
go run .          # Run
```

## Where to Look

| Task | Location |
|------|----------|
| Add CLI command | cmd/ |
| Modify UI/keybindings | internal/ui/ |
| Change agent behavior | internal/agent/ |
| Terminal/PTY handling | internal/terminal/ |
| Board/ticket logic | internal/board/ |
| Project management | internal/project/ |
| Configuration | internal/config/ |
| Git operations | internal/git/ |

## Architecture

```
cmd/           CLI entry (cobra)
internal/
  ui/          BubbleTea Model - central orchestrator
  agent/       Agent config, status detection, spawning prep
  terminal/    PTY management, charm/x/vt emulator wrapper, scrollback
  board/       Ticket/column data structures
  project/     Multi-project registry, settings cascade
  config/      JSON config, validation, themes
  git/         Worktree operations
```

## Key Flows

**Ticket → Agent spawn:**
ui.spawnAgent() → terminal.New() → pty.Start() → agent process

**Settings cascade:**
ticket.Field → project.Settings.Field → config.Defaults.Field

## Agent Workflow

Scout finds → Librarian reads → You plan → Worker implements → Validator checks

## Guidance

Context-specific guidance lives in nested CLAUDE.md files:
- internal/CLAUDE.md - Go patterns, imports, testing
- internal/ui/CLAUDE.md - BubbleTea patterns
- internal/agent/CLAUDE.md - Agent integration
- internal/terminal/CLAUDE.md - PTY/terminal handling

## Relevant workspace memories

When spawned to work on this repo (e.g. from an openkanban ticket via a git worktree), the calling user's project-scoped memory directory may carry context that's not in this repo. For Chris (this fork's maintainer), those memories live at `~/.claude/projects/-Users-cmeid-manifold-dev/memory/`. Read these after `/prime` if they exist — they describe how openkanban is actually used here, not just how the code works:

- `project_openkanban_personal_fork.md` — the fork's diverged state, key features beyond upstream, why there's no upstream PR pending
- `feedback_openkanban_session_linking.md` — `openkanban ticket new --session` link vs migrate semantics, and the "don't `--migrate --force` from inside the session you're migrating" trap
- `feedback_openkanban_store_volatile.md` — openkanban's on-disk store is operational state, not source of truth; canonical ticket briefs live in repo `tickets/<slug>.md` files
- `reference_openkanban_dev_loop.md` — `update-openkanban` install script, fork remote setup, branch + commit conventions, the 50/72/no-AI commit hook
