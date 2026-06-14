# chase error

## Brief

The TUI repeatedly toasts `✗ Failed to save: rename tmp to dest: ... no such file or directory` whenever a daemon session event fires while more than one openkanban TUI process is connected to the daemon.

Full error captured from `~/.cache/openkanban/tui.log`:

```
rename /Users/cmeid/.config/openkanban/tickets/<projectID>/<slug>-<uuid8>.md.tmp
       /Users/cmeid/.config/openkanban/tickets/<projectID>/<slug>-<uuid8>.md
: no such file or directory
```

## Root cause

`~/.config/openkanban/` is a shared on-disk store across all openkanban TUI processes. The daemon broadcasts every `session.event` (started / exited / attached / detached) to all subscribers, and each TUI's `handleDaemonSessionEvent` calls `m.saveTicket(ticket)` independently.

Three storage call sites use the same `path + ".tmp"` literal as their tmp filename, so concurrent writers from different processes converge on the *same* tmp path:

- `internal/project/tickets.go:172-179` — `SaveTicket` (the call that fires in the wild; hot path on every status event)
- `internal/project/store.go:95-99` — projects.json save
- `internal/project/filter.go:137-141` — saved-filters save

Race (two processes A, B saving the same ticket):
1. A: `os.WriteFile(<path>.tmp, …)` → tmp exists with A's data
2. B: `os.WriteFile(<path>.tmp, …)` → overwrites with B's data
3. A: `os.Rename(<path>.tmp, <path>)` → succeeds; tmp is consumed
4. B: `os.Rename(<path>.tmp, <path>)` → **ENOENT** because A removed it

Daemon log at the time of failure: `client 9 disconnected (remaining=6)` and `3 live session(s) still attached` — confirms multi-process state.

## Acceptance

1. `SaveTicket`, `ProjectRegistry.Save`, and `FilterRegistry.Save` use a per-writer unique tmp filename (`os.CreateTemp(dir, base+".tmp-*")`).
2. On `os.Rename` failure the unique tmp file is best-effort cleaned up via `os.Remove`.
3. `LoadTicketStore` sweeps stale `<slug>-<uuid8>.md.tmp-*` orphans (from crashed writers) from each project's ticket directory before walking.
4. A new test `TestSaveTicket_ConcurrentSavesAllSucceed` in `internal/project/tickets_test.go` launches N goroutines all calling `SaveTicket` on the same ticket and asserts that every call returns nil and the final on-disk file parses back to the ticket.
5. The existing `TestTicketStore_AtomicSaveNoTmpLeftover` is updated to also reject `.tmp-*` orphans (i.e. asserts no `.tmp` substring leftovers, not just `.tmp` suffix).
6. `go test ./...` passes; `go build ./...` passes.

## Must NOT

- Add any cross-process file lock — the rename is already atomic; uniqueness on the tmp filename is sufficient.
- Change the on-disk schema (frontmatter, filename format) of the ticket files.
- Squelch the user-facing "Failed to save" toast on rename failure — if the new path still hits a real rename error (different cause), the user should still see it.

## File anchors

- `internal/project/tickets.go:154-194` — `SaveTicket`
- `internal/project/store.go:80-100` — `ProjectRegistry.Save`
- `internal/project/filter.go:120-142` — `FilterRegistry.Save`
- `internal/project/tickets.go:79-136` — `LoadTicketStore` walk (add sweep here)
- `internal/project/tickets_test.go:246-266` — existing leftover-tmp test to update

## Context (read these)

- `~/.cache/openkanban/tui.log` lines 192-208 — the failure burst
- `~/.cache/openkanban/daemon.log` lines 331-382 — concurrent-subscriber evidence
- `internal/ui/daemon_subscribe.go:113-167` — `handleDaemonSessionEvent` (one saveTicket per fan-out)
- `feedback_openkanban_store_volatile.md` — background on why `~/.config/openkanban/` is shared volatile state

<!-- openkanban:card-notes start -->
## Notes (from openkanban card)

seen in main openkanban window: ✗ Failed to save: rename tmp to dest: rename /Users/cmeid/.config/openkanban/tickets/fbe577a1-3b1d-4980-bacd-b528e412e953/fi
<!-- openkanban:card-notes end -->
