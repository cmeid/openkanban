# Getting Started with OpenKanban

OpenKanban is a TUI kanban board for running AI coding agents in parallel — one board, many concurrent Claude sessions, each isolated in its own git worktree.

This guide walks from a fresh install to a running workflow in about ten minutes.

## What you get

**Automatic per-ticket git worktrees.** Every ticket lives on its own branch in its own directory. Spawn an agent and it starts in that worktree — no manual `git worktree add`, no branch-switching risk between sessions.

**One board for everything.** Five columns — Backlog → Next → In Progress → In Review → Done — show every task across all registered projects. At a glance you can see which agents are working, waiting for your input, or done.

**Shorter, more focused sessions.** One ticket = one Claude session. Small, well-scoped tickets keep context windows lean and make running several sessions in parallel practical. Three tickets in parallel is as natural as one.

**Sessions can create and modify subtasks.** From inside a running session, `openkanban ticket new` creates a child ticket (with its own worktree and branch). Any ticket `.md` file is also live-editable — changes appear in the running TUI within ~150 ms. An agent can decompose its task, track subtasks, and re-prioritize without leaving the terminal.

> OpenKanban supports Claude Code, OpenCode, Aider, Gemini, and Codex. This guide is written around Claude Code — it has the deepest integration (session linking, per-ticket approval persistence, live working/waiting/idle status). The board and spawn workflow apply to any agent.

---

## 1. Install

**Prerequisites:** Go 1.21+, `git`, Claude Code (or another supported agent) on `$PATH`.

```bash
git clone https://github.com/cmeid/openkanban.git
cd openkanban
./scripts/install.sh
```

The script builds the binary, installs it to `$GOBIN`, and — if Claude Code is detected — offers to wire Claude's `SessionStart` / `Stop` / `Notification` hooks into `~/.claude/settings.json`. Accept the prompt; these hooks are what drive the live **working / waiting / idle** status on ticket cards.

> **Use the script, not `go install .`** — the script injects build flags the binary depends on at runtime. Bare `go install .` produces a stub that refuses every command except `version`. See [`docs/INSTALL.md`](INSTALL.md) for prerequisites, `$PATH` setup, and troubleshooting.

To update later: `openkanban update` (or accept the prompt on next launch). To remove: `openkanban uninstall`.

---

## 2. Register a project and pin an agent

From any git repo you want to track:

```bash
cd ~/projects/my-app
openkanban new "My App"   # register as an openkanban project
openkanban                # open the TUI
```

Run `openkanban new` from your project's root — that directory becomes the registered repo path (or pass `--path /path/to/repo` explicitly).

Or, from inside the running TUI: press **`a`** in the sidebar to add a project.

### Pin an agent — required before first spawn

**This step is required before you can spawn any session.** OpenKanban has no global fallback agent — an unpinned project refuses to spawn, so you can never accidentally launch the wrong Claude install (e.g., work account on a personal project).

With the project focused in the sidebar, press **`g`** to cycle through available agents and land on one. The sidebar shows `↳ unpinned · g` until you pin; once pinned, no extra line is shown.

For a single Claude install, press **`g`** once to pin `Claude (Default)`.

**Work and personal Claude installs.** If you run two Claude accounts, openkanban ships two presets out of the box:

| Preset | Behavior |
|--------|----------|
| `Claude (Default)` | Runs `claude` with your default `~/.claude` config |
| `Claude (Custom)` | Runs `claude` with `CLAUDE_CONFIG_DIR=~/.claude-personal` |

