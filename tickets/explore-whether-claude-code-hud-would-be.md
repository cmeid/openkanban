# Explore whether a Claude Code HUD would improve detection of working vs waiting vs other

## Triage finding (2026-08-17)

Reviewed in the 2026-08-14 open-ticket triage
(`cmeid/assistant:triage/open-ticket-triage-2026-08-14.md`). Verdict **KEEP**, confidence L.

This ticket was drafted as a **DELETE** and then flipped back by the adversarial review pass. The
reasoning matters, because the same mistake is easy to repeat:

Every individual *detection bug* ticket really is `done` (`401ca295`, `80d1b62d`, `3bd48d91`,
`eb80172b`, `c4c0f329`, `f3874545`, `9cfaca9a`, `9af1e95b` — all verified `status: done`). So the
symptoms are gone and this looks superseded. But the thing that fixed them **is** the fragility
this ticket wanted to interrogate:

- `4da5fba` broadens `permissionPromptSignatures` with **literal Claude Code prompt strings**,
  "all three verified verbatim against the bundled binary (claude-code 2.1.179)", and ships a
  load-bearing AskUserQuestion footer **drift-guard** test — i.e. a detector deliberately designed
  to break when Claude Code updates.
- `59f02a0` layers a 60s PTY-activity override on top (`README.md:137-150`).

So the question — *could a first-class HUD signal replace version-pinned string matching?* — has
never been answered. `hud` appears nowhere on `origin/main`, and the branch
`task/explore-whether-claude-code-hud-would-be` carries no commits.

**This is a decision ticket: its deliverable is an answer, not code.** It closes when someone
records a yes/no with reasoning, not when the string matching is working again.

## File anchors

- `4da5fba` — `permissionPromptSignatures`, version-pinned to claude-code 2.1.179.
- `59f02a0` / `README.md:137-150` — the 60s PTY-activity override.

## Context

- Symptoms fixed ≠ question answered. If you are about to close this because detection currently
  works, re-read the paragraph above first.
