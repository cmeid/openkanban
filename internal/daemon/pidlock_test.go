package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPidLock_AcquireAndRelease(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "test.pid")

	lock, err := AcquirePidLock(path)
	if err != nil {
		t.Fatalf("AcquirePidLock: %v", err)
	}

	// Pidfile should exist and contain our pid.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	if len(data) == 0 {
		t.Errorf("pidfile is empty after AcquirePidLock")
	}

	if err := lock.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("Release should have removed pidfile, stat=%v", err)
	}
}

func TestPidLock_DoubleAcquireReturnsErrAlreadyLocked(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "test.pid")

	lock1, err := AcquirePidLock(path)
	if err != nil {
		t.Fatalf("first AcquirePidLock: %v", err)
	}
	defer lock1.Release()

	_, err = AcquirePidLock(path)
	if err == nil {
		t.Fatal("second AcquirePidLock: expected error, got nil")
	}
	var already *ErrAlreadyLocked
	if !errors.As(err, &already) {
		t.Fatalf("second AcquirePidLock: got %T (%v), want *ErrAlreadyLocked", err, err)
	}
	if already.Pid != os.Getpid() {
		t.Errorf("ErrAlreadyLocked.Pid = %d, want %d", already.Pid, os.Getpid())
	}
	if already.Error() == "" {
		t.Errorf("ErrAlreadyLocked.Error() returned empty string")
	}
}

func TestPidLock_ReleaseIsIdempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "test.pid")

	lock, err := AcquirePidLock(path)
	if err != nil {
		t.Fatalf("AcquirePidLock: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Errorf("first Release: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Errorf("second Release: %v", err)
	}
}
