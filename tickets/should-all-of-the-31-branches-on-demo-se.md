# should all of the 31 branches on demo-seeder have been deleted on merge?

## Brief

Yes — the 31 branches (30 non-`main` + `main`) should have been cleaned up on
merge. All 30 non-`main` branches were verified stale (content in `main`) and
deleted on 2026-06-30. The repo now has two branches: `main` and one
in-progress worktree (`task/refactor-seeder-to-use-new-instance-auth`,
ahead=5, no PR — left untouched).

## Findings

### Q1 — Should they have been deleted on merge?

**Yes.** Of the 30 non-`main` branches:

- **27 branches** were `ahead=0` vs `main` — content fully merged.
- **`docs/post-demo-backlog-updates`** — appeared `ahead=1` three-dot, but its
  commit (`fix(asi04): fix SSH key injection on macOS`) was confirmed present in
  `main` (squash-merge artifact).
- **`docs/session-learnings`** — `ahead=1` with unique ephemeral
  "Pending follow-ups (2026-06-12)" notes in CLAUDE.md not in main. Stale;
  deleted by choice.
- **`feat/colima-toolchain`** — `ahead=4`, PR #9 closed-not-merged. Intentional
  abandonment; deleted by choice.
- **`chore/remove-openkanban-dir`** — PR #37 merged 2026-06-30T12:22Z; deleted.

All deleted branches are restorable via GitHub "Restore branch" on their
respective PR pages.

One in-progress branch (`task/refactor-seeder-to-use-new-instance-auth`,
ahead=5, no PR) was found and left untouched.

### Q2 — Who is responsible?

**Nothing automatic was responsible** — and that's the root cause. Two factors
compounded:

1. **`deleteBranchOnMerge` was `false`** — GitHub's "Automatically delete head
   branches" setting was off, so every merged PR left its head branch behind.
   Fixed manually in the repo settings UI (2026-06-30); future merges auto-delete.

2. **OpenKanban's `ticket delete` is local-only** — it prunes the local worktree
   and git ref but never pushes a remote branch deletion. Every
   `task/*` branch spawned by OpenKanban leaves a remote orphan after the ticket
   is closed. See memory `reference_openkanban_remote_branch_orphan_source`. This
   is a known gap; the fix (adding `git push origin --delete <branch>` to ticket
   cleanup) is a separate ticket.

The three branches that *were* previously deleted
(`feat/c2-generalissimos-revenge`, `feat/scenarios-canon`,
`task/asi04-fix-stdout-vs-stderr`) were cleaned manually, inconsistently, by
whoever merged those PRs via `gh pr merge --delete-branch`.

## Acceptance

- [x] All 31 verified-stale non-`main` branches deleted from demo-seeder
- [x] `deleteBranchOnMerge` enabled on demo-seeder (via Settings UI)
- [x] Q1/Q2 answers documented here
- [ ] OpenKanban ticket: add `git push origin --delete` to ticket-delete cleanup
  (separate scope — deferred)

<!-- openkanban:card-notes start -->
## Notes (from openkanban card)

who is responsible for doing that?
<!-- openkanban:card-notes end -->
