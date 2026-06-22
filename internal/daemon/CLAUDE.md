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

## Shutdown must actually terminate (initiateShutdown closes client conns)

`initiateShutdown` closes the shutdown channel, closes the listener, AND
force-closes every registered client connection via `closeClientConns()`. That
last step is **load-bearing, not cleanup politeness**: `Serve` finishes by
blocking on `s.wg.Wait()` for every `handleConn` goroutine to return, and a
persistent-mode TUI never disconnects on its own — its `handleConn` is parked in
`ReadFrame` (or a blocked `handleAttach` binary loop). Closing the listener also
*unlinks the socket file* (Go `UnixListener` default), so without closing the
conns the daemon ends up in a **zombie state**: socket gone (`daemon list` →
ENOENT, new TUIs can't dial) yet the process is alive and still serving the
attached TUI, and the launchd respawn-on-new-binary never fires. Closing the
conn trips the parked `ReadFrame` (`net.ErrClosed`/EOF) → goroutine returns →
`wg.Wait()` completes → `cleanup()` → process exits → launchd respawns. Pinned by
`TestServerLifecycle_ShutdownTerminatesWithAttachedClient`. `closeClientConns`
snapshots the conns under `clientsMu` then closes them *outside* the lock (the
close cascades into each `handleConn` defer, which re-takes `clientsMu`).

`handleSpawn` is **gated on `s.shutdown`** at its entry: a daemon that has
decided to exit must reject new spawns. The spawn RPC is reachable over an
already-attached client's connection even after the listener is closed, and a
fresh session both orphans agent work `cleanup()` is about to kill and
re-populates the registry the shutdown is draining. This is the bug that let the
wedged daemon spawn three sessions *after* "shutdown initiated". Pinned by
`TestHandleSpawn_RejectsAfterShutdown`.

### Safety nets are bound to process-exit, not shutdown-initiation

The diagnostic/recovery mechanisms must survive shutdown-*initiation* and only
go away on process *exit* — otherwise a hung shutdown is the one state you can
neither inspect nor recover, which is exactly what happened in the field:

- **SIGUSR1 goroutine-dump handler**: its teardown is a `defer` in `Serve` (runs
  on return), NOT a `<-s.shutdown` goroutine. The old code dropped the handler
  the instant shutdown began, so `kill -USR1` on the wedged daemon produced
  nothing. Keep the teardown deferred so the dump works through the whole
  shutdown + `cleanup()` window.
- **Shutdown-completion backstop** (`runWedgeWatchdog` → `awaitShutdownCompletion`):
  on `<-s.shutdown` the watchdog does NOT `return`; it waits for `s.serveDone`
  (closed when `Serve` returns) and, if that doesn't arrive within
  `shutdownCompletionDeadline` (30s), dumps goroutines and `os.Exit(1)` so
  launchd respawns. This catches a hung shutdown — note launchd's `KeepAlive`
  can't help on its own, since it only respawns on process *exit* and the zombie
  never exits. The deadline can't false-fire during a default-mode
  `awaitSessionDrain`: that path keeps the daemon alive WITHOUT closing
  `s.shutdown`, so the backstop only arms once `initiateShutdown` has fired, by
  which point exit should be prompt. Decision logic is the pure, unit-tested
  `awaitCompletionOrExit` (os.Exit kept out of the tested path, mirroring
  `wedgeMonitor.evaluate`). The watchdog's force-exit routes through the
  `s.exitFunc` seam (default `os.Exit`, overridable in tests) and the deadline
  through `s.shutdownDeadline` — so a test can drive the real backstop without
  killing the test binary. Both mirror the `staleCheck` seam.

`cleanup()` kills sessions **concurrently** (a local `WaitGroup` over
`sess.Kill`, then waits). Sequential kills made total cleanup scale as
`N × shutdownGraceSeconds`, which with many live sessions could exceed the
backstop deadline and trip a force-exit mid-cleanup. Concurrent-and-awaited keeps
cleanup bounded by ~one grace window regardless of N while still reaping every
child before the socket is removed and the process exits.

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

`runWedgeWatchdog` samples `dispatchSeq` (incremented when a dispatch completes) and **`shortInflight`** — the count of *short, deadline-wrapped* handlers in flight (`wedgeStats()`). It deliberately does NOT sample the raw `inflight` gauge: the by-design-blocking handlers (`handleAttach` for the session's whole life, `handleShutdown`) are excluded from the 10s deadline, so counting them made an idle daemon with parked attaches look identical to a wedged one — the 2026-06-22 false positive that force-killed 11 live sessions. If short work is in flight AND `dispatchSeq` has not advanced for 90 s (45 s when the binary is stale), the daemon is *suspected* wedged.

**On a suspected wedge the watchdog REPORTS — it does NOT `os.Exit`.** It sets the lock-free `s.suspectedWedged` flag (answered on `handleHello` → `HelloResp.SuspectedWedged`, so a newly dialing TUI is told at startup), emits a `daemon_wedged` `SessionEvent` to subscribers (the TUI raises a header banner with the `openkanban daemon restart` hint), and dumps goroutines to the log — once per episode. When `dispatchSeq` advances again the flag clears and a `daemon_unwedged` event is emitted so a connected TUI drops the banner. Both events carry an empty `TicketID` (daemon-global); the client handles them before the per-ticket switch in `applyDaemonSessionEvent`. Rationale: a dispatch wedge does NOT stop the per-session PTY pumps, so the running agents are fine; self-restarting would only destroy live work. With no TUI connected the suspicion is harmless. Recovery is operator-driven (`openkanban daemon restart`), not a unilateral self-kill.

The `os.Exit` force-restart is reserved for `awaitShutdownCompletion` — a *hung shutdown* (the genuine zombie: socket unlinked, process never exits), which only arms after `initiateShutdown` and is not destructive of live work. Its respawn only happens if the launchd service is loaded. When the service is not loaded the real respawner is the next TUI autostart, not launchd. The startup log emits a `WARN` for the `source=tui-fork`-despite-installed-plist state, and `openkanban daemon health` flags it; `openkanban daemon install-service` restores supervision.

That tui-fork-shadows-launchd state is now **rare**: the client autostart (`internal/daemonclient/dial.go` → `startDaemon`) prefers an installed launchd service over forking. When no daemon is listening AND a plist is installed, it calls `service.Start` (kickstart, or bootstrap+kickstart if not loaded) so launchd's supervised instance binds the socket — instead of a tui-fork that would grab the socket + pidlock and lock launchd's `KeepAlive` respawn out. It only forks when no plist is installed (incl. all non-Darwin) or when `service.Start` itself fails (best-effort fallback so the user still gets a daemon; the warning then surfaces the shadow). So a `source=tui-fork` with an installed plist now means launchd start failed, not that autostart ignored launchd.

A sustained *dispatch* wedge is now reported, not self-restarted (see above) — the daemon does not destroy live sessions to recover from a state that doesn't threaten them. The only self-restart paths left are the graceful stale-binary swap (`stalenessStep`, waits for session drain) and the hung-shutdown backstop. The TUI-side stallwatch is a separate, unrelated mechanism.

## Attach keepalive (half-open detection)

`binaryLoop` (`attach.go`) sets a per-iteration read deadline of `attachReadKeepalive` (60 s). On timeout that is NOT an external detach (the watcher's `DetachCh` trip), it probes the conn with a **zero-length `TypePTYOutput` frame** — the client skips it (`len(payload)==0 → continue`), and unknown frames are dropped, so it is backward-compatible with no new frame type. If the probe write fails the conn is dead and the attach is released; otherwise the deadline resets and reading continues. This bounds the latent leak where a half-open client conn (gone without a FIN/EOF) would pin a session's single attach slot until daemon restart. Unix sockets have no `SO_KEEPALIVE`, hence the app-level probe.

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

`daemon restart` is self-contained: it shuts the old daemon down (if one
is running), waits for the process to release its pidlock
(`daemonclient.WaitForExit` keyed on the pre-shutdown pid — the socket is
unlinked early in shutdown but the lock is held until process exit, so a
fork keyed on socket-gone would race `ErrAlreadyLocked`), then starts a
fresh detached daemon via `daemonclient.EnsureStarted`. It no longer
relies on the next launch to autostart — a clean shutdown exits 0, which
does NOT trip launchd's `KeepAlive={SuccessfulExit:false}`. Restarting a
*stopped* daemon just runs the start half. `openkanban daemon start` is
the same detached `EnsureStarted` start with no shutdown; bare `openkanban
daemon` stays the FOREGROUND entry point (launchd/autostart exec it).

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
