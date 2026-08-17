# Auto-heal lost launchd supervision

## Triage finding (2026-08-17)

Reviewed in the 2026-08-14 open-ticket triage
(`cmeid/assistant:triage/open-ticket-triage-2026-08-14.md`). Verdict **KEEP**, confidence M.
This is a **partial completion** — one half shipped, and the shipped half is masking the half
that did not.

**Already shipped:** the re-bootstrap-on-update/install half, `a049d99` (PR #147). Installing or
updating now re-registers the LaunchAgent, so supervision lost *at install time* self-heals.

**Still open:** `daemon restart` still performs a graceful shutdown rather than
`launchctl kickstart -k` — `cmd/daemon.go:280-345`. A daemon whose launchd supervision was lost
while running is therefore not healed by a restart, which is the case the ticket was filed for.

Scope this ticket down to the restart path; do not re-do the install path.

## File anchors

- `cmd/daemon.go:280-345` — the graceful-shutdown restart path that still needs to become a
  kickstart.

## Context

- Sibling ticket **`a58c4e9e`** (self-heal wedged launchd job on daemon start) is the adjacent
  launchd-recovery lane and has its own partial-completion note. Read both before starting; they
  share `internal/service/launchd_darwin.go` and should not be worked blind of each other.
- Verify against `origin/main`, not a local checkout — see the triage report's Appendix C.
