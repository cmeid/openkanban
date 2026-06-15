# macOS App Bundle

The `openkanbankd` daemon ships inside a real `.app` bundle so that user-facing
notifications carry the OpenKanban name and icon. macOS attributes notifications
to the `CFBundleIdentifier` (`dev.cmeid.openkanban`) of the process that posted
them; without a bundle, alerts would appear as generic "Terminal" or "Script
Editor" toasts with no icon.

The `openkanban` TUI itself stays on `$PATH` as a regular binary. Only the
daemon needs the bundle identity.

## Regenerating

1. Build the daemon: `go build -o /tmp/openkanbankd ./cmd/openkanbankd`
2. Assemble the bundle: `./dist/macos/build-bundle.sh /tmp/openkanbankd /tmp/out`
3. Install: `cp -R /tmp/out/OpenKanban.app ~/Applications/`

The script is idempotent — an existing `OpenKanban.app` in the output directory
is removed and rebuilt — and it calls `lsregister` so Launch Services picks up
the new identity immediately.

## Signing

The bundle is currently unsigned. macOS Gatekeeper may prompt the first time
the daemon runs. Proper code signing and notarization is a future step.
