# Symmetric `daemonOwned` cleanup on pane delete

## Brief

`m.panes` and `m.daemonOwned` are the two parallel maps that openkanban's UI uses to track "this ticket has a live agent session." They have distinct sources of truth — `panes` is the client-side `PaneView`, `daemonOwned` is the daemon-reported ownership marker — but they overlap in membership and several UI features predicate on one or the other.

After PR #43 (2026-06-16), the WRITE sites for `daemonOwned` are aligned with `panes`:

- Startup reconcile (`internal/ui/model.go:~533`)
- `handleDaemonResyncMsg` (`internal/ui/daemon_resync.go:~213, ~224`)
- `spawnReadyMsg` handler (`internal/ui/model.go:~806`) ← PR #43

But the DELETE sites remain asymmetric. Five user-driven pane-teardown sites do `delete(m.panes, ticketID)` WITHOUT a matching `delete(m.daemonOwned, ticketID)`:

| File:line                                          | Trigger                                |
|----------------------------------------------------|----------------------------------------|
| `internal/ui/model.go:~1016` (`PaneExitMsg` case)  | daemon-side session exited via stream  |
| `internal/ui/model.go:~3377` (`performTicketCleanup`) | ticket-side cleanup flow            |
| `internal/ui/model.go:~4622` (`stopAgent`)         | user pressed `S` to stop the agent     |
| `internal/ui/model.go:~4664` (`wrapUpSessionForTicket`) | quick-move / drag → in_review/done |
| `internal/ui/model.go:~5005` (`finishSpawnCleanup`)| spawn cleanup branch                   |

The disconnect sweep at `model.go:~1058-1060` clears both maps wholesale and is symmetric.

This ticket is to make the five sites listed above symmetric — either via the same `delete(m.daemonOwned, ticketID)` line at each site, or via a single `forgetPane(ticketID)` helper that does both.

## Why this is deferred (not blocking)

The 30s daemon resync in `handleDaemonResyncMsg` (`internal/ui/daemon_resync.go:~243-270`) explicitly prunes stale `daemonOwned` entries — its Pass 2 removes any TicketID that is not in the daemon's authoritative `owned` response. So the user-visible "phantom session in W filter" window is bounded by one resync tick (≤30s) and only manifests when the daemon ALSO tore down the session.

For the common teardown paths (`wrapUpSessionForTicket`, `stopAgent`, `performTicketCleanup`) the daemon's `TicketDone` RPC is issued before or alongside the pane delete, so the daemon's next `listSessions` response correctly omits the ticket and Pass 2 cleans up. The visible artifact is brief and self-healing.

