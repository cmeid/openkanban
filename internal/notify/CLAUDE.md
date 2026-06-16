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

NSUserNotification on macOS 26 silently no-ops if the calling process is not running from a registered `.app` bundle. The daemon process must be launched from `~/Applications/OpenKanban.app/Contents/MacOS/openkanbankd` (or `/Applications/...` once signed) — NOT from `$PATH`. The lookup that ensures this lives in `internal/daemon/binary.go::ResolveBinary` and is shared by both `autostart` and the launchd service installer.

If you call `Send` from a binary that's running outside the bundle, the function returns `nil` (no API error) but the user sees nothing. There's no way to detect this from inside the cgo call — Apple deliberately keeps it silent.

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
- Don't try to detect "running outside bundle" — there's no reliable cross-macOS-version way to do it; ResolveBinary in `internal/daemon` is the single chokepoint
- Don't add UNUserNotificationCenter while keeping NSUserNotification — when migrating, swap fully; the async authorization dance changes the public API
