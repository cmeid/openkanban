//go:build !darwin

// Platform backend stub for non-darwin builds. notify.Send falls back
// to a silent no-op so callers can invoke it unconditionally.
package notify

// platformSend is the non-darwin backend invoked by notify.Send's
// default. It does nothing and returns nil so notification calls on
// linux / windows / etc. are silently dropped.
func platformSend(body string) error {
	return nil
}
