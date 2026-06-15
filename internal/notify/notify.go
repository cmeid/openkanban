// Package notify delivers desktop notifications.
//
// The public entry point is the package-level Send variable. Production
// callers invoke notify.Send(body); tests can swap the var to capture
// calls without invoking the platform backend.
//
// On darwin Send dispatches via NSUserNotification (see notify_darwin.go).
// On other platforms it's a silent no-op (notify_other.go).
package notify

// Send delivers a desktop notification with the given body. Returns nil
// on success, or a platform-specific error.
//
// Send is a package-level var (not a function) so tests can overwrite
// it to capture invocations without firing a real notification:
//
//	prev := notify.Send
//	notify.Send = func(body string) error { recorded = body; return nil }
//	defer func() { notify.Send = prev }()
//
// Not goroutine-safe with respect to the swap itself — tests that
// reassign Send should not run in parallel with production code paths.
var Send = sendDefault

// sendDefault dispatches to the platform-specific implementation
// (platformSend). Kept as a separate named function so the package
// var's default value is stable across builds.
func sendDefault(body string) error { return platformSend(body) }
