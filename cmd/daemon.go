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

// daemonFlagPersistent controls whether the daemon stays alive when
// the last client disconnects. Default (false) preserves the original
// TUI-managed lifecycle; --persistent is set in the launchd plist so
// the system-managed daemon outlives any one TUI session.
var daemonFlagPersistent bool

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

	srv, err := daemon.NewServerWithOptions(sock, pidpath, daemon.Options{Persistent: daemonFlagPersistent})
	if err != nil {
		var already *daemon.ErrAlreadyLocked
		if errors.As(err, &already) {
			fmt.Fprintf(os.Stderr, "openkanbankd: already running with pid %d\n", already.Pid)
			os.Exit(1)
		}
		return err
	}

	// SIGHUP is treated as a clean-shutdown trigger alongside SIGINT
	// and SIGTERM. macOS launchd does not typically send SIGHUP to
	// GUI-domain LaunchAgents on logout, but the cost of handling it
	// is one signal entry and Go's default disposition (terminate
	// with exit 129) is the wrong default for a daemon that owns
	// live PTYs — exit 129 would orphan sessions instead of running
	// cleanup() with its 3s SIGTERM-then-SIGKILL grace per session.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
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
			ClientName:      daemon.ClientNameCLI,
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

// daemonStopFlagForce is the --force flag on `daemon stop` — when
// set, skip the interactive prompt even if sessions are alive and no
// TUI is watching.
var daemonStopFlagForce bool

