// Package service installs and uninstalls openkanbankd as a
// system-managed background service. On macOS that's a launchd
// LaunchAgent under the user's gui/<uid> domain. The implementation
// is split by GOOS via build tags; this file holds the launchd
// integration. A future linux file would add a systemd user-unit
// backend with the same exported shape.
package service

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
)

// Label is the launchd service identifier. Used as the plist filename
// (Label + ".plist") and as the gui/<uid>/<label> service target for
// launchctl bootstrap / bootout / print.
const Label = "dev.openkanban.daemon"

// plistTemplate renders the LaunchAgent plist. We write the XML by
// hand rather than via encoding/xml because Apple's plist dialect has
// subtle escaping rules (CDATA, integer vs string vs bool tags) that
// are easier to audit as a literal template than as marshaller calls.
//
// ExitTimeOut budget: cleanup() and handleShutdown in internal/daemon
// kill sessions sequentially with shutdownGraceSeconds=3 each (see
// internal/daemon/server.go). Worst-case wall clock for a clean
// shutdown is wg.Wait() + 3N seconds + cleanup overhead, where N is
// the live session count. We pick 30s as the launchd hard-kill
// budget: covers ~8 concurrent sessions plus wg.Wait() overhead and
// signals that the daemon's clean-shutdown path is the load-bearing
// one. If shutdownGraceSeconds or the kill loop's concurrency changes,
// recompute this budget — the two values must move together.
//
// OPENKANBAN_DAEMON_SOURCE=launchd lets the daemon log line at startup
// announce who spawned it (vs tui-fork / manual), which is the
// diagnostic we wished we had when the lifecycle bug was discovered.
const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{.Label}}</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{.BinPath}}</string>
        <string>daemon</string>
        <string>--persistent</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>ProcessType</key>
    <string>Interactive</string>
    <key>ThrottleInterval</key>
    <integer>10</integer>
    <key>ExitTimeOut</key>
    <integer>30</integer>
    <key>StandardOutPath</key>
    <string>{{.LogPath}}</string>
    <key>StandardErrorPath</key>
    <string>{{.LogPath}}</string>
    <key>WorkingDirectory</key>
    <string>{{.Home}}</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HOME</key>
        <string>{{.Home}}</string>
        <key>PATH</key>
        <string>{{.Path}}</string>
        <key>OPENKANBAN_DAEMON_SOURCE</key>
        <string>launchd</string>
    </dict>
