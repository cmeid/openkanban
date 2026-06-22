---
name: finishing-an-openkanban-ticket
description: Use when implementation work on an openkanban-spawned ticket is complete and you are ready to hand back. Walks one opinionated path — verify, self-evaluate readiness, ONE enumerated permission prompt, then land via commit → PR → merge and a reflective wind-down — without asking branch/worktree-deletion questions (openkanban handles cleanup on ticket delete).
---

# Finishing an OpenKanban Ticket

## Overview

OpenKanban spawns one focused session per ticket inside a git worktree. When the work is done, Chris used to type the same two prompts every time:

1. **Land it:** "write documentation and memories, commit everything, write a PR, push, and merge so it's picked up on the next start of openkanban, and prepare to close the session."
2. **Reflect:** "anything else to be done, experience to be saved, lessons to be learned before I close out this session?"

This skill standardizes both so he types neither. The agent **self-evaluates readiness** (using whatever code-review / validation subagents the environment provides), presents **one enumerated permission prompt**, and on a single grant **lands the work via commit → PR → merge** and then runs the **reflective wind-down** automatically.

**Announce at start:** "I'm using the finishing-an-openkanban-ticket skill to wrap up."

**Core principle:** Verify (evidence) → Self-evaluate (fail closed) → Propose once (enumerated, destination-verified) → Land (commit → PR → merge) → Reflect → Hand back. Never ask whether to delete the branch or worktree.

**The one rule that never bends:** the single permission prompt is the safety gate. A grant authorizes only the outward actions it names, against the destination it names, after the destination's ownership has been verified. Everything outward flows through that one prompt.

## When to Use

- The init prompt named this skill — invoke it when implementation is complete and you're ready to hand back.
- You were spawned by openkanban (CWD is a git worktree of a project repo, with a `tickets/<slug>.md` brief).
- The work satisfies the brief's `## Acceptance` (or, if no brief, the ticket title/description).

## When NOT to Use

- The work isn't done. Finish first.
- You weren't spawned by openkanban (no worktree, no `tickets/<slug>.md`). Use `superpowers:finishing-a-development-branch` instead.
- Chris explicitly asked for a different end-state (e.g. "just commit, don't land it" or "open the PR but don't merge"). His explicit ask wins — honor it directly; don't override it with this skill's default.

## The Process

### Step 1: Verify (evidence, not assertion)

Run the project's build + tests. **Read the output yourself and keep the tail** — you will show it in the permission prompt. Don't trust prior "it worked" memories; re-run.

```bash
# Go projects (openkanban itself)
go build ./... && go test ./...

# Other stacks: use the project's standard command
```

If anything fails: **stop. Fix it. Do not proceed.** This skill never lands broken work.

If you changed only docs/config/memory files and no build/test would meaningfully exercise the change, say so explicitly and skip — but only when truthfully nothing executable changed.

### Step 2: Self-evaluate readiness (fail closed — this is not theater)

Ask: *is this genuinely ready to land?* Use the code-review and validation subagents your environment provides (your global `~/.claude/CLAUDE.md` may name them; this skill describes roles, not names, so it works in any environment). When such agents aren't available, do the review yourself and **say so** in the permission prompt (a stated lower-confidence caveat, not silence).

