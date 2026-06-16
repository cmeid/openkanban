# Install openkanbankd as a launchd LaunchAgent for persistent daemon

## Brief

`openkanbankd` currently runs in two modes (per `feedback_openkanban_daemon_lifecycle`): the default TUI-bound lifecycle (exits when the last TUI client disconnects), and a `--persistent` mode opted in via `daemon install-service`. The persistent-mode plumbing has been written (`cmd/daemon_service.go`, `internal/service/launchd_darwin.go`), but the LaunchAgent has not actually been installed on this machine. This ticket is to enable that — to make `openkanbankd` start at user-login and run continuously.

## Why

1. **Notifications survive TUI absence.** Today, when no openkanban TUI is attached, no daemon runs, so no notifications fire. If a Claude session were running headless (spawned via CLI, no TUI attached), the user wouldn't see any "waiting for input" prompts. With a launchd-managed daemon, the notification path stays live regardless of TUI state — that's the architectural promise we deliberately built into the app-bundle approach (per `project_openkanban_notifications_architecture`).
2. **Faster spawn.** A pre-warmed daemon shaves the autostart latency off `openkanban` CLI invocations. The TUI dials an already-running socket instead of forking + waiting for handshake.
3. **Decouples daemon lifecycle from interactive sessions.** Background tasks the daemon owns (PTY watching, status broadcasts, session bookkeeping) currently die when the last TUI exits. Persistent daemon keeps them alive.
4. **Matches modern macOS service patterns.** Background tools on macOS are expected to be launchd-managed; user-process daemons that die with their controlling terminal are a Unix-ism with rough edges on macOS.

## How

### Prerequisites

- OpenKanban.app installed at `~/Applications/OpenKanban.app` (or `/Applications/OpenKanban.app` post-signing). `cmd/daemon_service.go` already uses `internal/daemon/binary.go::ResolveBinary` to find the bundled daemon binary, so the LaunchAgent will point INTO the bundle — which is what gives notifications the OpenKanban identity.
- No other openkanban TUI / daemon running at install time (`daemon install-service` refuses to clobber a live daemon).

### Concrete steps

1. **Run the installer:**
   ```bash
   openkanban daemon install-service
   ```
   This writes `~/Library/LaunchAgents/dev.cmeid.openkanban.plist` (or whatever bundle ID is current) pointing at the daemon binary inside the bundle, and calls `launchctl bootstrap gui/$(id -u)` to register it with launchd. The plist sets `KeepAlive=true { SuccessfulExit=false }` so launchd restarts the daemon if it crashes but NOT if it exits cleanly.

2. **Verify it's running:**
   ```bash
   launchctl list | grep openkanban
   pgrep -fl openkanbankd
   openkanban daemon status   # should report "daemon running, persistent"
   ```

3. **Verify notifications still work.** From a fresh openkanban session:
   - Spawn a Claude session via a ticket.
   - Trigger a waiting-for-input state.
   - Confirm macOS notification appears with OpenKanban identity.
   The daemon doing the work is now the LaunchAgent one, not a TUI-spawned one.

4. **Verify TUI fork is suppressed.** `cmd/root.go` already supports `--no-launch-daemon` and a `cfg.Daemon.Autostart` config knob. With the LaunchAgent running, the TUI should detect the live daemon via the unix socket and skip autostart. Confirm via `openkanban daemon status` showing "1 client (TUI)" not "no clients."

5. **Update the user's openkanban config** to set `daemon.autostart=false` (or rely on `--no-launch-daemon` per-invocation). This is the long-term clean state: launchd owns the daemon; the TUI never autostarts; if the daemon dies, launchd restarts it within seconds.

6. **Configure log rotation.** The LaunchAgent's `StandardOutPath` / `StandardErrorPath` (set by `internal/service/launchd_darwin.go`) writes to `~/Library/Logs/openkanbankd.{out,err}.log`. macOS doesn't rotate these — they grow unbounded. Add a `newsyslog.conf` snippet or a launchd-managed logrotate. Defer this if the log volume is low.

### Verification checklist

- [ ] `~/Library/LaunchAgents/<bundle-id>.plist` exists, points at `~/Applications/OpenKanban.app/Contents/MacOS/openkanbankd`
- [ ] `launchctl list <bundle-id>` shows status (PID if alive, exit code if not)
- [ ] Daemon survives a logout + login (launchd brings it back up)
- [ ] Daemon survives a kill -9 (launchd restarts within seconds due to KeepAlive)
- [ ] Notifications fire from a Claude session even with NO openkanban TUI open
- [ ] `openkanban daemon uninstall-service` cleanly removes the plist + bootouts launchd (test the unwind path before depending on it)

### Reversal path

If launchd-managed daemon causes problems (resource leaks, hung-process issues, unexpected interactions with notifications):
```bash
openkanban daemon uninstall-service     # bootouts + removes plist
pkill -f openkanbankd                    # kill any residual
```
TUI will resume autostarting its own daemon as before.

## What to avoid

- **Don't install the LaunchAgent under unsigned-app development cycles.** Every time you `./scripts/install.sh` and replace the bundle, the LaunchAgent's plist path may need re-bootouting + re-bootstrapping for launchd to notice the new binary. Easier to NOT install the service until the bundle is stable (likely post-signing, this ticket waits on `code-sign-and-notarize-app-bundle`).
- **Don't manually edit `~/Library/LaunchAgents/<bundle-id>.plist`** — let `daemon install-service` regenerate it. Hand-edits get clobbered on next install.
- **Don't use `launchctl load/unload`** — those are deprecated in favor of `launchctl bootstrap/bootout` per Apple's launchd transition. `internal/service/launchd_darwin.go` already uses the modern verbs.

## Soft dependency

This ticket is best done AFTER `code-sign-and-notarize-app-bundle`. Reason: an unsigned bundle in `/Applications` triggers Gatekeeper warnings on each launchd-initiated launch; under `~/Applications` macOS is more lenient. Once signed, install the bundle into `/Applications` and then install the LaunchAgent pointing at it.

## Related context

- Service abstraction philosophy: `feedback_openkanban_no_premature_service_abstraction` (keep `internal/service/` as build-tag-gated files; no Backend interface until a second backend lands)
- Daemon lifecycle modes: `feedback_openkanban_daemon_lifecycle`
- Launch system architecture: `reference_openkanban_launchd_service`
- See also (memory): `project_openkanban_notifications_architecture` for why notifications-survive-without-TUI was a design goal
