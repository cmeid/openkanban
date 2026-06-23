# Notify Package

Thin platform-abstracted desktop notification dispatcher used by the daemon when a wrapped agent emits an OSC 9 sequence.

## API

```go
notify.Send(body string) error
```

`Send` accepts the OSC 9 payload verbatim (1:1 transparent content) and fires a desktop notification. Title is left empty so macOS uses `CFBundleDisplayName` from the surrounding `.app` bundle.

Errors are returned but rarely actionable from the caller — the daemon logs them and continues.

## Platform Builds

- `notify.go` — package-level `Send` is a `var` (not `func`) so tests can swap it
- `notify_darwin.go` — cgo wrapper around `NSUserNotification` (Foundation framework); active on `GOOS=darwin`
- `notify_other.go` — no-op stub; active on every other GOOS via build tags. Returns nil; the daemon doesn't need cross-platform stubs to work, but `go build` should succeed everywhere.

## Critical: must run from inside an .app bundle

The darwin backend is now `UNUserNotificationCenter` (migrated 2026-06-18; see `notify_darwin.go`). It only delivers from a process running inside a registered `.app` bundle, so the daemon must be launched from `~/Applications/OpenKanban.app/Contents/MacOS/openkanbankd` (or `/Applications/...` once signed) — NOT from `$PATH`. The lookup that ensures this lives in `internal/daemon/binary.go::ResolveBinary` and is shared by both `autostart` and the launchd service installer.

**Off-bundle is FATAL unless guarded — and it is.** Unlike the old `NSUserNotification` (which silently no-op'd off-bundle), `[UNUserNotificationCenter currentNotificationCenter]` raises an `NSInternalInconsistencyException` and `abort()`s the **whole process** when there is no bundle identity. From a `$PATH`/tui-fork daemon that would crash the daemon (killing every live session) the first time a notification fires; from a `go test` binary it SIGABRTs the suite. So `openkanbanSendNotification` early-returns when `[[NSBundle mainBundle] bundleIdentifier] == nil`, restoring the silent-off-bundle contract: `Send` returns `nil` (no API error) but the user sees nothing. **Do not remove that guard** — it is load-bearing, not politeness.

Bundle assembly contract lives in `dist/macos/`. See:
- `dist/macos/Info.plist` — `CFBundleIdentifier=dev.cmeid.openkanban`, `LSUIElement=true`
- `dist/macos/build-bundle.sh <binary> <output-dir>` — idempotent; calls `lsregister -f`
- `dist/macos/icon/` — placeholder icon assets; regenerate from `gen.go`

## Test seam

```go
saved := notify.Send
notify.Send = func(body string) error { received = body; return nil }
defer func() { notify.Send = saved }()
```

The `internal/terminal` OSC 9 handler test exercises this seam end-to-end — drives OSC 9 bytes through a real `charm/x/vt` emulator and asserts our handler invokes the stubbed `Send` with the stripped payload.

The package's own e2e test (`notify_darwin_test.go`) is opt-in via `OPENKANBAN_NOTIFY_E2E=1` because it actually fires a real macOS notification — useful during local development, not in CI.

## Anti-Patterns

- Don't add a title argument — the bundle's display name IS the title, by design (1:1 passthrough of the agent's OSC 9 payload)
- Don't construct enriched notification text (e.g. prepending ticket name) — that's the daemon's caller decision, not this package's
- Don't remove the `bundleIdentifier == nil` guard in `openkanbanSendNotification`. (This reverses the old "don't detect outside-bundle" guidance, which applied to NSUserNotification's *silent* off-bundle no-op — detection bought nothing then. `UNUserNotificationCenter` instead `abort()`s off-bundle, so the check is now mandatory; and `bundleIdentifier == nil` is reliable here because it is the exact precondition the framework itself aborts on, one call earlier.) `ResolveBinary` in `internal/daemon` remains the chokepoint for launching *from* the bundle; the guard is the backstop for when we aren't.
- Don't add UNUserNotificationCenter while keeping NSUserNotification — when migrating, swap fully; the async authorization dance changes the public API
