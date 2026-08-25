# Token / Context Optimization for Spawned Sessions

OpenKanban spawns a `claude` agent per ticket. Each spawn pays for the context that process
loads at startup. This doc records **where those tokens go**, and why the `claude-lean`
preset that used to live here was **removed rather than finished**.

It is a decision record. Read it before building a preset to strip the environment — that
was tried. Note up front: the removal argument is *not* "the savings were negligible." They
weren't. It is that the same savings are mostly available more cheaply, and that the preset
bought the rest by silently breaking things.

Measurements below: Claude Code **2.1.231**, 2026-08-25, macOS, run from an openkanban
worktree.

## TL;DR

- A spawned session loads **~34k input tokens** before any work. OpenKanban's own argv
  prompt is ~1.2k of that — not the problem.
- **The startup floor is the smaller half of the bill.** Real sessions here average ~109k
  context per turn, growing to ~170k before auto-compaction; cache-read is **80–92% of all
  billed tokens**. Accumulated work dominates the floor. (Provenance: transcript analysis of
  this repo's worktree sessions, mid-2026 — see "Provenance" below. Not reproducible from
  the repo.)
- **The composition inverted between 2.1.179 and 2.1.231.** The baseline fell 38% on its own
  as the CLI began deferring tool schemas and skill bodies. Auto-memory ~9.5k → ~0.9k;
  skills ~11.8k → ~3.2k; MCP *rose* ~2k → ~8.4k.
- The lever with a real payoff and **no capability cost** is now **scoping MCP servers**
  (−6,246, 18%), not stripping the environment.

## How this was measured

```bash
# Baseline row. Strips CLAUDE_* to mirror the daemon; see the caveat below.
env $(env | grep -oE '^(CLAUDE|ANTHROPIC)[A-Z_]*' | sed 's/^/-u /' | tr '\n' ' ') \
  claude -p "Reply with a single word: ok" \
  --output-format json --no-session-persistence < /dev/null
```

Metric = **total input tokens** = `input_tokens + cache_creation_input_tokens +
cache_read_input_tokens` from the `usage` object. Prompt size is cache-independent, so runs
are comparable regardless of cache warmth.

Other rows add exactly one variable to that command:

| Row | Added to the baseline command |
|---|---|
| − auto-memory | `-e CLAUDE_CODE_DISABLE_AUTO_MEMORY=1` |
| − skills/slash-commands | `--disable-slash-commands` |
| MCP scoped | `--strict-mcp-config --mcp-config <file with only code servers>` |
| − all MCP | `--strict-mcp-config` |

Caveats, stated plainly:

- **The env-strip does not fully mirror the daemon.** `buildCleanEnv`
  (`internal/terminal/pane.go:1937`) strips `CLAUDE*`, `OPENCODE*`, `GEMINI*`, `CODEX*`,
  `OPENKANBAN_*` — and **not** `ANTHROPIC_*`. The string `ANTHROPIC` appears in zero `.go`
  files in this repo. The probe above strips `ANTHROPIC_*` too, so it diverges from a real
  spawn whenever any are set (`ANTHROPIC_BASE_URL` is set on this machine). Everything
  outside those five prefixes is inherited verbatim.
- `claude -p` is the closest non-interactive proxy for a spawned worker (same
  plugins/CLAUDE.md/memory/MCP) but is not byte-identical to a PTY-attached interactive
  spawn.
- Ratios hold; exact totals will not reproduce elsewhere, because most of the mass is the
  *user's own environment*, not OpenKanban.

## Where the tokens go

| Config | 2.1.231 | % of baseline | 2.1.179 (historical) |
|---|---:|---:|---:|
| Baseline (everything on) | 34,265 | 100% | 55,234 |
| − auto-memory | 33,341 | 97% | 45,719 |
| − skills/slash-commands | 31,113 | 91% | 43,407 |
| **MCP scoped to code tools only** | **28,019** | **82%** | — |
| − all MCP | 25,834 | 75% | 53,179 |
| MCP-scoped + skills off | 24,868 | 73% | — |
| **`CLAUDE_CONFIG_DIR=<slim dir>`** | **unmeasured** | — | ~2.4k residual (derived) |

| Lever | 2.1.179 | 2.1.231 |
|---|---:|---:|
| Auto-memory | ~9.5k | **~0.9k** |
| Skills / slash-commands | ~11.8k | **~3.2k** |
| MCP listings | ~2k | **~8.4k** |
| Built-in tool schemas | ~19.6k | now deferred |

