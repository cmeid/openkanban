# Watchdog accuracy + daemon resiliency

## Why this exists — mined from a live specimen (2026-06-22)

Debugging "new TUI can't see a session another TUI held" surfaced a chain of
defects. The running daemon (pid 899) was **force-restarted by its own wedge
watchdog at 19:02:20 on a FALSE POSITIVE, killing 11 live agent sessions**:

```
19:02:20 WEDGE WATCHDOG firing (stale binary wedged (no dispatch completion)); inflight=2 seq=939
19:02:20 exiting(1) for supervisor respawn
```

The goroutine dump showed **nothing stuck on a lock** — `inflight=2` was two
idle, by-design-parked `handleAttach` handlers; `dispatchSeq` was frozen only
because the daemon was quiet. It was healthy. It was destroyed anyway.

## Finding ledger (verified this session)

| # | Finding | Status | Evidence |
|---|---------|--------|----------|
| 1 | Attach-slot leak: `exitToBoard()` (model.go:1975) resets mode/focusedPane but never `pane.Detach()`. Leaving the agent view emits `SetViewing(false)` only — the daemon attach is held for the connection's whole life, not the view's. | CONFIRMED | client 26 held `cd67b77e` 17:51→death, zero `detached` events |
| 2 | Wedge `inflight` counts by-design-blocking handlers: `dispatch()` (server.go:1111-1116) does `inflight.Add(1)` around EVERY handler incl. `handleAttach`, which blocks for the session lifetime. Idle parked attaches read as "work in flight". | CONFIRMED | dump: inflight=2 = two parked attach goroutines |
| 3 | Detector is structurally near-tautological: `runHandlerWithDeadline` (server.go:63-77) abandons short handlers at 10s, so `dispatch()` returns and bumps `dispatchSeq` regardless. The ONLY thing that can sustain `inflight>0 + frozen seq` for 45-90s is `handleAttach`/`handleShutdown` (deadline-exempt). So the wedge condition fires almost exclusively on healthy-idle daemons; it cannot reliably catch a real handler-level wedge. | CONFIRMED | code analysis of dispatch + runHandlerWithDeadline |
| 4 | `staleWedge` 45s path force-kills live sessions — contradicting the graceful stale-restart's "never kill live work; wait for drain". The two mechanisms conflict; the wedge backdoors the very kill the graceful path refuses. | CONFIRMED | watchdog.go:83-84 + daemon CLAUDE.md stale-restart contract |
| 5 | New TUI on `already_attached`: the takeover modal DOES arm and DOES render (both P1 `doAttach`:3605 and P2 `attachExistingFastPath`:5474 → `attachConflictMsg` → `armTakeoverPrompt`). The real defect is narrower: the agent-view BACKDROP behind the modal is `blankPaneView` because `SetLastAttachErr` is not called before returning `attachConflictMsg`, so the session area is blank/uninformative ("nothing visible in the session"). | VERIFIED (hypothesis corrected) | armTakeoverPrompt:2119; renderAgentViewWithTakeoverModal:1288; paneview View():~1700-1729 |
| 6 | `binaryLoop` (attach.go:256-370) has no steady-state read-deadline/keepalive; no force-detach on client disconnect (server.go handleConn defer only `cleanupViewersForClient`, never iterates attaches). A half-open client conn pins the attach slot until daemon restart. | CONFIRMED (latent) | attach.go steady-state ReadFrame; server.go:1064-1077 + 1051 |
| 7 | Ruled out: snapshot/version-format skew — render/snapshot/emulator code byte-identical since Jun 19. Don't chase. | RULED OUT | git log --since 2026-06-19 empty for internal/terminal + snapshot path |

## Locked design