</dict>
</plist>
`

// PlistInstalled reports whether the LaunchAgent plist file exists on
// disk. A true result does not mean the service is loaded by launchd —
// use Status() for that. The distinction matters for the supervision
// warning: a plist on disk means Install was run and the user expects
// launchd to manage the daemon, so a tui-forked daemon is surprising.
func PlistInstalled() (bool, error) {
	p, err := PlistPath()
	if err != nil {
		return false, err
	}
	_, statErr := os.Stat(p)
	if statErr == nil {
		return true, nil
	}
	if os.IsNotExist(statErr) {
		return false, nil
	}
	return false, statErr
}

// PlistPath returns the absolute path to the LaunchAgent plist that
// would be created by Install.
func PlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("service: resolve home dir: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
}

// Install registers openkanbankd as a launchd LaunchAgent under the
// user's gui/<uid> domain. The flow is intentionally idempotent so
// re-running install-service refreshes the loaded plist:
//
//  1. Sanity-check binPath (reject transient go-run / go-build paths).
//  2. Ensure ~/Library/LaunchAgents exists.
//  3. Best-effort `launchctl bootout` to drop any currently-loaded
//     instance (tolerated when nothing is loaded — see
//     errIndicatesNotLoaded).
//  4. Atomic write of the plist (tmp + rename).
//  5. `launchctl bootstrap` to load the new plist.
//
// Returns the plist path on success so the caller can print it.
func Install(binPath, logPath string) (string, error) {
	if err := sanityCheckBinPath(binPath); err != nil {
		return "", err
	}

	plistPath, err := PlistPath()
	if err != nil {
		return "", err
	}
	plistDir := filepath.Dir(plistPath)
	if err := os.MkdirAll(plistDir, 0o700); err != nil {
		return "", fmt.Errorf("service: mkdir %s: %w", plistDir, err)
	}

	// Best-effort drop of any currently-loaded instance so the new
	// plist replaces (rather than collides with) it.
	target, err := serviceTarget()
	if err != nil {
		return "", err
	}
	if _, stderr, code, runErr := runLaunchctl("bootout", target); runErr != nil {
		// Real exec failure (launchctl missing, etc.) — surface it.
		return "", fmt.Errorf("service: launchctl bootout exec: %w", runErr)
	} else if code != 0 && !errIndicatesNotLoaded(stderr.String(), code) {
		return "", fmt.Errorf("service: launchctl bootout failed (exit %d): %s", code, strings.TrimSpace(stderr.String()))
	}

	// Render + write the plist atomically.
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("service: resolve home dir: %w", err)
	}
	rendered, err := renderPlist(plistData{
		Label:   Label,
		BinPath: binPath,
		LogPath: logPath,
		Home:    home,
		Path:    os.Getenv("PATH"),
	})
	if err != nil {
		return "", err
	}
	if err := atomicWrite(plistPath, rendered, 0o600); err != nil {
		return "", err
	}

	// Load it.
	domain, err := domainTarget()
	if err != nil {
		return "", err
	}
	if _, stderr, code, runErr := runLaunchctl("bootstrap", domain, plistPath); runErr != nil {
		return "", fmt.Errorf("service: launchctl bootstrap exec: %w", runErr)
	} else if code != 0 {
		return plistPath, fmt.Errorf("service: launchctl bootstrap failed (exit %d): %s", code, strings.TrimSpace(stderr.String()))
	}

	return plistPath, nil
}

// Uninstall removes the LaunchAgent plist and asks launchd to drop
// any currently-loaded instance. Tolerates "not currently loaded"
// errors so re-running uninstall after the service is already gone
// is a no-op.
func Uninstall() error {
	plistPath, err := PlistPath()
	if err != nil {
		return err
	}

	target, err := serviceTarget()
	if err != nil {
		return err
	}
	if _, stderr, code, runErr := runLaunchctl("bootout", target); runErr != nil {
		return fmt.Errorf("service: launchctl bootout exec: %w", runErr)
	} else if code != 0 && !errIndicatesNotLoaded(stderr.String(), code) {
		return fmt.Errorf("service: launchctl bootout failed (exit %d): %s", code, strings.TrimSpace(stderr.String()))
	}

	if err := os.Remove(plistPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("service: remove %s: %w", plistPath, err)
	}
	return nil
}

// Status reports whether launchd currently has the service loaded.
// On macOS this shells out to `launchctl print gui/<uid>/<label>` and
// parses the `pid` line; an exit code of 113 (or stderr containing
// "Could not find service") means not loaded.
func Status() (running bool, pid int, err error) {
	target, err := serviceTarget()
	if err != nil {
		return false, 0, err
	}
	stdout, stderr, code, runErr := runLaunchctl("print", target)
	if runErr != nil {
		return false, 0, fmt.Errorf("service: launchctl print exec: %w", runErr)
	}
	if code != 0 {
		if errIndicatesNotLoaded(stderr.String(), code) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("service: launchctl print failed (exit %d): %s", code, strings.TrimSpace(stderr.String()))
	}

	// Parse the `pid = N` line (launchctl print's output is
	// roughly key/value pairs). The service is "loaded" even if pid=0
	// — that means launchd has it registered but it's not currently
	// executing. We only report running=true when pid > 0.
	for _, line := range strings.Split(stdout.String(), "\n") {
		line = strings.TrimSpace(line)
		const prefix = "pid = "
		if strings.HasPrefix(line, prefix) {
			n, perr := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, prefix)))
			if perr == nil && n > 0 {
				return true, n, nil
			}
		}
	}
	// Loaded but no current pid (between respawns, perhaps).
	return false, 0, nil
}

// ErrNotInstalled is returned by Start when no LaunchAgent plist is on
// disk — the caller should fork its own daemon rather than ask launchd
// to manage one the user never opted into.
var ErrNotInstalled = errors.New("service: launchd plist not installed")

// errExConfig is a sentinel returned by kickstart when launchctl exits
// with EX_CONFIG (78), indicating the job registration was invalidated
// (e.g. by a codesign --force during openkanban update). Start catches
// this sentinel and performs a bootout + bootstrap + kickstart recovery.
var errExConfig = errors.New("service: launchctl EX_CONFIG (78) — registration invalidated")

// Start asks launchd to run the already-installed service now. It's the
// autostart counterpart to Install: where Install (re)writes and
// bootstraps the plist, Start assumes the plist already exists and just
// makes launchd run it — bootstrapping it first if it isn't loaded, then
// kickstarting to force a running instance (closing the window between a
// crash / KeepAlive respawn and the socket actually being bound).
//
// This is what lets the TUI autostart path defer to launchd instead of
// forking a tui-fork daemon that would grab the socket + pidlock and
// shadow launchd's supervised instance. Returns ErrNotInstalled when no
// plist is on disk (caller should fork instead) and ErrUnsupported on
// non-Darwin.
func Start() error {
	installed, err := PlistInstalled()
	if err != nil {
		return err
	}
	if !installed {
		return ErrNotInstalled
	}

	target, err := serviceTarget()
	if err != nil {
		return err
	}

	plistPath, err := PlistPath()
	if err != nil {
		return err
	}
	domain, err := domainTarget()
	if err != nil {
		return err
	}

	// Fast path: if the service is already loaded, kickstart forces a
	// running instance now (idempotent when it's already running).
	if ok, kerr := kickstart(target); kerr != nil {
		if !errors.Is(kerr, errExConfig) {
			return kerr
		}
		// EX_CONFIG: registration was invalidated (e.g. codesign during
		// update). Recover by dropping the stale registration and
		// re-bootstrapping. Start is only called when no daemon is bound,
		// so bootout here never kills live sessions.
		return recoverExConfig(target, domain, plistPath)
	} else if ok {
		return nil
	}

	// Not loaded → bootstrap the plist (RunAtLoad=true starts it), then
	// kickstart to remove the ThrottleInterval / socket-bind race.
	if _, stderr, code, runErr := runLaunchctl("bootstrap", domain, plistPath); runErr != nil {
		return fmt.Errorf("service: launchctl bootstrap exec: %w", runErr)
	} else if code != 0 {
		if errIndicatesExConfig(code) {
			// EX_CONFIG on bootstrap: stale registration in the domain.
			// Recover the same way as the kickstart path.
			return recoverExConfig(target, domain, plistPath)
		}
		return fmt.Errorf("service: launchctl bootstrap failed (exit %d): %s", code, strings.TrimSpace(stderr.String()))
	}
	if ok, kerr := kickstart(target); kerr != nil {
		return kerr
	} else if !ok {
		return fmt.Errorf("service: bootstrapped %s but launchctl could not find it to kickstart", target)
	}
	return nil
}

// recoverExConfig handles EX_CONFIG (78) from launchctl by doing a
// best-effort bootout of the stale registration, re-bootstrapping from
// the plist already on disk, and kickstarting. Returns nil on success or
// the original EX_CONFIG sentinel on second failure (no retry loop).
func recoverExConfig(target, domain, plistPath string) error {
	// Best-effort: ignore errors — the job may already be unloaded.
	runLaunchctl("bootout", target) //nolint:errcheck
	if _, stderr, code, runErr := runLaunchctl("bootstrap", domain, plistPath); runErr != nil {
		return fmt.Errorf("service: EX_CONFIG recovery bootstrap exec: %w", runErr)
	} else if code != 0 {
		return fmt.Errorf("service: EX_CONFIG recovery bootstrap failed (exit %d): %s", code, strings.TrimSpace(stderr.String()))
	}
	if ok, kerr := kickstart(target); kerr != nil {
		return kerr
	} else if !ok {
		return fmt.Errorf("service: EX_CONFIG recovery: bootstrapped %s but kickstart could not find it", target)
	}
	return nil
}

// kickstart runs `launchctl kickstart <target>` (no -k: never restart a
// running instance). Returns (true, nil) on success, (false, nil) when
// the service isn't loaded (caller should bootstrap first),
// (false, errExConfig) when launchd signals EX_CONFIG (78) meaning the
// registration was invalidated (e.g. codesign after bootstrap), and
// (false, err) on any other real failure.
func kickstart(target string) (bool, error) {
	_, stderr, code, runErr := runLaunchctl("kickstart", target)
	if runErr != nil {
		return false, fmt.Errorf("service: launchctl kickstart exec: %w", runErr)
	}
	if code == 0 {
		return true, nil
	}
	// We assume kickstart's "service not loaded" signature matches the one
	// print / bootout emit (exit 3/113 or a "could not find service"
	// string). If a future macOS returns something else here, Start treats
	// it as a hard error and the caller falls back to forking — degraded,
	// but the user still gets a daemon.
	if errIndicatesNotLoaded(stderr.String(), code) {
		return false, nil
	}
	// EX_CONFIG (78): codesign-invalidated registration. Return a sentinel
	// so Start can perform a bootout + bootstrap + kickstart recovery.
	if errIndicatesExConfig(code) {
		return false, errExConfig
	}
	return false, fmt.Errorf("service: launchctl kickstart failed (exit %d): %s", code, strings.TrimSpace(stderr.String()))
}

// --- internals ---

type plistData struct {
	Label   string
	BinPath string
	LogPath string
	Home    string
	Path    string
}

func renderPlist(d plistData) ([]byte, error) {
	tmpl, err := template.New("plist").Parse(plistTemplate)
	if err != nil {
		return nil, fmt.Errorf("service: parse plist template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, d); err != nil {
		return nil, fmt.Errorf("service: render plist template: %w", err)
	}
	return buf.Bytes(), nil
}

// atomicWrite writes data to path via a sibling .tmp + rename so a
// partially-written plist never appears on disk (which would make
// launchctl bootstrap fail with a confusing parse error).
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return fmt.Errorf("service: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("service: rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// domainTarget returns "gui/<uid>" — the launchctl domain a user-mode
// service is bootstrapped into.
func domainTarget() (string, error) {
	return "gui/" + strconv.Itoa(os.Getuid()), nil
}

// serviceTarget returns "gui/<uid>/<label>" — the launchctl service
// target used by bootout and print.
func serviceTarget() (string, error) {
	d, err := domainTarget()
	if err != nil {
		return "", err
	}
	return d + "/" + Label, nil
}

// runLaunchctl execs `launchctl <args...>` and returns its stdout,
// stderr, and exit code. A non-zero exit code is NOT returned as
// runErr — only an exec failure (binary missing, permission denied)
// is. Callers check `code` to distinguish "command ran but failed"
// from "couldn't run command at all."
func runLaunchctl(args ...string) (stdout, stderr bytes.Buffer, code int, err error) {
	cmd := exec.Command("launchctl", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		// Distinguish "non-zero exit" (which we want to surface via
		// code) from "couldn't run" (binary missing, etc.).
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return stdout, stderr, exitErr.ExitCode(), nil
		}
		return stdout, stderr, -1, runErr
	}
	return stdout, stderr, 0, nil
}

// errIndicatesNotLoaded returns true when launchctl's stderr / exit
// code combination means "the requested service isn't currently
// loaded" — the canonical idempotent-bootout case. Sonoma+ varies
// these slightly across point releases, so we check several known
// signatures.
func errIndicatesNotLoaded(stderr string, code int) bool {
	if code == 3 || code == 113 {
		return true
	}
	low := strings.ToLower(stderr)
	for _, marker := range []string{
		"could not find specified service",
		"could not find service",
		"no such process",
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// errIndicatesExConfig returns true when launchctl's exit code signals
// EX_CONFIG (78) — the error launchd emits when a codesign-invalidated
// job registration is kicked. The most common cause: `codesign --force`
// on the app bundle after bootstrap (e.g. during `openkanban update`).
// We key primarily off the exit code; the stderr message varies across
// macOS versions ("Invalid property list", "could not find service", etc.).
func errIndicatesExConfig(code int) bool {
	return code == 78
}

// sanityCheckBinPath rejects binPath values that point at transient
// build artifacts: anything under os.TempDir() or with "/go-build" in
// the path is almost certainly a `go run` / build-cache leftover that
// will 404 after the temp dir is GC'd. Installing such a path would
// produce a service that mysteriously stops working a few minutes
// later — refuse with a clear error.
func sanityCheckBinPath(binPath string) error {
	if binPath == "" {
		return errors.New("service: empty binPath")
	}
	if strings.Contains(binPath, "/go-build") {
		return fmt.Errorf("service: binary at %s looks like a go-build cache artifact; install a stable binary first via `go install` or scripts/install.sh", binPath)
	}
	if tmp := os.TempDir(); tmp != "" {
		// Resolve symlinks on tmp ("/tmp" -> "/private/tmp" on macOS)
		// so the prefix check is robust.
		realTmp, err := filepath.EvalSymlinks(tmp)
		if err == nil {
			tmp = realTmp
		}
		realBin, err := filepath.EvalSymlinks(binPath)
		if err == nil {
			binPath = realBin
		}
		if strings.HasPrefix(binPath, tmp+string(os.PathSeparator)) || binPath == tmp {
			return fmt.Errorf("service: binary at %s lives under %s and looks like a transient build artifact; install a stable binary first via `go install` or scripts/install.sh", binPath, tmp)
		}
	}
	return nil
}
