#!/usr/bin/env bash
# Assemble an OpenKanban.app bundle around the openkanbankd daemon binary.
#
# Usage: build-bundle.sh <openkanbankd-binary-path> <output-dir>
#
# Produces <output-dir>/OpenKanban.app/ with Info.plist, the daemon binary at
# Contents/MacOS/openkanbankd, and the app icon at Contents/Resources/AppIcon.icns.
# Registers the bundle with Launch Services so macOS picks up its identity for
# notifications. Idempotent: an existing OpenKanban.app at the destination is
# removed and rebuilt from scratch.

set -euo pipefail

if [[ $# -ne 2 ]]; then
    echo "usage: $0 <openkanbankd-binary-path> <output-dir>" >&2
    exit 2
fi

DAEMON_BIN="$1"
OUTPUT_DIR="$2"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INFO_PLIST="${SCRIPT_DIR}/Info.plist"
ICON_FILE="${SCRIPT_DIR}/icon/AppIcon.icns"

if [[ ! -f "${DAEMON_BIN}" ]]; then
    echo "error: daemon binary not found: ${DAEMON_BIN}" >&2
    exit 1
fi
if [[ ! -f "${INFO_PLIST}" ]]; then
    echo "error: Info.plist not found: ${INFO_PLIST}" >&2
    exit 1
fi
if [[ ! -f "${ICON_FILE}" ]]; then
    echo "error: icon not found: ${ICON_FILE}" >&2
    exit 1
fi

BUNDLE_DIR="${OUTPUT_DIR}/OpenKanban.app"

echo "==> Preparing output directory: ${OUTPUT_DIR}"
mkdir -p "${OUTPUT_DIR}"

if [[ -e "${BUNDLE_DIR}" ]]; then
    echo "==> Removing existing bundle: ${BUNDLE_DIR}"
    rm -rf "${BUNDLE_DIR}"
fi

echo "==> Creating bundle skeleton at ${BUNDLE_DIR}"
mkdir -p "${BUNDLE_DIR}/Contents/MacOS"
mkdir -p "${BUNDLE_DIR}/Contents/Resources"

echo "==> Copying Info.plist"
cp "${INFO_PLIST}" "${BUNDLE_DIR}/Contents/Info.plist"

echo "==> Copying daemon binary -> Contents/MacOS/openkanbankd"
cp "${DAEMON_BIN}" "${BUNDLE_DIR}/Contents/MacOS/openkanbankd"
chmod +x "${BUNDLE_DIR}/Contents/MacOS/openkanbankd"

echo "==> Copying icon -> Contents/Resources/AppIcon.icns"
cp "${ICON_FILE}" "${BUNDLE_DIR}/Contents/Resources/AppIcon.icns"

# Re-sign the assembled bundle as one coherent unit.
#
# Go's linker leaves the binary ad-hoc-signed with Identifier=a.out and
# Info.plist=not bound. macOS 26 (Tahoe) silently drops notifications
# whose calling process has a signed identifier that doesn't match the
# bundle ID declared in Info.plist. We re-sign here with --identifier
# dev.cmeid.openkanban (matching CFBundleIdentifier) and --deep so the
# whole bundle (binary + Info.plist + Resources) seals as one unit;
# without this every install.sh run produces a notifications-broken
# bundle even when the Go code is correct.
#
# --sign - is intentional ad-hoc signing for local/dev installs. When
# we move to Developer-ID-signed + notarized builds, replace "-" with
# the team's Developer ID Application identity and add a notarization
# step after this block. The --options=runtime flag opts into the
# hardened runtime, which is required for notarization and is harmless
# for ad-hoc builds.
echo "==> Code-signing bundle as dev.cmeid.openkanban (deep)"
if ! codesign --force --deep --identifier dev.cmeid.openkanban \
              --sign - --options=runtime "${BUNDLE_DIR}" 2>&1; then
    echo "error: codesign failed; macOS may silently drop notifications from this bundle" >&2
    exit 1
fi
if ! codesign --verify --deep --strict "${BUNDLE_DIR}" 2>&1; then
    echo "error: bundle failed post-sign verification" >&2
    exit 1
fi

LSREGISTER="/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"
if [[ -x "${LSREGISTER}" ]]; then
    echo "==> Registering bundle with Launch Services"
    "${LSREGISTER}" -f "${BUNDLE_DIR}"
else
    echo "warning: lsregister not found at ${LSREGISTER}; skipping registration" >&2
fi

echo "==> Done: ${BUNDLE_DIR}"