**Hard stop condition:** if any reviewer/validator returns blocking findings, **do not proceed to the permission prompt.** Halt, report the findings, fix the underlying issue (don't rationalize past it), then re-verify from Step 1. Subagent verdicts are *evidence you show Chris*, never authorization to land — spawning a reviewer and then ignoring it is worse than not spawning one.

### Step 3: Assemble the wrap-up proposal (memories + docs + the land plan)

Gather everything Chris's "Q1" used to ask for, into one proposal you'll present in Step 4.

**Memories — default to MAXIMUM capture.** Per Chris's standing directive: "We will continually be seeing the same challenges and answering the same architectural questions, and I want to ensure we are consistent." The triage bar is "*might recur*," not "definitely useful." Walk the four memory types (user / feedback / project / reference) from the global `~/.claude/CLAUDE.md` auto-memory guide. Architectural decisions, coverage maps, verification protocols, operational disciplines, and workflow patterns that worked are all worth memorizing on sight. **Prefer updating an existing memory** over creating a duplicate; when a memory's claim is contradicted by what you verified, update it.

**Docs.** Did the change make any of these stale or under-documented? Root `CLAUDE.md`; nested `CLAUDE.md` files; `docs/*.md`; `README.md`; the ticket brief (`tickets/<slug>.md`) — but only if you found the *spec* itself wrong, never as a changelog.

**Polish — the "one last thing" pass.** Looking at what you just built, are there 1–3 small, low-risk improvements *to this work* you'd otherwise be tempted to leave as a "follow-up" — a clearer name, a missing test, a doc line, an obvious edge case? If they're small and within the spirit of the work, **implement them now** so they ride this single land — don't park small in-scope polish as a backlog ticket (`[[openkanban-in-scope-ticket-pivot]]`). Larger or out-of-scope ideas are the exception — handle them in Step 6.

**The land plan.** Determine the outward actions precisely (you'll enumerate them in Step 4):
- Files to commit, and the commit messages (see Step 5 for conventions).
- `git remote -v` → the push remote and the destination `<owner/repo>`.
- **Verify destination ownership** against Chris's global push-gate (owned: `github.com/cmeid/*` or the allowlist → may push after grant; anything else → commit only, surface that pushing needs his explicit per-destination call).

### Step 4: The single permission prompt (the contract)

Present ONE consolidated prompt. It must let Chris approve the whole wrap-up in a single pass, while naming every outward action precisely. Lead with the outward actions — those are the gated part.

```
Ready to wrap up — task/<slug>.

READINESS (evidence):
- Build/tests: <one-line result + tail of output>
- Review: <reviewer verdict, or "self-reviewed (no review subagent available)">

OUTWARD ACTIONS (your approval authorizes exactly these):
- commit: <files / commit subjects>
- push:   <branch> → <remote>            (destination <owner/repo> — VERIFIED owned by you)
- PR:     open on <owner/repo>, base main
- merge:  into main via merge-commit
  [If destination is NOT owned: "destination <owner/repo> is NOT yours — I will COMMIT ONLY
   and stop; say the word to push/PR to a non-owned destination."]

LOCAL CAPTURE (no bytes leave the machine):
- memories: <list, flag borderline ones>
- docs:     <list>

Approve all? (y / drop items / adjust)
```

Rules for this prompt:
- **Enumerate the outward actions; never bundle them as "commit and push."** The grant covers only what's listed, against the destination listed.
- **Owned-destination only for auto-push.** If `git remote -v` is not an owned/allowlisted destination, downgrade the outward section to commit-only and make Chris ask explicitly.
- If readiness is shaky (Step 2 caveats, flaky tests), say so here — don't bury it.

### Step 5: On grant — land via commit → PR → merge (the ONLY route)

Execute, in order. **Never push directly to main** (`git push origin <branch>:main` / `HEAD:main` is forbidden — see `[[worktree-push-to-main]]`). Always commit → PR → merge (`[[openkanban-commit-and-push]]`).

1. **Stage by name** (avoid `git add -A` / `.` so stray files aren't swept). Commit:
   - **Ticket brief first** if untracked/modified: `chore(tickets): brief for <2–3 word topic>` (untracked) or `docs(tickets): update <topic> brief` (modified) — a short topic, **not** the full slug.
   - **The work**: conventional commits (`feat(scope): …`, `fix(scope): …`, etc.). **Subject ≤ 50 chars** (prefix counts) — the repo's commit hook rejects *and unstages* on violation, so count first. Docs/CLAUDE.md ride with the work commit unless they were the only change.
   - Sign per Chris's global rules (1Password SSH signing is configured; if `git commit` fails with `gpg failed to sign the data`, Chris must unlock 1Password — don't disable signing).
2. **Rebase onto `origin/main`** if main moved (`git fetch && git rebase origin/main`) and **re-run build + tests** to catch drift.
3. `git push -u origin <branch>` — push the branch only (never `:main`). Pass the remote explicitly (don't rely on a bare `git push` default, which in a fork can target a remote you don't own).
4. `gh pr create --repo <owner/repo> --base main --head <branch> --title "<subject>" --body "<Summary / What landed / Test plan>"`.
5. `gh pr merge <num> --repo <owner/repo> --merge` — merge commits, not squash/rebase.
   - **Self-merge may be blocked even on an approved destination.** When the granted destination is an allowlisted `manifold-security` repo (e.g. `demo-seeder`), its `require_last_push_approval` ruleset refuses a self-merge (`[[manifold-security-pr-ruleset]]`): you can push + open the PR, but the merge needs approval from someone other than you. If merge is refused, stop there and hand back the PR URL + reason — don't fight the ruleset. (A non-owned, non-allowlisted org repo never reaches this step — Step 4 keeps an unowned destination commit-only.)

**Memories** are written now (they live outside any repo; not committed). Apply only the doc edits Chris approved; keep them minimal.

### Step 6: Reflective wind-down (Q2 — auto-chained, but genuine)

Immediately after landing, run the reflective pass. This is Chris's second question, and per `[[explicit-wind-down-review]]` it is **a real honest re-walk, not a perfunctory trailer** after the merge. Actually re-examine:

- **Loose ends:** anything deferred, half-done, or a small thing you noticed you could improve? **Bias to implementing it now**, not filing it — if it's small, low-risk, and within the spirit of this work, just do it (landing the new bytes still needs a fresh prompt — see below). Reserve a backlog ticket for items that are genuinely out of scope, risky, or only worth doing once some trigger fires (`[[openkanban-deferred-item-backlog-ticket]]`).
- **Experience to save:** any memory you under-captured in Step 3, now that the work is fully landed?
- **Lessons:** anything about the approach, the tooling, or a recurring pattern worth recording?

Surface what you find. **A new outward action discovered here (e.g. a memory-driven doc fix that needs its own commit + PR) requires a FRESH enumerated permission prompt — the Step 4 grant does not extend to new bytes.**

### Step 7: Hand back

Print a short summary. **Lead with the push/PR/merge state — three fixed lines, scannable in one glance.** Always include all three, even when a value is "no" — the absence of a line reads as "I forgot to check."

```
Done — task/<slug>.

Pushed:  <remote + branch, or "no">
PR:      <owner/repo#NN (merged) | (open) | "none">
Merged:  <into main (merge <sha>), or "no">

What landed:
- <commit subject 1>
- <commit subject 2>

Memories saved: <list, or "none">
Docs updated: <list, or "none">
Verification: <test/build output one-liner>
Wind-down: <loose ends / follow-ups, or "none">

Status stays in_progress — move to done after review.
OpenKanban will clean up the worktree on ticket delete (and prompt to delete the branch).
```

| Line       | "No" value | "Yes" value (examples)                                       |
| ---------- | ---------- | ----------------------------------------------------------- |
| `Pushed:`  | `no`       | `cmeid/openkanban task/<slug>`                               |
| `PR:`      | `none`     | `cmeid/openkanban#42 (merged)` / `cmeid/openkanban#42 (open)`|
| `Merged:`  | `no`       | `into main (merge a1b2c3d)`                                  |

Then stop. Do not run further tools. Chris reads the summary and either redirects you, moves the ticket to `done`, or deletes the ticket — at which point openkanban's UI handles the worktree + branch.

## What Changed From the Old "Commit Only" Skill

The previous version stopped at commit ("the agent commits; Chris decides where the bytes go") and asked nothing about landing — so Chris re-typed the land-it prompt every time. This version **lands the work** by default, because the standard close-out *is* commit → PR → merge so it's picked up on the next openkanban start. The safety that the old "don't push" rule provided is now carried by the **single enumerated, destination-verified permission prompt** — push happens, but only after Chris grants the exact actions against a verified-owned destination.

## Safeguards (non-negotiable)

1. **One permission prompt, fully enumerated** — push remote+branch, PR repo, merge target+strategy. No "commit and push" ambiguity.
2. **Evidence, not assertion** — paste build/test output + reviewer findings into the readiness claim.
3. **Destination verified owned** (`git remote -v` ∈ `cmeid/*` / allowlist) *before* push is offered; otherwise commit-only + ask.
4. **Grant is action-scoped** — the reflective pass cannot reuse it to push new bytes; fresh prompt required.
5. **Fail closed** — any verification or blocking-review failure stops the land entirely.
6. **Project-agnostic** — describe reviewer/validator *roles*, never name specific agents; derive destination ownership at runtime from `git remote -v`, never hardcode a repo.
7. **Always commit → PR → merge** — never direct-to-main, anywhere.
8. **Keep the hand-back state lines** — `Pushed:` / `PR:` / `Merged:`, all three, every time.

## Common Mistakes

**Spawning a reviewer, then landing regardless of its findings**
- *Why it's wrong:* That's theater — it launders a rubber-stamp into apparent diligence. Blocking findings must stop the land.
- *Fix:* Reviewer FAIL ⇒ halt, fix, re-verify. The verdict is evidence for Chris, not authorization.

**Bundling outward actions as one vague "commit and push"**
- *Why it's wrong:* A grant must cover known, named actions against a known destination. "Commit and push" hides whether a merge-to-main is included and where.
- *Fix:* Enumerate commit / push (remote+branch) / PR (repo) / merge (target+strategy).

**Auto-pushing to a non-owned destination**
- *Why it's wrong:* Chris's global gate requires re-asking per destination; a single in-session "yes" doesn't widen it.
- *Fix:* Verify ownership first. Non-owned ⇒ commit only, surface that pushing needs his explicit call.

**Pushing directly to main to "land it"**
- *Why it's wrong:* Direct-to-main is retired; it bypasses the PR/merge-commit record and risks force-push/stale-main hazards.
- *Fix:* Always `push branch → gh pr create → gh pr merge`.

**Treating the reflective pass as a victory lap**
- *Why it's wrong:* Q2 is an honest re-walk; a perfunctory "all good!" after a merge misses the loose ends and lessons it exists to surface.
- *Fix:* Actually re-examine loose ends, under-captured experience, and lessons.

**Sweeping all unstaged files into the commit / over-long subject**
- *Fix:* Stage by name; draft subjects ≤ 50 chars (the hook unstages on violation).

## Red Flags

**Never:**
- Present a multi-option "what should I do with this work?" menu (merge/PR/keep/discard).
- Run `git worktree remove` or `git branch -d` — openkanban does that.
- Push directly to main, or push to a non-owned destination without an explicit per-destination grant.
- Land on blocking review findings or a failing build.
- Set ticket status to `done`/`in_review` programmatically.
- Save memory without surfacing it first (unless Chris previously said "always save X").
- Reuse the land grant for new outward bytes discovered in the reflective pass.

**Always:**
- Verify build/tests and show the output before proposing the land.
- Self-evaluate via review/validation roles (or state the caveat if unavailable); fail closed on blocking findings.
- Enumerate the outward actions and verify destination ownership in the one prompt.
- Land via commit → PR → merge; handle self-merge refusal by handing back the PR URL.
- Sign commits (1Password SSH — unlock if needed).
- Lead the hand-back with `Pushed:` / `PR:` / `Merged:` — all three, every time.