**The last table row is the honest gap in this record.** `CLAUDE_CONFIG_DIR` pointed at a
slim directory was the deleted preset's *defining* mechanism, and it is the one lever never
measured directly — because measuring it needs an authenticated slim config dir, and a fresh
config dir has no credentials (see reason 2 below). The ~2.4k is *derived*, not measured:
`55,234 − 9,515 − 11,827 − 2,055 = 31,837` vs. the historical measured lean total of 29,432
leaves 2,405 unexplained by the three flag levers. Anyone reconsidering this decision should
measure that row first.

Notes:

- **Auto-memory is near-free for openkanban workers.** Memory is keyed by the cwd's project
  bucket, and a ticket worktree gets its own nearly-empty bucket. The historical ~9.5k came
  from a cwd with a large corpus; it does not apply to spawns. Measured cost here: 924.
- **Nested `CLAUDE.md` costs ~nothing at spawn.** Claude Code lazy-loads subdirectory
  `CLAUDE.md` only when reading files in that directory. `internal/*/CLAUDE.md` is 91,884 B
  (94,061 B including `internal/CLAUDE.md`, which that glob does not match) — not startup
  cost. The lazy-load behavior is an external CLI claim, not verifiable from this repo.
- **`--disable-slash-commands` is too blunt to ship**: it disables *all* skills, including
  OpenKanban's own `finishing-an-openkanban-ticket` close-out. Whether it also disables
  user skills under `~/.claude/skills/` is an external CLI behavior we did not verify — if
  it only disables slash commands, the 3,152 figure measures something narrower than the
  label suggests.
- The historical preset used `"enabledPlugins": {}` in a slim config dir, a *different*
  mechanism from `--disable-slash-commands` with different coverage. The table substitutes
  one for the other.

### What OpenKanban itself contributes (small)

`internal/config/agent_prompt.tmpl` (rendered by `agent.BuildContextPrompt`) is 6,754 B,
~1.2k tokens for the fresh-spawn branch. Deliberately not trimmed: load-bearing, saving
≲0.5k.

## Why `claude-lean` was removed

Ordered strongest first — meaning most verifiable from this repo and most immune to CLI
version drift.

**1. It silently broke the machinery it depended on, and a previous attempt already got
this wrong.** Three paths hardcode `~/.claude` and are blind to `CLAUDE_CONFIG_DIR`:

- `internal/finishskill/finishskill.go:27` writes the close-out skill to
  `~/.claude/skills/…`, called with `os.UserHomeDir()` from `cmd/root.go`. A lean worker
  cannot see the skill **that the lean template itself instructed it to use.** The deleted
  template's own header comment claimed the skill would survive because it is "a user skill,
  not a plugin" — wrong, because `~/.claude/skills/` is not `$CLAUDE_CONFIG_DIR/skills/`.
  That is the durable finding here: a rebuild attempt already reasoned about this and erred.
- `cmd/hooks.go:101` installs openkanban's hooks into `~/.claude/settings.json`. Under a
  different config dir the hooks are absent, so `openkanban status set` never runs,
  `StatusDir()/<session>.status` (`~/.cache/openkanban-status/`, `internal/agent/status.go:21`)
  is never written, and detection falls through to `detectFromTerminalContent`
  (`status.go:127` → `:417`), which keyword-matches the last terminal lines — the comment at
  `status.go:438` says this path "is only reached when there is NO status file (a hookless
  agent)." Note `hooks install` does take a `--path` override (`cmd/hooks.go:93`), so this
  one is fixable without code changes; `finishskill.InstallPath` takes only `home` and has
  no override.
- `cmd/launch_check.go:202-204` resolves review subagents under `~/.claude/agents` and
  `~/.claude/plugins/cache/*`. This check runs in the **openkanban** process against the
  real `~/.claude`, finds them, and suppresses the "subagents not found" warning — actively
  reassuring the user while the lean worker cannot see them.

`.claude/status.json` — the `StatusFile` config field (`internal/config/config.go:172`) — is
*not* the artifact involved: no status-detection code reads it (only the project-editor
round-trip and the merge backfill). An earlier draft of this doc named it and was wrong.

**2. It required an interactive `/login` that nothing automated.** A fresh
`CLAUDE_CONFIG_DIR` has no credentials; auth is not inherited from the default profile. A
probe against a throwaway dir returns `api_error` with zero tokens. The doc's own proposed
follow-up, `openkanban lean-init`, was never built.

