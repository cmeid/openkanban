# Token / Context Optimization for Spawned Sessions

OpenKanban spawns a `claude` agent per ticket. Each spawn pays for whatever
context the `claude` process loads at startup. This doc measures where those
tokens go, ships a capability-complete `claude-lean` preset (~53% of a default
session), and documents the opt-in `--tools` lever that reaches the ~30% target
at a deliberate capability cost.

## TL;DR

- A default spawned session loads **~55k input tokens** of context before it
  does any work.
- The token mass is **the environment**, not OpenKanban's own prompt: enabled
  plugins (skills + agents + SessionStart hook injections), auto-memory
  (`MEMORY.md`), the global `~/.claude/CLAUDE.md`, MCP listings, and — the
  single biggest chunk — **built-in tool schemas (~19.6k)**. OpenKanban's argv
  prompt is only ~1.2k tokens.
- The **`claude-lean`** preset points a worker at a slimmed `CLAUDE_CONFIG_DIR`,
  disables auto-memory, forbids MCP, and improves prompt-cache reuse — while
  staying **capability-complete (all built-in tools)**. Measured ~29k ≈ **53% of
  baseline**, capability-neutral. Pin a project to it (sidebar `g`/`e`).
- **The ~30% target *is* reachable** — but only by also trimming the built-in
  toolset with **`--tools`** (the ~19.6k lever). Measured **~28% with a minimal
  tool set**. That trades worker capability (a tool outside the set is
  unavailable), so it is an **opt-in**, not the default — see "Going further"
  below.
- The true floor is **~18%** (`--tools ""`, no tools — unusable): Claude Code's
  own system prompt + env is the only genuinely fixed part.

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
| − **all built-in tools** (`--tools ""`) | 9,843 | 18% | **built-in tools ≈ 19.6k** (the biggest single chunk) |
| **`claude-lean`** (plugins off + memory off + MCP off, **all tools kept**) | **29,432** | **53%** | combined, capability-neutral |
| plugins off + slash off + memory off + MCP off | 25,787 | 47% | *breaks the close-out skill — not shippable* |

The full combination matrix (relevant to the design choice below):

| Profile | Total tokens | % | Setup | Capability cost |
|---|---:|---:|---|---|
| `claude-lean` (config-dir; all tools) | 29,441 | **53%** | one-time `/login` | none |
| Zero-setup pure-flags + `--tools` (minimal set) | 21,822 | 40% | none | loses WebSearch/NotebookEdit/… |
| `claude-lean` + `--tools` (minimal set) | 15,523 | **28%** | one-time `/login` | + tools |
| Floor: `--tools ""` (no tools) | 9,843 | 18% | — | unusable |

`--exclude-dynamic-system-prompt-sections` (in the preset) measured **no change**
to the token total — it relocates per-machine sections (cwd/env/git-status) into
the first user message for cross-session prompt-**cache** reuse, a cost win, not a
token-count win. Its benefit is marginal for OpenKanban (each worker is a unique
cwd/ticket, so little prefix is shared), but it is free.

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
    Label:   "Claude (Lean)",
    Command: "claude",                       // inherits Claude-class spawn behavior via basename
    Args: []string{
        "--dangerously-skip-permissions",
        "--strict-mcp-config",
        "--exclude-dynamic-system-prompt-sections",
    },
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
- **`--exclude-dynamic-system-prompt-sections`** relocates per-machine prompt
  sections into the first user message for cross-session cache reuse (a cost
  win, not a token-count win — marginal here, but free).
- **All built-in tools are kept** (no `--tools`): the preset is
  **capability-complete**, so research, web search, notebooks, background bash,
  and skill invocation all work. The `--tools` cut to ~30% is opt-in (below).
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
under the config dir, not the macOS keychain). The forthcoming `openkanban
lean-init` helper (see Scope split) will automate this; until then, one-time:

