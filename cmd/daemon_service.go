package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/service"
)

// daemonInstallServiceCmd installs openkanbankd as a system-managed
// background service. On macOS that writes a launchd LaunchAgent
// plist under ~/Library/LaunchAgents and loads it via
// `launchctl bootstrap gui/<uid>`. The service runs `openkanban daemon
// --persistent` so it survives last-client-disconnect, and is set up
// with KeepAlive={SuccessfulExit:false} so a clean `openkanban daemon
// stop` won't trigger respawn — only crashes / signals do.
//
// Refuses to run if a daemon is currently running (would race the
// service for the pidlock). The user must `openkanban daemon stop`
// first.
//
// Intentionally does NOT modify the user's config — the install-time
// prompt in scripts/install.sh is the path that also flips
// daemon.autostart=false. The bare subcommand keeps concerns
// separated; it prints a hint about the autostart flag instead.
var daemonInstallServiceCmd = &cobra.Command{
	Use:           "install-service",
	Short:         "Install openkanbankd as a system-managed background service (macOS launchd)",
	Long:          "Installs openkanbankd as a per-user background service so it runs across TUI restarts and login sessions. On macOS this writes a LaunchAgent plist and loads it via `launchctl bootstrap`. To prevent the TUI from also autostarting its own daemon, set `daemon.autostart: false` in ~/.config/openkanban/config.json (or pass --no-launch-daemon at launch).",
	SilenceUsage:  true,
	SilenceErrors: false,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Liveness check. Wrap with a short timeout so a wedged
		// daemon (socket present, accept loop hung) doesn't make us
		// hang here forever — Dial itself has a 1s dial timeout but
		// we belt-and-suspenders it.
		ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
		conn, err := daemonclient.Dial(ctx)
		cancel()
		if err == nil {
			conn.Close()
			return fmt.Errorf("openkanbankd is currently running; run `openkanban daemon stop` first, then re-run install-service")
		}
		if !errors.Is(err, daemonclient.ErrDaemonUnavailable) && !errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("checking for running daemon: %w", err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("a daemon may be running but isn't responsive within 2s; investigate with `openkanban daemon log` and either stop it or `kill` its pid before re-running install-service")
		}

		// Resolve via the shared lookup so the launchd plist points at the
		// OpenKanban.app bundle whenever it's installed — required for
		// branded macOS notifications. Falls back to the CLI binary on
		// dev machines without the bundle (notifications will lack the
		// OpenKanban identity in that case; we surface that in the
		// printed status below).
		binPath, binSrc, err := daemon.ResolveBinary()
		if err != nil {
			return fmt.Errorf("resolve daemon binary: %w", err)
		}
		logPath, err := daemon.LogPath()
		if err != nil {
			return fmt.Errorf("resolve daemon log path: %w", err)
		}

		plistPath, err := service.Install(binPath, logPath)
		if err != nil {
			return err
		}

		fmt.Printf("installed: %s\n", plistPath)
		fmt.Printf("binary:    %s %s\n", binPath, binarySourceAnnotation(binSrc))
		fmt.Printf("log:       %s\n", logPath)
		fmt.Println()
		fmt.Println("Next steps:")
		fmt.Println("  - Verify it's running:  launchctl print gui/$(id -u)/" + service.Label + " | head")
		fmt.Println("  - Set `\"daemon\": {\"autostart\": false}` in ~/.config/openkanban/config.json")
		fmt.Println("    (or run openkanban with --no-launch-daemon) so the TUI doesn't fork its own daemon.")
		return nil
	},
}

// daemonUninstallServiceCmd reverses install-service: drops any
// currently-loaded launchd instance and removes the plist file.
// Tolerant of "service not currently installed" — running it twice
// is a no-op on the second run.
var daemonUninstallServiceCmd = &cobra.Command{
	Use:           "uninstall-service",
	Short:         "Remove the system-managed background service installation",
	Long:          "Asks launchd to drop the openkanbankd LaunchAgent and removes the plist file. The currently-running daemon (if any) is signaled to exit cleanly. Safe to re-run if the service is already gone.",
	SilenceUsage:  true,
	SilenceErrors: false,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := service.Uninstall(); err != nil {
			return err
		}
		fmt.Println("uninstalled. If you want the TUI to start daemons on demand again, ensure `daemon.autostart: true` in ~/.config/openkanban/config.json (the default).")
		return nil
	},
}

// binarySourceAnnotation returns the parenthesized suffix printed after the
// resolved daemon binary path, so the user can tell at a glance whether the
// launchd service will get branded notifications (bundle) or unbranded ones
// (CLI fallback / explicit override).
func binarySourceAnnotation(src daemon.BinarySource) string {
	switch src {
	case daemon.BinarySourceBundle:
		return "(bundle — notifications use OpenKanban identity)"
	case daemon.BinarySourceEnv:
		return "(from $OPENKANBAN_DAEMON_BINARY override)"
	case daemon.BinarySourceFallback:
		return "(CLI fallback — notifications will lack OpenKanban identity; install the bundle via `scripts/install.sh`)"
	default:
		return ""
	}
}

func init() {
	daemonCmd.AddCommand(daemonInstallServiceCmd)
	daemonCmd.AddCommand(daemonUninstallServiceCmd)
}
