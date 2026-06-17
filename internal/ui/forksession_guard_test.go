package ui

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoForkSessionLiteralInUI is a build-time guard against the
// reintroduction of `--fork-session` as a Claude spawn argument. The
// flag was removed in task/enforce-one-to-one-session because forking
// silently violated the 1:1 ticket↔session invariant the daemon
// enforces at the PTY layer (each fork divergent JSONLs from the same
// ticket; re-spawn lands in a stale fork, not the live conversation).
//
// This test walks internal/ui/ and cmd/ source (excluding *_test.go
// files since the literal string may legitimately appear in a "must
// not contain" comparison like this one) and fails if any production
// source file mentions the literal `--fork-session`. A future PR that
// re-adds the flag must intentionally weaken this guard — making the
// regression structurally impossible to land accidentally.
//
// The literal string is wrapped in concatenation so this file itself
// doesn't trip the guard.
func TestNoForkSessionLiteralInUI(t *testing.T) {
	forbidden := "--fork" + "-session"

	roots := []string{
		"./",        // internal/ui/
		"../../cmd", // cmd/
	}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() {
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(data), forbidden) {
				t.Errorf("forbidden literal %q found in %s — see "+
					"task/enforce-one-to-one-session for why --fork-session "+
					"was eliminated. If a future feature legitimately needs "+
					"this flag back, weaken or remove this guard with an "+
					"explicit comment naming the new invariant.",
					forbidden, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}