**3. It was almost certainly never used, and was calibrated to a CLI that no longer
exists.** `~/.claude-lean` does not exist on this machine. That is suggestive, not proof:
reason 2 means `claude` may abort on the credential check *before* creating the directory,
so absence is also consistent with "ran and died instantly." The checkable evidence is that
the `core` project pinned `default_agent: claude-lean` in `projects.json` on 2026-08-02 with
`auto_spawn_agent: true`, and no lean-era worktree or authenticated lean spawn followed.
Separately, its two biggest claimed wins were auto-memory (~9.5k) and plugins/skills
(~11.8k), now ~0.9k and ~3.2k.

**4. Most of its benefit is available more cheaply, and the remainder is bought with the
breakage above.** Being precise, because the loose version of this argument is wrong:

The original claim was 29,432 ≈ **53% of baseline** — i.e. a **47% cut**, an absolute saving
of ~25.8k. (An earlier draft of this doc called it a "53% cut" and argued the target was
"unreachable because the baseline fell 38%." Both were wrong: a ratio target is not
invalidated by its denominator shrinking.) Reconstructing a lean-equivalent on 2.1.231 from
the rows above:

```
34,265 − 924 − 3,152 − 8,431              = 21,758  (63.5% of baseline)
  − derived config-dir residual (2,405)   = 19,353  (56.5%)
  − today's ~/.claude/CLAUDE.md + RTK.md
    (17,292 B ≈ 4.3k)                     = 17,435  (50.9%)
```

So **the ratio is essentially preserved** (~51–64% of baseline, vs. 53.3% originally), and
the absolute floor saving would be **~12.5–16.8k**. Against a ~109k/turn session that is
**~11–15% of the bill** — real, not a rounding error. Anyone claiming the savings are
negligible has not done this arithmetic.

The reason to drop it anyway: **~6.2k of that — a third to a half — is available from MCP
scoping alone**, with zero capability loss, no config-dir games, and none of the three
breakages above. The rest is paid for in lost status hooks, a missing close-out skill,
falsely-suppressed subagent warnings, no memory writes at wind-down, and no code-review
pass. And `--strict-mcp-config` strips the code-graph and docs MCP servers, which are what
hold the *larger* accumulated-context half down by substituting graph queries for full-file
reads — so it can be a net loss on total spend even while cutting the floor.

**Note:** `claude-custom` still ships `CLAUDE_CONFIG_DIR: ~/.claude-personal`
(`internal/config/config.go:176-183`) and `~/.claude-personal` exists on this machine — so
reason 1's breakage is **not** lean-specific and applies to that preset today. It was kept
because it is a general-purpose escape hatch users actively rely on, not a token play; the
asymmetry is deliberate but the bug is real and untracked.

### What would reverse this decision

The one lever moving the wrong way is MCP: ~2k → ~8.4k across 52 CLI versions. If MCP
listing cost keeps climbing, or if `CLAUDE_CONFIG_DIR` gains credential inheritance (killing
reason 2), a config-dir-scoped preset gets *more* attractive, not less. Revisit if either
happens — and measure the `CLAUDE_CONFIG_DIR` row first.

## Two traps when changing any of this

Both bit the change that removed the preset; neither is obvious from a diff.

**1. Editing the shipped prompt does not reach existing installs.** `mergeAgentDefaults`
backfills `init_prompt` only when it is *empty* (`internal/config/config.go:404`), and
`GetEffectiveInitPrompt` (`:415`) prefers the persisted value over the embed. The first run
bakes the template into `~/.config/openkanban/config.json` permanently, so later edits to
`agent_prompt.tmpl` reach **new installs only**. Detect it:

```bash
diff <(git show HEAD:internal/config/agent_prompt.tmpl) internal/config/agent_prompt.tmpl
python3 -c "import json,os;print(len(json.load(open(os.path.expanduser('~/.config/openkanban/config.json')))['agents']['claude']['init_prompt']))"
```

If the persisted string matches a previously shipped template byte-for-byte it is a stale
copy, not a deliberate override: delete that `init_prompt` key and it falls back to the
embed. Nothing in the code does this for you.

**2. Removing a built-in preset leaves an orphan in every existing config.** Nothing prunes
removed keys. The orphan keeps its inlined `init_prompt` and `env`, stays selectable in the
agent picker and project editor, and **still spawns** — `validate.go` only checks that the
pinned `default_agent` key exists and that its `command` resolves on PATH, both of which an
orphan satisfies. A fresh install refuses with a visible "Agent not configured" toast
(`internal/ui/model.go:4639`), but an existing install spawns the stale prompt with a
possibly-nonexistent `CLAUDE_CONFIG_DIR`, silently.

