# Make ticket briefs durable on worktree removal

## RE-CHARTER (2026-08-17) — premise shifted, and the fix is now decided

Filed 2026-06-17 as "briefs die when the worktree is removed." That is still true, but the ticket
now has a **specific design**, chosen by Chris on 2026-08-17 after a prior-art pass. Read this
section before the original framing.

The triage of 2026-08-14 rated this **KEEP, confidence L** precisely because the premise had
shifted under it: `851b188` (PR #173) added the `ignore-ticket-briefs` setting, which changed the
problem without solving it. This section resolves that ambiguity.

## The actual gap — one missing row

Brief handling today is a **boolean**: `ProjectSettings.IgnoreTicketBriefs`, surfaced in the
project editor as `Briefs: land | ignore`.

| setting | writes into the work repo | brief survives the ticket |
|---|---|---|
| `land` | **yes** | yes |
| `ignore` | no | **no** |
| *(missing)* | no | yes |

Neither row works for a **shared company repo**. `land` commits personal work-tracking artifacts
into a team codebase — for `manifold-security/core` this is a hard no (Chris, 2026-08-17: *"we
never write this stuff to core. this is for personal projects only."*). `ignore` avoids that but
makes the brief ephemeral, because `RemoveWorktree` destroys it (see anchors).

The loss is not theoretical and not limited to the generated block. `MergeTicketBrief` only ever
rewrites bytes **inside** the `<!-- openkanban:card-notes -->` fence, so everything the agent
writes outside it — the analysis, the evidence anchors, the "do not do X" warnings — is, in the
code's own words, *"worktree-only state the store has no copy of."* Under `ignore` that is exactly
what gets deleted.

Two facts that close off the obvious alternatives:

- `~/.config/openkanban` is **not a git repo**, so the store is unversioned.
- Ticket verbs are `new / delete / move / done / list` — there is **no `edit`**. A description can
  be set at creation (`--description-file`) but never updated, so "just put it in the description"
  is not currently possible either.

## Decision — add `ProjectSettings.BriefRepo`

**Chosen 2026-08-17 by Chris.** When set, briefs resolve into a **personal repo** instead of the
work repo:

```
ProjectSettings.BriefRepo string   // "" = today's behavior, fully backward compatible

core project  ->  BriefRepo: ~/manifold/dev/assistant
    brief  ->  assistant/tickets/core/<slug>.md      (durable, versioned, personal)
    work   ->  core worktree                          (nothing written there at all)
```

**This subsumes `ignore` rather than layering on it.** When `BriefRepo` is set, nothing is written
to the work repo, so `EnsureTicketsGitExcluded` and its `.git/info/exclude` manipulation are not
needed on that path. Do not stack the two mechanisms.

**Namespace by project** (`tickets/<project>/<slug>.md`). One personal repo will host briefs for
several work repos, and slugs are only unique within a project.

### Considered and rejected

- **Version the store + add `ticket edit`.** Arguably more with-the-grain — `brief.go` states the
  store's `ticket.Description` is the source of truth and the brief is "a one-way generated view."
  Rejected because it forces analysis into ticket descriptions instead of rich per-repo markdown.
  *(The missing `edit` verb is a real separate gap and worth its own ticket.)*
- **Formalize the pointer convention.** Already happens ad hoc — `eb5a62f9`'s body says "See
  `tickets/facelift-l8-core-crossed-agent-identity.md` in the demo-seeder repo." Zero code, but
  zero enforcement, and the brief still is not durable.

## Acceptance

1. `BriefRepo` empty ⇒ **byte-identical** behavior to today. This is the regression that matters;
   test it explicitly rather than assuming.
2. `BriefRepo` set ⇒ brief is created at `<BriefRepo>/tickets/<project>/<slug>.md`, and **nothing**
   is written under the work worktree — assert the absence, do not just assert the presence.
3. The spawned agent is told the brief's real location and can read it from inside the work
   worktree.
4. Brief survives `RemoveWorktree` on the work repo. **Prove it by removing the worktree**, not by
   reasoning that it lives elsewhere.

## Must NOT

- Do not make `BriefRepo` change behavior when unset. Every existing project must be unaffected.
- Do not write the brief into both locations — that recreates the pollution this exists to stop.
- Do not assume the agent can write outside its worktree. See the sandbox risk below.

## Carrying costs — surfaced deliberately, decide before building

- **Each ticket now spans two repos.** The `finishing-an-openkanban-ticket` skill commits the brief
  and the work; with `BriefRepo` those are different repos with different push gates. That skill
  needs to learn the split, and a personal-repo brief commit must not be bundled into a shared-repo
  PR. This is the largest second-order cost.
- **Sandbox.** The agent would write outside its worktree, which the Seatbelt `allowWrite` list may
  not permit. Currently moot — `sandbox.enabled` is `false` (see assistant ticket `854adba9`) — but
  it lands the moment the sandbox is re-enabled. Worth an `allowWrite` entry in that ticket's fix.
- **Brief orphaning.** Deleting a ticket removes the worktree; it will not remove a brief sitting in
  the personal repo. Decide whether that is a feature (durable history) or a leak (accreting dead
  briefs), and write the answer down.

## File anchors

- `internal/project/project.go:30` — `IgnoreTicketBriefs`; add `BriefRepo` beside it.
- `internal/ui/model.go:4778-4801` — `resolveBrief`; `:4801` is the hardcoded
  `filepath.Join(worktreePath, "tickets", slug+".md")`. **There is no path indirection today** —
  this is the change site.
- `internal/agent/context.go:25` — `BriefPath` in the spawn context; feeds the prompt template.
- `internal/agent/brief.go:14` — `briefSubdir = "tickets"`.
- `internal/agent/brief.go:82-100` — the one-way-view concurrency contract and the "worktree-only
  state" comment that defines what is being lost.
- `internal/agent/brief_exclude.go` — `EnsureTicketsGitExcluded`; not needed on the `BriefRepo`
  path.
- `internal/git/worktree.go:182` — `RemoveWorktree`: `git worktree remove --force` followed by
  `os.RemoveAll`. **Unconditional** — this is what destroys the brief, and it is unchanged since
  the ticket was filed.
- `internal/ui/project_edit.go:494` — the `Briefs:` editor row; needs a third state or a companion
  field.
- `851b188` (PR #173) — added `ignore-ticket-briefs`; the commit that shifted this ticket's premise.

## Context

- Triage: `cmeid/assistant:triage/open-ticket-triage-2026-08-14.md` — this ticket at conf L.
- The motivating case is `eb5a62f9` (Facelift L8), a core-targeting ticket whose brief has nowhere
  legitimate to live today.
