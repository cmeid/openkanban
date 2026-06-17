# Resolve cleanup artifacts: stash + backed-up ticket briefs

## Resolution (2026-06-17)

The user reframed this from "dispose of the artifacts" to "fix the root
cause of the inconsistencies." Both root causes were traced to code/process;
the artifacts were then cleared.

**Root cause (briefs):** `MergeTicketBrief` (`internal/agent/brief.go`) wrote
the per-worktree `tickets/<slug>.md` with a non-atomic `os.WriteFile`, and the
store is the actual source of truth (the brief is a one-way generated view).
Fix shipped on this branch: **atomic temp+rename brief write** (the user's
chosen axis — "safest method for concurrent read/modify"), mirroring
`TicketStore.SaveTicket`, plus a documented concurrency contract. See commit
`fix(agent): atomic brief write via temp+rename`.

**The stash:** the ticket's original `stash@{0}: ... wip-unrelated-readme-ticket`
on `task/fix-claude-code-tty-corruption-from-tui` **no longer exists** — it was
already resolved between filing and now. The two stashes present today are
quarantine stashes owned by **other active tickets** and are deliberately left
untouched (out of scope):
- `stash@{0}` — `task/investigate-whether-there-might-ever-be`
- `stash@{1}` — `fix/ui-session-exit-freeze`

**The 4 /tmp briefs:** verified byte-identical (body) to their openkanban store
copies under `~/.config/openkanban/tickets/fbe577a1.../` → redundant → removed
along with `/tmp/openkanban-cleanup-briefs/`.

**Deferred (filed as backlog tickets):**
1. Brief durability on worktree removal (commit-on-spawn vs preserve-on-cleanup
   vs ephemeral) — the user reframed away from this; the store already preserves
   card-notes content, so only out-of-block agent notes are at risk.
2. Quarantine-stash auto-cleanup discipline — nothing pops/drops these stashes;
   they accumulate.

<!-- openkanban:card-notes start -->
## Notes (from openkanban card)

## Context

The 2026-06-14 worktree/branch cleanup pass produced two kinds of artifacts that still need a decision:

1. **A git stash** that was sitting on the cwd worktree before the cleanup started.
2. **Four untracked ticket-brief markdown files** that lived in deleted worktrees' `tickets/` dirs. They were backed up to `/tmp/openkanban-cleanup-briefs/` so removal would not destroy them.

## 1. The stash

```
stash@{0}: On task/fix-claude-code-tty-corruption-from-tui: wip-unrelated-readme-ticket
```

Contents (per the audit) likely match what was uncommitted on the cwd at the time:
- `M README.md`
- `M internal/ui/model.go`
- `?? internal/ui/cleanup_test.go`
- `?? tickets/fix-claude-code-tty-corruption-from-tui.md`

The stash message says `wip-unrelated-readme-ticket` — implies the README+ticket edits were the deliberately-stashed piece, separate from the in-progress TTY-corruption work.

### Actions

```bash
cd /Users/cmeid/manifold/dev/openkanban
git stash show -p stash@{0}        # confirm contents
# If still wanted: apply on a new branch and commit
# If now redundant: git stash drop stash@{0}
```

## 2. Backed-up briefs at /tmp/openkanban-cleanup-briefs/

```
/tmp/openkanban-cleanup-briefs/
├── add-key-mappings-to-raise-lower-priority.md
├── prompt-for-branch-deletion-on-ticket-cle.md
├── review-the-time-shown-in-the-title-bar-o.md
└── when-exiting-an-openkanban-claude-sessio.md
```

These were the untracked `tickets/<slug>.md` files that lived in worktrees deleted during cleanup. Per the project convention (see `feedback_openkanban_store_volatile.md` — 'canonical ticket briefs live in repo `tickets/<slug>.md` files'), these should have been committed but weren't.

### Why this happened

The spawning workflow creates briefs in the per-worktree `tickets/` directory but doesn't auto-commit them. When the worktree is removed without an explicit commit pass, the brief disappears with it. The workflow gap is tracked separately — for now, decide what to do with these four backed-up copies.

### Actions

For each brief:
- Compare to the version in `~/.config/openkanban/tickets/.../*.md` (the openkanban store). If the volatile store has the same content, the brief is redundant — `rm` it.
- If the brief contains content not in the store (e.g. design notes, decisions captured during work), either:
  - Commit it to `main` in a single chore commit (`git add tickets/<slug>.md && git commit -m 'chore(tickets): commit briefs from cleanup'`), or
  - Merge its content back into the openkanban store ticket and `rm` the file.

```bash
# Quick diff against the store copy
for f in /tmp/openkanban-cleanup-briefs/*.md; do
  slug=$(basename \"\" .md)
  store=$(ls ~/.config/openkanban/tickets/fbe577a1*/-*.md 2>/dev/null | head -1)
  echo \"===  ===\"
  diff -u \"\" \"\" || echo '(differs)'
done
```

## /tmp is volatile

`/tmp` may be wiped on reboot. **Get this done before next restart**, or move the briefs to a more durable location.

## Acceptance

- `git stash list` is empty (or the stash is intentionally retained with a note here).
- `/tmp/openkanban-cleanup-briefs/` is empty/removed, with each brief either committed to main, merged into store, or confirmed redundant and deleted.
- The workflow gap that produced uncommitted briefs is captured as a separate ticket if not already.
<!-- openkanban:card-notes end -->
