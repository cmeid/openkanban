# build handling for a stuck session

## Brief

A large clipboard paste into an attached agent session's input wedged the
session and then the whole daemon. Live investigation (2026-06-17, daemon
SIGUSR1 goroutine dump captured in `.stuck-session-capture/`) found the root
cause was a **lock-held-across-blocking-IO deadlock**, not (only) the render
storm first suspected:

1. **Seed — `Pane.WriteInput` held `p.mu` across a blocking `p.pty.Write`**
   (`internal/terminal/pane.go`). Forwarding the giant paste blocked on claude's
   full PTY input buffer (claude busy), and `p.mu` stayed pinned (goroutine 342,
   67 min). With `p.mu` held, `handleOutput` (the PTY→emulator output drain)
   couldn't run, so claude's output also stopped draining → the session wedged
   completely ("unresponsive session").
2. **Cascade into the daemon** — `handleList → Session.Info → Pane.Size` takes
   that pane's `p.mu` *while holding `sessionsMu.RLock`*. So the one stuck pane
   pinned `sessionsMu`; queued `handleTicketDone` writers (Go writer-priority)
   then stalled every subsequent client RPC → daemon-wide hang while background
   broadcasts kept ticking.
3. **Render storm (secondary, TUI-side)** — the paste echo also arrived as a
   flood of 64 KB frames, each emitting a `PaneOutputMsg` that forced a full
   `RenderVT` rebuild (`attachLoop` / `paneview.go`). N frames ⇒ N re-renders,
   spiking the TUI client to ~2 GB. Bounded scrollback (10k ring) means this is
   allocation churn, not a leak.

A second, pre-existing deadlock surfaced while testing: **`teardownEmulatorLocked`
woke the drain goroutine by writing a sentinel byte to the emulator's
*synchronous* `io.Pipe`** — which blocked forever whenever the drain had already
stopped reading (no reader), hanging teardown and any caller holding `p.mu`.

## Scope landed (this session)

This session owns the **underlying session hang** (the daemon-resilience
cascade — `sessionsMu` lock-ordering in `handleList` — is a sibling session's
lane). Three fixes, each with a red→green test, all green under `go test -race`:

- **`WriteInput` lock release** (`internal/terminal/pane.go`) — snapshot the pty
  handle under `p.mu`, release `p.mu`, then write under a dedicated leaf
  `inputMu` (preserves writer serialization without pinning `p.mu`). A stalled
  child can no longer wedge the pane. *Test:* `TestPane_WriteInputDoesNotHoldLockAcrossBlockingWrite`.
- **Render coalescing** (`internal/daemonclient/paneview.go`) — a
  `renderSignalPending` flag collapses a burst of output frames into one render
  signal (`applyOutput` still writes every byte; only the render signal is
  gated). Reset on emulator (re)init so it never strands true across re-attach.
  *Test:* `TestPaneView_CoalescesRenderSignals` / `…RearmsAfterConsume`.
- **Teardown deadlock fix** (`internal/daemonclient/paneview.go`) — unblock the
  drain by `InputPipe().(*io.PipeWriter).CloseWithError(io.EOF)`: makes `Read`
  return EOF, never blocks (vs the sentinel), and touches no unsynchronized
  emulator state (vs `Emulator.Close()`, which races `e.closed` under `-race` —
  see the sibling `terminal.Pane.stopDrainUnlocked`). *Test:* `TestPaneView_TeardownDoesNotDeadlock`.

## Deferred (follow-ups, intentionally not in this session)

- **Bracketed-paste forwarding** — wrap a `KeyMsg{Paste:true}` in
  `\x1b[200~ … \x1b[201~` (gated on the child's `?2004h`) so claude ingests the
  paste atomically and drains its input fast. Pure prevention now that the
  `WriteInput` fix removes the hang; reduces the flood that triggers a slow
  drain. Anchor: `translateKey` / `HandleKey` in `paneview.go`.
- **Watchdog self-detach (auto-recovery)** — give `stallwatch.go` teeth
  (detach-to-board via `program.Send` on a detected stall). TUI-side graceful
  recovery; overlaps the graceful-handling session's lane — coordinate.
- **`handleList`/`Session.Info` lock-ordering** — don't take a per-pane lock
  while holding `sessionsMu` (the cascade amplifier). Sibling session's lane.

## Acceptance (met)

- A blocked `WriteInput` does not hold `p.mu` (a concurrent `Size()` returns).
- A burst of N output frames between two model consumes yields one
  `PaneOutputMsg`; emulator state still reflects all N.
- Repeated init/output/teardown never deadlocks (stress test, also `-race`).
- `go build ./...`, `go vet`, and `go test -race` on `terminal` / `daemonclient`
  / `ui` are green; each fix fails its test when reverted (red-before-green).

## File anchors

- `internal/terminal/pane.go` — `WriteInput`, `inputMu`.
- `internal/daemonclient/paneview.go` — `signalRender`/`consumeRenderSignal`,
  `attachLoop`, `initEmulatorLocked`, `teardownEmulatorLocked`.

## Context (read these)

- `.stuck-session-capture/ANALYSIS.md` — captured goroutine dump + cascade.
- [[project_openkanban_personal_fork]]
- [[reference_openkanban_tui_stall_watchdog]]
