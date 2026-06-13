package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/techdufus/openkanban/internal/daemon"
)

// daemonCmd is the parent command for openkanbankd-related operations.
// `openkanban daemon` itself runs the daemon in the foreground; the
// list/stop/log subcommands are client-side helpers that dial into a
// running daemon.
var daemonCmd = &cobra.Command{
	Use:           "daemon",
	Short:         "Run or query the openkanbankd daemon",
	Long:          "openkanbankd is the per-user daemon that owns long-lived agent PTYs so the TUI can be restarted without killing in-progress agent sessions. `openkanban daemon` runs the daemon in the foreground; the subcommands list/stop/log are client-side helpers.",
	SilenceUsage:  true,
	SilenceErrors: false,
	RunE:          runDaemonForeground,
}

func runDaemonForeground(cmd *cobra.Command, args []string) error {
	if err := daemon.EnsureRuntimeDir(); err != nil {
		return err
	}

	sock, err := daemon.SocketPath()
	if err != nil {
		return err
	}
	pidpath, err := daemon.PidPath()
	if err != nil {
		return err
	}

	// Log to stderr; when launched via the autostart fork helper,
	// stderr is already redirected to the daemon log file. When run
	// in the foreground it shows up on the terminal — which is what
	// you want for `openkanban daemon` invoked manually.
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	srv, err := daemon.NewServer(sock, pidpath)
	if err != nil {
		var already *daemon.ErrAlreadyLocked
		if errors.As(err, &already) {
			fmt.Fprintf(os.Stderr, "openkanbankd: already running with pid %d\n", already.Pid)
			os.Exit(1)
		}
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	return srv.Serve(ctx)
}

// daemonListCmd dials the running daemon and prints one line per
// session. Lightweight inventory, suitable for grepping from shell.
var daemonListCmd = &cobra.Command{
	Use:           "list",
	Short:         "List sessions owned by the running daemon",
	SilenceUsage:  true,
	SilenceErrors: false,
	RunE: func(cmd *cobra.Command, args []string) error {
		conn, err := dialDaemon(cmd.Context())
		if err != nil {
			return err
		}
		defer conn.Close()

		r := bufio.NewReader(conn)
		if _, err := exchange(conn, r, daemon.MsgHelloReq, daemon.HelloReq{
			ProtocolVersion: daemon.ProtocolVersion,
			BinaryVersion:   Version,
			ClientName:      "openkanban-cli",
		}); err != nil {
			return fmt.Errorf("hello: %w", err)
		}

		raw, err := exchange(conn, r, daemon.MsgListReq, daemon.ListReq{})
		if err != nil {
			return fmt.Errorf("list: %w", err)
		}
		var list daemon.ListResp
		if err := json.Unmarshal(raw, &list); err != nil {
			return fmt.Errorf("decode ListResp: %w", err)
		}

		if len(list.Sessions) == 0 {
			fmt.Println("(no sessions)")
			return nil
		}
		for _, s := range list.Sessions {
			short := s.SessionID
			if len(short) > 8 {
				short = short[:8]
			}
			fmt.Printf("%s ticket=%s session=%s pid=%d running=%v started=%s\n",
				short, s.TicketID, s.SessionName, s.PID, s.Running, time.Since(s.StartedAt).Round(time.Second))
		}
		return nil
	},
}

// daemonStopCmd asks the running daemon to shut itself down. With
// Force=false the daemon kills any live sessions defensively (and
// reports how many it killed) before exiting.
var daemonStopCmd = &cobra.Command{
	Use:           "stop",
	Short:         "Ask the running daemon to shut down",
	SilenceUsage:  true,
	SilenceErrors: false,
	RunE: func(cmd *cobra.Command, args []string) error {
		conn, err := dialDaemon(cmd.Context())
		if err != nil {
			return err
		}
		defer conn.Close()

		r := bufio.NewReader(conn)
		if _, err := exchange(conn, r, daemon.MsgHelloReq, daemon.HelloReq{
			ProtocolVersion: daemon.ProtocolVersion,
			BinaryVersion:   Version,
			ClientName:      "openkanban-cli",
		}); err != nil {
			return fmt.Errorf("hello: %w", err)
		}

		raw, err := exchange(conn, r, daemon.MsgShutdownReq, daemon.ShutdownReq{Force: false})
		if err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		var resp daemon.ShutdownResp
		if err := json.Unmarshal(raw, &resp); err != nil {
			return fmt.Errorf("decode ShutdownResp: %w", err)
		}
		fmt.Printf("stopped; killed %d sessions\n", resp.KilledSessions)
		return nil
	},
}

// daemonRestartFlagForce is the --force flag on `daemon restart` — when
// set, skip the interactive prompt even if sessions are alive.
var daemonRestartFlagForce bool

// daemonRestartCmd shuts the running daemon down (terminating any live
// sessions) and lets the next `openkanban` invocation autostart a fresh
// one. The point of "restart" is "pick up a new binary after upgrade";
// the daemon does not survive its own upgrade (see docs/AGENT_INTEGRATION.md).
//
// Safety:
//   - If sessions > 0 AND stderr is a TTY AND --force is NOT set, prompt
//     interactively before tearing them down.
//   - --force or a non-TTY stderr skips the prompt (so the command is
//     scriptable from CI / pipelines without blocking).
var daemonRestartCmd = &cobra.Command{
	Use:           "restart",
	Short:         "Terminate the running daemon (next openkanban will autostart a fresh one)",
	Long:          "Asks the running openkanbankd to shut down. Any live agent PTYs it owns are killed. The next `openkanban` invocation will autostart a fresh daemon (typically built from a newer binary — that's the whole point). With sessions still alive, prompts on an interactive TTY; pass --force to skip the prompt.",
	SilenceUsage:  true,
	SilenceErrors: false,
	RunE: func(cmd *cobra.Command, args []string) error {
		conn, err := dialDaemon(cmd.Context())
		if err != nil {
			return err
		}
		defer conn.Close()

		r := bufio.NewReader(conn)
		if _, err := exchange(conn, r, daemon.MsgHelloReq, daemon.HelloReq{
			ProtocolVersion: daemon.ProtocolVersion,
			BinaryVersion:   Version,
			ClientName:      "openkanban-cli",
		}); err != nil {
			return fmt.Errorf("hello: %w", err)
		}

		// Learn the live-session count before pulling the trigger.
		raw, err := exchange(conn, r, daemon.MsgPrepareExitReq, daemon.PrepareExitReq{})
		if err != nil {
			return fmt.Errorf("prepare_exit: %w", err)
		}
		var prep daemon.PrepareExitResp
		if err := json.Unmarshal(raw, &prep); err != nil {
			return fmt.Errorf("decode PrepareExitResp: %w", err)
		}

		liveSessions := len(prep.Sessions)
		if liveSessions > 0 && !daemonRestartFlagForce && stderrIsTTY() {
			fmt.Fprintf(os.Stderr, "daemon restart will terminate %d live agent session(s). Continue? [y/N] ", liveSessions)
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			ans := strings.TrimSpace(line)
			if ans != "y" && ans != "Y" {
				fmt.Fprintln(os.Stderr, "aborted")
				os.Exit(1)
			}
		}

		raw, err = exchange(conn, r, daemon.MsgShutdownReq, daemon.ShutdownReq{Force: false})
		if err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		var resp daemon.ShutdownResp
		if err := json.Unmarshal(raw, &resp); err != nil {
			return fmt.Errorf("decode ShutdownResp: %w", err)
		}
		fmt.Printf("daemon restart: terminated %d session(s); next openkanban will autostart a fresh daemon\n", resp.KilledSessions)
		return nil
	},
}

// stderrIsTTY reports whether stderr is attached to a character device.
// Used to decide whether `daemon restart` should prompt the user before
// killing live sessions. We deliberately use os.Stderr.Stat instead of
// pulling in golang.org/x/term so the project stays dependency-free for
// this single check; the trade-off is "no terminfo-aware detection," but
// for "is this a pipe/file vs a terminal" os.ModeCharDevice is correct.
func stderrIsTTY() bool {
	fi, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// daemonLogCmd replaces the current process with `tail -F <log>` so the
// user sees a live stream of the daemon's log. We don't reimplement
// tail in-process — exec'ing the system tool is shorter, safer, and
// behaves identically across shells.
var daemonLogCmd = &cobra.Command{
	Use:           "log",
	Short:         "Tail the daemon's log file",
	SilenceUsage:  true,
	SilenceErrors: false,
	RunE: func(cmd *cobra.Command, args []string) error {
		logPath, err := daemon.LogPath()
		if err != nil {
			return err
		}
		if _, err := os.Stat(logPath); err != nil {
			if os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "openkanbankd: log file %s does not exist yet\n", logPath)
				return nil
			}
			return err
		}

		tail, err := exec.LookPath("tail")
		if err != nil {
			fmt.Println(logPath)
			return nil
		}
		// syscall.Exec replaces this process with tail; if it
		// returns it failed.
		argv := []string{"tail", "-F", logPath}
		if err := syscall.Exec(tail, argv, os.Environ()); err != nil {
			return fmt.Errorf("exec tail: %w", err)
		}
		return nil
	},
}

