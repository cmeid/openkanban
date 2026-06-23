package daemon

import (
	"os"
	"testing"

	"github.com/techdufus/openkanban/internal/notify"
)

// TestMain swaps the package-level notify.Send seam to a no-op for the whole
// daemon test suite. Daemon code paths (checkSessionWedge, the OSC-9 forward)
// fire desktop notifications, and the darwin backend would otherwise reach a
// platform call from a non-bundled test binary. The cgo backend already guards
// that (no-op off-bundle), so this is defense-in-depth: it keeps daemon tests
// from depending on the platform notification backend at all, and pins the
// contract against a future migration re-introducing a fatal off-bundle path.
// No restore is needed — the process exits when m.Run returns.
func TestMain(m *testing.M) {
	notify.Send = func(string) error { return nil }
	os.Exit(m.Run())
}
