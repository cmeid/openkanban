# Self-heal wedged launchd job on daemon start

## Triage finding (2026-08-17)

Reviewed in the 2026-08-14 open-ticket triage
(`cmeid/assistant:triage/open-ticket-triage-2026-08-14.md`). Verdict **KEEP**, confidence M.
A **partial completion**: the escalation shipped, the timeout did not.

**Already shipped:** `817fab4` — the `EX_CONFIG` bootout+bootstrap escalation, plus the fork
fallback at `dial.go:252`.

**Still open (acceptance item 1):** kickstart still runs a **context-free** `exec.Command`.
`internal/service/launchd_darwin.go:362` is `func kickstart`; the un-timed
`exec.Command("launchctl", args...)` it relies on is at `:445` in `runLaunchctl`. Because there is
no context/deadline, a wedged job blocks roughly 6 seconds on daemon start — the exact symptom
this ticket exists to remove.

Note the path correction: this is `internal/service/launchd_darwin.go`, **not** anything under
`internal/daemon/`. An earlier draft of the triage cited the wrong directory.

## File anchors

- `internal/service/launchd_darwin.go:362` — `kickstart`.
- `internal/service/launchd_darwin.go:445` — `runLaunchctl`, the context-free `exec.Command`.
- `dial.go:252` — the shipped fork fallback.

## Context

- Sibling ticket **`841f6ae6`** (auto-heal lost launchd supervision) covers the `daemon restart`
  → kickstart half. Same file, adjacent problem — coordinate rather than working blind.