**Resolved alongside this commit**, all of it config-side — none of it a repo change, and
none of it automated:

- `core` re-pinned from `claude-lean` to `claude` in `~/.config/openkanban/projects.json`
  (agent picker, sidebar `g`).
- The orphaned `agents["claude-lean"]` entry removed from `~/.config/openkanban/config.json`
  — it still carried `CLAUDE_CONFIG_DIR=~/.claude-lean` and a 3,317-char inlined prompt.
- A stale 6,080-char `agents.claude.init_prompt` deleted, so it falls back to the embed.
  This was trap 1 above, live: a byte-for-byte copy of an older shipped template, which had
  been silently shadowing every `agent_prompt.tmpl` change since it was written — including
  the two this commit makes. Timestamped backups of both JSON files were taken first.
- The now-moot `openkanban lean-init` backlog ticket deleted.

Still outstanding, and deliberately left alone as a separate concern: `codex`, `gemini`,
`opencode`, and `claude-custom` all carry the same stale 6,080-char `init_prompt`, and
`claude-custom` pins `CLAUDE_CONFIG_DIR=~/.claude-personal` — so it hits the three
`~/.claude`-hardcoded breakages catalogued above *today*, independent of `claude-lean`. That
is a pre-existing bug in a shipped preset and wants its own ticket.

## What to do instead

Ranked by tokens saved per unit of effort. None of it requires OpenKanban code changes.

1. **Scope MCP servers to the ones a code worker uses.** Measured **−6,246 (18%)** with zero
   coding-capability loss: `--strict-mcp-config --mcp-config <file>` listing just the
   code-oriented servers. General-purpose connectors (mail, chat, CRM, calendar, drive) are
   listed at startup in every session and a ticket worker never calls them. Add the flags per
   project via the agent editor (sidebar `e`); the config path is user-specific, which is why
   it is not a shipped default.
2. **Trim the global `~/.claude/CLAUDE.md`.** 17,292 B including its `@RTK.md` import ≈
   **4.3k tokens**, loaded in *every* session on the machine. Pure config, no capability
   cost. (An earlier draft claimed ~9k by adding the auto-memory index `MEMORY.md`. That was
   wrong here and contradicted this doc's own 924-token measurement: memory is per-cwd-bucket,
   and the 21,368 B index belongs to a *different* cwd. It is a lever for interactive
   sessions in a large-corpus directory, not for worktree spawns.)
3. **Attack the growth half, not the floor** — it is 80–92% of the bill. Keep never-touched
   exploration reads out of the main context: delegate multi-file understanding to subagents
   and prefer code-graph queries over full-file reads. Behavioral, and it dwarfs anything
   available at startup.

### Deliberately not done

- **`--tools`** (historically the ~19.6k rock, now deferred). Trades worker capability and
  fails loudly mid-task.
- **`--exclude-dynamic-system-prompt-sections`** on the default preset. Measured no material
  token change (we did not record a before/after pair, so treat this as weaker evidence than
  the table rows); its prompt-cache benefit is marginal here because every worker has a
  unique cwd and ticket, so little prefix is shared.
- **Trimming `agent_prompt.tmpl`** — load-bearing, saving ≲0.5k.

## A note on growing the prompt

Don't add restatements of the user's global `CLAUDE.md` to the shipped template without a
reason. `validateInitPromptOverlap` (`internal/config/validate.go:298`) helps, but its reach
is narrow and it is **warning-only**: three regexes (`HARD RULE`, `NEVER (run )?(gh pr
create|git push)`, `\bglobal rule\b`) plus H2-header keyword collision against nine
git/PR/signing terms. It cannot detect bullet-level restatement on any other topic — that is
on the reviewer, not the linter.

Worked example, from the commit that removed the preset: it added a "Route reads by purpose"
bullet (~700 B) restating a rule already in this maintainer's global `CLAUDE.md`. The linter
did not flag it, and could not. It was kept deliberately — the rule is generic, is *not* in
most users' global config, and targets the growth half that item 3 above identifies as the
real cost — but it is a judgment call, not a linter-approved one, and it did grow a template
this doc argues should not grow casually.

## Provenance

Floor measurements: reproducible via the commands above, on the stated version/date.

Session-telemetry figures (~109k average context/turn, ~170k pre-compaction, 80–92%
cache-read, 71.8M billed in one 607-turn session) come from analysing Claude Code transcript
JSONL for this repo's worktree sessions in mid-2026. They are **not** reproducible from the
repo and are not re-derived on each edit. Treat them as order-of-magnitude, and re-measure
before leaning on them for a new decision.
