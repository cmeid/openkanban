# Triage + prune unmerged remote branches on the fork

## Triage finding (2026-08-17)

Reviewed in the 2026-08-14 open-ticket triage
(`cmeid/assistant:triage/open-ticket-triage-2026-08-14.md`). Verdict **KEEP**, confidence H.

**No prune has landed.** Roughly 40 `origin/*` refs were still marked unmerged in the branch
corpus at triage time — for example `origin/task/add-ticket-numbers-to-openkanban` and
`origin/task/when-exiting-an-openkanban-claude-sessio`. The report's wider count was ~59 stale
remote refs.

**Sequencing — this ticket runs second.** It shares mechanics with `c0189206` but is deliberately
not merged into it:

- `c0189206` is a **correctness audit** — five branches carry commits absent from `main` while
  their tickets read `done`. It carries the risk of real lost work.
- This ticket is **hygiene**.

Pruning before the audit completes would delete the evidence the audit needs. Wait for `c0189206`,
then prune what it clears.

## Method note

A branch reported `merged` with **0 commits ahead** was cut at main's tip and never committed to —
work never started. Do not treat that as "safe because it merged"; it is a different case from a
branch whose commits are genuinely in `main`, and it should be confirmed against `origin/main`
rather than a local checkout.

## Context

- Blocking sibling: **`c0189206`** (reconcile unmerged branches against done-status tickets).
