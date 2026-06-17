# Daemon Package

In-process daemon (`openkanbankd`) that owns long-lived agent PTYs so the TUI can be restarted without killing in-progress sessions. RPC over a Unix socket with a framed JSON+binary protocol.

## Invariant: 1:1 ticket↔session

The daemon enforces that each TicketID owns at most one live session. `handleSpawn` is **idempotent per TicketID** — a second Spawn for an already-owned ticket returns the existing `SessionID` and `PID` rather than constructing a new session. This is the single enforcement point; every client-side spawn-discipline gap collapses here.

Two-phase check closes the construct-outside-lock race window:

1. **RLock fast path** — look up an existing session for `req.TicketID`. If found, return it without calling `NewSession` (which forks/execs an agent).
2. **WLock re-check** — after `NewSession` returns, re-scan under the write lock. If a concurrent spawn won the race, `sess.Kill(0)` the loser before returning the winner's info.

Empty `TicketID` is rejected outright at the entry of `handleSpawn` with `fmt.Errorf("spawn: empty TicketID rejected (anonymous sessions disallowed)")`. Anonymous sessions are structurally impossible: with no TicketID the daemon cannot dedup, route `TicketDone`, or reap on ticket deletion — an orphan by construction. The wire shape still allows the field to be empty, but the server refuses it.

Helper: `findSessionForTicketLocked(ticketID string) *Session` — caller must hold `sessionsMu` (R or W). Linear scan; the map is small in practice.

## Defense-in-depth: handleTicketDone iterates

A daemon that ran on a pre-dedup binary may have ended up with two sessions sharing a TicketID. `handleSpawn` refuses to create such a duplicate going forward, but any inherited pair is cleaned up by the next ticket-done flow: `handleTicketDone` iterates ALL matches and kills each, logging a `WARN` if it finds more than one. The response carries the first match's `SessionID` for wire backward-compat; per-session `"exited"` SessionEvents surface the rest.

## Last-client-disconnect lifecycle (default vs persistent)

When the clients map drops to zero, `handleLastClientDisconnect` decides the
daemon's fate:

- **Persistent mode** (`--persistent`, launchd/systemd): stays up; only explicit
  `ShutdownReq` / signals / stale-binary self-restart exit it.
- **Default mode, zero live sessions**: shuts down immediately (the daemon must
  not outlive its TUI).
- **Default mode, >0 live sessions**: does **NOT** force-kill them. A last-client
  disconnect with live sessions means the TUI's exit-guard failed to capture
  user intent, and killing in-progress agent work to preserve the
  "daemon-doesn't-outlive-TUI" invariant compounds the bug. Instead it spawns
  `awaitSessionDrain` (poll-based, single-in-flight via `drainMu`/`drainPending`)
  which keeps the daemon alive until the registry drains naturally, then calls
  `initiateShutdown` so it doesn't linger as an orphan. A future TUI may re-attach
  in the meantime.

  Accepted tradeoff: there is **no drain timeout**. A session that never exits
  (a wedged agent with no TUI to wind it down) keeps the default-mode daemon
  alive indefinitely — by design, since a live session is real work, not
  transient UI state. The defer is logged once at start (`deferring shutdown
  until they exit`); recovery is a future TUI re-attaching. If a bounded
  grace period is ever wanted, add it in `awaitSessionDrain`, not by
  reinstating the force-kill. Re-attach is single-in-flight: a second
  last-client-disconnect while `drainPending` is set spawns no second watcher
  (covered by `TestServerLifecycle_DeferralIsSingleInFlightAcrossReattach`).

`cleanup()` (which kills any sessions still in the registry) is therefore reached
only via *legitimate* shutdown signals — ctx-cancel, `ShutdownReq`, binary
staleness, or drained-to-zero — never from the last-client-disconnect-with-live
path. Don't reintroduce a force-kill there. This is orthogonal to the TUI-side
exit-guard, which is about making the guard *fire reliably*; this is about what
the daemon does *when* it doesn't.

## Session field immutability

`Session.id` and `Session.ticketID` are de-facto immutable after `NewSession`. The pane-exit watcher (`watchSessionExit`) reads `sess.TicketID()` from a goroutine without taking `sessionsMu` — only safe because of this invariant. Don't mutate `s.ticketID` post-construction; if you need to "re-ticket" a session, kill it and spawn a new one.

## Where to look

| Task | Location |
|------|----------|
| RPC dispatch table | `server.go` (handleSpawn, handleKill, handleAttach, handlePeek, handleList, handleOwns, handleTicketDone) |
| Wire types | `protocol.go` (SpawnReq/Resp, KillReq/Resp, etc.) |
| Session struct + lifecycle | `session.go` |
| PTY attach loop | `attach.go` (handleAttach — blocking RPC) |
| Snapshot without attach | `attach.go` (handlePeek — non-blocking; ships `Session.Snapshot()` bytes, no AttemptAttach/resize/subscribe/events; leaves the current attacher undisturbed). Client side: `PaneView.Peek`. |
| Subscription / SessionEvent fanout | broadcastEvents in server.go |

## Anti-Patterns

- Don't construct a `*Session` and insert into `s.sessions` directly — go through `handleSpawn` so the dedup logic and `watchSessionExit` wiring fire.
- Don't read `s.sessions` without holding `sessionsMu` (R or W). The map is mutated from multiple goroutines (handleSpawn, handleKill, handleTicketDone, watchSessionExit).
- Don't break early on the first TicketID match in any iteration over `s.sessions`. Post-dedup there should only be one, but defense-in-depth means cleanup paths iterate all.
- Don't take `sessionsMu` inside a PTY callback / OSC handler — those run synchronously inside `vt.Write` and would deadlock. See the OSC reentrant-deadlock memory.
