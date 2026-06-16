# Daemon Package

In-process daemon (`openkanbankd`) that owns long-lived agent PTYs so the TUI can be restarted without killing in-progress sessions. RPC over a Unix socket with a framed JSON+binary protocol.

## Invariant: 1:1 ticket↔session

The daemon enforces that each TicketID owns at most one live session. `handleSpawn` is **idempotent per TicketID** — a second Spawn for an already-owned ticket returns the existing `SessionID` and `PID` rather than constructing a new session. This is the single enforcement point; every client-side spawn-discipline gap collapses here.

Two-phase check closes the construct-outside-lock race window:

1. **RLock fast path** — look up an existing session for `req.TicketID`. If found, return it without calling `NewSession` (which forks/execs an agent).
2. **WLock re-check** — after `NewSession` returns, re-scan under the write lock. If a concurrent spawn won the race, `sess.Kill(0)` the loser before returning the winner's info.

Empty `TicketID` skips the dedup (preserves semantics for any anonymous spawn, currently theoretical but supported by the wire shape).

Helper: `findSessionForTicketLocked(ticketID string) *Session` — caller must hold `sessionsMu` (R or W). Linear scan; the map is small in practice.

## Defense-in-depth: handleTicketDone iterates

A daemon that ran on a pre-dedup binary may have ended up with two sessions sharing a TicketID. `handleSpawn` refuses to create such a duplicate going forward, but any inherited pair is cleaned up by the next ticket-done flow: `handleTicketDone` iterates ALL matches and kills each, logging a `WARN` if it finds more than one. The response carries the first match's `SessionID` for wire backward-compat; per-session `"exited"` SessionEvents surface the rest.

## Session field immutability

`Session.id` and `Session.ticketID` are de-facto immutable after `NewSession`. The pane-exit watcher (`watchSessionExit`) reads `sess.TicketID()` from a goroutine without taking `sessionsMu` — only safe because of this invariant. Don't mutate `s.ticketID` post-construction; if you need to "re-ticket" a session, kill it and spawn a new one.

## Where to look

| Task | Location |
|------|----------|
| RPC dispatch table | `server.go` (handleSpawn, handleKill, handleAttach, handleList, handleOwns, handleTicketDone) |
| Wire types | `protocol.go` (SpawnReq/Resp, KillReq/Resp, etc.) |
| Session struct + lifecycle | `session.go` |
| PTY attach loop | `attach.go` (handleAttach — blocking RPC) |
| Subscription / SessionEvent fanout | broadcastEvents in server.go |

## Anti-Patterns

- Don't construct a `*Session` and insert into `s.sessions` directly — go through `handleSpawn` so the dedup logic and `watchSessionExit` wiring fire.
- Don't read `s.sessions` without holding `sessionsMu` (R or W). The map is mutated from multiple goroutines (handleSpawn, handleKill, handleTicketDone, watchSessionExit).
- Don't break early on the first TicketID match in any iteration over `s.sessions`. Post-dedup there should only be one, but defense-in-depth means cleanup paths iterate all.
- Don't take `sessionsMu` inside a PTY callback / OSC handler — those run synchronously inside `vt.Write` and would deadlock. See the OSC reentrant-deadlock memory.
