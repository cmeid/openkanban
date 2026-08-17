# Reconcile unmerged branches against done-status tickets

## Triage finding (2026-08-17)

Reviewed in the 2026-08-14 open-ticket triage
(`cmeid/assistant:triage/open-ticket-triage-2026-08-14.md`). Verdict **KEEP**, confidence M.

**The audit never ran.** Three of its branches are still marked unmerged in the branch corpus:

- `task/address-tui-height-harmonization`
- `task/set-claude-color-to-match-project`
- `task/fix-selection-in-openkanban-to-support-c`

These carry commits absent from `main` while their tickets read `done` — which is the exact
correctness defect this ticket exists to find. Five such branches were counted at triage time.

**Do not merge this with `0502d022`.** The two share mechanics ("is this branch's unique work
already in main?") and it is tempting to fold them together. They are deliberately separate:

- **This ticket is a correctness audit** — work may have been lost, and the answer might be "these
  tickets are wrongly marked done."
- **`0502d022` is hygiene** — pruning ~59 stale remote refs.

The audit carries the risk and must stand alone. Prune only after the audit says a branch is safe
to lose; doing it in the other order destroys the evidence.

## Method note

Compute merge state against **`origin/main`**, never a local checkout. And `branch merged with 0
commits ahead` is **not** evidence of shipping — it means the branch was cut at main's tip and
never committed to, i.e. work never started. Both traps bit the triage that produced this note.

## Context

- Sibling ticket **`0502d022`** — remote-branch pruning. Sequenced *after* this one.
