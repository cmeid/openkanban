package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// envBinary is the test/CI override env var honored by ResolveBinary as the
// first-priority lookup. Must match the constant of the same purpose in
// internal/daemonclient/dial.go so a single export drives both sides.
const envBinary = "OPENKANBAN_DAEMON_BINARY"

// BinarySource describes which lookup branch produced the path returned by
// ResolveBinary. Callers that want to surface "branded vs fallback" status
// (e.g. install-service) can switch on this rather than re-parsing the path.
type BinarySource int

const (
	// BinarySourceEnv means $OPENKANBAN_DAEMON_BINARY was set and used.
	BinarySourceEnv BinarySource = iota
	// BinarySourceBundle means a bundled OpenKanban.app/Contents/MacOS/
	// openkanbankd was found and used. Notifications will carry the
	// OpenKanban app identity (CFBundleIdentifier=dev.cmeid.openkanban).
	BinarySourceBundle
	// BinarySourceFallback means os.Executable() was used because no
	// bundle was found. Notifications will be attributed to the parent
	// terminal app instead of OpenKanban.
	BinarySourceFallback
)

// ResolveBinary picks the binary that should be exec'd as the daemon —
// whether by the TUI autostart fork, the daemon package's own DialOrStart,
// or the launchd service installer. Search order:
//
//  1. $OPENKANBAN_DAEMON_BINARY (test/CI override; honored verbatim).
//  2. ~/Applications/OpenKanban.app/Contents/MacOS/openkanbankd — the
//     per-user bundle install. Required for macOS notifications to carry
//     the OpenKanban identity (CFBundleIdentifier=dev.cmeid.openkanban).
//     Falls through if absent or not executable.
//  3. /Applications/OpenKanban.app/Contents/MacOS/openkanbankd — system-wide
//     bundle install (anticipating a future signed/notarized release).
//  4. os.Executable() — the binary the caller is currently running from.
//     Dev/CI fallback. When this branch fires we log a warning because
//     notifications posted from this path won't have the OpenKanban app
//     identity (they'll be attributed to the parent terminal app instead).
//
// The bundle path is preferred over os.Executable() even when the caller was
// itself launched from the bundle — the lookup is bundle-presence based,
// not parent-process based — so the daemon path is stable across invocations
// regardless of how the caller was started. On non-macOS the bundle paths
// simply don't exist on disk, so the fallback branch fires silently.
func ResolveBinary() (string, BinarySource, error) {
	if v := os.Getenv(envBinary); v != "" {
		return v, BinarySourceEnv, nil
	}

	for _, p := range bundleDaemonCandidates() {
		if isExecutableFile(p) {
			return p, BinarySourceBundle, nil
		}
	}

	exe, err := os.Executable()
	if err != nil {
		return "", BinarySourceFallback, fmt.Errorf("daemon: locate self: %w", err)
	}
	log.Printf("daemon: running daemon from %s (os.Executable) instead of OpenKanban.app — notifications will lack our app identity. Run `./scripts/install.sh` to install the bundle.", exe)
	return exe, BinarySourceFallback, nil
}

// bundleDaemonCandidates returns OpenKanban.app daemon paths to probe.
// Empty on non-macOS environments via the absence of those paths on disk
// (we don't gate on runtime.GOOS because the Stat fallthrough is cheap).
func bundleDaemonCandidates() []string {
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, "Applications", "OpenKanban.app", "Contents", "MacOS", "openkanbankd"))
	}
	candidates = append(candidates, filepath.Join("/", "Applications", "OpenKanban.app", "Contents", "MacOS", "openkanbankd"))
	return candidates
}

// isExecutableFile reports whether path is a regular file with at least one
// execute bit set. Errors collapse to false so the caller falls through.
func isExecutableFile(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	if !st.Mode().IsRegular() {
		return false
	}
	return st.Mode().Perm()&0o111 != 0
}