// dialDaemon connects to the running daemon's socket. Unlike the
// autostart helper in internal/daemon, the CLI subcommands should NOT
// silently fork a fresh daemon when the user runs `openkanban daemon
// list` — that would lie to the user about the daemon's state. So we
// just dial and surface a clean error.
func dialDaemon(ctx context.Context) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sock, err := daemon.SocketPath()
	if err != nil {
		return nil, err
	}
	conn, err := daemon.Dial(ctx, sock)
	if err != nil {
		if errors.Is(err, daemon.ErrDaemonNotRunning) {
			return nil, fmt.Errorf("openkanbankd is not running (no socket at %s)", sock)
		}
		return nil, err
	}
	return conn, nil
}

// exchange writes a JSONReq envelope and reads the next JSONResp,
// returning the payload bytes for the caller to unmarshal into the
// expected response type.
func exchange(conn net.Conn, r *bufio.Reader, msgType string, payload any) (json.RawMessage, error) {
	raw, err := daemon.EncodeMsg(msgType, payload)
	if err != nil {
		return nil, err
	}
	if err := daemon.WriteFrame(conn, daemon.TypeJSONReq, raw); err != nil {
		return nil, err
	}
	typ, frame, err := daemon.ReadFrame(r)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("daemon closed the connection")
		}
		return nil, err
	}
	if typ != daemon.TypeJSONResp {
		return nil, fmt.Errorf("unexpected frame type 0x%02x", typ)
	}
	name, body, err := daemon.DecodeEnvelope(frame)
	if err != nil {
		return nil, err
	}
	if name == daemon.MsgErrorResp {
		var er daemon.ErrorResp
		if err := json.Unmarshal(body, &er); err != nil {
			return nil, fmt.Errorf("daemon error (undecodable): %w", err)
		}
		return nil, fmt.Errorf("daemon error %s: %s", er.Code, er.Message)
	}
	return body, nil
}

func init() {
	daemonRestartCmd.Flags().BoolVar(&daemonRestartFlagForce, "force", false, "skip the interactive 'continue?' prompt even if live sessions exist")

	daemonCmd.AddCommand(daemonListCmd)
	daemonCmd.AddCommand(daemonStopCmd)
	daemonCmd.AddCommand(daemonRestartCmd)
	daemonCmd.AddCommand(daemonLogCmd)
	rootCmd.AddCommand(daemonCmd)
}
