package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/techdufus/openkanban/internal/agent"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/daemonclient"
	"github.com/techdufus/openkanban/internal/git"
	"github.com/techdufus/openkanban/internal/project"
	"github.com/techdufus/openkanban/internal/ui"
	"github.com/techdufus/openkanban/internal/update"
	"github.com/techdufus/openkanban/internal/watch"
)

func Run(cfg *config.Config, filterPath, version string) error {
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
	daemonClient, daemonErr := daemonclient.New(daemonCtx)
	daemonCancel()
	if daemonErr != nil {
		if errors.Is(daemonErr, daemonclient.ErrProtocolVersionSkew) {
			fmt.Fprintln(os.Stderr, "openkanban: daemon version skew detected — run `openkanban daemon restart` to refresh; running in degraded mode without the daemon.")
		} else {
			log.Printf("openkanban: daemon unavailable, agents cannot be spawned (%v)", daemonErr)
		}
		daemonClient = nil
	}

	updateChecker := update.NewChecker(version)
	model := ui.NewModel(cfg, globalStore, registry, agentMgr, opencodeServer, filterProjectID, updateChecker, daemonClient)

	defer func() {
		model.Cleanup()
		if daemonClient != nil {
			_ = daemonClient.Close()
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseAllMotion())

	// Start the file watcher; failure is non-fatal (TUI works without
	// hot-reload, just no live updates for external edits).
	if watcher := startFileWatcher(registry, program); watcher != nil {
		defer watcher.Close()
	}

	go func() {
		<-sigChan
		model.Cleanup()
		program.Quit()
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
// watched. Users adding a project externally need to quit + relaunch
// to pick up its tickets.
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

		fmt.Printf("  %s (%s)\n", p.Name, p.ID[:8])
		fmt.Printf("    Path: %s\n", p.RepoPath)
		fmt.Printf("    Tickets: %d total, %d in progress\n", total, inProgress)
		fmt.Println()
	}

	return nil
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

	if err := registry.Delete(target.ID); err != nil {
		return fmt.Errorf("failed to delete project: %w", err)
	}

	fmt.Printf("Deleted project '%s' (%s)\n", target.Name, target.RepoPath)
	return nil
}
