package daemonclient

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadPidFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "daemon.pid")
	if err := os.WriteFile(p, []byte("12345\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pid, err := readPidFile(p)
	if err != nil || pid != 12345 {
		t.Fatalf("readPidFile=%d,%v want 12345,nil", pid, err)
	}
	if _, err := readPidFile(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("expected error for missing pidfile")
	}
}
