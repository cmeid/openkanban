# Default-mode daemon kills live sessions on exit-guard bypass

<!-- openkanban:card-notes start -->
## Notes (from openkanban card)

## Problem

When openkanbankd runs in default (non-`--persistent`) mode and the last TUI client disconnects while sessions are still live in the registry, `handleLastClientDisconnect` logs `WARN: ... exit-guard was bypassed; terminating sessions` and then calls `initiateShutdown`, which walks the registry and kills every session (`shutdown-cleanup killing session ...`). From the user's seat this is indistinguishable from sessions vanishing on their own.

Code path:
- `internal/daemon/server.go:1037-1054` — decision (default branch falls through to shutdown regardless of `live > 0`)
- `internal/daemon/server.go:493-507` (`cleanup`) — wipes registry, kills processes
- `internal/daemon/server.go:503` — emits the `shutdown-cleanup killing session ...` log line

Stated rationale (server.go:1033-1036 comment): a daemon outliving its TUI would leak sessions, so the bypass path force-cleans. In `--persistent` mode the same function branches at line 1042 to log-and-stay-alive, preserving sessions for a future TUI re-attach.

## Evidence (daemon.log on this machine)

- 2026-06-14 23:32:12 — bypassed with 7 live sessions; daemon killed all of them (tickets `80c3e4fc`, `4e7836ea`, `fbbe4ff6`, `981c83b9`, `dce9e0cc`, `1c11161f`, `5d2faef1`).
- 2026-06-15 17:05:10 — bypassed with 1 live session; killed ticket `61bba6b6-901d-4446-9d9f-38ca2c4680bc`.

The 06-15 17:05 incident is after the v2 atomic exit-intent commit (`bc24dd4`, 2026-06-15) — so v2 reduced the race window but did not eliminate it.

## Why fix it

A bypass means the TUI failed to obtain user intent. Killing live work in that branch compounds the bug rather than recovers from it. The "defensive cleanup" framing is backwards: the user's work is the thing that needs defending, not the daemon's invariant about not outliving its TUI.

## Options to weigh

1. **Defer shutdown until sessions finish naturally.** Daemon stays alive past the last-client-disconnect, but only until session count returns to zero. No new TUI required. Smallest change — likely a few lines in `handleLastClientDisconnect` + a watcher.
2. **Detach and orphan the daemon.** Let live sessions outlive both TUI and daemon process via re-parenting. Harder; matches the "sessions are work, not transient UI state" model. Probably overkill.
3. **Surface a re-attach prompt on next TUI launch.** Treat the orphan registry as a recoverable state. Requires the daemon to persist (the in-memory `sessions` map is currently never written to disk — see server.go:185), so this is a bigger lift than option 1.

All three eliminate the destructive branch. Option 1 is the smallest plausible fix.

## Out of scope

- `--persistent` mode already does the right thing (server.go:1042-1044). No change needed there; in fact this whole ticket only matters for users who haven't installed the launchd service.
- The exit-guard itself (the `clientsMu`-protected `exiting` bool from v2) is a separate concern. This ticket is about what the daemon does *when* the guard fails to fire, not about making the guard itself more reliable. Both directions are valuable; they're orthogonal.

## Acceptance

- Default-mode daemon no longer destroys live sessions on exit-guard bypass.
- Existing `--persistent` behavior unchanged.
- Test that simulates a last-client-disconnect with N > 0 live sessions and asserts sessions survive (or that the daemon stays alive until they exit naturally, per chosen option).
<!-- openkanban:card-notes end -->
