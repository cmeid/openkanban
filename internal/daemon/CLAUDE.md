# Daemon Package

In-process daemon (`openkanbankd`) that owns long-lived agent PTYs so the TUI can be restarted without killing in-progress sessions. RPC over a Unix socket with a framed JSON+binary protocol.

## Invariant: 1:1 ticket↔session

The daemon enforces that each TicketID owns at most one live session. `handleSpawn` is **idempotent per TicketID** — a second Spawn for an already-owned ticket returns the existing `SessionID` and `PID` rather than constructing a new session. This is the single enforcement point; every client-side spawn-discipline gap collapses here.

The two-phase RLock/WLock re-check is replaced by a single atomic operation: `reg.storeIfNoTicket(ticketID, id, sess)` performs the check-and-insert under `reg.writeMu`. If a concurrent spawn won the race, `sess.Kill(0)`s the loser before returning the winner's info — same outcome, one lock acquisition.

Empty `TicketID` is rejected outright at the entry of `handleSpawn` with `fmt.Errorf("spawn: empty TicketID rejected (anonymous sessions disallowed)")`. Anonymous sessions are structurally impossible: with no TicketID the daemon cannot dedup, route `TicketDone`, or reap on ticket deletion — an orphan by construction. The wire shape still allows the field to be empty, but the server refuses it.

## Defense-in-depth: handleTicketDone iterates

A daemon that ran on a pre-dedup binary may have ended up with two sessions sharing a TicketID. `handleSpawn` refuses to create such a duplicate going forward, but any inherited pair is cleaned up by the next ticket-done flow: `handleTicketDone` iterates ALL matches via `reg.snapshot()` + `reg.deleteIf(m.ID(), m)` and kills each, logging a `WARN` if it finds more than one. The response carries the first match's `SessionID` for wire backward-compat; per-session `"exited"` SessionEvents surface the rest.

## Session registry (replaces sessionsMu + s.sessions)

Sessions live in a copy-on-write `sessionRegistry` (`registry.go`), not a mutex-guarded map. Read operations (`snapshot()`/`get()`/`len()`/`findByTicket()`) are **lock-free** — they do an atomic pointer load of an immutable snapshot map. Writes (`store`/`delete`/`deleteIf`/`storeIfNoTicket`/`drain`) clone the current map, apply the mutation, then atomically publish the new pointer under a small `writeMu`.

Rationale: the old global `sessionsMu` RWMutex used writer-priority scheduling, so a single stuck `RLock` holder plus one queued writer froze every RPC — the multi-hour daemon wedge. With the registry a stuck reader blocks only itself; all other RPCs proceed unimpeded.

Access the registry via `s.reg`; never construct a bare `map[string]*Session` guarded by a hand-rolled lock.

`watchSessionExit` uses `reg.deleteIf(sessID, sess)` — delete only if the stored entry is still the same pointer (guards a hypothetical re-use of the same ID). After `removeSession()` and `emit()`, it also calls `sess.pane.Stop()` so a naturally-exiting session (child dies on its own, no Kill/TicketDone) reclaims its master fd, drain goroutine, and scrollback emulator — closing a pre-existing leak where teardown only ran on the Kill/TicketDone paths. `Stop()` is idempotent via `teardownOnce`, so the Kill path's prior teardown is a safe no-op.

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

## Authoritative session status (resolveSessionStatus)

The daemon owns the live PTY grid for **every** session it runs — attached or
not — so it's the one place that can classify working-vs-waiting correctly for an
unattached / bg-spawned session (the client's local grid is empty, and recent PTY
activity is ambiguous: a re-rendering waiting prompt looks like activity).
`broadcastActivity` stamps `SessionEvent.Status` with `resolveSessionStatus(sess)`
on every activity heartbeat; the client applies it via `Model.applyDaemonStatus`
(guards: empty/`none` no-op, `AgentCompleted` terminal).

`resolveSessionStatus` → `resolveStatusVerdict` (pure, unit-tested in
`resolve_status_test.go`) delegates to `agent.StatusDetector.DetectStatusWithActivity`,
feeding it the live grid + `LastActivity` + the hook status file — same
prompt-gate + work-evidence logic the UI uses. Returns `""` (no verdict — leave
the client's file-poll in charge) for: a missing agent type (older client didn't
send `SpawnReq.AgentType`), **opencode** (UI resolves its status via HTTP), and an
`AgentNone` result. `AgentType` is recorded on the `Session` at spawn.