The headline insight (finding #3): you cannot reliably *detect* this wedge, and
the current *response* is catastrophic. So make the response non-destructive and
operator-driven — the watchdog becomes a REPORTER, not a guillotine.

A dispatch wedge does NOT stop running agents (PTY pumps drain on their own
per-session goroutines). So a wedge with **no TUI connected is harmless**; it
only matters when someone wants to interact — and then the daemon should TELL
them, not self-destruct.

### Phase 1 — Watchdog: report, don't `os.Exit`
- Retire the wedge `s.forceExit(1)` path (watchdog.go:124-133). On the wedge
  condition, instead: set a lock-free `s.suspectedWedged atomic.Bool` and
  `emitEvent(SessionEvent{Event: "daemon_wedged", Reason: ...})` to subscribers.
  Dump goroutines to the log (keep the postmortem).
- **KEEP `awaitShutdownCompletion`** (watchdog.go:146-159) untouched — that is a
  different, legitimate backstop for a hung *shutdown* (zombie daemon), only arms
  after `initiateShutdown`, and is not destructive of live work.
- Accuracy (#2): the wedge signal must sample only **short** (deadline-wrapped)
  handlers. Add `s.shortInflight atomic.Int64`, incremented at the start of the
  `runHandlerWithDeadline` goroutine and decremented on its completion (defer).
  `wedgeSample.inflight` is fed from `shortInflight`, not the raw `inflight`
  (which stays for health reporting). Parked attaches no longer register.
- #4 resolves for free: with the `os.Exit` wedge path gone, the `staleWedge`
  backdoor disappears. Stale-binary restart is handled ONLY by the graceful
  drain-based `stalenessStep` (never kills live work).

### Phase 2 — Hello fast-path tells a NEW TUI
- Add `SuspectedWedged bool` to `HelloResp` (protocol.go) — additive JSON field,
  back-compatible. `handleHello` reads `s.suspectedWedged` and reports it.
- A newly dialing TUI thus learns "daemon suspects it's wedged" on the hello
  handshake instead of hanging/going blank.

### Phase 3 — Attach-slot resiliency (#6)
- On client disconnect (server.go handleConn defer), force-`Detach` any session
  this client held attached — iterate `s.reg.snapshot()`, release where
  `s.attached.ClientID == c.id`. Belt: set a periodic read-deadline (or keepalive)
  so `binaryLoop` can observe a half-open conn rather than block forever.

### Phase 4 — Client/UI
- #5: call `pv.SetLastAttachErr(attachErr)` before returning `attachConflictMsg`
  on BOTH conflict paths (model.go ~3605 P1, ~5474 P2) so the backdrop renders
  the actionable overlay, not blank.
- #1: `exitToBoard()` (and other focus-drop paths) must release the daemon attach
  — call `pane.Detach()` when leaving the agent view so a backgrounded TUI stops
  hostage-holding the single attach slot. (Re-attach on re-entry; cheap snapshot.)
- Wedged signal: handle the `daemon_wedged` SessionEvent + `HelloResp.SuspectedWedged`
  → surface a modal/notification and offer operator-driven `daemon restart`
  (ends N sessions, knowingly). No TUI present → nothing happens.

## Acceptance
- Watchdog NEVER `os.Exit`s live sessions on a dispatch-wedge; it emits a
  `daemon_wedged` event + sets the flag. (Shutdown-completion backstop still exits
  a hung shutdown.)
- `wedgeMonitor` fed by short-handler inflight: idle parked attaches do not trip it
  (RED-first unit test in watchdog_test.go).
- `HelloResp.SuspectedWedged` plumbs the flag; new TUI is told.
- Client disconnect releases that client's attach slots; half-open conn can't pin a
  slot indefinitely.
- Leaving the agent view detaches; a second TUI can attach without takeover.
- P1 + P2 conflict paths set `lastAttachErr` → informative backdrop, not blank.
- `go build ./...` and `go test ./...` green.

## Must NOT
- Do not remove or weaken `awaitShutdownCompletion` (hung-shutdown backstop).
- Do not reintroduce force-kill of live sessions for a stale-binary swap.
- Do not take a session-level lock in a PTY callback / OSC handler.
- Do not mutate `m.*` from a tea.Cmd goroutine (UI rule).

## File anchors
- internal/daemon/watchdog.go — wedgeMonitor.evaluate, runWedgeWatchdog, awaitShutdownCompletion
- internal/daemon/server.go — dispatch() (~1111), runHandlerWithDeadline (~63), dispatchStats (~1915), handleHello (~1302), handleConn defer (~1064), emitEvent (~766)
- internal/daemon/protocol.go — HelloResp (~311), SessionEvent (~611)
- internal/daemon/attach.go — binaryLoop (256-370)
- internal/daemon/watchdog_test.go — pure evaluate tests (RED-first here)
- internal/ui/model.go — armTakeoverPrompt (2119), exitToBoard (1975), doAttach (~3590), attachExistingFastPath (~5450)
- internal/ui/daemon_subscribe.go — applyDaemonSessionEvent (~192)
- internal/daemonclient/paneview.go — detach (1009), View (~1700)

## Provenance
Specimen + full reasoning in assistant session (2026-06-22). Related memory:
[[reference_openkanban_known_latent_bugs]], [[reference_openkanban_takeover_warning_and_peek]],
[[reference_openkanban_daemon_concurrency]].
