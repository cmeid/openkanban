package daemonclient

import (
	"errors"
	"testing"
)

// TestStartDaemon_PrefersLaunchdOverFork pins the fix for "the TUI takes
// control of the daemon": when a launchd plist is installed, the
// autostart path must ask launchd to start its supervised instance
// (service.Start) and must NOT fork a tui-fork that would shadow it. It
// only falls back to forking when no plist is installed, the
// installed-check errors, or the launchd start itself fails.
func TestStartDaemon_PrefersLaunchdOverFork(t *testing.T) {
	tests := []struct {
		name           string
		installed      bool
		installedErr   error
		startErr       error
		wantStartCalls int
		wantForkCalls  int
		wantErr        bool
	}{
		{
			name:           "installed + launchd start ok → launchd, no fork",
			installed:      true,
			wantStartCalls: 1,
			wantForkCalls:  0,
		},
		{
			name:           "installed + launchd start fails → fall back to fork",
			installed:      true,
			startErr:       errors.New("boom"),
			wantStartCalls: 1,
			wantForkCalls:  1,
		},
		{
			name:           "not installed → fork, never touch launchd",
			installed:      false,
			wantStartCalls: 0,
			wantForkCalls:  1,
		},
		{
			name:           "plist check errors → treat as not installed, fork",
			installedErr:   errors.New("stat failed"),
			wantStartCalls: 0,
			wantForkCalls:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var startCalls, forkCalls int

			origPlist, origStart, origFork := plistInstalledFn, startSupervisedFn, forkDaemonFn
			t.Cleanup(func() {
				plistInstalledFn, startSupervisedFn, forkDaemonFn = origPlist, origStart, origFork
			})

			plistInstalledFn = func() (bool, error) { return tt.installed, tt.installedErr }
			startSupervisedFn = func() error { startCalls++; return tt.startErr }
			forkDaemonFn = func() error { forkCalls++; return nil }

			err := startDaemon()

			if (err != nil) != tt.wantErr {
				t.Fatalf("startDaemon() err = %v, wantErr %v", err, tt.wantErr)
			}
			if startCalls != tt.wantStartCalls {
				t.Errorf("service.Start calls = %d, want %d", startCalls, tt.wantStartCalls)
			}
			if forkCalls != tt.wantForkCalls {
				t.Errorf("forkDaemon calls = %d, want %d", forkCalls, tt.wantForkCalls)
			}
		})
	}
}

// TestStartDaemon_ForkErrorSurfaces confirms that when the fork fallback
// is reached and fork itself fails, startDaemon returns the error rather
// than swallowing it (DialOrStart wraps it as ErrDaemonUnavailable).
func TestStartDaemon_ForkErrorSurfaces(t *testing.T) {
	origPlist, origStart, origFork := plistInstalledFn, startSupervisedFn, forkDaemonFn
	t.Cleanup(func() {
		plistInstalledFn, startSupervisedFn, forkDaemonFn = origPlist, origStart, origFork
	})

	wantErr := errors.New("fork blew up")
	plistInstalledFn = func() (bool, error) { return false, nil } // no launchd → fork
	startSupervisedFn = func() error { t.Fatal("service.Start must not be called when no plist"); return nil }
	forkDaemonFn = func() error { return wantErr }

	if err := startDaemon(); !errors.Is(err, wantErr) {
		t.Fatalf("startDaemon() err = %v, want %v", err, wantErr)
	}
}
