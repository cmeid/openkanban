# Token / Context Optimization for Spawned Sessions

OpenKanban spawns a `claude` agent per ticket. Each spawn pays for whatever
context the `claude` process loads at startup. This doc measures where those
tokens go, ships a `claude-lean` preset that cuts a worker session to roughly
**half** a default session, and explains why the literal "30% of tokens" goal
is only partially reachable (the floor is Claude Code's own fixed system
prompt + tool definitions, which no config can remove).

## TL;DR

- A default spawned session loads **~55k input tokens** of context before it
  does any work.
- The token mass is **the environment**, not OpenKanban's own prompt: enabled
  plugins (skills + agents + SessionStart hook injections), auto-memory
  (`MEMORY.md`), the global `~/.claude/CLAUDE.md`, and MCP server listings.
  OpenKanban's argv prompt is only ~1.2k tokens.
- The **`claude-lean`** agent preset points a worker at a slimmed
  `CLAUDE_CONFIG_DIR`, disables auto-memory, and forbids MCP servers. Pin a
  project to it (sidebar `g`/`e`). Measured equivalent: **~29k tokens ≈ 53% of
  baseline**; with the global `CLAUDE.md` also gone it projects to ~45%.
- **Reaching literal ~30% (≈16.5k) is not achievable by config alone** — the
  irreducible floor (Claude Code system prompt + built-in tool schemas, ~10–13k)
  plus the repo's root `CLAUDE.md` and OpenKanban's own prompt put the practical
  worker floor around 40–50% of a fully-loaded interactive session.

## How this was measured

Reproducible probe (Claude Code `2.1.179`):

```bash
# inherited CLAUDE_*/ANTHROPIC_* are stripped first, to mirror the daemon's
# buildCleanEnv (internal/terminal/pane.go) — a real spawned worker never
# inherits the parent shell's CLAUDE_* vars.
claude -p "Reply with the single word: ok" --output-format json --no-session-persistence
```

Metric = **total input tokens** = `input_tokens + cache_creation_input_tokens +
cache_read_input_tokens` from the `usage` object. This is the full prompt size
and is cache-independent (how it splits across cached/uncached doesn't change
the total), so runs are comparable regardless of cache warmth.

> Caveat: `claude -p` (headless) is the closest non-interactive proxy for a
> spawned session and loads the same plugins/CLAUDE.md/memory/MCP, but it is not
> byte-identical to a PTY-attached interactive spawn. Numbers are directional
> (±, and the ratios are the point), not exact. A fully clean before/after on a
> real spawned worker requires the one-time lean-dir setup below, then comparing
> `/context` in each.

## Where the tokens go (measured)

Single-variable toggles, each removing **one** thing from the baseline:

| Config | Total input tokens | % of baseline | Lever cost |
|---|---:|---:|---|
| Baseline (everything on) | 55,234 | 100% | — |
| − auto-memory (`CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`) | 45,719 | 83% | **memory ≈ 9.5k** |
| − skills/slash-commands (`--disable-slash-commands`) | 43,407 | 79% | **skills ≈ 11.8k** |
| − MCP (`--strict-mcp-config`) | 53,179 | 96% | **MCP ≈ 2k** (deferred — names only) |
| **`claude-lean` equivalent** (plugins off + memory off + MCP off, **skills kept**) | **29,432** | **53%** | combined |
| plugins off + slash off + memory off + MCP off | 25,787 | 47% | *breaks the close-out skill — not shippable* |

Notes:
- **Plugins are the biggest rock.** Disabling the 9 enabled plugins removes their
  skills, agents, and — crucially — their **SessionStart hook injections** (e.g.
  the `superpowers` "using-superpowers" block and the oh-my-claude orchestration
  block), which `--disable-slash-commands` alone does *not* remove.
- **`--disable-slash-commands` is too blunt to ship.** It disables *all* skills,
  including OpenKanban's own `finishing-an-openkanban-ticket` close-out skill.
  The shippable lean profile disables *plugins* (which removes their skills)
  while leaving standalone user skills — including the close-out — intact. That
  costs ~3.6k vs the fully-stripped 47% figure, and is the right trade.
- **MCP is already cheap** (~2k): tool *schemas* are deferred by default
  (tool-search), so only names are listed at startup.
- **Nested `internal/*/CLAUDE.md` cost ~nothing at spawn.** Claude Code
  lazy-loads subdirectory `CLAUDE.md` files only when it reads files in those
  directories — they are *not* in the startup context. Only the repo-root
  `CLAUDE.md` (+ ancestor/global `CLAUDE.md`) load at startup.

### What OpenKanban itself contributes (small)

| Source | Size | At spawn? |
|---|---|---|
| Default argv prompt (`internal/config/agent_prompt.tmpl`) | 6.1 KB / ~1.2k tok | yes (positional arg) |
| Root `CLAUDE.md` | 6.3 KB / ~1.7k tok | yes |
| nested `internal/*/CLAUDE.md` (UI 33 KB, daemon 21 KB, …) | ~92 KB | **no — lazy** |
| Brief file `tickets/<slug>.md` | varies | no — read on demand, only the path is injected |

Trimming OpenKanban's own shipped text can save at most ~2–3k tokens and most of
that is load-bearing workflow guidance. It is not where a meaningful cut lives.

## The `claude-lean` preset