The grid read is **non-blocking** (`Pane.GetContentTry`, TryLock): if a `Stop()`
is mid-teardown holding `pane.mu` (the pre-existing emulator-drain hang, ticket
`6fc0fdbd`), the broadcaster skips that tick rather than block — otherwise one
stuck teardown would freeze the status heartbeat for **every** session. `running`
is passed as the constant `true` (the broadcaster only resolves sessions whose
`LastActivity` advanced this tick, so they're live), avoiding a second blocking
`Running()` lock.

## Wedge watchdog (watchdog.go)

`runWedgeWatchdog` samples two atomic counters on each tick — `dispatchSeq` (incremented when a handler completes) and `inflight` (active handler count, incremented on accept / decremented on return). If work is in-flight AND `dispatchSeq` has not advanced for 90 seconds (45 s when the on-disk binary is stale and self-restart is imminent), the watchdog dumps all goroutine stacks and calls `os.Exit(1)`.

The exit **clears the wedge**, but automatic respawn only happens if the launchd service is loaded. When the service is not loaded the real respawner is the next TUI autostart, not launchd. The startup log emits a `WARN` for the `source=tui-fork`-despite-installed-plist state, and `openkanban daemon health` flags it; `openkanban daemon install-service` restores supervision.

That tui-fork-shadows-launchd state is now **rare**: the client autostart (`internal/daemonclient/dial.go` → `startDaemon`) prefers an installed launchd service over forking. When no daemon is listening AND a plist is installed, it calls `service.Start` (kickstart, or bootstrap+kickstart if not loaded) so launchd's supervised instance binds the socket — instead of a tui-fork that would grab the socket + pidlock and lock launchd's `KeepAlive` respawn out. It only forks when no plist is installed (incl. all non-Darwin) or when `service.Start` itself fails (best-effort fallback so the user still gets a daemon; the warning then surfaces the shadow). So a `source=tui-fork` with an installed plist now means launchd start failed, not that autostart ignored launchd.

The daemon is no longer detect-only: it self-restarts on a sustained wedge. The TUI-side stallwatch is a separate, unrelated mechanism.

## Bounded + time-boxed dispatch (sem.go)

The accept loop caps concurrent connection handlers at `maxConcurrentConns` (256). Over-cap connections receive a `server_busy` error and are closed immediately — they do not queue.

Short-lived RPC handlers run under `runHandlerWithDeadline` (10 s timeout) so a stuck handler abandons rather than pinning a goroutine indefinitely. Two handlers are **deliberately excluded** from the deadline:
- `handleAttach` — blocks for the session's lifetime by design.
- `handleShutdown` — timeout is grace-scaled to the number of live sessions.

## Kill accounting + Health RPC

Session kills run via `trackedKill` (asynchronous, accounted):
- `inflightKills` gauge — number of kills currently in-flight.
- `reapFailures` gauge — incremented while a kill exceeds `reapTimeout` (30 s), indicating a kernel-stuck child.

`MsgHealthReq` → `handleHealth` returns a snapshot of daemon vitals — goroutines, sessions, inflight-handlers, inflight-kills, reap-failures, dispatch-seq, PID — all via lock-free reads. Surfaced by the `openkanban daemon health` CLI subcommand.

## Session field immutability

`Session.id` and `Session.ticketID` are de-facto immutable after `NewSession`. The pane-exit watcher (`watchSessionExit`) reads `sess.TicketID()` from a goroutine without taking `sessionsMu` — only safe because of this invariant. Don't mutate `s.ticketID` post-construction; if you need to "re-ticket" a session, kill it and spawn a new one.

## Deploying daemon changes

`openkanban update` now refreshes the macOS app-bundle daemon binary
(`~/Applications/OpenKanban.app/Contents/MacOS/openkanbankd`) after
`go install` via `dist/macos/build-bundle.sh` (non-fatal if the script is
absent). Previously only `./scripts/install.sh` refreshed the bundle, so
daemon-side changes were silently not deployed after a plain `update`.

After `openkanban update` completes, the bundle contains the new binary and a
running daemon **picks it up on its own** — `watchBinaryStaleness` →
`stalenessStep` (server.go) polls `update.BinaryStale()` every 30 s and:

- **0 live sessions** (now, or once they drain on a later tick): `initiateShutdown`,
  so the next launch (default mode) or launchd respawn (`--persistent`, via
  `KeepAlive={SuccessfulExit:false}`) runs the new binary.
- **>0 sessions, `--persistent`**: keeps polling and restarts the moment the
  registry drains to zero — it never kills live work to force the swap, and no
  longer stays pinned on the stale binary indefinitely (the old behavior).
- **>0 sessions, default mode**: hands the exit to the last-client-disconnect
  path (`awaitSessionDrain`); the daemon exits when the TUI quits, next launch
  is new.

So a manual restart is only needed to apply the update to **in-progress
sessions immediately** (which ends them):

```
openkanban daemon restart
```

`stalenessStep` is the unit-testable per-tick core (see `staleness_step_test.go`);
the `Server.staleCheck func() bool` field is a test seam defaulting to
`update.BinaryStale` — never reassigned in production.

## Where to look

| Task | Location |
|------|----------|
| RPC dispatch table | `server.go` (handleSpawn, handleKill, handleAttach, handlePeek, handleList, handleOwns, handleTicketDone, handleHealth) |
| Wire types | `protocol.go` (SpawnReq/Resp, KillReq/Resp, HealthReq/Resp, etc.) |
| Session struct + lifecycle | `session.go` |
| Session registry (copy-on-write) | `registry.go` |
| Concurrency semaphore + deadline wrap | `sem.go` |
| Wedge watchdog | `watchdog.go` |
| Kill accounting | `server.go` (`trackedKill`, `inflightKills`, `reapFailures`) |
| PTY attach loop | `attach.go` (handleAttach — blocking RPC) |
| Snapshot without attach | `attach.go` (handlePeek — non-blocking; ships `Session.Snapshot()` bytes, no AttemptAttach/resize/subscribe/events; leaves the current attacher undisturbed). Client side: `PaneView.Peek`. |
| Subscription / SessionEvent fanout | broadcastEvents in server.go |

## Anti-Patterns

- Don't construct a `*Session` and insert into `s.reg` directly — go through `handleSpawn` so the dedup logic and `watchSessionExit` wiring fire.
- Don't access sessions via a hand-rolled map + mutex — go through `s.reg`. Never reintroduce a global lock around session access.
- Don't break early on the first TicketID match in any iteration. Post-dedup there should only be one, but defense-in-depth means cleanup paths iterate all (use `reg.snapshot()` + filter).
- Don't hold `reg.writeMu` across blocking work (fork/exec, `Kill`, PTY I/O). `writeMu` is a narrow clone-and-publish lock; long holds defeat the purpose.
- Don't take any session-level lock inside a PTY callback / OSC handler — those run synchronously inside `vt.Write` and would deadlock. See the OSC reentrant-deadlock memory.
