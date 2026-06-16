# Replace placeholder icon with custom OpenKanban icon

## Brief

The OpenKanban.app bundle currently ships with a placeholder icon I generated programmatically during the notification-architecture work (PR #12). It reads as a kanban board (three columns of cards, middle "in-progress" card highlighted in Manifold-ish red on a dark slate background) but is clearly placeholder — flat geometric shapes, no character, no openkanban brand identity. It functions correctly at every macOS icon size; it just isn't the icon you'd want representing the project to users.

This ticket is to replace it.

## Why

1. **Code-signing distribution** (parallel ticket `code-sign-and-notarize-app-bundle`) is the right moment to lock in a real icon. Once signed + notarized, the icon becomes part of the bundle's distributed identity; replacing it later is a re-sign + re-notarize round-trip.
2. **macOS Notification Center renders the bundle icon** in the notification card. Today every Claude-waiting notification shows the placeholder geometry. A real icon = clearer "this is from openkanban" signal at a glance.
3. **System Settings → Notifications** lists the app with its icon. Placeholder geometry there reads as "unfinished tool" to anyone the user shares their machine with.
4. The placeholder is documented as placeholder (`dist/macos/icon/README.md`) and the regeneration path (`dist/macos/icon/gen.go`) makes it explicit this is temporary scaffolding.

## How

### Constraints

- macOS app icons must fill the full canvas (the OS applies the rounded-squircle mask itself; do NOT draw rounded corners into the artwork — leads to double-rounding).
- Must read at every standard size: 16, 32, 64, 128, 256, 512, 1024 (+ @2x variants of all except 1024). Test the 16px and 32px renders early — geometric detail collapses fast.
- The 1024×1024 master is what gets shipped; everything else is downsampled via `sips`.

### Concrete steps

1. **Design the master.** 1024×1024 PNG. Keep it identifiable at 16×16 — usually one bold mark + at most one supporting element. Avoid fine typography; "OK" monogram or a stylized kanban-card glyph are starting points. Reference the placeholder at `dist/macos/icon/icon-1024.png` for what NOT to ship (too generic, too literal).
2. **Replace files in `dist/macos/icon/`:**
   - `icon-1024.png` — new master
   - `AppIcon.icns` — regenerated via the script below (or via `dist/macos/icon/gen.go` if you keep using a programmatic source — though for a real icon you'll probably hand-design the master and skip the generator)
3. **Regenerate the .icns:**
   ```bash
   cd dist/macos/icon
   mkdir -p AppIcon.iconset
   for sz in 16 32 64 128 256 512 1024; do
     sips -s format png -z $sz $sz icon-1024.png --out AppIcon.iconset/icon_${sz}x${sz}.png
     if [[ $sz -lt 1024 ]]; then
       sz2=$((sz * 2))
       sips -s format png -z $sz2 $sz2 icon-1024.png --out AppIcon.iconset/icon_${sz}x${sz}@2x.png
     fi
   done
   iconutil --convert icns AppIcon.iconset --output AppIcon.icns
   rm -rf AppIcon.iconset
   ```
   Both `sips` and `iconutil` are built-in macOS — no brew deps.
4. **Update `dist/macos/icon/README.md`** to remove the "placeholder" warning and document the new icon's origin/license.
5. **Rebuild + reinstall the bundle locally** to verify:
   ```bash
   ./scripts/install.sh    # rebuilds OpenKanban.app via dist/macos/build-bundle.sh
   ```
   Then check System Settings → Notifications → OpenKanban shows the new icon, and fire a test notification (any Claude session that hits a waiting-for-input state) to verify the notification card renders cleanly.
6. **If you've already code-signed the bundle**, re-sign + re-notarize after the icon swap. The icon is part of the signature bundle.

### What to avoid

- Don't draw rounded corners into the artwork (macOS rounds the squircle itself).
- Don't ship an icon that requires zooming in to read at 16×16 — it'll look smudged in Notification Center's small thumbnail.
- Don't change the `gen.go` programmatic generator to render the final icon; for a real icon use a vector tool (Affinity Designer, Figma, Sketch, Illustrator) and export a high-quality master. The generator can stay around for regenerating a placeholder if needed, but the master should be artist-authored.

## Related context

- Placeholder shipped in PR #12 (`feat(notify): macOS notifications via bundle`)
- Bundle layout, install path, build script: `dist/macos/build-bundle.sh`, `dist/macos/Info.plist`
- See also (memory): `reference_macos_notification_bundle_requirements`, `reference_openkanban_app_bundle_layout`, `project_openkanban_notifications_architecture`
