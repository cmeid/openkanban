package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// setPidfile points PidPath() at a temp file (via OPENKANBAN_DAEMON_PID)
// and writes the given contents. Passing want=="" creates no file, so
// PidPath() resolves to a path that does not exist.
func setPidfile(t *testing.T, contents string, create bool) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daemon.pid")
	t.Setenv("OPENKANBAN_DAEMON_PID", path)
	if create {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatalf("write pidfile: %v", err)
		}
	}
}

func TestUnresponsiveHint_LivePidNamesKillCommand(t *testing.T) {
	// os.Getpid() is by definition alive — the test process itself.
	self := os.Getpid()
	setPidfile(t, strconv.Itoa(self)+"\n", true)

	msg := UnresponsiveHint()

	pidStr := strconv.Itoa(self)
	if !strings.Contains(msg, "pid "+pidStr) {
		t.Errorf("message should name the live PID %d; got:\n%s", self, msg)
	}
	if !strings.Contains(msg, "kill -9 "+pidStr) {
		t.Errorf("message should give the exact kill command for PID %d; got:\n%s", self, msg)
	}
	if !strings.Contains(msg, "daemon restart") {
		t.Errorf("message should still point at restart; got:\n%s", msg)
	}
}

func TestUnresponsiveHint_FallsBackWithoutAKillablePid(t *testing.T) {
	tests := []struct {
		name    string
		create  bool
		content string
	}{
		{name: "no pidfile", create: false},
		{name: "garbage contents", create: true, content: "not-a-pid\n"},
		{name: "empty file", create: true, content: ""},
		// Max int32: far above macOS's default PID ceiling, so it will not
		// name a live process — exercises the dead/stale-PID fallback.
		{name: "dead pid", create: true, content: "2147483647\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setPidfile(t, tt.content, tt.create)

			msg := UnresponsiveHint()

			if strings.Contains(msg, "kill -9") {
				t.Errorf("no live PID known — message must not suggest a kill command; got:\n%s", msg)
			}
			if !strings.Contains(msg, "daemon restart") {
				t.Errorf("fallback must still point at restart; got:\n%s", msg)
			}
		})
	}
}
