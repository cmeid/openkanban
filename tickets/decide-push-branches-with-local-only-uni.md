# Decide + push branches with local-only unique commits

<!-- openkanban:card-notes start -->
## Notes (from openkanban card)

## Context

During a worktree/branch cleanup pass on 2026-06-14, two task branches were found to have unique commits that exist **only in the local repo at /Users/cmeid/manifold/dev/openkanban** — never pushed to the fork (`origin`), no PR, no other copy. If anything happens to local refs (rebase, force-delete, disk loss), those commits are gone.

A snapshot of all branch tips at cleanup time was saved to `/tmp/openkanban-branches-backup-20260614-223100.txt` for recovery.

## The branches

### task/re-examing-the-startup-prompting-and-pri
- Worktree: `/Users/cmeid/manifold/dev/openkanban-worktrees/task-re-examing-the-startup-prompting-and-pri`
- 3 unique commits (none pushed anywhere):
  - `699eb8a feat(spawn): auto-clean dead claude sessions`
  - `e57027d feat(spawn): respawn modal for stale brief`
  - `a54f59a feat(agent): sync card into ticket brief`
- Openkanban ticket for this slug is currently marked `done` — but the commits' content is not reachable from main. Investigate whether the work was redone elsewhere or got dropped.

### task/add-ticket-numbers-to-openkanban
- Worktree: `/Users/cmeid/manifold/dev/openkanban-worktrees/task-add-ticket-numbers-to-openkanban`
- 1 unique commit: `0675d58 feat(ui): number tickets within each column`
- Openkanban ticket marked `done` — same mismatch question.

## The push gate (why it wasn't done in-session)

The Claude auto-mode classifier blocked `git push origin <branch>` because the global CLAUDE.md describes the convention 'origin = upstream, fork = fork' — but **this repo has only one remote: `origin = github.com/cmeid/openkanban.git` (the fork itself)**. There is no `upstream` remote configured. A push to `origin` here IS a push to `cmeid/openkanban`, which is on the auto-allow list.

Either:
1. Re-attempt the push in a fresh session after clarifying the remote ('origin is the fork in this repo, push is fine'), or
2. Add an explicit `fork` remote alias pointing to the same URL so the global memory matches: `git remote add fork https://github.com/cmeid/openkanban.git`, then `git push fork <branch>`.

## Decisions to make

For each branch:

| Branch | If work is needed | If work is duplicated/abandoned |
|---|---|---|
| re-examing... (3 commits) | Push to fork, open PR, merge with `--squash --delete-branch` | `git cherry main task/re-examing-the-startup-prompting-and-pri` to confirm patch-equivalence; if `-` for all, delete branch with `-D` |
| add-ticket-numbers (1 commit) | Same | Same |

## Verification commands

```bash
cd /Users/cmeid/manifold/dev/openkanban
# Are the commits' content already on main?
git cherry main task/re-examing-the-startup-prompting-and-pri
git cherry main task/add-ticket-numbers-to-openkanban

# View the actual changes
git log -p main..task/re-examing-the-startup-prompting-and-pri
git log -p main..task/add-ticket-numbers-to-openkanban
```

## Acceptance

- Each branch is either:
  - Merged to main (via PR + squash-merge with `--delete-branch`), or
  - Pushed to `origin` and an open PR exists, or
  - Confirmed redundant and deleted locally with `-D`
- The corresponding openkanban ticket status reflects reality (done means in main; if commits exist nowhere, ticket should not be done)
<!-- openkanban:card-notes end -->