Pin each project to the right one. See [Configuration → Agents](CONFIGURATION.md#agents) for the full agent reference, including how to add more presets.

---

## 3. Model override per project (e.g. `opusplan`)

There is no dedicated per-project model field. The mechanism is: **define a Claude agent variant whose `args` carry `--model <value>`, then pin the project to it**.

Add the following to `~/.config/openkanban/config.json` under the `"agents"` key:

```json
"claude-opusplan": {
  "label": "Claude (opusplan)",
  "command": "claude",
  "args": ["--dangerously-skip-permissions", "--model", "opusplan"],
  "status_file": ".claude/status.json"
}
```

Then pin the project via **`g`** (sidebar cycle) or **`e`** (full project + agent editor). The `--model` flag passes through to Claude unchanged — openkanban only manages `--dangerously-skip-permissions` and `--permission-mode`, nothing else. `opusplan` is a valid `--model` value (`claude --help`'s alias list is introduced with "e.g." and is non-exhaustive).

The same pattern works for any model (`opus`, `sonnet`, `fable`, …) or for combining a model override with a different `CLAUDE_CONFIG_DIR`. The shipped `claude-custom` preset in `config.json` is a good reference for the full shape of a Claude-class variant. Note that a non-default `CLAUDE_CONFIG_DIR` needs its own one-time `claude` login, and that three things resolve under the real `~/.claude` regardless of it: openkanban's status hooks (`~/.claude/settings.json`), the bundled close-out skill (`~/.claude/skills/`), and the launch-time subagent check. A worker under a different config dir therefore reports status via coarse terminal parsing and cannot see the close-out skill — see [`TOKEN_OPTIMIZATION.md`](TOKEN_OPTIMIZATION.md).

> To edit agents without touching `config.json` by hand: sidebar **`e`** opens a unified project + agent editor in the TUI.

---

## 4. Spawn a session from a ticket

1. Press **`n`** to create a ticket in the focused column. (Creating from In Review or Done routes to In Progress.)
2. Fill in the title and description, then **`Enter`** to save.
3. With the ticket focused, press **`s`** to spawn. OpenKanban provisions the git worktree if needed, injects ticket context (title, description, branch name) as the opening prompt, and starts the session inside the daemon.
4. Press **`Enter`** to attach and watch the session in the embedded terminal.
5. Press **`Ctrl+G`** to return to the board without stopping the agent.

The session keeps running while you navigate the board, switch tickets, or restart the TUI. The daemon (`openkanbankd`) owns the PTY — you never lose a running session to a TUI exit.

---

## 5. Sessions creating subtasks

From inside a running session, spawn a child ticket:

```bash
openkanban ticket new \
  --project "My App" \
  --title "Investigate rate-limit handling" \
  --description "Check how the API handles 429s in the retry loop"
```

The ticket appears on the board immediately (~150 ms hot-reload). The session can also edit ticket `.md` files directly — description, labels, priority — and the board reflects changes live.

For scripts: add `--json` for a stable `{id, path, slug, status, project_id, worktree_path, branch_name, base_branch}` object. Add `--worktree` to provision the git worktree immediately rather than lazily at spawn time.

---

## 6. Closing a ticket from inside the session

Don't just `exit`. Let openkanban know so it can update the board.

**Work needs human review first:**
```bash
openkanban ticket move --id "$OPENKANBAN_TICKET_ID" --status in_review
```
Moves the ticket to **In Review**. The card sits in the In Review column waiting for you.

**Work is complete — no review needed:**
```bash
openkanban ticket done
```
Moves the ticket to **Done** and writes the completed badge.

**Neither command ends your session.** The agent keeps running, and pressing `Enter` on the card — in any column — drops you back into the same live session with its full scrollback. A session ends only when you delete the ticket, press `x` on it, kill it from the exit-guard modal, or the agent exits on its own.

### Pulling a ticket back to In Progress

If a reviewed or done ticket needs more work:

- On the board: **`-`** or **`Backspace`** steps the ticket left (Done → In Review → In Progress → Next → Backlog).
- From the CLI: `openkanban ticket move --id "$OPENKANBAN_TICKET_ID" --status in_progress`.

Either way the ticket keeps its session — that's the whole point of the round trip. `Enter` puts you back where you were.

> Full reference for in-session commands, the hook-driven status cycle, and session linking: [`docs/AGENT_INTEGRATION.md` → Agent-callable commands](AGENT_INTEGRATION.md#agent-callable-commands-in-session).

---

## Next steps

| Topic | Where to look |
|-------|---------------|
| All agent config options (init prompt, env, enabled states) | [Configuration Guide](CONFIGURATION.md) |
| In-session commands, status detection, session linking | [Agent Integration](AGENT_INTEGRATION.md) |
| Ticket `.md` format and frontmatter fields | [Data Model](DATA_MODEL.md) |
| Install, update, uninstall, troubleshooting | [Install Guide](INSTALL.md) |
| Daemon lifecycle, backup and restore | [README → Daemon](../README.md#daemon) |

The daemon (`openkanbankd`) runs invisibly — it starts when the TUI opens and shuts down after the last TUI closes (waiting for any still-running sessions to finish first). In normal use you never need to interact with it directly.
