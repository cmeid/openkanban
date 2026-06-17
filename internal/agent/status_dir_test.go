package agent

import (
	"path/filepath"
	"testing"
)

// TestStatusDir_LiteralSync pins the StatusDir() resolution so a future
// relocation of the default cannot silently un-guard the status tree:
// the default literal MUST stay in sync with config.computeGuardedDirs'
// "openkanban-status" literal (config can't import agent — would cycle).
func TestStatusDir_LiteralSync(t *testing.T) {
	t.Run("default under HOME/.cache/openkanban-status", func(t *testing.T) {
		t.Setenv("OPENKANBAN_STATUS_DIR", "")
		home := t.TempDir()
		t.Setenv("HOME", home)

		want := filepath.Join(home, ".cache", "openkanban-status")
		if got := StatusDir(); got != want {
			t.Errorf("StatusDir() = %q, want %q", got, want)
		}
	})

	t.Run("OPENKANBAN_STATUS_DIR override wins", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("OPENKANBAN_STATUS_DIR", dir)

		if got := StatusDir(); got != dir {
			t.Errorf("StatusDir() = %q, want %q", got, dir)
		}
	})
}
