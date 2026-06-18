//go:build !darwin

// Package service install/uninstall stub for non-Darwin platforms.
// Today launchd (macOS) is the only supported backend. A Linux
// implementation backed by systemd user units can land in a sibling
// file (launchd_linux.go) when we're ready — the exported shape is
// already pinned by the Darwin implementation.
package service

import "errors"

// Label is the service identifier; kept consistent with Darwin so
// callers can reference it without GOOS-specific imports.
const Label = "dev.openkanban.daemon"

// ErrUnsupported is the canonical "this platform doesn't support
// system-managed daemon installation yet" error.
var ErrUnsupported = errors.New("openkanban: system service installation is only supported on macOS today")

// PlistPath returns ErrUnsupported on non-Darwin.
func PlistPath() (string, error) {
	return "", ErrUnsupported
}

// Install returns ErrUnsupported on non-Darwin.
func Install(binPath, logPath string) (string, error) {
	return "", ErrUnsupported
}

// Uninstall returns ErrUnsupported on non-Darwin.
func Uninstall() error {
	return ErrUnsupported
}

// Status returns ErrUnsupported on non-Darwin.
func Status() (running bool, pid int, err error) {
	return false, 0, ErrUnsupported
}

// Start returns ErrUnsupported on non-Darwin. The autostart path reaches
// it only when PlistInstalled reported true, which never happens off
// Darwin, so callers always fall back to forking their own daemon.
func Start() error {
	return ErrUnsupported
}

// PlistInstalled returns false on non-Darwin: there is no launchd plist
// concept, so there is no supervision concern to report.
func PlistInstalled() (bool, error) {
	return false, nil
}
