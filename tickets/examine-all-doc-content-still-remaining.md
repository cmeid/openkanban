# Examine all doc content still remaining from the original source repo and remove or rewrite

## Triage finding (2026-08-17)

Reviewed in the 2026-08-14 open-ticket triage
(`cmeid/assistant:triage/open-ticket-triage-2026-08-14.md`). Verdict **KEEP**, confidence M.
A **partial completion**.

**Already shipped:** `bba3fd1` removed the inherited images **from the README**.

**Still open:**

- `docs/assets/demo.gif` is **still tracked** — 3.3 MB, added in `1619c42`, TechDufus-authored.
  Removing it from the README did not remove it from the repo.
- `docs/CONFIGURATION.md:363` and `:373` still route readers to TechDufus/oh-my-claude.

**Explicitly out of scope — do not "fix" these.** The fork attribution and the AGPL notice are
deliberate and must stay. This ticket is about *stale upstream content*, not about erasing
provenance. Getting this wrong creates a licensing problem, so the distinction is load-bearing.

## File anchors

- `docs/assets/demo.gif` — tracked, 3.3 MB, from `1619c42`.
- `docs/CONFIGURATION.md:363,373` — upstream links.
- Fork attribution + AGPL notice — **leave in place**.

## Context

- Sibling ticket **`7da3448c`** covers the hard-coded `/Users/cmeid` paths, the other half of
  making this repo presentable to a non-Chris reader.
