package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"
	"github.com/techdufus/openkanban/internal/agent"
	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemon"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/git"
	"github.com/techdufus/openkanban/internal/project"
	"github.com/techdufus/openkanban/internal/ui"
	"github.com/techdufus/openkanban/internal/watch"
)

// Run launches the TUI. autostartDaemon controls whether the TUI
// tries to fork openkanbankd if it's not already running:
//   - true (default): old behavior — call daemonclient.New, which
//     forks the daemon on dial failure.
//   - false: call daemonclient.NewNoAutostart, which dials only.
//     Used when openkanbankd is managed by launchd / systemd, or
//     when the user passed --no-launch-daemon.
//
// If the daemon can't be reached — dial failure, version skew, or a
// bounded preflight List that times out against a wedged daemon — Run
// prints an actionable PID+kill/restart hint and returns an error
// (clean exit) rather than launching a daemon-less board. A board with
// no daemon can't spawn or attach, and proceeding once risked hanging
// startup on a later unbounded RPC.
func Run(cfg *config.Config, filterPath string, autostartDaemon bool) error {
	// MUST be the first statement: project.LoadGlobalTicketStore below
	// fans out to ~11 log.Printf sites in internal/project/{tickets,
	// migration}.go that fire on migrations and duplicate-removal. Any
	// later insertion point silently corrupts Bubble Tea's alt-screen
	// rendering on first launch.
	if logCloser := redirectTUILog(); logCloser != nil {
		defer logCloser.Close()
	}

	registry, err := project.LoadRegistry()
	if err != nil {
		return fmt.Errorf("failed to load project registry: %w", err)
	}

	globalStore, err := project.LoadGlobalTicketStore(registry)
	if err != nil {
		return fmt.Errorf("failed to load tickets: %w", err)
	}

	if !globalStore.HasProjects() {
		return fmt.Errorf("no projects registered. Create one with: openkanban new")
	}

	var filterProjectID string
	if filterPath != "" {
		absPath, _ := filepath.Abs(filterPath)
		absPath = git.ResolveMainRepo(absPath)
		if p, err := registry.FindByPath(absPath); err == nil {
			filterProjectID = p.ID
		}
	}

	agentMgr := agent.NewManager(cfg)

	opencodeServer := agent.NewOpencodeServer(cfg)

	// Only auto-start server if default agent is opencode
	if cfg.Defaults.DefaultAgent == "opencode" {
		if err := opencodeServer.Start(); err != nil {
			return fmt.Errorf("failed to start opencode server: %w", err)
		}
		defer opencodeServer.Stop()
	}

	// Connect to (and autostart if needed) the openkanbankd daemon.
	// Failure is non-fatal: the TUI degrades to "no spawn, no
	// daemon-owned panes" but the rest of the board still works. This
	// matches the design's "client nil-checks everywhere" contract.
	//
	// Version-skew is called out separately: it almost always means the
	// user upgraded the openkanban binary but a daemon from the previous
	// version is still running. We print an actionable hint to stderr
	// (the daemon's log eats log.Printf output if launched from autostart,
	// but app.Run is reached from the foreground TUI command, so stderr
	// goes to the user's terminal) and then continue in degraded mode.
	daemonCtx, daemonCancel := context.WithTimeout(context.Background(), 5*time.Second)
	var daemonClient *daemonclient.Client
	var daemonErr error
	if autostartDaemon {
		daemonClient, daemonErr = daemonclient.New(daemonCtx)
	} else {
		daemonClient, daemonErr = daemonclient.NewNoAutostart(daemonCtx)
	}
	daemonCancel()
	if daemonErr != nil {
		// Can't reach a working daemon. Don't launch a daemon-less board
		// (no spawn, no attach), and don't risk a later unbounded RPC
		// hanging startup — tell the user how to recover, then exit.
		if errors.Is(daemonErr, daemonclient.ErrProtocolVersionSkew) {
			fmt.Fprintln(os.Stderr, "openkanban: daemon version skew detected — run `openkanban daemon restart` to refresh.")
		} else {
			fmt.Fprintln(os.Stderr, daemon.UnresponsiveHint())
		}
		return errors.New("openkanbankd unavailable")
	}

	// Preflight: a wedged daemon passes the dial + hello above but stalls
	// on real RPCs. Probe with a bounded List BEFORE building the TUI so a
	// wedge is caught here (message + exit) rather than hanging startup on
	// a later unbounded call. On success the snapshot seeds NewModel — the
	// only startup daemon RPC, now gated and bounded.
	ownedByDaemon, perr := ui.PreflightListSessions(daemonClient)
	if perr != nil {
		if autostartDaemon && errors.Is(perr, daemon.ErrDaemonUnresponsive) {
			fmt.Fprintln(os.Stderr, "openkanban: daemon is wedged — force-restarting it...")
			_ = daemonClient.Close()
			rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
			fresh, rerr := daemonclient.ForceRestartDaemon(rctx)
			rcancel()
			if rerr == nil {
				daemonClient = fresh
				ownedByDaemon, perr = ui.PreflightListSessions(daemonClient)
			}
		}
		if perr != nil {
			fmt.Fprintln(os.Stderr, daemon.UnresponsiveHint())
			_ = daemonClient.Close()
			return errors.New("openkanbankd unresponsive")
		}
	}

	model := ui.NewModel(cfg, globalStore, registry, agentMgr, opencodeServer, filterProjectID, ownedByDaemon, daemonClient, autostartDaemon)

	// Arm the diagnostic stall watchdog for the real TUI (Cleanup stops
	// it). Captures a goroutine dump if the Update/View loop freezes.
	model.StartStallMonitor()

	defer func() {
		restoreHostTerminalModes(os.Stdout)
		model.Cleanup()
		if daemonClient != nil {
			_ = daemonClient.Close()
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// WithMouseAllMotion enables mouse tracking on the host terminal; bubbletea
	// emits ?1002l/?1003l/?1006l on exit but never resets ?1007 (alt-scroll).
	// restoreHostTerminalModes (in the defer above) is the safety net for any
	// raw host DEC-mode write: every sequence added here — or reintroduced in
	// future, e.g. ?1007h for trackpad scroll (see #155 arrow-key leak) — MUST
	// also be reset there. restoreHostTerminalModes writes to os.Stdout; if
	// tea.WithOutput(...) is ever added here, update it to match the same fd.
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseAllMotion())

	// Give the stall watchdog teeth: on a sustained "starved" stall it now
	// detaches the focused agent view to the board (program.Send is the
	// goroutine-safe entry point, same as the signal handler below). Wired
	// here because the program must exist first; the watchdog stays
	// diagnostic-only until this sink is set.
	model.SetStallRecoverySink(program.Send)

	// Start the file watcher; failure is non-fatal (TUI works without
	// hot-reload, just no live updates for external edits).
	if watcher := startFileWatcher(registry, program); watcher != nil {
		defer watcher.Close()
	}

	// Route SIGINT/SIGTERM through the exit-guard so signals don't
	// silently destroy live agent sessions. program.Send is the
	// goroutine-safe Bubble Tea entry point; the Update loop picks up
	// QuitRequestedMsg and dispatches handleQuitRequested, which fires
	// the PrepareExit RPC and either silent-quits (peers attached or
	// no live sessions) or opens the exit-confirm modal. The deferred
	// model.Cleanup() above still runs when program.Run returns, so
	// no cleanup work is lost.
	//
	// User-visible behavior change: Ctrl-C with a live session no
	// longer instantly exits — the user must answer the modal (kill or
	// cancel) just like pressing `q`. With NO live sessions the modal
	// short-circuits to tea.Quit so Ctrl-C still feels instant.
	go func() {
		<-sigChan
		program.Send(ui.QuitRequestedMsg{})
	}()

	_, err = program.Run()
	return err
}

// startFileWatcher constructs a watch.Watcher rooted at the config
// dir, subscribes to each existing project's tickets subdir, and
// pumps debounced events into the Bubble Tea program. Returns nil if
// the watcher could not be created; the TUI continues without
// hot-reload in that case.
//
// Note: projects added or removed mid-session are not retroactively
// watched by fsnotify. The 1s board resync tick
// (internal/ui/board_resync.go) covers this gap — newly-added
// projects and their tickets surface within ~1s without a restart,
// just slower than the sub-second fsnotify fast path.
func startFileWatcher(registry *project.ProjectRegistry, program *tea.Program) *watch.Watcher {
	configDir, err := config.ConfigDir()
	if err != nil {
		log.Printf("openkanban: file watcher disabled (config dir resolution failed: %v)", err)
		return nil
	}
	w, err := watch.New(configDir)
	if err != nil {
		log.Printf("openkanban: file watcher disabled (%v)", err)
		return nil
	}
	for _, p := range registry.List() {
		if perr := w.AddProject(p.ID); perr != nil {
			log.Printf("openkanban: watch project %s: %v", p.ID, perr)
		}
	}
	go func() {
		for ev := range w.Events() {
			program.Send(ui.FsChangedMsg{
				Domain:    ev.Domain,
				Path:      ev.Path,
				ProjectID: ev.ProjectID,
			})
		}
	}()
	return w
}

func CreateProject(cfg *config.Config, name, repoPath string) error {
	if _, err := os.Stat(filepath.Join(repoPath, ".git")); err != nil {
		return fmt.Errorf("not a git repository: %s", repoPath)
	}

	registry, err := project.LoadRegistry()
	if err != nil {
		return fmt.Errorf("failed to load project registry: %w", err)
	}

	if existing, _ := registry.FindByPath(repoPath); existing != nil {
		return fmt.Errorf("project already exists for %s: %s", repoPath, existing.Name)
	}

	p := project.NewProject(name, repoPath)
	// Project settings only store explicit user overrides.
	// Empty values cascade to global config defaults at runtime.

	if err := registry.Add(p); err != nil {
		return fmt.Errorf("failed to save project: %w", err)
	}

	fmt.Printf("Created project '%s' for %s\n", name, repoPath)
	fmt.Printf("Project ID: %s\n", p.ID)
	return nil
}

func ListProjects() error {
	registry, err := project.LoadRegistry()
	if err != nil {
		return err
	}

	projects := registry.List()
	if len(projects) == 0 {
		fmt.Println("No projects found. Create one with: openkanban new")
		return nil
	}

	fmt.Println("Available projects:")
	fmt.Println()

	for _, p := range projects {
		tickets, err := project.LoadTicketStore(p)
		if err != nil {
			continue
		}

		total := tickets.Count()
		inProgress := tickets.CountByStatus("in_progress")

		fmt.Printf("  %s (%s)\n", p.Name, shortID(p.ID))
		fmt.Printf("    Path: %s\n", p.RepoPath)
		fmt.Printf("    Tickets: %d total, %d in progress\n", total, inProgress)
		fmt.Println()
	}

	return nil
}

// shortID truncates a project ID to its first 8 characters for display,
// returning the full ID unchanged when it is shorter than 8 chars (e.g.
// "proj-1"). Guards against the slice-bounds panic of a bare id[:8].
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func DeleteProject(nameOrID string) error {
	registry, err := project.LoadRegistry()
	if err != nil {
		return err
	}

	var target *project.Project
	for _, p := range registry.List() {
		if p.Name == nameOrID || p.ID == nameOrID || (len(p.ID) >= 8 && p.ID[:8] == nameOrID) {
			target = p
			break
		}
	}

	if target == nil {
		return fmt.Errorf("project not found: %s", nameOrID)
	}

	// Daemon-cleanup pass BEFORE the registry delete: any live daemon
	// session whose TicketID belongs to this project must be wound down
	// so we don't orphan sessions whose backing tickets are about to
	// disappear from disk. We load the project's ticket store to build
	// the set of TicketIDs we own, then ask the daemon to TicketDone
	// each one that's currently live.
	//
	// Failure-tolerance contract:
	//   - ticket-store load failure → log + skip the daemon pass and
	//     proceed with the registry delete. Better to let the user
	//     finish removing a corrupted project than wedge them on a
	//     parser error.
	//   - daemon not running → no sessions to clean up; proceed.
	//   - daemon up but TicketDone for an individual ticket fails →
	//     log and continue with the rest; the daemon's own
	//     handleTicketDone is idempotent and a future restart will
	//     reap stragglers, so a transient RPC error must not block
	//     the user-visible delete.
	cleanupDaemonSessionsForProject(target)

	if err := registry.Delete(target.ID); err != nil {
		return fmt.Errorf("failed to delete project: %w", err)
	}

	fmt.Printf("Deleted project '%s' (%s)\n", target.Name, target.RepoPath)
	return nil
}

// cleanupDaemonSessionsForProject is the daemon half of DeleteProject.
// Split out so the failure-tolerance is centralised: every failure
// inside this function is logged and swallowed — the caller continues
// with the registry delete regardless.
func cleanupDaemonSessionsForProject(target *project.Project) {
	store, err := project.LoadTicketStore(target)
	if err != nil {
		log.Printf("openkanban: skipping daemon cleanup for project %s: load ticket store: %v",
			target.ID, err)
		return
	}

	owned := make(map[board.TicketID]struct{}, len(store.Tickets))
	for id := range store.Tickets {
		owned[id] = struct{}{}
	}
	if len(owned) == 0 {
		return
	}

	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := daemonclient.NewNoAutostart(dialCtx)
	if err != nil {
		// Daemon not running is the expected fallback path — nothing
		// to clean up, no problem. Any other error gets logged but is
		// still swallowed so the registry delete can proceed.
		if !errors.Is(err, daemonclient.ErrDaemonUnavailable) {
			log.Printf("openkanban: skipping daemon cleanup for project %s: dial daemon: %v",
				target.ID, err)
		}
		return
	}
	defer client.Close()

	listCtx, listCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer listCancel()
	list, err := client.List(listCtx)
	if err != nil {
		log.Printf("openkanban: skipping daemon cleanup for project %s: list sessions: %v",
			target.ID, err)
		return
	}

	for _, info := range list.Sessions {
		tid := board.TicketID(info.TicketID)
		if tid == "" {
			continue
		}
		if _, ok := owned[tid]; !ok {
			continue
		}
		doneCtx, doneCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, derr := client.TicketDone(doneCtx, info.TicketID)
		doneCancel()
		if derr != nil {
			log.Printf("openkanban: daemon TicketDone for %s (project %s) failed: %v",
				info.TicketID, target.ID, derr)
		}
	}
}

// redirectTUILog points the default log package at a file under
// ~/.cache/openkanban/tui.log (honoring OPENKANBAN_TUI_LOG). Without
// this, the TUI process's many log.Printf calls (daemonclient,
// ui/daemon_subscribe, project migration) write to stderr and corrupt
// Bubble Tea's alt-screen rendering.
//
// Returns the open file so Run can defer Close. Returns nil if logs
// were discarded (failure path) — Close is a no-op in that case.
func redirectTUILog() io.Closer {
	_ = daemon.EnsureRuntimeDir()

	path, err := TUILogPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "openkanban: could not resolve TUI log path: %v (logs disabled)\n", err)
		log.SetOutput(io.Discard)
		return nil
	}

	config.GuardHomeWrite(path)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "openkanban: could not open TUI log %s: %v (logs disabled)\n", path, err)
		log.SetOutput(io.Discard)
		return nil
	}

	log.SetOutput(f)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	return f
}

