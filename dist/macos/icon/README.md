# macOS app icon

Placeholder icon for the openkanban macOS app bundle. `AppIcon.icns` is the
multi-size icon ready to drop into `Contents/Resources/`; `icon-1024.png` is
the 1024x1024 master; `gen.go` is the Go program that draws the master (kept
so the icon can be regenerated from source).

To regenerate from this directory:

```bash
mkdir -p /tmp/openkanban-icon && go run gen.go            # writes /tmp/openkanban-icon/icon-1024.png
cp /tmp/openkanban-icon/icon-1024.png ./icon-1024.png
mkdir -p AppIcon.iconset
for s in 16 32 128 256 512; do
  d=$((s*2))
  sips -z $s $s   icon-1024.png --out AppIcon.iconset/icon_${s}x${s}.png
  sips -z $d $d   icon-1024.png --out AppIcon.iconset/icon_${s}x${s}@2x.png
done
cp icon-1024.png AppIcon.iconset/icon_512x512@2x.png
iconutil -c icns AppIcon.iconset -o AppIcon.icns
rm -rf AppIcon.iconset
```
