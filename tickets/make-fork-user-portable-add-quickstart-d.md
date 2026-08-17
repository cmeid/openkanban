# Make fork user-portable + add quickstart docs

## Triage finding (2026-08-17)

Reviewed in the 2026-08-14 open-ticket triage
(`cmeid/assistant:triage/open-ticket-triage-2026-08-14.md`). Verdict **KEEP**, confidence M.
A **partial completion** — the visible half shipped, so the ticket looks done and is not.

**Already shipped:** the quickstart, as `docs/GETTING_STARTED.md` (`9b7d311`, PR #149).

**Still open — the genericize strand.** Chris-specific absolute paths are still hard-coded on
`origin/main`:

- `CLAUDE.md:7` — `/Users/cmeid/manifold/dev/openkanban`
- `CLAUDE.md:118` — a `-Users-cmeid-` memory path
- `docs/DATA_MODEL.md:195` — a `/Users/cmeid/wt/` path

Anyone who forks this repo hits these immediately. Re-scope the ticket to the portability half
only; the docs half is done.

## File anchors

- `CLAUDE.md:7`, `CLAUDE.md:118`
- `docs/DATA_MODEL.md:195`
- `docs/GETTING_STARTED.md` — shipped, leave alone.

## Context

- Sibling ticket **`691f0f13`** (remove/rewrite doc content inherited from the upstream repo) is
  the other half of the "make this repo not obviously a personal fork" theme.