```bash
mkdir -p ~/.claude-lean
printf '{\n  "enabledPlugins": {}\n}\n' > ~/.claude-lean/settings.json   # no plugins
# optional: a tiny ~/.claude-lean/CLAUDE.md, or none at all
CLAUDE_CONFIG_DIR=~/.claude-lean claude   # run /login once, then exit
```

Keep `~/.claude-lean` deliberately minimal: no plugins, no `MEMORY.md`, a
slim-or-absent global `CLAUDE.md`. The repo's own `CLAUDE.md` and the ticket
brief still give the worker everything it needs to do scoped work.

## Going further: reaching ~30% with `--tools` (opt-in)

The single biggest remaining chunk is **built-in tool schemas (~19.6k)**. The
`--tools` flag restricts which built-in tools load (allow-list); unlisted tools'
schemas are dropped. Adding a minimal set to a `claude-lean` project takes a
worker to **~28%** (measured `--tools "Bash,Edit,Read,Write,Grep,Glob,Task,TodoWrite"`
= 15,523 tokens on the config-dir base). This is **opt-in, not the default**,
because it trades capability:

- **A tool outside the set is unavailable** — the worker loses WebSearch (web
  research), NotebookEdit (Jupyter), etc. Capability cuts fail *loudly* mid-task
  (unlike context cuts), so only opt in for projects you know are pure
  implementation.
- **The allow-list is load-bearing and easy to get wrong.** A usable set must
  include not just the obvious editors but the workflow-critical tools:
  `Bash` + `BashOutput` + `KillShell` (background builds/tests), the skill-
  invocation tool (or the `finishing-an-openkanban-ticket` close-out can't run),
  and `WebFetch`/`WebSearch` if the project ever does research. **Test your set
  on a throwaway ticket before relying on it.**
- **Subagent inheritance is unverified** — whether `--tools` on the parent also
  constrains `Task` subagents' toolsets is not confirmed; check if you fan out
  research subagents from a lean parent.

Add it per project via the agent editor (sidebar `e`) on a `claude-lean`-pinned
project, e.g. append `--tools "Bash,BashOutput,KillShell,Edit,Read,Write,Grep,Glob,Task,TodoWrite,WebFetch,WebSearch"`.

## The floor (~18%)

The only genuinely fixed part is Claude Code's own system prompt + environment
(`--tools ""`, no tools = 9,843 tokens = 18% — but that session can't *do*
anything). Repo root `CLAUDE.md` (~1.7k) and OpenKanban's lean argv prompt
(~0.5–1k) are the other near-irreducibles. A usable worker therefore floors
around **~28% with `--tools`** (config-dir tier) or **~40%** zero-setup, vs **53%**
capability-complete.

## Scope split (what landed here vs. follow-up)

**Landed in this ticket (OpenKanban code):**
- `claude-lean` preset (`internal/config/config.go`) + tests.
- Lean InitPrompt (`internal/config/agent_prompt_lean.tmpl`).
- This doc + the pointer in `AGENT_INTEGRATION.md`.

**Follow-up (separate ticket):**
- **`openkanban lean-init`** — a CLI helper to scaffold + authenticate
  `~/.claude-lean` in one command, including a `--from <profile>` mode that
  derives a lean config dir from an existing profile (copies settings structure,
  strips plugins/MCP/memory/global CLAUDE.md; credentials are never copied — it
  finishes with a one-time `claude /login`). Eliminates the manual recipe above.

**Opt-in, documented (not default):**
- The **`--tools`** cut to ~30% (see "Going further") — per-project, capability-
  trading, added via the agent editor.
- Deciding which (if any) plugins/MCP a worker should keep (a project that
  genuinely uses a plugin can pin the default `claude` instead).

**Deliberately not done:** trimming the shared default `agent_prompt.tmpl` — it
is load-bearing, saves ≲0.5k tokens, and would regress every default (non-lean)
session. The lean variant is the right vehicle for the trim. And `--tools` is
**not** baked into the default `claude-lean`: it trades worker capability
(WebSearch, notebooks, etc.) and needs a carefully-maintained allow-list, so the
default stays capability-complete.
