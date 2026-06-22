package daemonclient

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/daemon"
)

// TestEnsureStarted_AlreadyRunning: when a daemon is already listening,
// EnsureStarted must report started=false and never invoke the start path.
func TestEnsureStarted_AlreadyRunning(t *testing.T) {
	_ = startTestDaemon(t) // socket is up before we call EnsureStarted

	// Guard: if EnsureStarted wrongly took the start path it would call
	// these; fail loudly rather than silently fork in a test.
	origPlist, origStart, origFork := plistInstalledFn, startSupervisedFn, forkDaemonFn
	t.Cleanup(func() { plistInstalledFn, startSupervisedFn, forkDaemonFn = origPlist, origStart, origFork })
	plistInstalledFn = func() (bool, error) {
		t.Fatal("plistInstalled called: should not start when already running")
		return false, nil
	}
	startSupervisedFn = func() error { t.Fatal("service.Start called: should not start when already running"); return nil }
	forkDaemonFn = func() error { t.Fatal("forkDaemon called: should not start when already running"); return nil }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	started, err := EnsureStarted(ctx)
	if err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	if started {
		t.Errorf("started = true, want false (daemon already running)")
	}
}

// TestEnsureStarted_StartsWhenDown: with no daemon listening and no plist
// installed, EnsureStarted takes the fork path; once the (stubbed) fork
// binds the socket it must report started=true.
func TestEnsureStarted_StartsWhenDown(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "okes-")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sock := filepath.Join(dir, "d.sock")
	pid := filepath.Join(dir, "d.pid")
	t.Setenv("OPENKANBAN_DAEMON_SOCK", sock)
	t.Setenv("OPENKANBAN_DAEMON_PID", pid)
	t.Setenv("OPENKANBAN_DAEMON_LOG", filepath.Join(dir, "d.log"))

	origPlist, origFork := plistInstalledFn, forkDaemonFn
	t.Cleanup(func() { plistInstalledFn, forkDaemonFn = origPlist, origFork })

	plistInstalledFn = func() (bool, error) { return false, nil } // force the fork path
	var srvCancel context.CancelFunc
	forkDaemonFn = func() error {
		srv, err := daemon.NewServer(sock, pid)
		if err != nil {
			return err
		}
		var c context.Context
		c, srvCancel = context.WithCancel(context.Background())
		go func() { _ = srv.Serve(c) }()
		return nil
	}
	t.Cleanup(func() {
		if srvCancel != nil {
			srvCancel()
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	started, err := EnsureStarted(ctx)
	if err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	if !started {
		t.Errorf("started = false, want true (daemon was down)")
	}
}

// TestWaitForExit covers the three branches that matter for restart: a
// no-op pid, a process that has already exited (returns nil), and a still
// -live process (must time out rather than return nil early — proving it
// actually waits on process death, not a stat).
func TestWaitForExit(t *testing.T) {
	ctx := context.Background()

	if err := WaitForExit(ctx, 0, time.Second); err != nil {
		t.Errorf("WaitForExit(pid=0): %v, want nil", err)
	}

	// A reaped process is ESRCH → WaitForExit returns nil promptly.
	dead := exec.Command("sleep", "30")
	if err := dead.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	deadPid := dead.Process.Pid
	_ = dead.Process.Kill()
	_ = dead.Wait() // reap so kill(pid,0) returns ESRCH, not zombie-alive
	if err := WaitForExit(ctx, deadPid, 2*time.Second); err != nil {
		t.Errorf("WaitForExit on exited pid %d: %v, want nil", deadPid, err)
	}

	// A live process must drive WaitForExit to its timeout error.
	live := exec.Command("sleep", "30")
	if err := live.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() { _ = live.Process.Kill(); _ = live.Wait() })
	if err := WaitForExit(ctx, live.Process.Pid, 200*time.Millisecond); err == nil {
		t.Errorf("WaitForExit on live pid %d: got nil, want timeout error", live.Process.Pid)
	}
}
