package daemon

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// PidLock is an advisory file lock around the daemon's pidfile. The
// lock is exclusive and non-blocking; AcquirePidLock returns
// *ErrAlreadyLocked if another process already holds it. Release
// drops the lock and best-effort removes the pidfile so the next
// daemon doesn't accumulate stale files.
type PidLock struct {
	f    *os.File
	path string
}

// ErrAlreadyLocked is returned by AcquirePidLock when another process
// holds the lock. Pid is the PID recorded in the existing pidfile, or
// 0 if the file is empty / malformed.
type ErrAlreadyLocked struct {
	Pid int
}

func (e *ErrAlreadyLocked) Error() string {
	if e.Pid > 0 {
		return fmt.Sprintf("daemon: already running with pid %d", e.Pid)
	}
	return "daemon: pidfile is already locked"
}

// AcquirePidLock opens path (creating it if necessary), takes an
// exclusive flock(LOCK_EX|LOCK_NB), and writes the current pid to the
// file. On EWOULDBLOCK / EAGAIN it reads the existing pid from the
// file and returns *ErrAlreadyLocked.
//
// The pidfile's parent directory must already exist; the caller is
// expected to have invoked EnsureRuntimeDir first.
func AcquirePidLock(path string) (*PidLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemon: open pidfile %s: %w", path, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// Another process holds the lock. Read the stored pid (best
		// effort) so the caller can produce a helpful message.
		pid := readPidFile(f)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, &ErrAlreadyLocked{Pid: pid}
		}
		return nil, fmt.Errorf("daemon: flock pidfile %s: %w", path, err)
	}

	// Truncate any stale contents, then write our own pid. We do this
	// AFTER acquiring the lock so a racing process that ignored the
	// lock can't see a half-written value.
	if err := f.Truncate(0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil, fmt.Errorf("daemon: truncate pidfile %s: %w", path, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil, fmt.Errorf("daemon: seek pidfile %s: %w", path, err)
	}
	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil, fmt.Errorf("daemon: write pidfile %s: %w", path, err)
	}

	return &PidLock{f: f, path: path}, nil
}

// readPidFile rewinds f and parses an integer pid out of its first
// line. Returns 0 on any error (the caller treats that as "unknown
// pid" rather than failing).
func readPidFile(f *os.File) int {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0
	}
	buf := make([]byte, 32)
	n, _ := f.Read(buf)
	if n == 0 {
		return 0
	}
	line := strings.TrimSpace(string(buf[:n]))
	if idx := strings.IndexAny(line, "\r\n"); idx >= 0 {
		line = line[:idx]
	}
	pid, err := strconv.Atoi(line)
	if err != nil {
		return 0
	}
	return pid
}

// Release drops the flock and closes the underlying file descriptor.
// It also best-effort removes the pidfile from disk; the next daemon
// will recreate it. Safe to call multiple times.
func (l *PidLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	err := l.f.Close()
	l.f = nil
	if l.path != "" {
		_ = os.Remove(l.path)
	}
	return err
}