Added to `defaultAgents()` in `internal/config/config.go` (a sibling of the
existing `claude-custom` preset, which already proves the `CLAUDE_CONFIG_DIR`
mechanism):

```go
"claude-lean": {
    Label:      "Claude (Lean)",
    Command:    "claude",                       // inherits Claude-class spawn behavior via basename
    Args:       []string{"--dangerously-skip-permissions", "--strict-mcp-config"},
    Env: map[string]string{
        "CLAUDE_CONFIG_DIR":               "~/.claude-lean",
        "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1",
    },
    StatusFile: ".claude/status.json",
    InitPrompt: defaultLeanAgentPrompt,          // agent_prompt_lean.tmpl — no /prime mandate
},
```

Why this shape:
- **`CLAUDE_CONFIG_DIR=~/.claude-lean`** is the generic plugin/CLAUDE.md cut. A
  config dir with no plugins enabled and a slim/empty `CLAUDE.md` drops the
  plugin stack *and* the global `CLAUDE.md` without hardcoding any user-specific
  plugin names (which a `--settings` override would require, and which would go
  stale). Same one-time-setup pattern as `claude-custom`'s `~/.claude-personal`.
- **`CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`** drops the ~9.5k-token `MEMORY.md`.
- **`--strict-mcp-config`** forbids MCP servers (incl. project `.mcp.json`).
- **`Command:"claude"`** means it rides `buildSpawnReq`'s basename-keyed switch
  and gets full Claude treatment (plan mode, prompt-suggestion disable) with
  **no `model.go` change**.
- **`defaultLeanAgentPrompt`** (`agent_prompt_lean.tmpl`, ~45% smaller than the
  default template) drops the mandatory `/prime` — pointless in a lean dir with
  no global memory to prime — while keeping the brief-read, scope/status, and
  `finishing-an-openkanban-ticket` close-out directives.

`mergeAgentDefaults` adds the new key to existing user configs automatically on
next load — no migration. Opt in by pinning a project to `claude-lean`
(sidebar `g` cycles the pin, `e` edits it).

### One-time setup of `~/.claude-lean` (required before pinning)

Like `claude-custom`'s `~/.claude-personal`, the lean config dir must exist and
be authenticated, or a spawn fails with `Not logged in` (the OAuth token lives
under the config dir, not the macOS keychain). One-time:

```bash
mkdir -p ~/.claude-lean
printf '{\n  "enabledPlugins": {}\n}\n' > ~/.claude-lean/settings.json   # no plugins
# optional: a tiny ~/.claude-lean/CLAUDE.md, or none at all
CLAUDE_CONFIG_DIR=~/.claude-lean claude   # run /login once, then exit
```

Keep `~/.claude-lean` deliberately minimal: no plugins, no `MEMORY.md`, a
slim-or-absent global `CLAUDE.md`. The repo's own `CLAUDE.md` and the ticket
brief still give the worker everything it needs to do scoped work.

### Zero-setup alternative (no second config dir)

If you don't want a second authenticated config dir, you can get most of the way
with per-session flags on a `claude-lean`-style preset that keep the default
config dir (auth intact). This was the measured 53% path. Trade-off: disabling
plugins this way needs an explicit `--settings '{"enabledPlugins":{...all false...}}'`
listing *your* plugin keys (brittle, user-specific, goes stale as you add
plugins), and it does **not** drop the global `CLAUDE.md`. The `CLAUDE_CONFIG_DIR`
approach above is preferred for being generic and slightly leaner.

## Why not literally 30%

The 30% target (≈16.5k tokens) is below the practical floor. The component
sizes below are **estimates** (the system prompt + tool definitions are not
isolated by the `claude -p` probe — unlike the measured table above); the
conclusion holds regardless of the exact split:

| Floor component | ~tokens | Removable? |
|---|---:|---|
| Claude Code system prompt | ~4.2k | no (fixed by Claude Code) |
| Built-in tool definitions | ~5–8k | no (fixed) |
| Repo root `CLAUDE.md` | ~1.7k | only by deleting/slimming repo docs |
| OpenKanban argv prompt (lean) | ~0.5–1k | already minimized |

Even with plugins, memory, MCP, and the global `CLAUDE.md` fully removed, a
working session can't go below the Claude Code system prompt + built-in tools.
The realistic worker floor is **~40–50% of a fully-loaded interactive session** —
a roughly 2× efficiency win, which `claude-lean` delivers. Closing the rest of
the gap to 30% would require Claude Code to slim its own baseline, which is
outside OpenKanban's control.

## Scope split (what landed here vs. follow-up)

**Landed in this ticket (OpenKanban code):**
- `claude-lean` preset (`internal/config/config.go`) + tests.
- Lean InitPrompt (`internal/config/agent_prompt_lean.tmpl`).
- This doc + the pointer in `AGENT_INTEGRATION.md`.

**Follow-up / user-environment (recommended, not forced — outside the repo):**
- Creating + authenticating `~/.claude-lean` with a minimal `settings.json`
  (the recipe above). This is per-user environment, not shippable repo state.
- Deciding which (if any) plugins/MCP a worker should keep (e.g. a project that
  genuinely uses a plugin can pin the default `claude` instead).

**Deliberately not done:** trimming the shared default `agent_prompt.tmpl` — it
is load-bearing, saves ≲0.5k tokens, and would regress every default (non-lean)
session. The lean variant is the right vehicle for the trim.
