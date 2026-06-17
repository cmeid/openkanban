package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestUnderAny(t *testing.T) {
	base := filepath.Clean("/home/u/.config/openkanban")
	other := filepath.Clean("/home/u/.cache/openkanban")
	tests := []struct {
		name     string
		clean    string
		dirs     []string
		expected bool
	}{
		{
			name:     "path under dir",
			clean:    filepath.Join(base, "projects.json"),
			dirs:     []string{base, other},
			expected: true,
		},
		{
			name:     "path equals dir",
			clean:    base,
			dirs:     []string{base},
			expected: true,
		},
		{
			name:     "path outside dirs",
			clean:    filepath.Clean("/tmp/somewhere/config.json"),
			dirs:     []string{base, other},
			expected: false,
		},
		{
			name:     "empty dir entries ignored",
			clean:    filepath.Clean("/anything"),
			dirs:     []string{"", ""},
			expected: false,
		},
		{
			name:     "nil dirs",
			clean:    base,
			dirs:     nil,
			expected: false,
		},
		{
			name:     "sibling prefix is not under",
			clean:    filepath.Clean("/home/u/.config/openkanban-other/x"),
			dirs:     []string{base},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := underAny(tt.clean, tt.dirs); got != tt.expected {
				t.Errorf("underAny(%q, %v) = %v, want %v", tt.clean, tt.dirs, got, tt.expected)
			}
		})
	}
}

// TestGuardHomeWrite_Probe proves the guard fires when a write targets a
// guarded dir, and stays silent for an unguarded one.
func TestGuardHomeWrite_Probe(t *testing.T) {
	dir := t.TempDir()
	restore := SetGuardedDirsForTest([]string{dir})
	defer restore()

	// A write under the guarded dir must panic.
	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatalf("expected GuardHomeWrite to panic for a guarded path, got no panic")
			}
			msg, ok := r.(string)
			if !ok {
				t.Fatalf("expected recovered value to be a string, got %T: %v", r, r)
			}
			if !strings.Contains(msg, "REAL user dir") {
				t.Errorf("recovered panic %q does not contain %q", msg, "REAL user dir")
			}
		}()
		GuardHomeWrite(filepath.Join(dir, "projects.json"))
	}()

	// A write to a different (unguarded) dir must NOT panic.
	other := t.TempDir()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("expected no panic for unguarded path %q, got panic: %v", other, r)
			}
		}()
		GuardHomeWrite(other)
	}()
}

// TestGuardHomeWrite_NoGuardedDirs verifies that with no guarded dirs the
// guard never panics for any path.
func TestGuardHomeWrite_NoGuardedDirs(t *testing.T) {
	restore := SetGuardedDirsForTest(nil)
	defer restore()

	for _, p := range []string{"/anything", "/home/u/.config/openkanban/x.json", t.TempDir()} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("expected no panic for %q with nil guarded dirs, got: %v", p, r)
				}
			}()
			GuardHomeWrite(p)
		}()
	}
}
