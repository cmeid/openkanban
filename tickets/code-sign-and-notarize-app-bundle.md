# Code-sign and notarize OpenKanban.app for /Applications distribution

## Brief

OpenKanban.app ships unsigned today, installed to `~/Applications/OpenKanban.app` per Chris's machine. The notification path works (macOS 26 honored the first-run permission grant + the Launch Services registration), but the bundle isn't ready to leave this machine: a fresh macOS install would see "OpenKanban can't be opened because Apple cannot check it for malicious software" Gatekeeper warnings, and the install path would have to stay `~/Applications` to dodge stricter `/Applications` policies.

This ticket is to sign + notarize the bundle so it can be installed to `/Applications` and so any future user (or any fresh user-machine setup) sees a clean trust chain.

## Why

1. **`/Applications` is the macOS-native install location.** Spotlight indexes it first; macOS's "default apps" surfaces it; users expect tools they install to live there. `~/Applications` is per-user and second-class.
2. **Gatekeeper enforcement is tightening per macOS release.** Today's "right-click → Open" workaround for unsigned apps may not survive future macOS versions. Signed + notarized is the only forward-compatible path.
3. **Notarization is the only way to bypass quarantine on a downloaded artifact.** If openkanban ever ships via a download URL or homebrew tap, an unsigned bundle gets quarantined on first launch and the user has to click through a security dialog. Notarized = no friction.
4. **Code signing is a prerequisite for some macOS APIs.** UNUserNotificationCenter (the modern notification API, deprecating the NSUserNotification path we use today) increasingly assumes signed apps. If we ever migrate, signing is on the critical path.

## How

### Prerequisites

1. **Apple Developer Program membership** — $99/year for individuals; $299 for organizations. Required to obtain a signing certificate. Chris is the maintainer; sign under his Apple ID or under a Manifold-org account if one exists.
2. **Developer ID Application certificate** — created via Xcode > Settings > Accounts > Manage Certificates, or via the Apple Developer portal. Must be the "Developer ID Application" type (NOT "Mac App Store" — different distribution path).
3. **App-specific password for `notarytool`** — generated at appleid.apple.com → Sign-In and Security → App-Specific Passwords. Used in the notarization step.
4. **Xcode command line tools** — for `codesign` and `notarytool`. Already installed if you've ever run `xcode-select --install`.

### Concrete steps

1. **Bundle ID decision.** Current `dev.cmeid.openkanban` works for Chris's personal signing identity. If signing under a Manifold Apple ID, change to `security.manifold.openkanban` (matches reverse-DNS of `manifold.security`). See the bundle-ID note in `reference_openkanban_app_bundle_layout`. Decide BEFORE signing — changing later invalidates user notification permission grants.

2. **Hardened runtime + entitlements.** Add an entitlements file at `dist/macos/openkanban.entitlements`:
   ```xml
   <?xml version="1.0" encoding="UTF-8"?>
   <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
   <plist version="1.0">
   <dict>
     <key>com.apple.security.cs.allow-unsigned-executable-memory</key>
     <true/>
     <!-- Required if Go's cgo NSUserNotification path triggers JIT-like memory write+execute.
          Verify empirically; remove if signing succeeds without it. -->
   </dict>
   </plist>
   ```
   The Go runtime sometimes trips hardened-runtime checks. Start permissive, tighten after empirical testing.

3. **Sign the bundle.** After `dist/macos/build-bundle.sh` produces `OpenKanban.app`:
   ```bash
   codesign --force --options runtime --timestamp \
     --entitlements dist/macos/openkanban.entitlements \
     --sign "Developer ID Application: Christopher Meidinger (TEAMID)" \
     ~/Applications/OpenKanban.app
   ```
   Replace TEAMID with the 10-character Team ID from Apple Developer portal.

4. **Verify signature locally:**
   ```bash
   codesign --verify --deep --strict --verbose=2 ~/Applications/OpenKanban.app
   spctl --assess --type execute --verbose ~/Applications/OpenKanban.app  # should say "accepted"
   ```

5. **Notarize.** Zip the bundle and submit:
   ```bash
   ditto -c -k --keepParent ~/Applications/OpenKanban.app /tmp/OpenKanban.zip
   xcrun notarytool submit /tmp/OpenKanban.zip \
     --apple-id "chris@manifold.security" \
     --team-id "TEAMID" \
     --password "<app-specific password>" \
     --wait
   ```
   The `--wait` blocks until Apple finishes scanning (typically 1–15 minutes). On success, returns a notarization ticket UUID.

6. **Staple the notarization ticket to the bundle:**
   ```bash
   xcrun stapler staple ~/Applications/OpenKanban.app
   xcrun stapler validate ~/Applications/OpenKanban.app  # should say "Accepted"
   ```
   Stapling lets the bundle pass Gatekeeper offline (without needing to phone home to Apple's notarization servers).

7. **Update `scripts/install.sh`** to optionally sign/notarize when an `OPENKANBAN_SIGNING_IDENTITY` env var is set. Keep the unsigned path working for dev iteration; signing only runs when explicitly enabled.

8. **Move install path to `/Applications`.** `internal/daemon/binary.go::ResolveBinary` already checks `~/Applications` first then `/Applications`; flip the order, or just have the install script place the bundle in `/Applications` once signed. `~/Applications` becomes the unsigned-dev fallback.

9. **Document the signing workflow** in `dist/macos/SIGNING.md` (new file) — capture the credentials sources, the Team ID, the entitlements rationale, and the notarytool keychain profile setup so future maintainers don't have to re-derive any of this.

### Verification on a clean machine

Test on a Mac that has never seen this bundle:
```bash
# Download the signed .app, place in /Applications, then:
spctl --assess --type execute --verbose /Applications/OpenKanban.app   # accepted, source=notarized
xattr -p com.apple.quarantine /Applications/OpenKanban.app             # quarantine should be CLEARED post-staple
open /Applications/OpenKanban.app                                       # no Gatekeeper prompt
```
Then run openkanban and trigger a Claude waiting state — notification fires under the signed identity, system permission persists across reinstall.

## What to avoid

- **Don't lose the signing certificate's private key.** It lives in your login keychain. Export it (Keychain Access > export `.p12`) and store the export in 1Password before doing anything destructive.
- **Don't sign with a "Mac App Store" certificate** — that's a different distribution path requiring sandboxing. "Developer ID Application" is the right type for direct distribution.
- **Don't hard-code credentials.** Use `notarytool store-credentials` to save an Apple-ID + app-password tuple as a keychain profile; reference it by name in CI/scripts.
- **Don't skip the staple step.** Without stapling, Gatekeeper needs network access to verify on first launch — fine for online machines, bad UX in airgapped/offline scenarios.

## Related context

- Bundle layout, build script: `dist/macos/build-bundle.sh`, `dist/macos/Info.plist`
- See also (memory): `reference_openkanban_app_bundle_layout`, `reference_macos_notification_bundle_requirements`
- This was deferred from the notification arc (PR #12 shipped unsigned)