// daemonStopCmd asks the running daemon to shut itself down. With
// Force=false the daemon kills any live sessions defensively (and
// reports how many it killed) before exiting.
//
// Safety (matches daemon restart's pattern): if live sessions exist
// AND stderr is a TTY AND --force is NOT set, prompt interactively
// before tearing them down. A watching TUI is NOT a substitute for
// explicit consent here — a stop invoked from a separate shell
// (e.g. `scripts/install.sh` reaching for the pidlock) would
// otherwise silently kill in-flight agent work the human never
// consented to. Pass --force to skip the prompt for scripting.
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
			ClientName:      daemon.ClientNameCLI,
		}); err != nil {
			return fmt.Errorf("hello: %w", err)
		}

		// Learn the live-session count and whether any other TUI is
		// watching before pulling the trigger.
		raw, err := exchange(conn, r, daemon.MsgPrepareExitReq, daemon.PrepareExitReq{})
		if err != nil {
			return fmt.Errorf("prepare_exit: %w", err)
		}
		var prep daemon.PrepareExitResp
		if err := json.Unmarshal(raw, &prep); err != nil {
			return fmt.Errorf("decode PrepareExitResp: %w", err)
		}

		liveSessions := len(prep.Sessions)
		// Any live session is enough to prompt — a watching TUI is
		// not a substitute for explicit consent, since the human
		// running `daemon stop` (or scripts/install.sh) may not even
		// realize a TUI is attached in another shell.
		if liveSessions > 0 && !daemonStopFlagForce && stderrIsTTY() {
			fmt.Fprintf(os.Stderr, "daemon stop will terminate %d live agent session(s). Continue? [y/N] ", liveSessions)
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
			ClientName:      daemon.ClientNameCLI,
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

// daemonCloseFlagYes is the -y/--yes flag on `daemon close` — when set,
// skip the interactive confirmation prompt.
var daemonCloseFlagYes bool

// daemonCloseFlagGrace is the --grace duration on `daemon close`. It
// becomes the SIGTERM-to-SIGKILL grace window the daemon honors when
// terminating the resolved session(s). The default of 3 seconds matches
// the daemon's internal shutdownGraceSeconds constant (see
// internal/daemon/server.go); that constant is package-private so we
// hard-code the same value here rather than reach across the package
// boundary.
var daemonCloseFlagGrace time.Duration

// daemonCloseDefaultGrace mirrors internal/daemon.shutdownGraceSeconds
// (3s). Kept here so the CLI doesn't need to import the daemon's private
// constant; if the daemon's grace changes, this default should be revised
// in lock-step.
const daemonCloseDefaultGrace = 3 * time.Second

// minSessionPrefixLen is the shortest SessionID prefix `daemon close`
// will accept. `daemon list` prints the leading 8 chars; 4 is the soft
// floor that keeps ambiguity manageable without requiring users to
// retype the full 16-char SessionID.
const minSessionPrefixLen = 4

// daemonClosePlan is what daemonCloseRun resolves an arbitrary arg into:
// either a Kill of a single SessionID or a TicketDone for a TicketID
// that may map to multiple sessions (defense-in-depth against pre-dedup
// duplicates).
type daemonClosePlan struct {
	// Kind is "kill" or "ticket_done".
	Kind string
	// Sessions are the sessions the daemon will terminate. For Kind=kill
	// this has exactly one entry; for Kind=ticket_done it has >=1.
	Sessions []daemon.SessionInfo
	// TicketID is set when Kind=ticket_done.
	TicketID string
}

// daemonCloseRun is the testable core of `openkanban daemon close`. It
// dials the daemon, lists sessions, resolves arg into a plan, and (if
// !dryRun) executes the kill/ticket_done RPC. The cobra RunE wraps this
// with TTY-aware confirmation between the resolve and the execute steps.
//
// Returns the resolved plan even when execute is false, so tests can
// assert on resolution independent of execution.
func daemonCloseRun(ctx context.Context, arg string, grace time.Duration, execute bool) (daemonClosePlan, error) {
	conn, err := dialDaemon(ctx)
	if err != nil {
		return daemonClosePlan{}, err
	}
	defer conn.Close()

	r := bufio.NewReader(conn)
	if _, err := exchange(conn, r, daemon.MsgHelloReq, daemon.HelloReq{
		ProtocolVersion: daemon.ProtocolVersion,
		BinaryVersion:   Version,
		ClientName:      daemon.ClientNameCLI,
	}); err != nil {
		return daemonClosePlan{}, fmt.Errorf("hello: %w", err)
	}

	raw, err := exchange(conn, r, daemon.MsgListReq, daemon.ListReq{})
	if err != nil {
		return daemonClosePlan{}, fmt.Errorf("list: %w", err)
	}
	var list daemon.ListResp
	if err := json.Unmarshal(raw, &list); err != nil {
		return daemonClosePlan{}, fmt.Errorf("decode ListResp: %w", err)
	}

	plan, err := resolveDaemonCloseArg(arg, list.Sessions)
	if err != nil {
		return plan, err
	}

	if !execute {
		return plan, nil
	}

	switch plan.Kind {
	case "kill":
		s := plan.Sessions[0]
		raw, err := exchange(conn, r, daemon.MsgKillReq, daemon.KillReq{
			SessionID:    s.SessionID,
			GraceSeconds: int(grace / time.Second),
		})
		if err != nil {
			return plan, fmt.Errorf("kill: %w", err)
		}
		var resp daemon.KillResp
		if err := json.Unmarshal(raw, &resp); err != nil {
			return plan, fmt.Errorf("decode KillResp: %w", err)
		}
		return plan, nil
	case "ticket_done":
		raw, err := exchange(conn, r, daemon.MsgTicketDoneReq, daemon.TicketDoneReq{
			TicketID: plan.TicketID,
		})
		if err != nil {
			return plan, fmt.Errorf("ticket_done: %w", err)
		}
		var resp daemon.TicketDoneResp
		if err := json.Unmarshal(raw, &resp); err != nil {
			return plan, fmt.Errorf("decode TicketDoneResp: %w", err)
		}
		return plan, nil
	default:
		return plan, fmt.Errorf("internal: unknown plan kind %q", plan.Kind)
	}
}

// resolveDaemonCloseArg maps a user-supplied positional argument to a
// daemonClosePlan using the documented precedence: exact SessionID > 4+
// char SessionID prefix > exact TicketID. No match returns an error; an
// ambiguous prefix returns an error listing the candidates.
func resolveDaemonCloseArg(arg string, sessions []daemon.SessionInfo) (daemonClosePlan, error) {
	if arg == "" {
		return daemonClosePlan{}, fmt.Errorf("empty id")
	}

	// 1. Exact SessionID match (single, by definition — SessionID is
	//    unique in the daemon's map).
	for _, s := range sessions {
		if s.SessionID == arg {
			return daemonClosePlan{Kind: "kill", Sessions: []daemon.SessionInfo{s}}, nil
		}
	}

	// 2. SessionID prefix match (min length guard). The 8-char display
	//    width of `daemon list` is the typical input, but anything
	//    >= minSessionPrefixLen is fair game.
	if len(arg) >= minSessionPrefixLen {
		var prefixMatches []daemon.SessionInfo
		for _, s := range sessions {
			if strings.HasPrefix(s.SessionID, arg) {
				prefixMatches = append(prefixMatches, s)
			}
		}
		if len(prefixMatches) == 1 {
			return daemonClosePlan{Kind: "kill", Sessions: prefixMatches}, nil
		}
		if len(prefixMatches) > 1 {
			var lines []string
			for _, s := range prefixMatches {
				short := s.SessionID
				if len(short) > 8 {
					short = short[:8]
				}
				lines = append(lines, fmt.Sprintf("  %s ticket=%s pid=%d", short, s.TicketID, s.PID))
			}
			return daemonClosePlan{}, fmt.Errorf("ambiguous prefix %q matches %d sessions:\n%s",
				arg, len(prefixMatches), strings.Join(lines, "\n"))
		}
	}

	// 3. Exact TicketID match. May yield multiple via pre-dedup defense;
	//    that's fine — handleTicketDone iterates and kills all.
	var ticketMatches []daemon.SessionInfo
	for _, s := range sessions {
		if s.TicketID == arg {
			ticketMatches = append(ticketMatches, s)
		}
	}
	if len(ticketMatches) > 0 {
		return daemonClosePlan{
			Kind:     "ticket_done",
			Sessions: ticketMatches,
			TicketID: arg,
		}, nil
	}

	return daemonClosePlan{}, fmt.Errorf("no session for %q; daemon list shows %d session(s)", arg, len(sessions))
}

// daemonCloseCmd is the user-facing recovery hatch for terminating a
// single daemon-owned session. Other commands (`daemon stop`, `daemon
// restart`) kill ALL sessions; `close` operates on exactly one (or, in
// the rare pre-dedup-duplicate case, all sessions sharing one TicketID,
// matching the daemon's own ticket-done semantics).
//
// Resolution precedence:
//
//	exact SessionID  →  4+ char SessionID prefix  →  exact TicketID
//
// The first non-empty match wins. An ambiguous prefix is reported as an
// error listing the candidates. Empty arg, no match, or a daemon that
// isn't running all return a clean error.
var daemonCloseCmd = &cobra.Command{
	Use:           "close <id>",
	Short:         "Gracefully terminate a single daemon session",
	Long:          "Resolves <id> as an exact SessionID, then a SessionID prefix (>= 4 chars), then a TicketID, and asks the daemon to terminate the matching session(s). The daemon honors a SIGTERM-then-SIGKILL grace window (see --grace). With sessions still alive, prompts on an interactive TTY; pass -y to skip the prompt.",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: false,
	RunE: func(cmd *cobra.Command, args []string) error {
		// First pass: resolve only — don't execute yet, so we can show
		// the plan and ask for confirmation before pulling the trigger.
		plan, err := daemonCloseRun(cmd.Context(), args[0], daemonCloseFlagGrace, false)
		if err != nil {
			return err
		}

		// Echo the matched session(s) in the same one-line format `daemon
		// list` uses, so users see what they're about to kill.
		for _, s := range plan.Sessions {
			short := s.SessionID
			if len(short) > 8 {
				short = short[:8]
			}
			fmt.Printf("%s ticket=%s session=%s pid=%d running=%v started=%s\n",
				short, s.TicketID, s.SessionName, s.PID, s.Running, time.Since(s.StartedAt).Round(time.Second))
		}

		if !daemonCloseFlagYes && stderrIsTTY() {
			if !confirm(os.Stdin, os.Stderr, "Close this session? [y/N] ") {
				return nil
			}
		}

		// Second pass: re-resolve and execute. We dial a fresh conn (the
		// first call's defer has already closed it) and re-run the
		// resolution — the live session set MAY have changed between
		// prompt and confirmation; refusing to assume a stale plan is
		// still valid is the safer move.
		plan, err = daemonCloseRun(cmd.Context(), args[0], daemonCloseFlagGrace, true)
		if err != nil {
			return err
		}

		switch plan.Kind {
		case "kill":
			s := plan.Sessions[0]
			short := s.SessionID
			if len(short) > 8 {
				short = short[:8]
			}
			fmt.Printf("closed: ticket=%s session=%s pid=%d\n", s.TicketID, short, s.PID)
		case "ticket_done":
			fmt.Printf("closed: %d session(s) for ticket=%s\n", len(plan.Sessions), plan.TicketID)
		}
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
	daemonCmd.Flags().BoolVar(&daemonFlagPersistent, "persistent", false, "Stay alive when the last client disconnects (used by launchd / systemd integration)")
	daemonStopCmd.Flags().BoolVar(&daemonStopFlagForce, "force", false, "skip the interactive 'continue?' prompt even if live sessions exist")
	daemonRestartCmd.Flags().BoolVar(&daemonRestartFlagForce, "force", false, "skip the interactive 'continue?' prompt even if live sessions exist")
	daemonCloseCmd.Flags().BoolVarP(&daemonCloseFlagYes, "yes", "y", false, "skip the interactive confirmation prompt")
	daemonCloseCmd.Flags().DurationVar(&daemonCloseFlagGrace, "grace", daemonCloseDefaultGrace, "SIGTERM-to-SIGKILL grace window when terminating the resolved session(s)")

	daemonCmd.AddCommand(daemonListCmd)
	daemonCmd.AddCommand(daemonStopCmd)
	daemonCmd.AddCommand(daemonRestartCmd)
	daemonCmd.AddCommand(daemonCloseCmd)
	daemonCmd.AddCommand(daemonLogCmd)
	rootCmd.AddCommand(daemonCmd)
}
