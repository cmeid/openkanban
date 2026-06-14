package update

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBinaryStaleAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openkanban")
	if err := os.WriteFile(path, []byte("fake binary"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}

	// Use a reference "process start" timestamp. We deliberately do NOT
	// rely on time.Now() — the test sets mtime explicitly via Chtimes
	// so the comparison is deterministic and immune to filesystem
	// timestamp granularity quirks.
	start := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		mtime time.Time
		want  bool
	}{
		{
			name:  "mtime strictly before start",
			mtime: start.Add(-time.Hour),
			want:  false,
		},
		{
			name:  "mtime equal to start",
			mtime: start,
			want:  false, // After() is strict; equal is NOT stale.
		},
		{
			name:  "mtime strictly after start",
			mtime: start.Add(time.Hour),
			want:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.Chtimes(path, tt.mtime, tt.mtime); err != nil {
				t.Fatalf("chtimes: %v", err)
			}
			got := binaryStaleAt(path, start)
			if got != tt.want {
				t.Errorf("binaryStaleAt(%s, %s) = %v, want %v",
					path, start, got, tt.want)
			}
		})
	}
}

func TestBinaryStaleAtMissingFile(t *testing.T) {
	// A non-existent path must NOT surface as "stale" — we don't want
	// a transient FS error (or a binary that was deleted mid-flight)
	// to prompt the user to restart for no reason.
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if got := binaryStaleAt(missing, time.Now()); got {
		t.Errorf("binaryStaleAt(missing) = true, want false")
	}
}

func TestBinaryStaleUsesProcessStartTime(t *testing.T) {
	// Smoke test that BinaryStale() runs without panicking and uses
	// the package-level processStartTime. We can't easily force this
	// process's executable to have a fresh mtime, but we can at least
	// assert the start-time accessor returns a value <= now.
	st := ProcessStartTime()
	if st.After(time.Now()) {
		t.Errorf("ProcessStartTime() = %v, which is in the future", st)
	}
	// BinaryStale must not panic.
	_ = BinaryStale()
}

func TestBinaryStaleCheckIntervalIsReasonable(t *testing.T) {
	// Guard against an accidental "interval = 0" edit. A zero interval
	// would hot-loop the tea.Tick callback and stat the binary
	// continuously; cheap, but pointless.
	if BinaryStaleCheckInterval <= 0 {
		t.Errorf("BinaryStaleCheckInterval = %v, want > 0", BinaryStaleCheckInterval)
	}
}
