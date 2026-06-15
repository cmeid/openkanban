//go:build darwin

package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanityCheckBinPath(t *testing.T) {
	t.Run("rejects empty", func(t *testing.T) {
		if err := sanityCheckBinPath(""); err == nil {
			t.Error("empty binPath: want error, got nil")
		}
	})

	t.Run("rejects go-build cache path", func(t *testing.T) {
		err := sanityCheckBinPath("/var/folders/foo/bar/go-build/baz/main")
		if err == nil {
			t.Error("go-build path: want error, got nil")
		}
		if err != nil && !strings.Contains(err.Error(), "go-build") {
			t.Errorf("error should mention go-build, got: %v", err)
		}
	})

	t.Run("rejects path under TempDir", func(t *testing.T) {
		// Create a real file under TempDir so EvalSymlinks resolves.
		f, err := os.CreateTemp("", "okbd-bin-")
		if err != nil {
			t.Fatalf("create temp: %v", err)
		}
		t.Cleanup(func() { os.Remove(f.Name()) })
		f.Close()

		err = sanityCheckBinPath(f.Name())
		if err == nil {
			t.Error("temp-dir path: want error, got nil")
		}
	})

	t.Run("accepts stable path", func(t *testing.T) {
		// $HOME/go/bin/openkanban-style path — doesn't exist on the
		// test runner, but sanityCheckBinPath shouldn't require it
		// to exist; it only rejects known-bad locations.
		stable := filepath.Join(os.Getenv("HOME"), "go", "bin", "openkanban")
		if err := sanityCheckBinPath(stable); err != nil {
			t.Errorf("stable path %q: want nil, got %v", stable, err)
		}
	})
}

func TestRenderPlist(t *testing.T) {
	data, err := renderPlist(plistData{
		Label:   Label,
		BinPath: "/Users/test/go/bin/openkanban",
		LogPath: "/Users/test/.cache/openkanban/daemon.log",
		Home:    "/Users/test",
		Path:    "/usr/local/bin:/usr/bin",
	})
	if err != nil {
		t.Fatalf("renderPlist: %v", err)
	}
	s := string(data)

	required := []string{
		`<key>Label</key>`,
		`<string>dev.openkanban.daemon</string>`,
		`<string>/Users/test/go/bin/openkanban</string>`,
		`<string>daemon</string>`,
		`<string>--persistent</string>`,
		`<key>RunAtLoad</key>`,
		`<key>KeepAlive</key>`,
		`<key>SuccessfulExit</key>`,
		`<false/>`,
		`<key>ThrottleInterval</key>`,
		`<integer>10</integer>`,
		`<string>/Users/test/.cache/openkanban/daemon.log</string>`,
		`<key>HOME</key>`,
		`<string>/usr/local/bin:/usr/bin</string>`,
	}
	for _, want := range required {
		if !strings.Contains(s, want) {
			t.Errorf("rendered plist missing %q\n--- plist ---\n%s", want, s)
		}
	}
}

func TestErrIndicatesNotLoaded(t *testing.T) {
	cases := []struct {
		stderr string
		code   int
		want   bool
	}{
		{"", 3, true},
		{"", 113, true},
		{"Could not find specified service\n", 1, true},
		{"Could not find service\n", 1, true},
		{"No such process\n", 1, true},
		{"", 0, false},
		{"some other failure\n", 1, false},
	}
	for _, c := range cases {
		got := errIndicatesNotLoaded(c.stderr, c.code)
		if got != c.want {
			t.Errorf("errIndicatesNotLoaded(%q, %d) = %v; want %v", c.stderr, c.code, got, c.want)
		}
	}
}

func TestAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "test.plist")
	payload := []byte("hello\n")

	if err := atomicWrite(target, payload, 0o600); err != nil {
		t.Fatalf("atomicWrite: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("content: got %q want %q", got, payload)
	}
	// Sanity: no leftover .tmp file.
	if _, err := os.Stat(target + ".tmp"); err == nil {
		t.Error(".tmp file should not exist after successful atomicWrite")
	}
}
