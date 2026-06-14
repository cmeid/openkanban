package update

import (
	"os"
	"time"
)

// BinaryStaleCheckInterval is the cadence at which long-lived openkanban
// processes (TUI, daemon) re-check whether their on-disk binary has been
// replaced under them. 30s is short enough that a `go install` (or
// `openkanban update`) in another shell is surfaced within a sensible
// window, while being far longer than typical filesystem stat latency so
// the check is effectively free.
const BinaryStaleCheckInterval = 30 * time.Second

// processStartTime captures when this process started. Used to detect
// binary-replacement updates (go install / openkanban update from another
// shell). Set at package init so every consumer compares against the same
// authoritative timestamp.
var processStartTime = time.Now()

// BinaryStale reports whether the executable on disk has been modified
// since this process started. False if the executable can't be located
// or stat-ed (don't surface a spurious "stale" warning on filesystem
// errors).
func BinaryStale() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	return binaryStaleAt(exe, processStartTime)
}

// binaryStaleAt is the pure, testable core of BinaryStale: returns true
// iff the file at path has an mtime strictly later than since. Returns
// false if the file can't be stat-ed (defensive: a transient FS error
// must not surface as a spurious "stale binary" warning).
func binaryStaleAt(path string, since time.Time) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.ModTime().After(since)
}

// ProcessStartTime exposes the captured start time for tests.
func ProcessStartTime() time.Time { return processStartTime }
