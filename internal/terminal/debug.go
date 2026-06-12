package terminal

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// ptyDebugLog appends every byte read from a PTY to a debug file when
// the OPENKANBAN_PTY_DEBUG_LOG env var is set to a writable path.
// Used for diagnosing terminal-emulation bugs by capturing the raw
// byte stream a child agent emits and replaying it through the
// terminal emulator in isolation.
//
// Format: a header line of the form
//
//	\n--- <rfc3339nano> pane=<id> bytes=<n> ---\n
//
// followed by the raw bytes of the chunk. Headers make it easy to
// `tail -f` the log and to bisect to a problematic chunk; raw bytes
// preserve every escape sequence verbatim.
//
// Disabled by default. Set OPENKANBAN_PTY_DEBUG_LOG=/some/path before
// launching openkanban (or update-openkanban) to enable.
func ptyDebugLog(paneID string, data []byte) {
	f := ptyDebugWriter()
	if f == nil {
		return
	}
	debugLogMu.Lock()
	defer debugLogMu.Unlock()
	fmt.Fprintf(f, "\n--- %s pane=%s bytes=%d ---\n", time.Now().Format(time.RFC3339Nano), paneID, len(data))
	_, _ = f.Write(data)
}

var (
	debugLogMu      sync.Mutex
	debugLogFile    *os.File
	debugLogInitErr error
	debugLogInit    sync.Once
)

// ptyDebugWriter returns the lazily-opened log file, or nil if
// debug logging is disabled. The file is opened once per process;
// changes to the env var mid-run are not picked up.
func ptyDebugWriter() *os.File {
	debugLogInit.Do(func() {
		path := os.Getenv("OPENKANBAN_PTY_DEBUG_LOG")
		if path == "" {
			return
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			debugLogInitErr = err
			return
		}
		debugLogFile = f
	})
	return debugLogFile
}