// writeHostTerminalModeReset emits the DEC private-mode resets that
// openkanban issues on exit to normalize the host terminal. Separated from
// restoreHostTerminalModes so it is unit-testable against any io.Writer
// (the isatty guard would otherwise require a real PTY).
//
//   - ?1007l: alt-scroll OFF — the actual leak vector. PR #155 set ?1007h on
//     the host so trackpad scroll became arrow keys; bubbletea never resets
//     1007, so a stuck ?1007h corrupted arrow-key handling in unrelated
//     programs (e.g. arrow selection in a later `claude` askuserquestion).
//     #155 is reverted; this is the safety net so any re-introduction cannot
//     leak again.
//   - ?1000l/?1002l/?1003l/?1006l: mouse-tracking OFF — belt-and-suspenders
//     (bubbletea already disables 1000/1003/1006 on its own exit).
func writeHostTerminalModeReset(w io.Writer) (int, error) {
	return io.WriteString(w, "\x1b[?1007l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l")
}

// restoreHostTerminalModes defensively resets host-terminal DEC modes that
// bubbletea's teardown does not cover. Safe to call after program.Run()
// returns: bubbletea has relinquished the terminal by then, and every
// sequence is a no-op when its mode is already off. No-op when stdout is not
// a terminal (emitting escapes into a pipe would corrupt redirected output).
func restoreHostTerminalModes(f *os.File) {
	if f == nil || !isatty.IsTerminal(f.Fd()) {
		return
	}
	_, _ = writeHostTerminalModeReset(f)
}