Risk-assessor (PR #43) called this LOW risk / 2-of-10 and explicitly recommended deferring. Code-reviewer (PR #43) flagged it as a pre-existing asymmetry NOT worth widening the fix scope to address.

## Acceptance

- All five `delete(m.panes, ticketID)` sites enumerated above also delete from `m.daemonOwned` (either via inline statement or via a shared helper).
- The daemon-disconnect sweep at `model.go:~1058-1060` continues to work unchanged.
- The startup reconcile + periodic resync continue to populate `m.daemonOwned` correctly (no double-delete races where resync re-adds and a delayed cleanup removes it).
- New regression tests: for each of the five teardown sites, a test that exercises the trigger via `m.Update(...)` (or direct method call) and asserts BOTH `m.panes` AND `m.daemonOwned` no longer contain the ticket ID.
- `go test ./internal/ui/...` clean.
- Manual smoke: in a TUI with several open sessions, stop one via `S` and verify the W toggle does NOT briefly show the stopped session for the next 30s.

## Must NOT

- **Do not** change the disconnect-sweep behavior (it's already symmetric and correct).
- **Do not** change the resync reconcile pass 2 behavior (it's the safety net; touching it risks breaking the self-healing path that backstops this fix).
- **Do not** add a "deferred delete" or "soft delete" pattern; the deletes are synchronous and the maps are single-threaded under bubbletea's `Update`.
- **Do not** introduce a new third map / interface — the goal is to align existing maps, not abstract them. A `forgetPane(ticketID)` helper is fine because it's a *write seam*, not a new layer.
- **Do not** modify `m.daemonViewing` cleanup as part of this — that has its own lifecycle (driven by ViewerCount push events) and isn't part of the panes/daemonOwned invariant.

## Implementation notes

Two viable shapes:

**Option A — inline at each site (5 lines added):**

```go
delete(m.panes, ticketID)
delete(m.daemonOwned, ticketID)
```

Pro: surgical, easy to review.
Con: when a 6th teardown site is added later (and it will be), someone has to remember the second line.

**Option B — `forgetPane(ticketID)` helper (1 helper + 5 call-site changes):**

```go
// forgetPane drops a ticket's pane and its daemonOwned ownership
// marker. Use whenever local teardown happens for a ticket the
// daemon is also about to lose / has already lost — keeps the two
// maps consistent without waiting on the next 30s resync.
func (m *Model) forgetPane(ticketID board.TicketID) {
    delete(m.panes, ticketID)
    delete(m.daemonOwned, ticketID)
}
```

Pro: future-proof; the seam is named and centralized; aligns with the existing pattern of giving load-bearing seams names ([[guardAPI-is-the-UI-daemon-seam]]).
Con: slight indirection.

Recommend Option B. It mirrors the `daemonGuardAPI` style of naming the seam where two systems intersect.

## Verification approach

1. Build a Model literal with `panes = {T: pane}` and `daemonOwned = {T: {}}`.
2. For each teardown site:
   - Dispatch the triggering message via `m.Update(...)` (or call the method directly for non-message paths like `stopAgent`).
   - Assert `_, ok := m.panes[T]; !ok` — existing invariant.
   - Assert `_, ok := m.daemonOwned[T]; !ok` — the new invariant this ticket adds.
3. Confirm the W toggle and the `w` session filter both immediately stop surfacing the ticket after teardown (integration shape — exercise refreshColumnTickets and inspect columnTickets).

The `daemonGuardAPI` fake at `internal/ui/exit_guard.go` can stand in for the daemon for any teardown path that sends `TicketDone` — those calls should still happen (they're how the daemon learns the session is gone). Just make sure the local maps reach the post-state expected by the new invariant.

## When to re-evaluate

Re-evaluate this backlog if any of:

- User-visible "phantom session for 30s" reports surface in real usage (would mean the bounded staleness is more bothersome than the assessment suggested).
- A NEW UI feature is added that predicates on `m.daemonOwned` and is more sensitive to the staleness window than the `W` / `w` filters are.
- Someone proposes unifying the two maps (panes and daemonOwned) into one — at which point this asymmetry becomes part of that refactor's scope and this ticket can be closed as superseded.

## File anchors

- `internal/ui/model.go` — five teardown sites (lines listed above), plus the disconnect sweep at ~1058 for reference
- `internal/ui/daemon_resync.go:~243-270` — Pass 2 reconcile (the safety net)
- `internal/ui/spawn_daemonowned_test.go` (PR #43) — test pattern to follow for new regression tests
- `internal/ui/exit_guard.go` — daemonGuardAPI / fake for tests that need daemon stubs

## Context (read these)

- [[panes-vs-daemonowned-invariant]] — the canonical reference for the two-map model
- [[test-must-traverse-propagation-path]] — write the regression tests by dispatching through `m.Update`, not by seeding the post-state
- [[openkanban-personal-fork]] — the W toggle + PR #43 history
- [[guardAPI-is-the-UI-daemon-seam]] — precedent for naming load-bearing UI↔daemon seams (informs Option B above)

## Concrete starting point

```bash
# From a fresh worktree off origin/main:
grep -n "delete(m\.panes," internal/ui/model.go
# Five hits; each is a candidate site.

# Write the failing test first (one site at a time, or all five in a
# table-driven test):
$EDITOR internal/ui/symmetric_cleanup_test.go

# Implement Option B (forgetPane helper) or Option A (5 inline deletes).
# Run the focused tests:
go test ./internal/ui/ -run TestSymmetricCleanup -v
```

Approximately a half-day of work for one engineer including tests; less if Option A is chosen and the table-driven test covers all five sites.
