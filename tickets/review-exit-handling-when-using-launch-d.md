# review exit handling when using launch daemon

<!-- openkanban:card-notes start -->
## Notes (from openkanban card)

Reported symptom: when openkanbankd runs under macOS launchd, agent sessions appear to be "lost and restarted from previous turns" — Claude Code resumes from its JSONL one turn back. Investigate whether exit / shutdown / signal handling around the launchd-managed daemon is dropping in-flight PTYs.
<!-- openkanban:card-notes end -->

## Investigation

Diagnostic from `~/.cache/openkanban/daemon.log` (300+ lines tail; counts over a few days):

| Metric                                    | Count |
| ----------------------------------------- | ----- |
| Daemon startups (`SIGUSR1 ... handler ready`) | 33    |
| Explicit shutdowns (`shutdown initiated`) | 32    |
| Of which `(last client disconnected)`     | 28    |
| Of which `(client N requested)`           | 2     |
| Of which `(context cancelled)`            | 2     |
| `last client disconnected` total hits     | 60    |
| `WARN: exit-guard was bypassed` events    | 2     |
| `panic` lines                             | 0     |

Two takeaways, in order of severity:

1. **The daemon was never running under launchd in the observed period.** Every shutdown line is `(last client disconnected)` — that branch only fires in *non-persistent* mode (`internal/daemon/server.go:958-975` returns early when `s.persistent` is true). The persistent / launchd-managed daemon doesn't shut down on last-client-disconnect; this one did. So the daemon was the TUI-forked variant, not the launchd one. The ticket framed it as a launchd concern; in practice it's a TUI-fork concern.
2. **The TUI's fork sites omit `--persistent`.** `internal/daemon/autostart.go:125` and `internal/daemonclient/dial.go:198` both called `exec.Command(exe, "daemon")` with no `--persistent` flag. Every time the TUI closed, the daemon hit `handleLastClientDisconnect` → `initiateShutdown` → `cleanup`, killing every live agent PTY with a 3-second grace window. The next TUI launch saw an empty registry, Claude Code's `--continue` walked back to the JSONL's last complete turn (skipping any partially-written in-flight turn frame). Hence "sessions lost and restarted from previous turns."

## Fix

| Layer | Change |
| ----- | ------ |
| `internal/daemon/autostart.go`, `internal/daemonclient/dial.go` | Fork sites now pass `--persistent`. TUI-forked daemons no longer commit suicide on TUI close. |
| `internal/daemon/server.go` | Panic-safe defers added to `broadcastEvents`, `watchBinaryStaleness`, `watchSessionExit`. The last uses a two-tier recover so its "remove from registry + emit exited" cleanup invariant survives a panic in emit. `handleConn` deliberately unwrapped (protocol panics must surface). Startup log includes `persistent=...` and `source=...`. |
| `cmd/daemon.go` | `signal.NotifyContext` includes `syscall.SIGHUP` so logout / hangup paths run `cleanup()` instead of dying with exit 129. |
| `internal/service/launchd_darwin.go` | Plist gets `ExitTimeOut=30` (sized for sequential `cleanup()` × `shutdownGraceSeconds=3`) and `OPENKANBAN_DAEMON_SOURCE=launchd` in `EnvironmentVariables`. |
| `docs/AGENT_INTEGRATION.md` | New must-not-regress invariants 13–17. |

## Verification

Four falsifiable manual repros, each with a clear pre/post check:

1. **TUI fork persistence** (load-bearing — directly exercises the headline bug):
   - `openkanban daemon stop` (clean exit; `KeepAlive={SuccessfulExit:false}` keeps launchd from respawning).
   - `openkanban` → open a ticket → confirm `~/.cache/openkanban/daemon.log` shows `persistent=true source=tui-fork`.
   - Quit the TUI (`q` or `Ctrl-C` → exit-guard → confirm exit).
   - `openkanban daemon list` — daemon must still be running. **Pre-fix:** "openkanbankd is not running." **Post-fix:** `(no sessions)`.

2. **Graceful-SIGTERM cleanup budget** (exercises plist `ExitTimeOut`):
   - Spawn 3+ concurrent agent sessions.
   - `kill -TERM $(cat ~/.cache/openkanban/daemon.pid)`.
   - Tail the log: every session must produce `removed from registry` before the listener-closed line, then exit 0. Then `tail -1` each session's JSONL — final frame should be the last completed turn. **Pre-fix (if `ExitTimeOut` had been 10):** SIGKILL mid-cleanup, partial JSONL writes, Claude resumes one turn back. **Post-fix:** clean exit, all JSONLs flushed.

3. **SIGHUP graceful shutdown** (exercises Task 3):
   - `kill -HUP $(cat ~/.cache/openkanban/daemon.pid)`.
   - **Pre-fix:** process dies abruptly (exit 129, no shutdown log line). **Post-fix:** `shutdown initiated (context cancelled)` log line, exit 0.

4. **Panic safety** (exercises Task 2): covered by `TestWatchSessionExit_PanicSafety` in `internal/daemon/watch_session_exit_test.go` — injects a panicking emit, verifies daemon stays up + session removed from registry + panic logged.

## Known-not-fixed

`launchctl kickstart -k gui/$(id -u)/dev.openkanban.daemon` SIGKILLs the daemon by design. In-flight PTYs are lost — this is launchd contract, not an openkanban bug. The `--persistent` mode + KeepAlive plist semantics ensure a normal restart cycle (graceful exit, launchd respawn) preserves sessions only across *clean* exits, not against forced-kill.

## Out of scope (explicit)

- Idle-timeout for orphan TUI-forked daemons. Decision: stay up indefinitely, mirror launchd lifecycle. `watchBinaryStaleness` recycles them on the next `openkanban update`.
- `install-service` writing config to set `daemon.autostart: false`. With persistent-fork as the new default, the autostart toggle is no longer session-losing.
- Fixing `scripts/install.sh` to actually flip `daemon.autostart=false` (currently it only prints a hint at line 199). Same reason — no longer load-bearing.
- Removing the duplicate fork-daemon code path (`internal/daemon/autostart.go` vs `internal/daemonclient/dial.go` both implement it). Real refactor; separate ticket.
- `handleConn` panic recovery (deliberate per advisor — protocol panics must surface).
- `s.wg.Wait()` timeout in `Serve` (potential hang if `handleConn` deadlocks). Not the observed bug.
